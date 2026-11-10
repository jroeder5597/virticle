package api

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/cluster"
	"virt-proj/internal/domxml"
	"virt-proj/internal/hostconn"
)

// Passthrough of host PCI and USB devices. The list comes from libvirt's node
// device API; what the host itself depends on (its default-route NIC, the
// controllers of mounted filesystems, bridges) is read with one fixed
// read-only script and such devices are refused server-side. Attaching and
// detaching use the device-level libvirt calls through hwCtx, like every other
// hardware change.

const passthroughWarning = "A VM with a passed-through device cannot be live-migrated (the migration pre-flight refuses it) and is tied to this node until the device is removed."

// factsScript prints what the node needs for itself. It only reads sysfs and
// /proc and takes no input.
const factsScript = `
pci_of() { echo "$1" | grep -oE '[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]' | tail -1; }
walk() { i=$1; d=$2; [ "$d" -gt 4 ] && return
  if [ -d /sys/class/net/$i/brif ]; then for m in /sys/class/net/$i/brif/*; do [ -e "$m" ] && walk "$(basename "$m")" $((d+1)); done
  elif [ -r /sys/class/net/$i/bonding/slaves ]; then for m in $(cat /sys/class/net/$i/bonding/slaves); do walk "$m" $((d+1)); done
  elif ls /sys/class/net/$i/ 2>/dev/null | grep -q '^lower_'; then for m in /sys/class/net/$i/lower_*; do walk "$(basename "$m" | sed 's/^lower_//')" $((d+1)); done
  else p=$(pci_of "$(readlink -f /sys/class/net/$i/device 2>/dev/null)"); [ -n "$p" ] && echo "nic $i $p"; fi; }
for i in $(ip -o route show default 2>/dev/null | awk '{for(k=1;k<=NF;k++) if($k=="dev") print $(k+1)}' | sort -u); do walk "$i" 0; done
wb() { n=$1; d=$2; [ "$d" -gt 5 ] && return
  if [ -d /sys/class/block/$n/slaves ] && [ -n "$(ls /sys/class/block/$n/slaves 2>/dev/null)" ]; then for s in /sys/class/block/$n/slaves/*; do wb "$(basename "$s")" $((d+1)); done
  else p=$(pci_of "$(readlink -f /sys/class/block/$n 2>/dev/null)"); [ -n "$p" ] && echo "blk $n $p"; fi; }
for s in $( { awk '$1 ~ "^/dev/" {print $1}' /proc/mounts; awk 'NR>1 {print $1}' /proc/swaps; } | sort -u); do
  n=$(basename "$(readlink -f "$s")"); [ -e /sys/class/block/$n ] && wb "$n" 0; done
for d in /sys/bus/pci/devices/*; do [ "$(cat $d/boot_vga 2>/dev/null)" = 1 ] && echo "bootvga $(basename $d)"; done
echo ok
`

func nodeHostFacts(h *hostconn.Host) hostFacts {
	out, err := h.Run("sh -c " + shq(factsScript))
	if err != nil {
		return hostFacts{NIC: map[string]string{}, Storage: map[string]string{}}
	}
	return parseFacts(out)
}

// collectHostDevs lists the PCI and USB devices of a node with their safety
// and usage. iommu says whether the node has IOMMU groups at all.
func collectHostDevs(h *hostconn.Host) (devs []hostDevice, iommu bool, facts hostFacts, err error) {
	list, _, err := h.ConnectListAllNodeDevices(1, uint32(libvirt.ConnectListNodeDevicesCapPciDev|libvirt.ConnectListNodeDevicesCapUsbDev))
	if err != nil {
		return nil, false, facts, err
	}
	for _, nd := range list {
		raw, err := h.NodeDeviceGetXMLDesc(nd.Name, 0)
		if err != nil {
			continue // the device went away while listing
		}
		d, ok, err := parseNodeDev(raw)
		if err != nil || !ok {
			continue
		}
		if d.IOMMUGroup != nil {
			iommu = true
		}
		devs = append(devs, d)
	}
	sort.SliceStable(devs, func(i, j int) bool {
		if devs[i].Kind != devs[j].Kind {
			return devs[i].Kind < devs[j].Kind
		}
		return devs[i].ID < devs[j].ID
	})
	facts = nodeHostFacts(h)
	markSafety(devs, facts)

	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return devs, iommu, facts, err
	}
	for _, dom := range doms {
		state, _, _, _, _, err := h.DomainGetInfo(dom)
		if err != nil {
			continue
		}
		ref := vmRef{UUID: cluster.FormatUUID(dom.UUID), Name: dom.Name, State: stateName(state)}
		var uses []vmHostdev
		for _, flags := range []libvirt.DomainXMLFlags{libvirt.DomainXMLInactive, 0} {
			if flags == 0 && ref.State == "shutoff" {
				continue
			}
			raw, err := h.DomainGetXMLDesc(dom, flags)
			if err != nil {
				continue
			}
			if hs, err := parseVMHostdevs(raw); err == nil {
				uses = append(uses, hs...)
			}
		}
		for i := range devs {
			for _, u := range uses {
				if u.uses(devs[i]) {
					devs[i].UsedBy = append(devs[i].UsedBy, ref)
					break
				}
			}
		}
	}
	return devs, iommu, facts, nil
}

type attachedDev struct {
	Key     string `json:"key"` // what DELETE takes
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Present bool   `json:"present"` // the device exists on the node now
}

func attachedKey(u vmHostdev) string {
	if u.Kind == "pci" {
		return "pci:" + u.PCI.String()
	}
	k := "usb:" + u.VendorID + ":" + u.Product
	if u.Bus != 0 {
		k += ":" + strconv.Itoa(u.Bus) + ":" + strconv.Itoa(u.Device)
	}
	return k
}

func (s *Server) vmHostdevs(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	devs, iommu, facts, err := collectHostDevs(h)
	if err != nil {
		fail(w, err)
		return
	}
	uuid := cluster.FormatUUID(dom.UUID)
	var mine []vmHostdev
	for _, flags := range []libvirt.DomainXMLFlags{libvirt.DomainXMLInactive, 0} {
		raw, err := h.DomainGetXMLDesc(dom, flags)
		if err != nil {
			continue
		}
		if hs, err := parseVMHostdevs(raw); err == nil {
			mine = append(mine, hs...)
		}
	}
	attached := []attachedDev{}
	seen := map[string]bool{}
	for _, u := range mine {
		k := attachedKey(u)
		if seen[k] {
			continue
		}
		seen[k] = true
		a := attachedDev{Key: k, Kind: u.Kind, Label: k}
		for _, d := range devs {
			if u.uses(d) {
				a.Present = true
				a.Label = strings.TrimSpace(d.Vendor+" "+d.Product) + " (" + d.ID + ")"
				break
			}
		}
		attached = append(attached, a)
	}
	writeJSON(w, map[string]any{
		"devices": devs, "attached": attached, "iommu": iommu, "factsKnown": facts.Known,
		"vm": uuid, "warning": passthroughWarning,
	})
}

func (s *Server) vmHostdevAttach(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, "attach host device", c.dom.Name+"/"+req.ID, c.h.Name, func(func(float64)) error {
		devs, iommu, _, err := collectHostDevs(c.h)
		if err != nil {
			return err
		}
		d, xmlText, note, err := pickHostdev(devs, iommu, req.ID, cluster.FormatUUID(c.dom.UUID))
		if err != nil {
			return err
		}
		_ = d
		applied, err := c.attach(xmlText)
		if err != nil {
			return err
		}
		res = c.result(applied)
		res.addNote(note)
		res.addNote(passthroughWarning)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// pickHostdev applies every server-side refusal and returns the device and
// the <hostdev> XML to attach.
func pickHostdev(devs []hostDevice, iommu bool, id, vmUUID string) (hostDevice, string, string, error) {
	var d *hostDevice
	for i := range devs {
		if devs[i].ID == id {
			d = &devs[i]
		}
	}
	if d == nil {
		return hostDevice{}, "", "", badRequest("no such host device " + id + " on this node")
	}
	if d.Unsafe {
		return *d, "", "", badRequest("refused: " + d.UnsafeReason)
	}
	for _, u := range d.UsedBy {
		if u.UUID == vmUUID {
			return *d, "", "", badRequest("this device is already attached to this VM")
		}
		return *d, "", "", badRequest("this device is already passed through to VM " + u.Name)
	}
	note := ""
	if d.Kind == "pci" {
		if d.IOMMUGroup == nil {
			msg := "this device has no IOMMU group"
			if !iommu {
				msg = "this node has no active IOMMU (enable intel_iommu=on or amd_iommu=on and reboot)"
			}
			return *d, "", "", badRequest("refused: " + msg + ", so VFIO cannot pass it through")
		}
		for _, p := range d.GroupPeers {
			for i := range devs {
				if devs[i].ID != p.ID {
					continue
				}
				for _, u := range devs[i].UsedBy {
					if u.UUID != vmUUID {
						return *d, "", "", badRequest("refused: it shares IOMMU group " + strconv.Itoa(*d.IOMMUGroup) + " with " + p.ID + ", which is passed through to VM " + u.Name)
					}
				}
			}
		}
		if len(d.GroupPeers) > 0 {
			ids := make([]string, len(d.GroupPeers))
			for i, p := range d.GroupPeers {
				ids[i] = p.ID
			}
			note = fmt.Sprintf("IOMMU group %d also holds %s; the group is handed over as a whole, so pass those to this VM too or leave them unused.", *d.IOMMUGroup, strings.Join(ids, ", "))
		}
	}
	same := 0
	for _, o := range devs {
		if o.Kind == "usb" && d.Kind == "usb" && o.VendorID == d.VendorID && o.ProductID == d.ProductID {
			same++
		}
	}
	return *d, hostdevXML(*d, same), note, nil
}

var hostdevKeyRE = regexp.MustCompile(`^(pci:[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]|usb:[0-9a-f]{4}:[0-9a-f]{4}(:[0-9]{1,3}:[0-9]{1,3})?)$`)

// hostdevMatch matches the <hostdev> element a detach key refers to.
func hostdevMatch(key string) func(*domxml.Node) bool {
	parts := strings.Split(key, ":")
	return func(n *domxml.Node) bool {
		src := n.Child("source")
		if src == nil || n.Attrs["mode"] != "subsystem" {
			return false
		}
		switch parts[0] {
		case "pci":
			a := src.Child("address")
			if n.Attrs["type"] != "pci" || a == nil {
				return false
			}
			want, ok := parsePCIID(strings.Join(parts[1:], ":"))
			return ok && want == pciAddr{hexNum(a.Attrs["domain"]), hexNum(a.Attrs["bus"]), hexNum(a.Attrs["slot"]), hexNum(a.Attrs["function"])}
		case "usb":
			v, p := src.Child("vendor"), src.Child("product")
			if n.Attrs["type"] != "usb" || v == nil || p == nil {
				return false
			}
			if strings.TrimPrefix(v.Attrs["id"], "0x") != parts[1] || strings.TrimPrefix(p.Attrs["id"], "0x") != parts[2] {
				return false
			}
			if len(parts) == 5 {
				a := src.Child("address")
				return a != nil && a.Attrs["bus"] == parts[3] && a.Attrs["device"] == parts[4]
			}
			return true
		}
		return false
	}
}

func (s *Server) vmHostdevDetach(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	key := strings.ToLower(r.PathValue("id"))
	if !hostdevKeyRE.MatchString(key) {
		fail(w, badRequest("invalid host device key"))
		return
	}
	var res hwResult
	err = s.task(r, "detach host device", c.dom.Name+"/"+key, c.h.Name, func(func(float64)) error {
		applied, pending, err := c.detach("hostdev", hostdevMatch(key))
		if err != nil {
			return err
		}
		res = c.result(applied)
		if pending {
			res.Applied, res.NeedsRestart = "config", true
			res.addNote("the guest has not released the device yet; it is released once the guest lets go of it, or at the next restart")
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}
