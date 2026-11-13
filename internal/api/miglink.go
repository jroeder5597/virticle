package api

// Which network the migration data of a node uses, and how fast it may go.
//
// A node has a "migration network": the address other nodes send migration data to (memory, the live disk copy,
// copied files, cross-node backups). Empty means the node's management address. This file reads what the nodes
// report about their interfaces (the same list the topology uses), offers the choices, and builds the facts
// for the decision in miglimit.go.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// nodeNet is what is known about the network of one node.
type nodeNet struct {
	Name      string
	Node      store.Node
	NICs      []hostNIC
	MgmtIPs   []string // the addresses the controller uses
	MgmtIface string   // the interface holding one of them ("" unknown)
	MigIP     string   // the address other nodes send migration data to
	MigIface  string   // the interface holding it ("" unknown)
}

func hostIPs(addr string) []string {
	h, _ := splitAddr(addr)
	return cluster.ResolveAddr(h)
}

// nicHolding is the interface that holds the address ip.
func nicHolding(nics []hostNIC, ip string) string {
	for _, n := range nics {
		for _, a := range n.Addresses {
			if addrIP(a) == ip {
				return n.Name
			}
		}
	}
	return ""
}

// nicByName finds an interface in the list.
func nicByName(nics []hostNIC, name string) (hostNIC, bool) {
	for _, n := range nics {
		if n.Name == name {
			return n, true
		}
	}
	return hostNIC{}, false
}

// nicSpeed is the link speed in Mbit/s of the interface, 0 when unknown. A bridge has none of its own: it
// takes the speed of its one physical card or bond (the ports of VMs do not count).
func nicSpeed(nics []hostNIC, name string) int {
	n, ok := nicByName(nics, name)
	if !ok {
		return 0
	}
	if n.SpeedMbps > 0 {
		return n.SpeedMbps
	}
	if !isBridgeDevice(n) {
		return 0
	}
	speed := 0
	for _, m := range nics {
		if m.Master == name && (m.Type == "physical" || m.Type == "bond") && m.SpeedMbps > 0 {
			if speed == 0 || m.SpeedMbps < speed {
				speed = m.SpeedMbps
			}
		}
	}
	return speed
}

// pickIP chooses, among the addresses a name resolves to, the one a node of this list holds (else the first).
func pickIP(nics []hostNIC, ips []string) string {
	for _, ip := range ips {
		if nicHolding(nics, ip) != "" {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// buildNodeNet works out the facts about one node from its record and interface list.
func buildNodeNet(name string, n store.Node, nics []hostNIC) nodeNet {
	nn := nodeNet{Name: name, Node: n, NICs: nics, MgmtIPs: hostIPs(n.Addr)}
	nn.MgmtIface = nicHolding(nics, pickIP(nics, nn.MgmtIPs))
	mh, _ := migrationTarget(n)
	nn.MigIP = pickIP(nics, cluster.ResolveAddr(mh))
	nn.MigIface = nicHolding(nics, nn.MigIP)
	return nn
}

func (s *Server) readNodeNet(h *hostconn.Host) (nodeNet, error) {
	cfg, ok := s.Nodes.ConfigOfHost(h)
	if !ok {
		return nodeNet{}, fmt.Errorf("%s is not a known node", h.Name)
	}
	nics, err := hostNICList(h)
	if err != nil {
		return nodeNet{}, err
	}
	return buildNodeNet(h.Name, cfg, nics), nil
}

// egressOf asks h which interface and source address it uses to reach ip.
func egressOf(h *hostconn.Host, ip string) (dev, src string) {
	if net.ParseIP(ip) == nil {
		return "", ""
	}
	out, err := h.Run("ip -j route get " + ip)
	if err != nil {
		return "", ""
	}
	var r []struct {
		Dev     string `json:"dev"`
		Prefsrc string `json:"prefsrc"`
	}
	if json.Unmarshal([]byte(out), &r) != nil || len(r) == 0 {
		return "", ""
	}
	return r[0].Dev, r[0].Prefsrc
}

// sideOf makes the link side of a node whose migration data uses iface with address addr.
func sideOf(nn nodeNet, iface, addr string) linkSide {
	return linkSide{
		Name: nn.Name, Iface: iface, Addr: addr,
		Shared:    iface != "" && iface == nn.MgmtIface,
		SpeedMbps: nicSpeed(nn.NICs, iface),
		Limit:     storedLimit(nn.Node.MigrationLimit),
	}
}

// transferNet is what is known about one transfer from the source node to the target node.
type transferNet struct {
	Src, Dst linkSide
	Row      string // "enp2s0 on both nodes (10.0.1.5 → 10.0.1.6)"
	Limit    limitDecision
}

// RateBps is the cap in bytes per second, 0 when the transfer is not limited.
func (t transferNet) RateBps() uint64 { return mbitToBytes(t.Limit.Mbit) }

// buildTransfer is the pure part: the egress of the source towards the target is given (empty: unknown, the
// source's own migration interface is used).
func buildTransfer(src, dst nodeNet, egressDev, egressAddr string) transferNet {
	dsts := sideOf(dst, dst.MigIface, dst.MigIP)
	iface, addr := egressDev, egressAddr
	if iface == "" {
		iface, addr = src.MigIface, src.MigIP
	}
	srcs := sideOf(src, iface, addr)
	t := transferNet{Src: srcs, Dst: dsts}
	t.Limit = decideLimit(srcs, dsts)
	t.Row = networkRow(srcs, dsts)
	return t
}

// networkRow is the sentence for the pre-flight row "Migration network".
func networkRow(src, dst linkSide) string {
	arrow := ""
	if src.Addr != "" && dst.Addr != "" {
		arrow = fmt.Sprintf(" (%s → %s)", src.Addr, dst.Addr)
	}
	name := func(s linkSide) string {
		if s.Shared || s.Iface == "" {
			return ""
		}
		return s.Iface
	}
	a, b := name(src), name(dst)
	switch {
	case a == "" && b == "":
		return "Migration uses the management network on both nodes" + arrow
	case a == b:
		return a + " on both nodes" + arrow
	case a == "":
		return fmt.Sprintf("Migration uses the management network on %s and %s on %s%s", src.Name, b, dst.Name, arrow)
	case b == "":
		return fmt.Sprintf("Migration uses %s on %s and the management network on %s%s", a, src.Name, dst.Name, arrow)
	}
	return fmt.Sprintf("%s on %s, %s on %s%s", a, src.Name, b, dst.Name, arrow)
}

// transferBetween collects the facts for a transfer from src to the node dstNode (whose host is dst).
func (s *Server) transferBetween(src, dst *hostconn.Host) (transferNet, error) {
	sn, err := s.readNodeNet(src)
	if err != nil {
		return transferNet{}, err
	}
	dn, err := s.readNodeNet(dst)
	if err != nil {
		return transferNet{}, err
	}
	dev, addr := egressOf(src, dn.MigIP)
	return buildTransfer(sn, dn, dev, addr), nil
}

// --- the choices offered for a node ---

type migOption struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	SpeedMbps int    `json:"speedMbps,omitempty"`
	Label     string `json:"label"`
}

// hiddenNIC is true for interfaces whose addresses other nodes cannot use: loopback, VM ports, container and
// libvirt NAT bridges.
func hiddenNIC(n hostNIC) bool {
	if n.Type == "tap" || n.State == "down" {
		return true
	}
	for _, p := range []string{"virbr", "docker", "veth", "vnet", "cni", "flannel", "podman", "lxcbr", "lxdbr"} {
		if strings.HasPrefix(n.Name, p) {
			return true
		}
	}
	return false
}

func usableIP(s string) (string, bool) {
	a, err := netip.ParseAddr(addrIP(s))
	if err != nil || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return "", false
	}
	return a.String(), true
}

func optionLabel(iface, ip string, speed int) string {
	l := fmt.Sprintf("%s (%s)", iface, ip)
	if speed > 0 {
		l += ", " + fmtLinkSpeed(speed)
	}
	return l
}

// migrationOptions lists the addresses of the node other than the management address, grouped by interface
// (in the order of the interface list).
func migrationOptions(nics []hostNIC, mgmtIPs []string) []migOption {
	mgmt := map[string]bool{}
	for _, m := range mgmtIPs {
		mgmt[m] = true
	}
	out := []migOption{}
	for _, n := range nics {
		if hiddenNIC(n) {
			continue
		}
		for _, a := range n.Addresses {
			ip, ok := usableIP(a)
			if !ok || mgmt[ip] {
				continue
			}
			sp := nicSpeed(nics, n.Name)
			out = append(out, migOption{Interface: n.Name, Address: ip, SpeedMbps: sp, Label: optionLabel(n.Name, ip, sp)})
		}
	}
	return out
}

// migNetInfo is the answer of GET /api/nodes/{node}/migration-network.
type migNetInfo struct {
	Online  bool   `json:"online"`
	Address string `json:"address"` // stored; empty: same as management
	Limit   string `json:"limit"`   // stored; "", "none" or "custom:<Mbit/s>"
	// Management is the first entry of the list, "Same as management (<address>)".
	Management migOption   `json:"management"`
	Options    []migOption `json:"options"`
	// Custom is a stored address that matches no interface of the node.
	Custom string `json:"custom,omitempty"`
	// Show is false when the node has a single usable address: there is nothing to choose.
	Show bool `json:"show"`
	// Current is the selected choice in words, for the row on the Settings tab.
	Current string `json:"current"`
	// Interface and Shared: the interface the data of this node uses, and whether it also carries management.
	Interface string `json:"interface,omitempty"`
	Shared    bool   `json:"shared"`
	SpeedMbps int    `json:"speedMbps,omitempty"`
	// LimitText is the limit in words, for the row.
	LimitText string `json:"limitText"`
}

func limitWords(l limitSetting) string {
	switch l.Mode {
	case limitNone:
		return "No limit"
	case limitCustom:
		return fmt.Sprintf("Custom (%d Mbit/s)", l.Mbit)
	}
	return fmt.Sprintf("%d%% of the link (default)", autoPercent)
}

// describeMigNet builds the answer from the node record and its interface list (nics is nil when the node is
// offline: the stored settings are still reported).
func describeMigNet(n store.Node, nics []hostNIC) migNetInfo {
	info := migNetInfo{Address: n.MigrationAddr, Limit: n.MigrationLimit, Options: []migOption{}, Online: nics != nil,
		LimitText: limitWords(storedLimit(n.MigrationLimit))}
	mgmtIPs := hostIPs(n.Addr)
	mh, _ := splitAddr(n.Addr)
	info.Management = migOption{Address: mh, Label: "Same as management (" + mh + ")"}
	info.Current = info.Management.Label
	if nics == nil {
		if n.MigrationAddr != "" {
			info.Custom = n.MigrationAddr
			info.Current = "Custom (" + n.MigrationAddr + ")"
		}
		info.Show = n.MigrationAddr != ""
		return info
	}
	nn := buildNodeNet("", n, nics)
	info.Management.Interface = nn.MgmtIface
	info.Options = migrationOptions(nics, mgmtIPs)
	info.Interface = nn.MigIface
	info.SpeedMbps = nicSpeed(nics, nn.MigIface)
	info.Shared = nn.MigIface != "" && nn.MigIface == nn.MgmtIface
	if host, _ := splitAddr(n.MigrationAddr); host != "" && !sameHost(n.MigrationAddr, n.Addr) && !slices.Contains(mgmtIPs, nn.MigIP) {
		found := false
		for _, o := range info.Options {
			if o.Address == host || nn.MigIP == o.Address {
				found, info.Current = true, o.Label
			}
		}
		if !found {
			info.Custom = n.MigrationAddr
			info.Current = "Custom (" + n.MigrationAddr + ")"
		}
	}
	info.Show = len(info.Options) > 0 || info.Custom != ""
	return info
}

// sameHost reports whether two node addresses name the same host (ports ignored).
func sameHost(a, b string) bool {
	ha, _ := splitAddr(a)
	hb, _ := splitAddr(b)
	return ha != "" && strings.EqualFold(ha, hb)
}
