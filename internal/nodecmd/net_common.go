package nodecmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"virticle/internal/nodeproto"
)

// Pieces of the plans that do not depend on the configuration system.

// taggedVLANs is the canonical list without id 1, which is the untagged default.
func taggedVLANs(canon string) string {
	var out []string
	for _, r := range nodeproto.VLANRanges(canon) {
		lo, hi := r[0], r[1]
		if lo == 1 {
			lo = 2
		}
		if lo > hi {
			continue
		}
		if lo == hi {
			out = append(out, fmt.Sprint(lo))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", lo, hi))
		}
	}
	return strings.Join(out, ",")
}

func vlanSummary(s *bridgeSpec) string {
	if !s.VLANAware {
		return "VLAN filtering stays off."
	}
	return fmt.Sprintf("The bridge is VLAN-aware (VLAN filtering on); its own address stays untagged (VLAN 1)%s.", func() string {
		if s.Uplink == "" {
			return ""
		}
		if s.Bond != "" {
			return fmt.Sprintf(", and the bond %s allows the VLAN ids %s", s.Bond, s.VLANs)
		}
		return fmt.Sprintf(", and %s allows the VLAN ids %s", s.Uplink, s.VLANs)
	}())
}

func moveSummary(in *planInput, s *bridgeSpec, method, gw string) (*nodeproto.NetAddressMove, string) {
	var addrs []string
	for _, a := range in.Uplink.globalAddrs() {
		addrs = append(addrs, a.CIDR())
	}
	mv := &nodeproto.NetAddressMove{From: in.Uplink.Name, To: s.Bridge, Method: method, Addresses: addrs, Gateway: gw, MAC: s.MAC}
	if gw == "" {
		mv.Gateway = defaultGateway(in.Snap, in.Uplink.Name)
	}
	txt := fmt.Sprintf("%s takes over the configuration of %s (%s, %s) and %s gets the same MAC address %s", s.Bridge, in.Uplink.Name, method, strings.Join(addrs, ", "), s.Bridge, s.MAC)
	if method == "dhcp" {
		txt += ", so that the DHCP server hands out the same address again (a lease follows the MAC address)"
	}
	return mv, txt + "."
}

// vlanIDs is the set of ids a spec allows on its port (without the default id 1).
func vlanIDs(s *bridgeSpec) map[int]bool {
	m := map[int]bool{}
	if !s.VLANAware {
		return m
	}
	for _, r := range nodeproto.VLANRanges(taggedVLANs(s.VLANs)) {
		for i := r[0]; i <= r[1]; i++ {
			m[i] = true
		}
	}
	return m
}

func rangesOfIDs(m map[int]bool) []string {
	ids := make([]int, 0, len(m))
	for i := range m {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	var out []string
	for i := 0; i < len(ids); i++ {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if i == j {
			out = append(out, fmt.Sprint(ids[i]))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		}
		i = j
	}
	return out
}

// vlanCommands are the runtime commands that turn the VLAN settings of from into those of to. The ids are
// set on the bridge's port: the bond when there is one.
func vlanCommands(from, to *bridgeSpec) []netStep {
	var add, del = map[int]bool{}, map[int]bool{}
	a, b := vlanIDs(from), vlanIDs(to)
	for i := range b {
		if !a[i] {
			add[i] = true
		}
	}
	for i := range a {
		if !b[i] {
			del[i] = true
		}
	}
	filt := func(on bool) netStep {
		v := "0"
		if on {
			v = "1"
		}
		return stepRun("ip", "link", "set", to.Bridge, "type", "bridge", "vlan_filtering", v)
	}
	var steps []netStep
	if to.VLANAware && !from.VLANAware {
		steps = append(steps, filt(true))
	}
	if from.Uplink != "" {
		for _, r := range rangesOfIDs(add) {
			steps = append(steps, stepRun("bridge", "vlan", "add", "dev", from.port(), "vid", r))
		}
		for _, r := range rangesOfIDs(del) {
			steps = append(steps, stepRun("bridge", "vlan", "del", "dev", from.port(), "vid", r))
		}
	}
	if !to.VLANAware && from.VLANAware {
		steps = append(steps, filt(false))
	}
	return steps
}

func ignoreAll(steps []netStep) []netStep {
	out := make([]netStep, len(steps))
	for i, s := range steps {
		s.Ignore = true
		out[i] = s
	}
	return out
}

func vlanChangeSummary(old, n *bridgeSpec) []string {
	if !n.VLANAware {
		return []string{fmt.Sprintf("Turn VLAN filtering off on %s.", n.Bridge)}
	}
	return []string{fmt.Sprintf("%s: VLAN filtering on, allowed VLAN ids %s (before: %s).", n.Bridge, n.VLANs, func() string {
		if !old.VLANAware {
			return "VLAN filtering off"
		}
		return old.VLANs
	}())}
}

// --- the node's own address on a VLAN ---

// vlanSelf are the commands that make the bridge itself a member of a VLAN (and undo it).
func vlanSelfAdd(s *bridgeSpec, id int) netStep {
	return stepRun("bridge", "vlan", "add", "dev", s.Bridge, "vid", strconv.Itoa(id), "self")
}
func vlanSelfDel(s *bridgeSpec, id int) netStep {
	return stepRun("bridge", "vlan", "del", "dev", s.Bridge, "vid", strconv.Itoa(id), "self")
}

func vlanAddrSummary(s *bridgeSpec, v vlanAddrSpec) string {
	how := "an address from DHCP"
	if v.Method == nodeproto.AddrStatic {
		how = "the address " + v.Address
	}
	route := "It adds no default route."
	if v.DefaultRoute {
		route = "It carries the node's default route"
		if v.Gateway != "" {
			route += " (gateway " + v.Gateway + ")"
		}
		route += "."
	}
	return fmt.Sprintf("The node gets %s on VLAN %d of %s, on the new interface %s. %s", how, v.ID, s.Bridge, v.iface(s), route)
}

// --- MTU ---

// mtuLayers lists the devices of a switch from the bottom up: the cards, the bond, the bridge, the VLAN interfaces.
func mtuLayers(s *bridgeSpec) []string {
	l := s.cardNames()
	if s.Bond != "" {
		l = append(l, s.Bond)
	}
	l = append(l, s.Bridge)
	for _, v := range s.VLANAddrs {
		l = append(l, v.iface(s))
	}
	return l
}

// mtuMoves sets the MTU of layers (bottom up) from the values in from to those in to. A layer that goes lower
// is changed before the ones below it, a layer that goes higher after them.
func mtuMoves(layers []string, fromM, toM map[string]int) []netStep {
	var steps []netStep
	set := func(l string, v int) netStep { return stepRun("ip", "link", "set", "dev", l, "mtu", strconv.Itoa(v)) }
	for i := len(layers) - 1; i >= 0; i-- {
		l := layers[i]
		if toM[l] > 0 && toM[l] < fromM[l] {
			steps = append(steps, set(l, toM[l]))
		}
	}
	for _, l := range layers {
		if toM[l] > 0 && toM[l] > fromM[l] {
			steps = append(steps, set(l, toM[l]))
		}
	}
	return steps
}

// mtuValues maps each layer of a switch to its MTU now.
func mtuValues(snap *netSnap, layers []string) map[string]int {
	m := map[string]int{}
	for _, l := range layers {
		if x := snap.link(l); x != nil {
			m[l] = x.MTU
		}
	}
	return m
}

func mtuTarget(layers []string, v int) map[string]int {
	m := map[string]int{}
	for _, l := range layers {
		m[l] = v
	}
	return m
}

const mtuNote = "The ports of the physical switch and the other nodes on this network must use the same MTU, or large packets are dropped."

func mtuSummary(n int) string {
	return fmt.Sprintf("Set the MTU to %d on every device of the switch: the cards, the bond, the bridge and the VLAN interfaces. %s", n, mtuNote)
}

// cardsSummary lines for a plan that changes the cards.
func cardsDropSummary(c planCard) string {
	if len(c.Link.globalAddrs()) == 0 {
		return ""
	}
	var a []string
	for _, x := range c.Link.globalAddrs() {
		a = append(a, x.CIDR())
	}
	return fmt.Sprintf("%s has the address %s. As a member of the switch it has none; the bridge keeps its own address. %s gets its own configuration back when it leaves the switch or the switch is deleted.", c.Link.Name, strings.Join(a, ", "), c.Link.Name)
}

func bondModeWords(m string) string {
	if m == nodeproto.BondLACP {
		return "load balance with LACP (802.3ad; the physical switch must be set up for LACP)"
	}
	return "failover (active-backup; works with any switch)"
}
