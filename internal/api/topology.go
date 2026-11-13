package api

import (
	"context"
	"encoding/xml"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// Network topology, read-only: for every node the virtual switches (bridges), their uplinks, the port groups
// (libvirt networks, or "direct to bridge") and the VM interfaces on each. composeTopology is the pure part; the
// handler only gathers its inputs. Every switch, uplink and port group carries an id that is stable within its
// node, so actions can be attached to them later.

type topoAddr struct {
	Address    string `json:"address"`
	Management bool   `json:"management,omitempty"`
	// Migration marks the address other nodes send migration data to, when it is not the management address.
	Migration bool `json:"migration,omitempty"`
	// VLAN and Iface are set for an address the node has on a VLAN of the switch (the VLAN device Iface).
	VLAN  int    `json:"vlan,omitempty"`
	Iface string `json:"iface,omitempty"`
}

// topoMember is one card of a bond.
type topoMember struct {
	Name      string `json:"name"`
	State     string `json:"state"` // up | down
	SpeedMbps int    `json:"speedMbps,omitempty"`
	Active    bool   `json:"active,omitempty"` // carries the traffic (failover mode)
}

type topoUplink struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"` // physical | bond | vlan
	State     string   `json:"state"`
	SpeedMbps int      `json:"speedMbps,omitempty"`
	MAC       string   `json:"mac,omitempty"`
	Members   []string `json:"members,omitempty"` // the cards of a bond
	// For a bond: Mode is failover or lacp, MemberStates the cards with their link state, and LACPAnswered
	// says whether the switch at the other end took part in LACP (only for lacp).
	Mode         string       `json:"mode,omitempty"`
	MemberStates []topoMember `json:"memberStates,omitempty"`
	LACPAnswered *bool        `json:"lacpAnswered,omitempty"`
	Addresses    []topoAddr   `json:"addresses"`
	Management   bool         `json:"management,omitempty"` // carries the address the controller uses
	Migration    bool         `json:"migration,omitempty"`  // carries the address other nodes send migration data to
}

type topoIface struct {
	VM       string   `json:"vm"`
	UUID     string   `json:"uuid"`
	State    string   `json:"state"`
	MAC      string   `json:"mac"`
	IPs      []string `json:"ips"`
	LinkDown bool     `json:"linkDown,omitempty"`
}

type topoPortGroup struct {
	ID         string      `json:"id"`   // "net:<name>", "direct" or "missing:<name>"
	Kind       string      `json:"kind"` // network | direct | missing
	Network    string      `json:"network,omitempty"`
	Mode       string      `json:"mode,omitempty"` // libvirt forward mode: nat, route, isolated, bridge, open ...
	VLAN       int         `json:"vlan,omitempty"`
	DHCP       string      `json:"dhcp,omitempty"` // "start to end" when libvirt serves DHCP
	Active     bool        `json:"active"`
	Interfaces []topoIface `json:"interfaces"`
}

type topoSwitch struct {
	ID            string          `json:"id"`   // "bridge:<name>"
	Kind          string          `json:"kind"` // linux-bridge | openvswitch | libvirt | other
	Name          string          `json:"name"`
	State         string          `json:"state,omitempty"`
	MTU           int             `json:"mtu,omitempty"`
	UplinkNote    string          `json:"uplinkNote,omitempty"` // "NAT to eth0", "Routed", "Isolated (no uplink)" instead of uplinks
	Uplinks       []topoUplink    `json:"uplinks"`
	HostAddresses []topoAddr      `json:"hostAddresses"`
	PortGroups    []topoPortGroup `json:"portGroups"`
}

type topoNode struct {
	Node     int64        `json:"node"`
	Name     string       `json:"name"`
	Error    string       `json:"error,omitempty"`
	Switches []topoSwitch `json:"switches"`
	Unused   []topoUplink `json:"unused"`
}

// topoVMNIC is one interface of a VM definition.
type topoVMNIC struct {
	MAC      string
	Type     string // network | bridge | direct | ...
	Network  string
	Bridge   string
	Dev      string
	LinkDown bool
}

type topoVM struct {
	Name, UUID, State string
	NICs              []topoVMNIC
}

type topoInput struct {
	NICs     []hostNIC
	Networks []networkInfo // with their DHCP leases
	// ForwardDev is the device a NAT or routed network was pinned to, by network name.
	ForwardDev map[string]string
	VMs        []topoVM
	// Mgmt are the IP addresses of the node as the controller reaches it.
	Mgmt []string
}

func addrIP(cidr string) string {
	if p, err := netip.ParsePrefix(cidr); err == nil {
		return p.Addr().String()
	}
	return cidr
}

func topoAddrs(cidrs []string, mgmt map[string]bool) (out []topoAddr, hasMgmt bool) {
	out = []topoAddr{}
	for _, c := range cidrs {
		m := mgmt[addrIP(c)]
		hasMgmt = hasMgmt || m
		out = append(out, topoAddr{Address: c, Management: m})
	}
	return out, hasMgmt
}

func uplinkOf(n hostNIC, all []hostNIC, mgmt map[string]bool) topoUplink {
	u := topoUplink{Name: n.Name, Type: n.Type, State: n.State, SpeedMbps: n.SpeedMbps, MAC: n.MAC}
	u.Addresses, u.Management = topoAddrs(n.Addresses, mgmt)
	if n.Type == "bond" {
		for _, m := range all {
			if m.Master == n.Name {
				u.Members = append(u.Members, m.Name)
			}
		}
		if b := n.Bond; b != nil {
			u.Mode = map[string]string{"active-backup": "failover", "802.3ad": "lacp"}[b.Mode]
			for _, m := range b.Members {
				st := "down"
				if m.Up {
					st = "up"
				}
				u.MemberStates = append(u.MemberStates, topoMember{Name: m.Name, State: st, SpeedMbps: m.SpeedMbps, Active: m.Name == b.ActiveMember})
			}
			sort.Slice(u.MemberStates, func(i, j int) bool { return u.MemberStates[i].Name < u.MemberStates[j].Name })
			if b.LACP != nil {
				a := b.LACP.Answered
				u.LACPAnswered = &a
			}
		}
	}
	return u
}

// portGroup finds or adds a port group of a switch.
func (s *topoSwitch) portGroup(id string, mk topoPortGroup) *topoPortGroup {
	for i := range s.PortGroups {
		if s.PortGroups[i].ID == id {
			return &s.PortGroups[i]
		}
	}
	mk.ID = id
	mk.Interfaces = []topoIface{}
	s.PortGroups = append(s.PortGroups, mk)
	return &s.PortGroups[len(s.PortGroups)-1]
}

// composeTopology builds one node's switches and unused cards from plain inputs.
//
//   - Every host bridge (Linux or Open vSwitch) is a switch; so is the bridge of every libvirt network.
//   - Uplinks of a host bridge are the physical cards, bonds and VLAN devices whose master it is. Taps (VM
//     ports), veth pairs and dummy devices are not uplinks.
//   - Each libvirt network is a port group on the bridge it uses; a VM interface of type bridge is a "direct"
//     port group on that bridge.
//   - The bridge's own addresses are the host port.
//   - Physical cards and bonds with no master are "unused". The one carrying a Mgmt address is marked.
func composeTopology(in topoInput) (switches []topoSwitch, unused []topoUplink) {
	mgmt := map[string]bool{}
	for _, a := range in.Mgmt {
		mgmt[a] = true
	}
	byName := map[string]*topoSwitch{}
	var order []string
	get := func(id, name, kind string) *topoSwitch {
		if s, ok := byName[id]; ok {
			return s
		}
		s := &topoSwitch{ID: id, Kind: kind, Name: name, Uplinks: []topoUplink{}, HostAddresses: []topoAddr{}, PortGroups: []topoPortGroup{}}
		byName[id] = s
		order = append(order, id)
		return s
	}
	bridge := func(name string) *topoSwitch { return get("bridge:"+name, name, "linux-bridge") }

	for _, n := range in.NICs {
		if !isBridgeDevice(n) {
			continue
		}
		s := bridge(n.Name)
		if n.Kind == "openvswitch" {
			s.Kind = "openvswitch"
		}
		s.State = n.State
		s.MTU = n.MTU
		s.HostAddresses, _ = topoAddrs(n.Addresses, mgmt)
		for _, v := range in.NICs {
			if v.Type == "vlan" && v.Parent == n.Name {
				as, _ := topoAddrs(v.Addresses, mgmt)
				if len(as) == 0 { // a DHCP address that has not arrived yet
					as = []topoAddr{{}}
				}
				for _, a := range as {
					a.VLAN, a.Iface = v.VLANID, v.Name
					s.HostAddresses = append(s.HostAddresses, a)
				}
			}
		}
		for _, m := range in.NICs {
			if m.Master == n.Name && (m.Type == "physical" || m.Type == "bond" || m.Type == "vlan") {
				s.Uplinks = append(s.Uplinks, uplinkOf(m, in.NICs, mgmt))
			}
		}
	}

	netSwitch := map[string]*topoSwitch{}
	leases := map[string]map[string]string{} // network -> lower-case MAC -> IP
	for _, nw := range in.Networks {
		var s *topoSwitch
		if nw.Bridge != "" {
			s = bridge(nw.Bridge)
		} else { // macvtap and hostdev networks have no bridge
			s = get("network:"+nw.Name, "Network "+nw.Name, "libvirt")
		}
		// A bridge-mode network only names a host bridge; every other mode makes libvirt own the bridge.
		if nw.Forward != "bridge" {
			s.Kind = "libvirt"
		}
		if s.State == "" {
			s.State = map[bool]string{true: "up", false: "down"}[nw.Active]
		}
		switch nw.Forward {
		case "nat":
			dev := in.ForwardDev[nw.Name]
			if dev == "" {
				dev = "any device"
			}
			s.UplinkNote = "NAT to " + dev
		case "route":
			s.UplinkNote = "Routed"
			if dev := in.ForwardDev[nw.Name]; dev != "" {
				s.UplinkNote += " to " + dev
			}
		case "isolated":
			s.UplinkNote = "Isolated (no uplink)"
		case "bridge", "open":
		default:
			if nw.Bridge == "" {
				s.UplinkNote = "Direct to a device (" + nw.Forward + ")"
			}
		}
		pg := s.portGroup("net:"+nw.Name, topoPortGroup{Kind: "network", Network: nw.Name, Mode: nw.Forward, VLAN: nw.VLAN, Active: nw.Active})
		if nw.DHCP && nw.DHCPStart != "" {
			pg.DHCP = nw.DHCPStart + " to " + nw.DHCPEnd
		}
		netSwitch[nw.Name] = s
		l := map[string]string{}
		for _, ls := range nw.Leases {
			l[strings.ToLower(ls.MAC)] = ls.IP
		}
		leases[nw.Name] = l
	}

	for _, vm := range in.VMs {
		for _, c := range vm.NICs {
			ifc := topoIface{VM: vm.Name, UUID: vm.UUID, State: vm.State, MAC: c.MAC, IPs: []string{}, LinkDown: c.LinkDown}
			switch {
			// A running VM on a bridge-mode libvirt network is type "bridge" in the live XML, with the network's
			// name still in source: it belongs to that network, not to "Direct to bridge".
			case (c.Type == "network" || c.Type == "bridge") && c.Network != "" && netSwitch[c.Network] != nil:
				if ip := leases[c.Network][strings.ToLower(c.MAC)]; ip != "" {
					ifc.IPs = append(ifc.IPs, ip)
				}
				g := netSwitch[c.Network].portGroup("net:"+c.Network, topoPortGroup{})
				g.Interfaces = append(g.Interfaces, ifc)
			case c.Type == "bridge" && c.Bridge != "":
				g := bridge(c.Bridge).portGroup("direct", topoPortGroup{Kind: "direct", Active: true})
				g.Interfaces = append(g.Interfaces, ifc)
			default: // a network that no longer exists, or an attachment that is not a bridge
				name := c.Network
				if c.Type != "network" {
					name = strings.TrimSpace(c.Type + " " + c.Dev)
				}
				o := get("other", "Other attachments", "other")
				g := o.portGroup("missing:"+name, topoPortGroup{Kind: "missing", Network: name, Active: true})
				g.Interfaces = append(g.Interfaces, ifc)
			}
		}
	}

	for _, id := range order {
		s := byName[id]
		sort.SliceStable(s.PortGroups, func(i, j int) bool {
			a, b := s.PortGroups[i], s.PortGroups[j]
			if (a.Kind == "direct") != (b.Kind == "direct") {
				return b.Kind == "direct"
			}
			return a.Network < b.Network
		})
		for _, g := range s.PortGroups {
			ifs := g.Interfaces
			sort.SliceStable(ifs, func(i, j int) bool {
				if ifs[i].VM != ifs[j].VM {
					return ifs[i].VM < ifs[j].VM
				}
				return ifs[i].MAC < ifs[j].MAC
			})
		}
		if s.UplinkNote == "" && len(s.Uplinks) == 0 && s.Kind != "other" {
			s.UplinkNote = "No uplink"
		}
		switches = append(switches, *s)
	}
	sort.SliceStable(switches, func(i, j int) bool {
		if (switches[i].Kind == "other") != (switches[j].Kind == "other") {
			return switches[j].Kind == "other"
		}
		return switches[i].Name < switches[j].Name
	})

	unused = []topoUplink{}
	for _, n := range in.NICs {
		if (n.Type == "physical" || n.Type == "bond") && n.Master == "" {
			unused = append(unused, uplinkOf(n, in.NICs, mgmt))
		}
	}
	return switches, unused
}

// forwardDev reads the device a NAT or routed network was pinned to.
func forwardDev(raw string) string {
	var x struct {
		Forward struct {
			Dev string `xml:"dev,attr"`
		} `xml:"forward"`
	}
	if xml.Unmarshal([]byte(raw), &x) != nil {
		return ""
	}
	return x.Forward.Dev
}

// mgmtAddrs turns the address the controller uses for a node into IP addresses (a literal IP is used as it is).
func mgmtAddrs(addr string) []string {
	addr, _ = splitAddr(addr)
	if ip, err := netip.ParseAddr(addr); err == nil {
		return []string{ip.String()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, addr)
	if err != nil {
		return nil
	}
	return ips
}

type topoResult struct {
	switches []topoSwitch
	unused   []topoUplink
}

// collectTopology reads one node. Per VM it costs one definition read and one state read; addresses come from
// DHCP leases only, never from the guest agent.
func collectTopology(h *hostconn.Host, addr, migAddr string) (topoResult, error) {
	nics, err := hostNICList(h)
	if err != nil {
		return topoResult{}, err
	}
	nets, _, err := h.ConnectListAllNetworks(1, 0)
	if err != nil {
		return topoResult{}, err
	}
	in := topoInput{NICs: nics, ForwardDev: map[string]string{}, Mgmt: mgmtAddrs(addr)}
	for _, n := range nets {
		info, err := describeNetwork(h, n, nil, false)
		if err != nil {
			continue // removed while listing
		}
		if raw, err := h.NetworkGetXMLDesc(n, 0); err == nil {
			in.ForwardDev[info.Name] = forwardDev(raw)
		}
		in.Networks = append(in.Networks, info)
	}
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return topoResult{}, err
	}
	for _, d := range doms {
		raw, err := h.DomainGetXMLDesc(d, 0)
		if err != nil {
			continue
		}
		var x domainXML
		if xml.Unmarshal([]byte(raw), &x) != nil {
			continue
		}
		state, _, _, _, _, err := h.DomainGetInfo(d)
		if err != nil {
			continue
		}
		vm := topoVM{Name: d.Name, UUID: cluster.FormatUUID(d.UUID), State: stateName(state)}
		for _, i := range x.Devices.Interfaces {
			vm.NICs = append(vm.NICs, topoVMNIC{MAC: i.MAC.Address, Type: i.Type, Network: i.Source.Network, Bridge: i.Source.Bridge, Dev: i.Source.Dev, LinkDown: i.Link.State == "down"})
		}
		in.VMs = append(in.VMs, vm)
	}
	var r topoResult
	r.switches, r.unused = composeTopology(in)
	if migAddr != "" && !sameHost(migAddr, addr) {
		markMigration(r.switches, r.unused, mgmtAddrs(migAddr))
	}
	return r, nil
}

// topology is GET /api/topology[?node=ID]: one entry per node; a failing node reports its own error.
func (s *Server) topology(w http.ResponseWriter, r *http.Request) {
	only := int64(-1)
	if v := r.URL.Query().Get("node"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fail(w, badRequest("node must be a number"))
			return
		}
		only = id
	}
	res := gatherNodes(s, nodeTimeout, func(h *hostconn.Host) ([]topoResult, error) {
		cfg, _ := s.Nodes.ConfigOfHost(h)
		if only >= 0 && cfg.ID != only {
			return nil, nil // not asked for: no work on that node
		}
		r, err := collectTopology(h, cfg.Addr, cfg.MigrationAddr)
		return []topoResult{r}, err
	})
	out := []topoNode{}
	for _, r := range res {
		if only >= 0 && r.Node != only {
			continue
		}
		e := topoNode{Node: r.Node, Name: r.Name, Error: r.Err, Switches: []topoSwitch{}, Unused: []topoUplink{}}
		if len(r.Items) == 1 {
			e.Switches, e.Unused = r.Items[0].switches, r.Items[0].unused
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Node < out[j].Node
	})
	writeJSON(w, map[string]any{"nodes": out})
}

// markMigration sets the Migration mark on the addresses and uplinks that hold one of ips.
func markMigration(switches []topoSwitch, unused []topoUplink, ips []string) {
	want := map[string]bool{}
	for _, ip := range ips {
		want[ip] = true
	}
	mark := func(as []topoAddr) bool {
		any := false
		for i := range as {
			if want[addrIP(as[i].Address)] {
				as[i].Migration, any = true, true
			}
		}
		return any
	}
	for i := range switches {
		mark(switches[i].HostAddresses)
		for j := range switches[i].Uplinks {
			u := &switches[i].Uplinks[j]
			u.Migration = mark(u.Addresses)
		}
	}
	for i := range unused {
		unused[i].Migration = mark(unused[i].Addresses)
	}
}
