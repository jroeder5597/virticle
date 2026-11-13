package api

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// netXML is the part of a network definition the UI shows and edits.
type netXML struct {
	Name    string `xml:"name"`
	UUID    string `xml:"uuid"`
	Forward *struct {
		Mode string `xml:"mode,attr"`
	} `xml:"forward"`
	Bridge struct {
		Name string `xml:"name,attr"`
	} `xml:"bridge"`
	Domain struct {
		Name string `xml:"name,attr"`
	} `xml:"domain"`
	VirtualPort *struct {
		Type string `xml:"type,attr"`
	} `xml:"virtualport"`
	VLAN *struct {
		Tags []struct {
			ID int `xml:"id,attr"`
		} `xml:"tag"`
	} `xml:"vlan"`
	IP []netIP `xml:"ip"`
}

type netIP struct {
	Family  string `xml:"family,attr"`
	Address string `xml:"address,attr"`
	Netmask string `xml:"netmask,attr"`
	Prefix  string `xml:"prefix,attr"`
	DHCP    *struct {
		Ranges []struct {
			Start string `xml:"start,attr"`
			End   string `xml:"end,attr"`
		} `xml:"range"`
		Hosts []dhcpHost `xml:"host"`
	} `xml:"dhcp"`
}

type dhcpHost struct {
	MAC  string `xml:"mac,attr" json:"mac"`
	IP   string `xml:"ip,attr" json:"ip"`
	Name string `xml:"name,attr" json:"name,omitempty"`
}

// prefix returns the IPv4 subnet of an <ip> element, with the address kept
// as the host part (the gateway).
func (i netIP) prefix() (netip.Prefix, bool) {
	a, err := netip.ParseAddr(i.Address)
	if err != nil || !a.Is4() {
		return netip.Prefix{}, false
	}
	bits := -1
	if i.Prefix != "" {
		fmt.Sscanf(i.Prefix, "%d", &bits)
	} else if m := net.ParseIP(i.Netmask).To4(); m != nil {
		bits, _ = net.IPMask(m).Size()
	}
	if bits < 0 || bits > 32 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, bits), true
}

type netIface struct {
	VM      string `json:"vm"`
	UUID    string `json:"uuid"`
	MAC     string `json:"mac"`
	Running bool   `json:"running"`
}

type netLease struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
	Expiry   int64  `json:"expiry"` // unix seconds
}

type networkInfo struct {
	Name       string     `json:"name"`
	UUID       string     `json:"uuid"`
	Active     bool       `json:"active"`
	Autostart  bool       `json:"autostart"`
	Persistent bool       `json:"persistent"`
	Forward    string     `json:"forward"` // isolated | nat | route | open | bridge ...
	Bridge     string     `json:"bridge,omitempty"`
	Address    string     `json:"address,omitempty"` // gateway/prefix
	DHCP       bool       `json:"dhcp"`
	DHCPStart  string     `json:"dhcpStart,omitempty"`
	DHCPEnd    string     `json:"dhcpEnd,omitempty"`
	Domain     string     `json:"domain,omitempty"`
	OVS        bool       `json:"ovs,omitempty"`
	VLAN       int        `json:"vlan,omitempty"`
	Hosts      []dhcpHost `json:"hosts"`
	Interfaces []netIface `json:"interfaces"`
	Leases     []netLease `json:"leases"`
	XML        string     `json:"xml,omitempty"`
}

func parseNetXML(raw string) (netXML, error) {
	var x netXML
	err := xml.Unmarshal([]byte(raw), &x)
	return x, err
}

// dhcpIP finds the IPv4 <ip> element that serves DHCP and its index among
// all <ip> elements, which is what NetworkUpdate calls the parent index.
func (x netXML) dhcpIP() (netIP, int, bool) {
	for i, ip := range x.IP {
		if ip.DHCP != nil {
			if _, ok := ip.prefix(); ok {
				return ip, i, true
			}
		}
	}
	return netIP{}, 0, false
}

func (x netXML) info() networkInfo {
	info := networkInfo{Name: x.Name, UUID: x.UUID, Forward: "isolated", Bridge: x.Bridge.Name, Domain: x.Domain.Name,
		Hosts: []dhcpHost{}, Interfaces: []netIface{}, Leases: []netLease{}}
	if x.Forward != nil {
		info.Forward = x.Forward.Mode
		if info.Forward == "" {
			info.Forward = "nat"
		}
	}
	info.OVS = x.VirtualPort != nil && x.VirtualPort.Type == "openvswitch"
	if x.VLAN != nil && len(x.VLAN.Tags) > 0 {
		info.VLAN = x.VLAN.Tags[0].ID
	}
	for _, ip := range x.IP {
		if p, ok := ip.prefix(); ok {
			info.Address = p.String()
			break
		}
	}
	if ip, _, ok := x.dhcpIP(); ok {
		info.DHCP = true
		if len(ip.DHCP.Ranges) > 0 {
			info.DHCPStart, info.DHCPEnd = ip.DHCP.Ranges[0].Start, ip.DHCP.Ranges[0].End
		}
		info.Hosts = append(info.Hosts, ip.DHCP.Hosts...)
	}
	return info
}

// netIfaces maps network name to the VM interfaces attached to it.
func netIfaces(h *hostconn.Host) (map[string][]netIface, error) {
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}
	out := map[string][]netIface{}
	for _, d := range doms {
		raw, err := h.DomainGetXMLDesc(d, 0)
		if err != nil {
			continue // the domain went away while we were looking
		}
		var x struct {
			Interfaces []struct {
				Type string `xml:"type,attr"`
				MAC  struct {
					Address string `xml:"address,attr"`
				} `xml:"mac"`
				Source struct {
					Network string `xml:"network,attr"`
				} `xml:"source"`
			} `xml:"devices>interface"`
		}
		if xml.Unmarshal([]byte(raw), &x) != nil {
			continue
		}
		active, _ := h.DomainIsActive(d)
		for _, i := range x.Interfaces {
			if i.Type == "network" && i.Source.Network != "" {
				out[i.Source.Network] = append(out[i.Source.Network], netIface{VM: d.Name, UUID: cluster.FormatUUID(d.UUID), MAC: i.MAC.Address, Running: active != 0})
			}
		}
	}
	return out, nil
}

// describeNetwork gathers everything the UI shows about one network.
func describeNetwork(h *hostconn.Host, n libvirt.Network, ifaces map[string][]netIface, withXML bool) (networkInfo, error) {
	raw, err := h.NetworkGetXMLDesc(n, 0)
	if err != nil {
		return networkInfo{}, err
	}
	x, err := parseNetXML(raw)
	if err != nil {
		return networkInfo{}, err
	}
	info := x.info()
	info.Name, info.UUID = n.Name, cluster.FormatUUID(n.UUID)
	if a, err := h.NetworkIsActive(n); err == nil {
		info.Active = a != 0
	}
	if a, err := h.NetworkIsPersistent(n); err == nil {
		info.Persistent = a != 0
	}
	if a, err := h.NetworkGetAutostart(n); err == nil {
		info.Autostart = a != 0
	}
	if withXML {
		// The persistent definition is what the editor changes.
		if inactive, err := h.NetworkGetXMLDesc(n, uint32(libvirt.NetworkXMLInactive)); err == nil {
			info.XML = inactive
		} else {
			info.XML = raw
		}
	}
	if is := ifaces[n.Name]; is != nil {
		info.Interfaces = is
	}
	if info.Active && info.DHCP {
		if leases, _, err := h.NetworkGetDhcpLeases(n, nil, 1, 0); err == nil {
			for _, l := range leases {
				nl := netLease{IP: l.Ipaddr, Expiry: l.Expirytime}
				if len(l.Mac) > 0 {
					nl.MAC = l.Mac[0]
				}
				if len(l.Hostname) > 0 {
					nl.Hostname = l.Hostname[0]
				}
				info.Leases = append(info.Leases, nl)
			}
			sort.Slice(info.Leases, func(i, j int) bool { return info.Leases[i].IP < info.Leases[j].IP })
		}
	}
	return info, nil
}

func (s *Server) listNetworks(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := collectNetworks(h)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, out)
}

// collectNetworks lists the virtual networks of one node, sorted by name.
func collectNetworks(h *hostconn.Host) ([]networkInfo, error) {
	nets, _, err := h.ConnectListAllNetworks(1, 0)
	if err != nil {
		return nil, err
	}
	ifaces, err := netIfaces(h)
	if err != nil {
		return nil, err
	}
	out := []networkInfo{}
	for _, n := range nets {
		info, err := describeNetwork(h, n, ifaces, false)
		if err != nil {
			continue // removed while listing
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) network(r *http.Request) (*hostconn.Host, libvirt.Network, error) {
	_, h, err := s.host(r)
	if err != nil {
		return nil, libvirt.Network{}, err
	}
	n, err := h.NetworkLookupByName(r.PathValue("net"))
	return h, n, err
}

func (s *Server) getNetwork(w http.ResponseWriter, r *http.Request) {
	h, n, err := s.network(r)
	if err != nil {
		fail(w, err)
		return
	}
	ifaces, err := netIfaces(h)
	if err != nil {
		fail(w, err)
		return
	}
	info, err := describeNetwork(h, n, ifaces, true)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, info)
}

// --- creation ---

type networkRequest struct {
	Name string `json:"name"`
	Mode string `json:"mode"` // nat | isolated | routed | open | bridge
	// BridgeName: the bridge to create for nat/isolated/routed/open (optional,
	// libvirt picks virbrN), or the existing host bridge to attach to (bridge).
	BridgeName string `json:"bridgeName"`
	Address    string `json:"address"` // gateway address with prefix, e.g. 10.77.1.1/24
	DHCP       bool   `json:"dhcp"`
	DHCPStart  string `json:"dhcpStart"`
	DHCPEnd    string `json:"dhcpEnd"`
	Domain     string `json:"domain"`
	OVS        bool   `json:"ovs"`  // bridge mode: the host bridge is Open vSwitch
	VLAN       int    `json:"vlan"` // bridge mode: access VLAN tag (Open vSwitch, or a VLAN-aware Linux bridge)
	// XML, when set, is used verbatim instead of the fields above.
	XML string `json:"xml"`
	// linuxVLAN is what the node says about the bridge, for a VLAN tag on a Linux bridge (set by createNetwork).
	linuxVLAN *linuxVLAN
}

var (
	netNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	netBridgeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)
	dnsDomainRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,250}[A-Za-z0-9])?$`)
	hostNameRE  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})$`)
)

func nextAddr(a netip.Addr, n int) netip.Addr {
	b := a.As4()
	v := binary.BigEndian.Uint32(b[:]) + uint32(n)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func broadcastOf(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | (^uint32(0) >> p.Bits())
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func netmaskOf(bits int) string {
	return net.IP(net.CIDRMask(bits, 32)).String()
}

// nextFreeSubnet returns the gateway address (10.77.N.1/24) of the first private
// /24 from 10.77.1.0 upwards that overlaps no existing network.
func nextFreeSubnet(existing []existingNet) (netip.Prefix, bool) {
	for n := 1; n <= 254; n++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 77, byte(n), 1}), 24)
		clash := false
		for _, e := range existing {
			if e.Subnet.IsValid() && e.Subnet.Overlaps(p.Masked()) {
				clash = true
				break
			}
		}
		if !clash {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// defaultDHCPRange is the whole subnet except the gateway (.2 to .254 for a gateway
// at .1 in a /24); false when the gateway sits in the middle of the subnet.
func defaultDHCPRange(p netip.Prefix) (start, end netip.Addr, ok bool) {
	subnet := p.Masked()
	first, last := nextAddr(subnet.Addr(), 1), nextAddr(broadcastOf(p), -1)
	switch p.Addr() {
	case first, subnet.Addr():
		return nextAddr(first, 1), last, true
	case last:
		return first, nextAddr(last, -1), true
	}
	return first, last, false
}

// existingNet is what a new network must not collide with.
type existingNet struct {
	Name   string
	Mode   string
	Bridge string
	Subnet netip.Prefix // zero when the network has no IPv4 address
}

// build validates the request and produces the XML of a new network.
func (q networkRequest) build(existing []existingNet) (string, error) {
	if !netNameRE.MatchString(q.Name) {
		return "", badRequest("name must start with a letter or digit and contain only letters, digits, dot, dash and underscore")
	}
	for _, e := range existing {
		if e.Name == q.Name {
			return "", badRequest("a network named " + q.Name + " already exists on this node")
		}
	}
	if q.BridgeName != "" && !netBridgeRE.MatchString(q.BridgeName) {
		return "", badRequest("bridge name must be at most 15 characters: letters, digits, dot, dash and underscore")
	}
	if q.Domain != "" && !dnsDomainRE.MatchString(q.Domain) {
		return "", badRequest("DNS domain name is not valid")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<network>\n  <name>%s</name>\n", esc(q.Name))
	if q.Mode == "bridge" {
		if q.BridgeName == "" {
			return "", badRequest("the name of the host bridge is required")
		}
		for _, e := range existing {
			// A bridge that libvirt itself manages for another network is not ours to share.
			if e.Bridge == q.BridgeName && e.Mode != "bridge" {
				return "", badRequest("bridge " + q.BridgeName + " is created by libvirt network " + e.Name + "; use a bridge made outside libvirt")
			}
		}
		if q.Address != "" || q.DHCP {
			return "", badRequest("a bridged network gets its addresses from the network behind the bridge; leave address and DHCP empty")
		}
		fmt.Fprintf(&b, "  <forward mode='bridge'/>\n  <bridge name='%s'/>\n", esc(q.BridgeName))
		switch {
		case q.OVS:
			b.WriteString("  <virtualport type='openvswitch'/>\n")
			if q.VLAN != 0 {
				if q.VLAN < 1 || q.VLAN > 4094 {
					return "", badRequest("VLAN tag must be between 1 and 4094")
				}
				fmt.Fprintf(&b, "  <vlan><tag id='%d'/></vlan>\n", q.VLAN)
			}
		case q.VLAN != 0:
			// libvirt 11.0 and newer tag the ports of a Linux bridge, but the bridge must filter VLANs and its
			// uplink must allow the id: the host side is set up by host networking (hostnet.go).
			tag, err := q.linuxVLAN.vlanXML(q.BridgeName, q.VLAN)
			if err != nil {
				return "", err
			}
			b.WriteString(tag)
		}
		b.WriteString("</network>\n")
		return b.String(), nil
	}

	switch q.Mode {
	case "nat", "routed", "open":
		fmt.Fprintf(&b, "  <forward mode='%s'/>\n", map[string]string{"nat": "nat", "routed": "route", "open": "open"}[q.Mode])
	case "isolated":
	default:
		return "", badRequest("mode must be nat, isolated, routed, open or bridge")
	}
	if q.OVS || q.VLAN != 0 {
		return "", badRequest("Open vSwitch and VLAN options apply to bridged networks only")
	}
	if q.BridgeName != "" {
		for _, e := range existing {
			if e.Bridge == q.BridgeName {
				return "", badRequest("bridge name " + q.BridgeName + " is already used by network " + e.Name)
			}
		}
		fmt.Fprintf(&b, "  <bridge name='%s' stp='on' delay='0'/>\n", esc(q.BridgeName))
	}
	if q.Domain != "" {
		fmt.Fprintf(&b, "  <domain name='%s'/>\n", esc(q.Domain))
	}
	if q.Address == "" && q.Mode != "isolated" {
		// The UI always sends one; a script that leaves it out gets the next free private /24.
		free, ok := nextFreeSubnet(existing)
		if !ok {
			return "", badRequest("no free private /24 subnet is left; give an address")
		}
		q.Address = free.String()
	}
	if q.Address == "" {
		if q.DHCP {
			return "", badRequest("DHCP needs an IPv4 address")
		}
		b.WriteString("</network>\n")
		return b.String(), nil
	}
	p, err := netip.ParsePrefix(q.Address)
	if err != nil || !p.Addr().Is4() {
		return "", badRequest("address must be an IPv4 address with prefix length, for example 10.77.1.1/24")
	}
	if p.Bits() < 8 || p.Bits() > 29 {
		return "", badRequest("prefix length must be between 8 and 29")
	}
	subnet := p.Masked()
	gw := p.Addr()
	if gw == subnet.Addr() { // given as the network address: use the first host
		gw = nextAddr(gw, 1)
	}
	if gw == broadcastOf(p) {
		return "", badRequest("the address is the broadcast address of its subnet")
	}
	for _, e := range existing {
		if e.Subnet.IsValid() && e.Subnet.Overlaps(subnet) {
			return "", badRequest(fmt.Sprintf("subnet %s overlaps %s of network %s", subnet, e.Subnet.Masked(), e.Name))
		}
	}
	fmt.Fprintf(&b, "  <ip address='%s' netmask='%s'>\n", gw, netmaskOf(p.Bits()))
	if q.DHCP {
		start, end, rangeOK := defaultDHCPRange(p)
		if q.DHCPStart != "" || q.DHCPEnd != "" {
			var e1, e2 error
			start, e1 = netip.ParseAddr(q.DHCPStart)
			end, e2 = netip.ParseAddr(q.DHCPEnd)
			if e1 != nil || e2 != nil || !start.Is4() || !end.Is4() {
				return "", badRequest("DHCP range start and end must both be IPv4 addresses")
			}
		} else if !rangeOK {
			return "", badRequest("please give the DHCP range explicitly: the gateway address is inside the subnet's middle")
		}
		if !subnet.Contains(start) || !subnet.Contains(end) || start == subnet.Addr() || end == broadcastOf(p) {
			return "", badRequest(fmt.Sprintf("DHCP range %s - %s is not inside subnet %s", start, end, subnet))
		}
		if start.Compare(end) > 0 {
			return "", badRequest("DHCP range start is after its end")
		}
		if gw.Compare(start) >= 0 && gw.Compare(end) <= 0 {
			return "", badRequest("DHCP range contains the gateway address " + gw.String())
		}
		fmt.Fprintf(&b, "    <dhcp><range start='%s' end='%s'/></dhcp>\n", start, end)
	}
	b.WriteString("  </ip>\n</network>\n")
	return b.String(), nil
}

// inventory lists what exists on the node, for collision checks.
func inventory(h *hostconn.Host) ([]existingNet, error) {
	nets, _, err := h.ConnectListAllNetworks(1, 0)
	if err != nil {
		return nil, err
	}
	var out []existingNet
	for _, n := range nets {
		e := existingNet{Name: n.Name, Mode: "isolated"}
		if raw, err := h.NetworkGetXMLDesc(n, 0); err == nil {
			if x, err := parseNetXML(raw); err == nil {
				info := x.info()
				e.Mode, e.Bridge = info.Forward, info.Bridge
				for _, ip := range x.IP {
					if p, ok := ip.prefix(); ok {
						e.Subnet = p
						break
					}
				}
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// createNetwork defines, starts and sets autostart on a new network; if a
// step fails the definition is removed again.
func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req networkRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	existing, err := inventory(h)
	if err != nil {
		fail(w, err)
		return
	}
	var doc string
	if req.XML != "" {
		var x netXML
		if err := xml.Unmarshal([]byte(req.XML), &x); err != nil || x.Name == "" {
			fail(w, badRequest("the XML is not a network definition with a name"))
			return
		}
		for _, e := range existing {
			if e.Name == x.Name { // defining over an existing name would replace that network
				fail(w, badRequest("a network named "+x.Name+" already exists on this node; edit it instead"))
				return
			}
		}
		doc, req.Name = req.XML, x.Name
	} else if doc, err = req.build(existing); err != nil {
		fail(w, err)
		return
	}
	if req.XML == "" && req.Mode == "bridge" && !req.OVS && req.VLAN != 0 {
		req.linuxVLAN = linuxVLANFor(h, req.BridgeName)
	}
	if req.XML == "" && req.Mode == "bridge" && !req.OVS {
		if _, err := h.Run("test -d /sys/class/net/" + shellQuote(req.BridgeName) + "/bridge"); err != nil {
			fail(w, badRequest("there is no Linux bridge named "+req.BridgeName+" on this node"))
			return
		}
	}
	err = s.task(r, "create network", req.Name, h.Name, func(func(float64)) error {
		n, err := h.NetworkDefineXMLFlags(doc, uint32(libvirt.NetworkDefineValidate))
		if err != nil {
			return err
		}
		if err := h.NetworkCreate(n); err != nil {
			h.NetworkUndefine(n)
			return fmt.Errorf("%w (the network definition was removed again)", err)
		}
		if err := h.NetworkSetAutostart(n, 1); err != nil {
			h.NetworkDestroy(n)
			h.NetworkUndefine(n)
			return fmt.Errorf("%w (the network was removed again)", err)
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"name": req.Name})
}

// --- actions ---

func attachedNames(is []netIface, runningOnly bool) []string {
	seen := map[string]bool{}
	var names []string
	for _, i := range is {
		if (!runningOnly || i.Running) && !seen[i.VM] {
			seen[i.VM] = true
			names = append(names, i.VM)
		}
	}
	return names
}

func (s *Server) networkAction(w http.ResponseWriter, r *http.Request) {
	h, n, err := s.network(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Action string `json:"action"` // start | stop | autostart | noautostart | delete
		Force  bool   `json:"force"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	active := func() bool { a, _ := h.NetworkIsActive(n); return a != 0 }
	var fn func() error
	switch req.Action {
	case "start":
		fn = func() error { return h.NetworkCreate(n) }
	case "autostart", "noautostart":
		on := int32(0)
		if req.Action == "autostart" {
			on = 1
		}
		fn = func() error { return h.NetworkSetAutostart(n, on) }
	case "stop", "delete":
		ifaces, err := netIfaces(h)
		if err != nil {
			fail(w, err)
			return
		}
		if !req.Force {
			if req.Action == "stop" {
				if names := attachedNames(ifaces[n.Name], true); len(names) > 0 {
					fail(w, badRequest("running VMs are attached to this network ("+strings.Join(names, ", ")+"); stopping it cuts their connectivity. Stop them first or force."))
					return
				}
			} else {
				if names := attachedNames(ifaces[n.Name], false); len(names) > 0 {
					fail(w, badRequest("VMs are attached to this network ("+strings.Join(names, ", ")+"); detach them first or force."))
					return
				}
				if active() {
					fail(w, badRequest("the network is active; stop it first or force"))
					return
				}
			}
		}
		if req.Action == "stop" {
			fn = func() error { return h.NetworkDestroy(n) }
		} else {
			fn = func() error {
				if active() {
					if err := h.NetworkDestroy(n); err != nil {
						return err
					}
				}
				if p, _ := h.NetworkIsPersistent(n); p != 0 {
					return h.NetworkUndefine(n)
				}
				return nil
			}
		}
	default:
		fail(w, badRequest("unknown action "+req.Action))
		return
	}
	if err := s.task(r, req.Action+" network", n.Name, h.Name, func(func(float64)) error { return fn() }); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// putNetworkXML redefines a network from edited XML. libvirt applies a
// changed definition to a running network only at its next restart.
func (s *Server) putNetworkXML(w http.ResponseWriter, r *http.Request) {
	h, n, err := s.network(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		XML string `json:"xml"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	x, err := parseNetXML(req.XML)
	switch {
	case err != nil:
		fail(w, badRequest("the XML could not be parsed: "+err.Error()))
		return
	case x.Name != n.Name:
		fail(w, badRequest("the name in the XML must stay "+n.Name+"; renaming a network is not possible"))
		return
	case x.UUID != "" && x.UUID != cluster.FormatUUID(n.UUID):
		fail(w, badRequest("the UUID in the XML must stay "+cluster.FormatUUID(n.UUID)))
		return
	}
	err = s.task(r, "edit network XML", n.Name, h.Name, func(func(float64)) error {
		_, err := h.NetworkDefineXMLFlags(req.XML, uint32(libvirt.NetworkDefineValidate))
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	note := "Saved."
	if a, _ := h.NetworkIsActive(n); a != 0 {
		note = "Saved. The network is running; the changes apply the next time it is stopped and started."
	}
	writeJSON(w, map[string]any{"ok": true, "note": note})
}

// --- DHCP reservations ---

// updateHosts applies one change to the DHCP host entries of a network, to
// the running network and its saved definition, without restarting it.
func (s *Server) updateHosts(w http.ResponseWriter, r *http.Request, cmd libvirt.NetworkUpdateCommand, mac string, h2 *dhcpHost) {
	h, n, err := s.network(r)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := h.NetworkGetXMLDesc(n, 0)
	if err != nil {
		fail(w, err)
		return
	}
	x, err := parseNetXML(raw)
	if err != nil {
		fail(w, err)
		return
	}
	ip, idx, ok := x.dhcpIP()
	if !ok {
		fail(w, badRequest("this network has no DHCP server"))
		return
	}
	var cur *dhcpHost
	for i := range ip.DHCP.Hosts {
		if strings.EqualFold(ip.DHCP.Hosts[i].MAC, mac) {
			cur = &ip.DHCP.Hosts[i]
		}
	}
	var entry dhcpHost
	switch cmd {
	case libvirt.NetworkUpdateCommandAddLast:
		if cur != nil {
			fail(w, badRequest("there is already a reservation for "+mac))
			return
		}
		entry = *h2
	case libvirt.NetworkUpdateCommandModify:
		if cur == nil {
			fail(w, badRequest("there is no reservation for "+mac))
			return
		}
		entry = *h2
	default: // delete: send the stored entry whole so libvirt matches it exactly
		if cur == nil {
			fail(w, badRequest("there is no reservation for "+mac))
			return
		}
		entry = *cur
	}
	if h2 != nil {
		p, _ := ip.prefix()
		a, err := netip.ParseAddr(entry.IP)
		switch {
		case err != nil || !a.Is4():
			fail(w, badRequest("the IP address is not a valid IPv4 address"))
			return
		case !p.Masked().Contains(a) || a == p.Masked().Addr() || a == broadcastOf(p):
			fail(w, badRequest(fmt.Sprintf("%s is not a host address of subnet %s", a, p.Masked())))
			return
		case a == p.Addr():
			fail(w, badRequest(a.String()+" is the gateway address of this network"))
			return
		}
		for _, o := range ip.DHCP.Hosts {
			if o.IP == entry.IP && !strings.EqualFold(o.MAC, mac) {
				fail(w, badRequest(entry.IP+" is already reserved for "+o.MAC))
				return
			}
		}
	}
	frag := fmt.Sprintf("<host mac='%s' ip='%s'", esc(entry.MAC), esc(entry.IP))
	if entry.Name != "" {
		frag += fmt.Sprintf(" name='%s'", esc(entry.Name))
	}
	frag += "/>"
	var flags libvirt.NetworkUpdateFlags
	if a, _ := h.NetworkIsActive(n); a != 0 {
		flags |= libvirt.NetworkUpdateAffectLive
	}
	if p, _ := h.NetworkIsPersistent(n); p != 0 {
		flags |= libvirt.NetworkUpdateAffectConfig
	}
	kind := map[libvirt.NetworkUpdateCommand]string{libvirt.NetworkUpdateCommandAddLast: "add", libvirt.NetworkUpdateCommandModify: "modify", libvirt.NetworkUpdateCommandDelete: "remove"}[cmd]
	err = s.task(r, kind+" DHCP reservation", n.Name+" "+mac, h.Name, func(func(float64)) error {
		// NetworkUpdateCompat picks the argument order the daemon expects;
		// older libvirt versions swapped command and section on the wire.
		return h.NetworkUpdateCompat(n, cmd, libvirt.NetworkSectionIPDhcpHost, int32(idx), frag, flags)
	})
	if err != nil {
		fail(w, err)
		return
	}
	// The UI refreshes on network events; this one is not a lifecycle event.
	s.Nodes.Publish(cluster.Event{Type: "network", Node: nodeID(r), Name: n.Name, Detail: "updated"})
	writeJSON(w, map[string]bool{"ok": true})
}

func nodeID(r *http.Request) int64 {
	var id int64
	fmt.Sscanf(r.PathValue("node"), "%d", &id)
	return id
}

func parseReservation(r *http.Request, mac string) (*dhcpHost, error) {
	var req dhcpHost
	if err := readJSON(r, &req); err != nil {
		return nil, err
	}
	if mac != "" {
		req.MAC = mac
	}
	hw, err := net.ParseMAC(req.MAC)
	if err != nil || len(hw) != 6 {
		return nil, badRequest("the MAC address is not valid (expected aa:bb:cc:dd:ee:ff)")
	}
	if hw[0]&1 != 0 {
		return nil, badRequest("a multicast MAC address cannot be reserved")
	}
	req.MAC = hw.String()
	if req.Name != "" && !hostNameRE.MatchString(req.Name) {
		return nil, badRequest("the host name may contain only letters, digits and dashes")
	}
	return &req, nil
}

func (s *Server) dhcpHostAdd(w http.ResponseWriter, r *http.Request) {
	e, err := parseReservation(r, "")
	if err != nil {
		fail(w, err)
		return
	}
	s.updateHosts(w, r, libvirt.NetworkUpdateCommandAddLast, e.MAC, e)
}

func (s *Server) dhcpHostUpdate(w http.ResponseWriter, r *http.Request) {
	e, err := parseReservation(r, r.PathValue("mac"))
	if err != nil {
		fail(w, err)
		return
	}
	s.updateHosts(w, r, libvirt.NetworkUpdateCommandModify, e.MAC, e)
}

func (s *Server) dhcpHostDelete(w http.ResponseWriter, r *http.Request) {
	hw, err := net.ParseMAC(r.PathValue("mac"))
	if err != nil || len(hw) != 6 {
		fail(w, badRequest("the MAC address is not valid"))
		return
	}
	s.updateHosts(w, r, libvirt.NetworkUpdateCommandDelete, hw.String(), nil)
}
