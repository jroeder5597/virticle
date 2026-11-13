package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"virticle/internal/hostconn"
	"virticle/internal/nodeproto"
)

// Network drift: a VM can only move to a node that has a network of the same name, and for the VM to work there
// the network must be the same kind, on the same VLAN and (for a bridge network) on the same bridge. compareNetworks
// is the pure comparison; the rest gathers its input from the nodes.
//
// Which nodes can exchange VMs: the app has no stored rule, the migration check asks the target to run the VM's
// machine type, which needs the same distribution family and CPU architecture. Nodes are therefore compared only
// within a group of the same package family (apt or dnf) and architecture; a node whose system is not recognised is
// left out.

type driftNet struct {
	Mode   string // libvirt forward mode: nat, route, isolated, bridge ...
	Bridge string
	VLAN   int
	OVS    bool
}

// driftBridge is a host bridge of a node, as far as a copy of a bridge-mode network cares.
type driftBridge struct {
	OVS bool
	// VLANKnown says the node reported its VLAN filtering (it needs the node command); without it a tagged
	// network on a Linux bridge cannot be copied safely.
	VLANKnown     bool
	VLANFiltering bool
	VLANs         string
}

type driftNode struct {
	ID      int64
	Name    string
	Group   string // nodes of one group can exchange VMs
	Nets    map[string]driftNet
	Used    map[string]int // network -> VMs that use it, in any state
	Running map[string]int // network -> running VMs that use it
	Bridges map[string]driftBridge
}

type driftFix struct {
	Label   string `json:"label"` // "Create on <node>"
	Network string `json:"network"`
	From    int64  `json:"from"` // the node whose definition is copied
	To      int64  `json:"to"`
}

type driftRow struct {
	Network string    `json:"network"`
	Kind    string    `json:"kind"` // missing | mode | vlan | bridge
	What    string    `json:"what"`
	Nodes   []string  `json:"nodes"`
	Used    bool      `json:"used"` // a VM uses the network on one of the nodes
	Fix     *driftFix `json:"fix,omitempty"`
}

type driftReport struct {
	Networks int        `json:"networks"` // how many network names differ
	Rows     []driftRow `json:"rows"`
	// Blocked are the networks that a running VM uses and that no other node of its group has: those VMs cannot
	// be migrated at all.
	Blocked []string `json:"blocked"`
	// Unread are the nodes that could not be read; they are left out of the comparison.
	Unread []string `json:"unread"`
}

func vlanText(v int) string {
	if v == 0 {
		return "none"
	}
	return fmt.Sprint(v)
}

// valueList writes "a on n1, n2; b on n3" for values in sorted order.
func valueList(byValue map[string][]string) string {
	vals := make([]string, 0, len(byValue))
	for v := range byValue {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	var parts []string
	for _, v := range vals {
		parts = append(parts, v+" on "+strings.Join(byValue[v], ", "))
	}
	return strings.Join(parts, "; ")
}

// compareNetworks finds the differences between the networks of nodes that can exchange VMs.
func compareNetworks(nodes []driftNode) driftReport {
	rep := driftReport{Rows: []driftRow{}, Blocked: []string{}, Unread: []string{}}
	groups := map[string][]driftNode{}
	for _, n := range nodes {
		if n.Group != "" {
			groups[n.Group] = append(groups[n.Group], n)
		}
	}
	differ := map[string]bool{}
	blocked := map[string]bool{}
	var gkeys []string
	for k := range groups {
		gkeys = append(gkeys, k)
	}
	sort.Strings(gkeys)
	for _, gk := range gkeys {
		g := groups[gk]
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].Name < g[j].Name })
		names := map[string]bool{}
		for _, n := range g {
			for name := range n.Nets {
				names[name] = true
			}
		}
		var sorted []string
		for name := range names {
			sorted = append(sorted, name)
		}
		sort.Strings(sorted)
		for _, name := range sorted {
			var have, lack []driftNode
			for _, n := range g {
				if _, ok := n.Nets[name]; ok {
					have = append(have, n)
				} else {
					lack = append(lack, n)
				}
			}
			used := false
			for _, n := range have {
				used = used || n.Used[name] > 0
			}
			add := func(r driftRow) {
				r.Network, r.Used = name, used
				rep.Rows = append(rep.Rows, r)
				differ[name] = true
			}
			haveNames := func(ns []driftNode) []string {
				var out []string
				for _, n := range ns {
					out = append(out, n.Name)
				}
				return out
			}
			for _, t := range lack {
				r := driftRow{Kind: "missing", Nodes: []string{t.Name},
					What: fmt.Sprintf("Exists on %s, missing on %s.", strings.Join(haveNames(have), ", "), t.Name)}
				if from, ok := copyableFrom(name, have, t); ok {
					r.Fix = &driftFix{Label: "Create on " + t.Name, Network: name, From: from, To: t.ID}
				}
				add(r)
			}
			// A running VM on a network that no other node of the group has can go nowhere.
			if len(have) == 1 && have[0].Running[name] > 0 {
				blocked[name] = true
			}
			if len(have) < 2 {
				continue
			}
			modes := map[string][]string{}
			var bridgeNodes []driftNode
			for _, n := range have {
				m := n.Nets[name].Mode
				modes[m] = append(modes[m], n.Name)
				if m == "bridge" {
					bridgeNodes = append(bridgeNodes, n)
				}
			}
			if len(modes) > 1 {
				add(driftRow{Kind: "mode", Nodes: haveNames(have), What: "Kind: " + valueList(modes) + "."})
			}
			if len(bridgeNodes) > 1 {
				vlans, bridges := map[string][]string{}, map[string][]string{}
				for _, n := range bridgeNodes {
					d := n.Nets[name]
					vlans[vlanText(d.VLAN)] = append(vlans[vlanText(d.VLAN)], n.Name)
					bridges[d.Bridge] = append(bridges[d.Bridge], n.Name)
				}
				if len(vlans) > 1 {
					add(driftRow{Kind: "vlan", Nodes: haveNames(bridgeNodes), What: "VLAN: " + valueList(vlans) + "."})
				}
				if len(bridges) > 1 {
					add(driftRow{Kind: "bridge", Nodes: haveNames(bridgeNodes), What: "Bridge: " + valueList(bridges) + "."})
				}
			}
		}
	}
	rep.Networks = len(differ)
	for name := range blocked {
		rep.Blocked = append(rep.Blocked, name)
	}
	sort.Strings(rep.Blocked)
	return rep
}

// copyableFrom says whether the network can be created on the target as an exact copy of the definition on the
// nodes that have it, and from which node. Only bridge-mode networks qualify: every node that has it must define it
// the same way, the bridge must exist on the target as the same kind of switch, and a VLAN tag must be allowed there.
// A NAT or isolated network is not offered: a copy would carry the same subnet, which is another network, not the
// same one.
func copyableFrom(name string, have []driftNode, target driftNode) (int64, bool) {
	if len(have) == 0 {
		return 0, false
	}
	first := have[0].Nets[name]
	for _, n := range have {
		if n.Nets[name] != first {
			return 0, false
		}
	}
	if first.Mode != "bridge" || first.Bridge == "" {
		return 0, false
	}
	b, ok := target.Bridges[first.Bridge]
	if !ok || b.OVS != first.OVS {
		return 0, false
	}
	if first.VLAN != 0 && !first.OVS {
		if !b.VLANKnown || !b.VLANFiltering || !nodeproto.VLANAllowed(b.VLANs, first.VLAN) {
			return 0, false
		}
	}
	return have[0].ID, true
}

// driftAlert is the one alert about drift, or none.
func driftAlert(rep driftReport) []Alert {
	if len(rep.Blocked) == 0 {
		return nil
	}
	return []Alert{{ID: "net-drift", Kind: "net-drift", Severity: "warning", Link: "/datacenter/network",
		Title:   "Networks differ between nodes; some virtual machines cannot migrate.",
		Message: "Running virtual machines use " + strings.Join(rep.Blocked, ", ") + ", which no other node has. See Datacenter, Network."}}
}

// --- gathering ---

// nodeGroup is the migration compatibility group of a node: its package family and architecture, read once and
// remembered for a while (a node's system does not change under us).
var groupCache = struct {
	sync.Mutex
	m map[int64]groupEntry
}{m: map[int64]groupEntry{}}

type groupEntry struct {
	group string
	at    time.Time
}

const groupCommand = `cat /etc/os-release 2>/dev/null; echo "VIRTICLE_ARCH=$(uname -m)"`

func groupOf(out string) string {
	family, _ := osFamily(out)
	arch := ""
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "VIRTICLE_ARCH="); ok {
			arch = v
		}
	}
	if family == "unsupported" || arch == "" {
		return ""
	}
	return family + "/" + arch
}

func nodeGroup(h *hostconn.Host, id int64) string {
	groupCache.Lock()
	e, ok := groupCache.m[id]
	groupCache.Unlock()
	if ok && time.Since(e.at) < 30*time.Minute {
		return e.group
	}
	out, err := h.Run(groupCommand)
	if err != nil {
		return e.group // the old answer, or none
	}
	e = groupEntry{group: groupOf(out), at: time.Now()}
	groupCache.Lock()
	groupCache.m[id] = e
	groupCache.Unlock()
	return e.group
}

// collectDrift reads what one node contributes to the comparison.
// withBridges also reads the host bridges, which only the copy action needs; the alert pass leaves them out.
func collectDrift(h *hostconn.Host, id int64, withBridges bool) (driftNode, error) {
	d := driftNode{ID: id, Nets: map[string]driftNet{}, Used: map[string]int{}, Running: map[string]int{}, Bridges: map[string]driftBridge{}}
	nets, _, err := h.ConnectListAllNetworks(1, 0)
	if err != nil {
		return d, err
	}
	for _, n := range nets {
		raw, err := h.NetworkGetXMLDesc(n, 0)
		if err != nil {
			continue
		}
		x, err := parseNetXML(raw)
		if err != nil {
			continue
		}
		info := x.info()
		d.Nets[n.Name] = driftNet{Mode: info.Forward, Bridge: info.Bridge, VLAN: info.VLAN, OVS: info.OVS}
	}
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return d, err
	}
	for _, dom := range doms {
		if isReplicaOrStale(h, dom) {
			continue
		}
		raw, err := h.DomainGetXMLDesc(dom, 0)
		if err != nil {
			continue
		}
		var x domainXML
		if xml.Unmarshal([]byte(raw), &x) != nil {
			continue
		}
		active, _ := h.DomainIsActive(dom)
		for _, i := range x.Devices.Interfaces {
			// A running VM on a bridge-mode network is type "bridge" in its live definition, with the network
			// still named in source.
			if (i.Type == "network" || i.Type == "bridge") && i.Source.Network != "" {
				d.Used[i.Source.Network]++
				if active != 0 {
					d.Running[i.Source.Network]++
				}
			}
		}
	}
	// Only a bridge-mode network can be copied, so the bridges are only needed when there is something to copy to.
	if !withBridges {
		return d, nil
	}
	if nics, err := hostNICList(h); err == nil {
		for _, n := range nics {
			if isBridgeDevice(n) {
				d.Bridges[n.Name] = driftBridge{OVS: n.Kind == "openvswitch"}
			}
		}
		if info, err := h.NetInfo(); err == nil {
			for _, b := range info.Bridges {
				if cur, ok := d.Bridges[b.Name]; ok {
					cur.VLANKnown, cur.VLANFiltering, cur.VLANs = true, b.VLANFiltering, b.VLANs
					d.Bridges[b.Name] = cur
				}
			}
		}
	}
	return d, nil
}

// computeDrift reads every online node and compares them.
func (s *Server) computeDrift(withBridges bool) driftReport {
	res := gatherNodes(s, nodeTimeout, func(h *hostconn.Host) ([]driftNode, error) {
		cfg, _ := s.Nodes.ConfigOfHost(h)
		d, err := collectDrift(h, cfg.ID, withBridges)
		if err != nil {
			return nil, err
		}
		d.Group = nodeGroup(h, cfg.ID)
		return []driftNode{d}, nil
	})
	var nodes []driftNode
	var unread []string
	offline := map[int64]bool{}
	for _, st := range s.Nodes.Status() {
		if st.State != "online" {
			offline[st.ID] = true
		}
	}
	for _, r := range res {
		switch {
		case len(r.Items) == 1:
			d := r.Items[0]
			d.ID, d.Name = r.Node, r.Name
			nodes = append(nodes, d)
		case !offline[r.Node]: // a node that is down is expected to be missing; one that is up but fails is not
			unread = append(unread, r.Name)
		}
	}
	rep := compareNetworks(nodes)
	sort.Strings(unread)
	rep.Unread = append(rep.Unread, unread...)
	return rep
}

// networkDrift is GET /api/cluster/network-drift.
func (s *Server) networkDrift(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.computeDrift(true))
}
