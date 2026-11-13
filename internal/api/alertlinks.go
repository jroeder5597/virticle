package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"virticle/internal/hostconn"
)

// Alerts about the network links of a node: a card that lost its link while a
// bridge, a bond or an address depends on it, and a bond that runs on fewer
// cards than it has. The facts come from one command per node and check (the
// same `ip` listing the interface page uses, plus the bonding status files);
// linkAlerts is the pure part that turns them into alerts.

// linksCommand is nicsCommand followed by the status of every bond. It is fixed
// text; nothing from a request reaches the shell.
const linksCommand = nicsCommand + `; for b in $(ls /sys/class/net); do if [ -d /sys/class/net/$b/bonding ]; then echo "BONDSTART $b"; cat /proc/net/bonding/$b 2>/dev/null; echo "BONDEND"; fi; done`

// bondStatus is what /proc/net/bonding/<bond> says that the alerts need.
type bondStatus struct {
	LACP bool // mode 802.3ad
	// PartnerMissing: LACP is on and the active aggregator has no partner (its partner MAC is all zeros).
	PartnerMissing bool
}

// parseBondStatus reads the text of /proc/net/bonding/<bond>.
func parseBondStatus(text string) bondStatus {
	var b bondStatus
	partners := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Bonding Mode:"):
			b.LACP = strings.Contains(line, "802.3ad")
		case strings.HasPrefix(line, "Partner Mac Address:"):
			mac := strings.TrimSpace(strings.TrimPrefix(line, "Partner Mac Address:"))
			partners++
			if mac != "00:00:00:00:00:00" {
				return bondStatus{LACP: b.LACP}
			}
		}
	}
	b.PartnerMissing = b.LACP && partners > 0
	return b
}

// linkFacts is the output of linksCommand taken apart.
type linkFacts struct {
	NICs       []hostNIC
	VLANParent map[string]string // VLAN device -> the device it runs on
	Bonds      map[string]bondStatus
}

// parseLinkFacts reads the output of linksCommand.
func parseLinkFacts(out string) (linkFacts, error) {
	f := linkFacts{VLANParent: map[string]string{}, Bonds: map[string]bondStatus{}}
	var rest, cur []string
	bond := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "BONDSTART "):
			bond, cur = strings.TrimSpace(strings.TrimPrefix(line, "BONDSTART ")), nil
		case strings.HasPrefix(line, "BONDEND"):
			if bond != "" {
				f.Bonds[bond] = parseBondStatus(strings.Join(cur, "\n"))
			}
			bond = ""
		case bond != "":
			cur = append(cur, line)
		default:
			rest = append(rest, line)
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "[") {
				var links []struct {
					Name string `json:"ifname"`
					Link string `json:"link"`
				}
				if json.Unmarshal([]byte(t), &links) == nil {
					for _, l := range links {
						if l.Link != "" {
							f.VLANParent[l.Name] = l.Link
						}
					}
				}
			}
		}
	}
	var err error
	f.NICs, err = parseHostNICs(strings.Join(rest, "\n"))
	return f, err
}

// readLinkAlerts reads one node and returns its link alerts.
func readLinkAlerts(h *hostconn.Host, node int64, nodeName, addr string) ([]Alert, error) {
	out, err := h.Run(linksCommand)
	if err != nil {
		return nil, fmt.Errorf("reading links: %w", err)
	}
	f, err := parseLinkFacts(out)
	if err != nil {
		return nil, err
	}
	return linkAlerts(node, nodeName, f, mgmtAddrs(addr)), nil
}

// linkUse says what depends on a device.
type linkUse struct {
	bridges   []string // Linux bridges it is a port of, directly or through a bond or VLAN device
	addressed bool     // it or a device on top of it carries an address
	ovs       bool     // it is a port of an Open vSwitch bridge (which bridge is not visible here)
}

func (u *linkUse) add(o linkUse) {
	u.bridges = append(u.bridges, o.bridges...)
	u.addressed = u.addressed || o.addressed
	u.ovs = u.ovs || o.ovs
}

func (u linkUse) used() bool { return len(u.bridges) > 0 || u.addressed || u.ovs }

// linkAlerts decides the link alerts of one node from plain facts.
//
//   - Only cards that something depends on count: a port of a bridge or of a bond that is itself in use, or a card
//     (or a VLAN device on it) with an address. A card nobody uses never alerts, plugged in or not.
//   - A card without link is a warning. It is critical when its bridge has no other uplink with link and the bridge
//     carries the node's address (mgmt) or has VM ports (tap devices).
//   - A bond with some but not all members up is degraded; an LACP bond whose switch does not answer is a warning.
//
// The bridge of an Open vSwitch port is not visible in this listing, so such cards only ever warn.
func linkAlerts(node int64, nodeName string, f linkFacts, mgmt []string) []Alert {
	byName := map[string]hostNIC{}
	for _, n := range f.NICs {
		byName[n.Name] = n
	}
	var use func(name string, depth int) linkUse
	use = func(name string, depth int) linkUse {
		var u linkUse
		n, ok := byName[name]
		if !ok || depth > 4 {
			return u
		}
		u.addressed = len(n.Addresses) > 0
		if n.Master != "" {
			switch m := byName[n.Master]; {
			case m.Type == "bridge":
				u.bridges = append(u.bridges, m.Name)
			case m.Kind == "openvswitch" || n.Master == "ovs-system":
				u.ovs = true
			case m.Type == "bond":
				u.add(use(m.Name, depth+1))
			}
		}
		for _, v := range f.NICs {
			if v.Type == "vlan" && f.VLANParent[v.Name] == name {
				u.add(use(v.Name, depth+1))
			}
		}
		return u
	}

	mgmtSet := map[string]bool{}
	for _, a := range mgmt {
		mgmtSet[a] = true
	}
	// A bridge is cut off when none of its uplinks has link; it matters when the node's address or a VM is on it.
	type bridgeFacts struct {
		cutOff, mgmt bool
		vms          int
	}
	bridges := map[string]bridgeFacts{}
	for _, b := range f.NICs {
		if b.Type != "bridge" {
			continue
		}
		bf := bridgeFacts{cutOff: true}
		for _, a := range b.Addresses {
			if mgmtSet[addrIP(a)] {
				bf.mgmt = true
			}
		}
		uplinks := 0
		for _, m := range f.NICs {
			if m.Master != b.Name {
				continue
			}
			switch m.Type {
			case "physical", "bond", "vlan":
				uplinks++
				if m.State == "up" {
					bf.cutOff = false
				}
			case "tap":
				bf.vms++
			}
		}
		bf.cutOff = bf.cutOff && uplinks > 0
		bridges[b.Name] = bf
	}

	var out []Alert
	id := strconv.FormatInt(node, 10)
	route := "/node/" + id + "/topology"
	for _, n := range f.NICs {
		switch n.Type {
		case "physical":
			if n.State != "down" {
				continue
			}
			u := use(n.Name, 0)
			if !u.used() {
				continue
			}
			sev, extra := "warning", ""
			for _, bn := range u.bridges {
				bf := bridges[bn]
				if !bf.cutOff {
					continue
				}
				if bf.vms > 0 {
					sev, extra = "critical", " Its virtual machines have no network."
				} else if bf.mgmt && sev != "critical" {
					sev, extra = "critical", " The node's own address uses it."
				}
			}
			title := fmt.Sprintf("Network card %s on %s has no link.", n.Name, nodeName)
			out = append(out, Alert{ID: "link:" + id + ":" + n.Name, Kind: "net-link", Severity: sev, Node: node, Link: route,
				Title: title, Message: title + extra})
		case "bond":
			if !use(n.Name, 0).used() {
				continue
			}
			total, up := 0, 0
			for _, m := range f.NICs {
				if m.Master == n.Name {
					total++
					if m.State == "up" {
						up++
					}
				}
			}
			if up > 0 && up < total {
				title := fmt.Sprintf("%s on %s runs on %d of %d cards.", n.Name, nodeName, up, total)
				out = append(out, Alert{ID: "bond:" + id + ":" + n.Name, Kind: "net-bond", Severity: "warning", Node: node, Link: route,
					Title: title, Message: title + " Check the cable and the switch port of the card without link."})
			}
			if st := f.Bonds[n.Name]; up > 0 && st.PartnerMissing {
				title := fmt.Sprintf("LACP is not answered by the switch for %s on %s.", n.Name, nodeName)
				out = append(out, Alert{ID: "lacp:" + id + ":" + n.Name, Kind: "net-bond", Severity: "warning", Node: node, Link: route,
					Title: title, Message: title + " The switch ports need a matching LACP group."})
			}
		}
	}
	return out
}
