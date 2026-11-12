package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// Taking a graphics card away from a host driver that still has it open can
// hang the whole node. Seen on real hardware: a Quadro P1000 was hot-plugged
// into a running VM while the display manager held it, the nvidia driver
// logged "Attempting to remove device with non-zero usage count", and the
// host stopped responding 90 seconds later. So: a graphics card is never
// hot-plugged, and a VM that has one does not start while processes on the
// node have the card open.

// parseGPUUsers reads the output of the node command's gpu-users verb; complete is false when the
// run did not finish.
func parseGPUUsers(out string) (users []string, complete bool) {
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case l == "ok":
			complete = true
		case !seen[l]:
			seen[l] = true
			users = append(users, l)
		}
	}
	sort.Strings(users)
	return users, complete
}

// hostDriverInUse says why the card at addr must not be taken from the host
// now, or "" when nothing on the node has it open (or it is already detached).
func hostDriverInUse(h *hostconn.Host, d hostDevice) string {
	if d.Kind != "pci" || d.Category != "gpu" || d.Driver == "" || d.Driver == "vfio-pci" || d.Driver == "pci-stub" {
		return ""
	}
	out, err := gpuUsers(h, d.ID)
	users, complete := parseGPUUsers(out)
	if err != nil || !complete {
		// Without root the node's other processes cannot be inspected; say so rather than guess.
		return fmt.Sprintf("it could not be checked whether the node is using graphics card %s (driver %s); this needs the node command on the node", d.ID, d.Driver)
	}
	if len(users) == 0 {
		return ""
	}
	return fmt.Sprintf("graphics card %s is in use on the node by %s (driver %s). Stop them first (for a desktop: systemctl stop gdm), or the node can hang when the card is taken away", d.ID, strings.Join(users, ", "), d.Driver)
}

// startGuard refuses to start a VM whose passed-through graphics card is
// still in use on the node.
func startGuard(h *hostconn.Host, dom libvirt.Domain) error {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return nil // the start itself will report what is wrong
	}
	attached, err := parseVMHostdevs(raw)
	if err != nil || len(attached) == 0 {
		return nil
	}
	devs, _, _, err := collectHostDevs(h)
	if err != nil {
		return nil
	}
	for _, d := range devs {
		for _, a := range attached {
			if a.uses(d) {
				if why := hostDriverInUse(h, d); why != "" {
					return badRequest("Not started: " + why)
				}
			}
		}
	}
	return nil
}
