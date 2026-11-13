package nodecmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"virticle/internal/nodeproto"
)

// The NetworkManager profiles of a switch (see net_nm.go for the rules):
//
//	virticle-BRIDGE        the bridge
//	virticle-bond-BOND     the bond (two or more cards), a port of the bridge
//	virticle-port-CARD     a card: a port of the bridge (one card) or a member of the bond
//	virticle-vlan-IF       the node's address on a VLAN

func (nmStack) plan(c *Ctx, in *planInput) (*netPlanned, error) {
	switch in.Req.Action {
	case nodeproto.NetCreate:
		return nmCreate(c, in)
	case nodeproto.NetDelete:
		return nmDelete(in), nil
	case nodeproto.NetSetUplinks:
		return nmSetUplinks(c, in)
	case nodeproto.NetSetMTU:
		return nmSetMTU(in), nil
	case nodeproto.NetAddVLANAddr:
		return nmAddVLANAddr(in), nil
	case nodeproto.NetDelVLANAddr:
		return nmDelVLANAddr(in), nil
	}
	return nmSetVLANs(in), nil
}

func nmBridgeID(s *bridgeSpec) string { return "virticle-" + s.Bridge }
func nmPortID(card string) string     { return "virticle-port-" + card }
func nmBondID(s *bridgeSpec) string   { return "virticle-bond-" + s.Bond }
func nmVLANID(s *bridgeSpec, v vlanAddrSpec) string {
	return "virticle-vlan-" + v.iface(s)
}

// nmBridgePortID is the profile that is the bridge's port: the bond's, or the one card's.
func nmBridgePortID(s *bridgeSpec) string {
	if s.Bond != "" {
		return nmBondID(s)
	}
	return nmPortID(s.Uplink)
}

// nmCaptured is a card as NetworkManager has it now.
type nmCaptured struct {
	card cardSpec
	prof map[string]string
	mtu  string
}

// nmCaptureCard reads the profile NetworkManager has active on a card. When there is none and the card has
// addresses, they cannot be given back safely.
func nmCaptureCard(c *Ctx, l *netLink, has bool, pl *netPlanned) (*nmCaptured, error) {
	r := &nmCaptured{card: cardSpec{Name: l.Name, MTU: l.MTU}}
	uuid, _ := c.netRun("nmcli", "-t", "-g", "GENERAL.CON-UUID", "device", "show", l.Name)
	uuid = strings.TrimSpace(uuid)
	if uuid == "" || uuid == "--" {
		if has {
			return nil, Fail("%s has addresses, but NetworkManager has no active connection on it; they cannot be moved safely", l.Name)
		}
		return r, nil
	}
	out, err := c.netRun("nmcli", "-t", "-f", "connection.id,connection.type,connection.autoconnect,802-3-ethernet.mtu,ipv4,ipv6", "connection", "show", "uuid", uuid)
	if err != nil {
		return nil, Fail("cannot read the NetworkManager connection of %s: %v", l.Name, err)
	}
	p := nmTerse(out)
	if p["connection.type"] != "802-3-ethernet" {
		return nil, Fail("the connection of %s is of type %s, not a plain ethernet one", l.Name, p["connection.type"])
	}
	r.prof = p
	r.card.UUID, r.card.ID, r.card.Autoconnect = uuid, p["connection.id"], p["connection.autoconnect"]
	if v := p["802-3-ethernet.mtu"]; v != "" && v != "auto" && v != "0" && v != "--" {
		r.mtu = v
	}
	pl.Saves = append(pl.Saves, netSave{"nm-original-profile-" + l.Name + ".txt", []string{"nmcli", "-t", "-s", "connection", "show", "uuid", uuid}})
	return r, nil
}

func nmCreate(c *Ctx, in *planInput) (*netPlanned, error) {
	s := in.New
	s.Data = map[string]string{}
	pl := &netPlanned{Marker: "put", Spec: s}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Create the Linux bridge %s as the NetworkManager connection %s.", s.Bridge, nmBridgeID(s)))
	if out, err := c.netRun("nmcli", "--version"); err == nil && nmVersionAtLeast(out, 1, 46) {
		s.Data["portKeys"] = "new"
	}
	ids, _ := c.netRun("nmcli", "-t", "-f", "NAME", "connection", "show")
	taken := map[string]bool{}
	for _, l := range strings.Split(ids, "\n") {
		taken[l] = true
	}
	for _, id := range []string{nmBridgeID(s), nmBondID(s)} {
		if taken[id] && (id != nmBondID(s) || s.Bond != "") {
			return nil, Fail("NetworkManager already has a connection named %s", id)
		}
	}
	for _, n := range s.cardNames() {
		if taken[nmPortID(n)] {
			return nil, Fail("NetworkManager already has a connection named %s", nmPortID(n))
		}
	}
	if in.Uplink != nil {
		up := in.Uplink
		has := len(up.globalAddrs()) > 0
		cap, err := nmCaptureCard(c, up, has, pl)
		if err != nil {
			return nil, err
		}
		if cap.card.UUID != "" {
			s.Data["origUUID"], s.Data["origID"], s.Data["origAutoconnect"] = cap.card.UUID, cap.card.ID, cap.card.Autoconnect
			if cap.mtu != "" {
				s.Data["mtu"] = cap.mtu
			}
			if s.MoveAddresses {
				var kv []string
				for k, v := range cap.prof {
					if nmCopyKeys[k] && v != "" && v != "--" {
						kv = append(kv, k, v)
					}
				}
				sortPairs(kv)
				b, _ := json.Marshal(kv)
				s.Moved = string(b)
				if has {
					method := "static"
					if cap.prof["ipv4.method"] == "auto" {
						method = "dhcp"
					}
					mv, txt := moveSummary(in, s, method, cap.prof["ipv4.gateway"])
					pl.Move = mv
					pl.Summary = append(pl.Summary, txt, fmt.Sprintf("%s becomes a plain port of %s without addresses; its own connection %q is kept, but set not to start by itself.", up.Name, s.Bridge, cap.prof["connection.id"]))
				}
			}
		}
		if !s.MoveAddresses {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s becomes a plain port of %s.", up.Name, s.Bridge))
			if has {
				pl.Warn = append(pl.Warn, fmt.Sprintf("The addresses of %s are removed and not moved to the bridge.", up.Name))
			}
		}
		for i, x := range in.Extra {
			xc, err := nmCaptureCard(c, x.Link, len(x.Link.globalAddrs()) > 0, pl)
			if err != nil {
				return nil, err
			}
			xc.card.Expect = x.Expect
			s.Members[i] = xc.card
			if w := cardsDropSummary(x); w != "" {
				pl.Summary = append(pl.Summary, w)
			}
		}
		if s.Bond != "" {
			pl.Summary = append(pl.Summary, fmt.Sprintf("The cards %s become members of the new bond %s, which is the only port of %s. Mode: %s.", strings.Join(s.cardNames(), ", "), s.Bond, s.Bridge, bondModeWords(s.BondMode)))
			if s.MoveAddresses {
				pl.Summary = append(pl.Summary, fmt.Sprintf("The bond also gets the MAC address %s.", s.MAC))
			}
			if s.BondMode == nodeproto.BondLACP {
				pl.Warn = append(pl.Warn, c.lacpWarnings(s.cardNames())...)
			}
		}
	}
	if s.MTUSet > 0 {
		pl.Summary = append(pl.Summary, mtuSummary(s.MTUSet))
	}
	if s.VLANAware {
		pl.Summary = append(pl.Summary, vlanSummary(s))
	}
	pl.Steps = nmCreateSteps(s)
	if s.Uplink != "" && s.MoveAddresses && len(in.Expect) > 0 {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: s.Bridge, Argv: in.Expect, Secs: 40})
	}
	pl.Restore = nmUndoSteps(s, true)
	return pl, nil
}

func sortPairs(kv []string) {
	// sort by key, keeping pairs together
	for i := 2; i < len(kv); i += 2 {
		for j := i; j >= 2 && kv[j] < kv[j-2]; j -= 2 {
			kv[j], kv[j-2] = kv[j-2], kv[j]
			kv[j+1], kv[j-1] = kv[j-1], kv[j+1]
		}
	}
}

func nmPortVLANs(s *bridgeSpec) string {
	if !s.VLANAware {
		return ""
	}
	v := "1 pvid untagged"
	if t := taggedVLANs(s.VLANs); t != "" {
		v += "," + t
	}
	return v
}

// nmSelfVLANs is the value of bridge.vlans: the ids the bridge itself belongs to.
func nmSelfVLANs(s *bridgeSpec) string {
	var p []string
	for _, id := range s.vlanAddrIDs() {
		p = append(p, strconv.Itoa(id))
	}
	return strings.Join(p, ",")
}

func nmCtl(s *bridgeSpec) (ctl, ptype string) {
	if s.Data["portKeys"] == "new" {
		return "controller", "port-type"
	}
	return "master", "slave-type"
}

func nmBridgeAdd(s *bridgeSpec) netStep {
	br := []string{"nmcli", "connection", "add", "type", "bridge", "ifname", s.Bridge, "con-name", nmBridgeID(s),
		"connection.autoconnect", "yes", "bridge.stp", "no"}
	if s.MoveAddresses && s.MAC != "" {
		br = append(br, "bridge.mac-address", s.MAC)
	}
	if m := s.mtu(); m != "" && (s.MTUSet > 0 || s.MoveAddresses) {
		br = append(br, "802-3-ethernet.mtu", m)
	}
	if s.VLANAware {
		br = append(br, "bridge.vlan-filtering", "yes", "bridge.vlan-default-pvid", "1")
	}
	if v := nmSelfVLANs(s); v != "" {
		br = append(br, "bridge.vlans", v)
	}
	var kv []string
	if s.Moved != "" {
		_ = json.Unmarshal([]byte(s.Moved), &kv)
	}
	if len(kv) == 0 {
		kv = []string{"ipv4.method", "disabled", "ipv6.method", "disabled"}
	}
	return netStep{Kind: "run", Argv: append(br, kv...)}
}

func nmBondAdd(s *bridgeSpec) netStep {
	ctl, ptype := nmCtl(s)
	a := []string{"nmcli", "connection", "add", "type", "bond", "ifname", s.Bond, "con-name", nmBondID(s),
		ctl, s.Bridge, ptype, "bridge", "connection.autoconnect", "yes", "bond.options", bondOptionsNM(s.BondMode)}
	if s.MoveAddresses && s.MAC != "" {
		a = append(a, "802-3-ethernet.cloned-mac-address", s.MAC)
	}
	if m := s.mtu(); m != "" {
		a = append(a, "802-3-ethernet.mtu", m)
	}
	if v := nmPortVLANs(s); v != "" {
		a = append(a, "bridge-port.vlans", v)
	}
	return netStep{Kind: "run", Argv: append(a, "ipv4.method", "disabled", "ipv6.method", "disabled")}
}

func nmPortAdd(s *bridgeSpec, card string) netStep {
	ctl, ptype := nmCtl(s)
	master, mtype := s.Bridge, "bridge"
	if s.Bond != "" {
		master, mtype = s.Bond, "bond"
	}
	port := []string{"nmcli", "connection", "add", "type", "ethernet", "ifname", card, "con-name", nmPortID(card),
		ctl, master, ptype, mtype, "connection.autoconnect", "yes"}
	if m := s.mtu(); m != "" {
		port = append(port, "802-3-ethernet.mtu", m)
	}
	if v := nmPortVLANs(s); v != "" && s.Bond == "" {
		port = append(port, "bridge-port.vlans", v)
	}
	return netStep{Kind: "run", Argv: port}
}

func nmCardOff(c cardSpec) []netStep {
	if c.UUID == "" {
		return nil
	}
	return []netStep{stepRun("nmcli", "connection", "modify", "uuid", c.UUID, "connection.autoconnect", "no")}
}

func nmVLANAdd(s *bridgeSpec, v vlanAddrSpec) netStep {
	a := []string{"nmcli", "connection", "add", "type", "vlan", "ifname", v.iface(s), "con-name", nmVLANID(s, v),
		"dev", s.Bridge, "id", strconv.Itoa(v.ID), "connection.autoconnect", "yes", "ipv6.method", "disabled"}
	if v.Method == nodeproto.AddrDHCP {
		a = append(a, "ipv4.method", "auto", "ipv4.ignore-auto-dns", "yes")
	} else {
		a = append(a, "ipv4.method", "manual", "ipv4.addresses", v.Address)
		if v.DefaultRoute && v.Gateway != "" {
			a = append(a, "ipv4.gateway", v.Gateway)
		}
	}
	if v.DefaultRoute {
		a = append(a, "ipv4.never-default", "no")
	} else {
		a = append(a, "ipv4.never-default", "yes")
	}
	if s.MTUSet > 0 {
		a = append(a, "802-3-ethernet.mtu", s.mtu())
	}
	return netStep{Kind: "run", Argv: a}
}

func nmUp(id string) netStep { return stepRun("nmcli", "--wait", "0", "connection", "up", "id", id) }

func nmCreateSteps(s *bridgeSpec) []netStep {
	steps := []netStep{nmBridgeAdd(s)}
	if s.Bond != "" {
		steps = append(steps, nmBondAdd(s))
	}
	for _, c := range s.cards() {
		steps = append(steps, nmPortAdd(s, c.Name))
		steps = append(steps, nmCardOff(c)...)
	}
	for _, v := range s.VLANAddrs {
		steps = append(steps, nmVLANAdd(s, v))
	}
	steps = append(steps, nmUp(nmBridgeID(s)))
	if s.Bond != "" {
		steps = append(steps, nmUp(nmBondID(s)))
	}
	for _, c := range s.cards() {
		steps = append(steps, nmUp(nmPortID(c.Name)))
	}
	for _, v := range s.VLANAddrs {
		steps = append(steps, nmUp(nmVLANID(s, v)))
	}
	return steps
}

func nmDel(id string) netStep {
	return netStep{Kind: "run", Argv: []string{"nmcli", "connection", "delete", "id", id}, Ignore: true}
}

// nmGiveBack brings the card's own profile back after its port profile is gone.
func nmGiveBack(s *bridgeSpec, c cardSpec, ignore, wait bool) []netStep {
	mk := func(ign bool, argv ...string) netStep { return netStep{Kind: "run", Argv: argv, Ignore: ign} }
	if c.UUID == "" {
		return nil
	}
	ac := c.Autoconnect
	if ac == "" {
		ac = "yes"
	}
	var steps []netStep
	if s.MTUSet > 0 && c.MTU > 0 {
		steps = append(steps, mk(true, "ip", "link", "set", "dev", c.Name, "mtu", strconv.Itoa(c.MTU)))
	}
	steps = append(steps,
		mk(ignore, "nmcli", "connection", "modify", "uuid", c.UUID, "connection.autoconnect", ac),
		mk(ignore, "nmcli", "--wait", "0", "connection", "up", "uuid", c.UUID))
	if wait && len(c.Expect) > 0 {
		steps = append(steps, netStep{Kind: "wait-addr", Path: c.Name, Argv: c.Expect, Secs: 40, Ignore: ignore})
	}
	return steps
}

// nmUndoSteps remove the profiles of the switch and bring the cards' own profiles back.
func nmUndoSteps(s *bridgeSpec, ignore bool) []netStep {
	var steps []netStep
	for _, v := range s.VLANAddrs {
		steps = append(steps, nmDel(nmVLANID(s, v)), stepRunIgnore("ip", "link", "del", v.iface(s)))
	}
	for _, c := range s.cards() {
		steps = append(steps, nmDel(nmPortID(c.Name)))
	}
	if s.Bond != "" {
		steps = append(steps, nmDel(nmBondID(s)), stepRunIgnore("ip", "link", "del", s.Bond))
	}
	steps = append(steps, nmDel(nmBridgeID(s)), stepRunIgnore("ip", "link", "del", s.Bridge))
	for i, c := range s.cards() {
		steps = append(steps, nmGiveBack(s, c, ignore || i > 0, i == 0 && s.MoveAddresses)...)
	}
	return steps
}

func nmDelete(in *planInput) *netPlanned {
	s := in.Old
	pl := &netPlanned{Marker: "remove", Spec: s}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Delete the Linux bridge %s (the NetworkManager connection %s), which virticle created.", s.Bridge, nmBridgeID(s)))
	if s.Bond != "" {
		pl.Summary = append(pl.Summary, fmt.Sprintf("The bond %s goes with it.", s.Bond))
	}
	for _, v := range s.VLANAddrs {
		pl.Summary = append(pl.Summary, fmt.Sprintf("The node's address on VLAN %d (%s) is removed.", v.ID, v.iface(s)))
	}
	for _, c := range s.cards() {
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s gets its own connection back (%s).", c.Name, strings.Join(c.Expect, ", ")))
	}
	pl.Saves = []netSave{{"nm-connections.txt", []string{"nmcli", "-t", "connection", "show"}}}
	pl.Steps = nmUndoSteps(s, false)
	pl.Restore = ignoreAll(nmCreateSteps(s))
	return pl
}

func nmModVLANs(s *bridgeSpec) []netStep {
	yn := "no"
	if s.VLANAware {
		yn = "yes"
	}
	steps := []netStep{stepRun("nmcli", "connection", "modify", "id", nmBridgeID(s), "bridge.vlan-filtering", yn, "bridge.vlan-default-pvid", "1")}
	if s.Uplink != "" {
		steps = append(steps, stepRun("nmcli", "connection", "modify", "id", nmBridgePortID(s), "bridge-port.vlans", nmPortVLANs(s)))
	}
	return steps
}

func nmSetVLANs(in *planInput) *netPlanned {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = vlanChangeSummary(old, n)
	pl.Steps = append(nmModVLANs(n), vlanCommands(old, n)...)
	pl.Restore = ignoreAll(append(nmModVLANs(old), vlanCommands(n, old)...))
	return pl
}

// nmCardsSteps turn the cards and the bond of from into those of to.
func nmCardsSteps(from, to *bridgeSpec) []netStep {
	d := diffCards(from, to)
	var steps []netStep
	flush := func(cs []cardSpec) {
		for _, c := range cs {
			steps = append(steps, stepRunIgnore("ip", "addr", "flush", "dev", c.Name))
		}
	}
	switch {
	case d.BondDropped:
		for _, c := range from.cards() {
			steps = append(steps, nmDel(nmPortID(c.Name)))
		}
		steps = append(steps, nmDel(nmBondID(from)), stepRunIgnore("ip", "link", "del", from.Bond))
		for _, c := range to.cards() {
			steps = append(steps, nmPortAdd(to, c.Name), nmUp(nmPortID(c.Name)))
		}
		for _, c := range d.Removed {
			steps = append(steps, nmGiveBack(from, c, true, false)...)
		}
	case d.BondCreated:
		for _, c := range from.cards() {
			steps = append(steps, nmDel(nmPortID(c.Name)))
		}
		steps = append(steps, nmBondAdd(to))
		for _, c := range to.cards() {
			if from.card(c.Name) == nil {
				steps = append(steps, nmCardOff(c)...)
			}
			steps = append(steps, nmPortAdd(to, c.Name))
		}
		steps = append(steps, nmUp(nmBondID(to)))
		for _, c := range to.cards() {
			steps = append(steps, nmUp(nmPortID(c.Name)))
		}
		flush(d.Added)
	default:
		for _, c := range d.Removed {
			steps = append(steps, nmDel(nmPortID(c.Name)))
			steps = append(steps, nmGiveBack(from, c, true, false)...)
		}
		for _, c := range d.Added {
			steps = append(steps, nmCardOff(c)...)
			steps = append(steps, nmPortAdd(to, c.Name), nmUp(nmPortID(c.Name)))
		}
		if d.ModeChanged {
			steps = append(steps, stepRun("nmcli", "connection", "modify", "id", nmBondID(to), "bond.options", bondOptionsNM(to.BondMode)), nmUp(nmBondID(to)))
			for _, c := range to.cards() {
				steps = append(steps, nmUp(nmPortID(c.Name)))
			}
		}
		flush(d.Added)
	}
	return steps
}

func nmSetUplinks(c *Ctx, in *planInput) (*netPlanned, error) {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	if n.Data == nil {
		n.Data = map[string]string{}
	}
	d := diffCards(old, n)
	// The added cards' own profiles are read now; they are switched off while the card belongs to the switch.
	for i := range in.Added {
		x := in.Added[i]
		xc, err := nmCaptureCard(c, x.Link, len(x.Link.globalAddrs()) > 0, pl)
		if err != nil {
			return nil, err
		}
		xc.card.Expect = x.Expect
		cs := n.cards()
		for j := range cs {
			if cs[j].Name == x.Link.Name {
				cs[j] = xc.card
			}
		}
		n.setCards(cs)
	}
	describeUplinkChange(c, pl, in, d)
	pl.Steps = nmCardsSteps(old, n)
	if n.MoveAddresses && len(n.UplinkExpect) > 0 {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: n.Bridge, Argv: n.UplinkExpect, Secs: 40})
	}
	pl.Restore = ignoreAll(nmCardsSteps(n, old))
	return pl, nil
}

func nmMTUProfiles(s *bridgeSpec) []string {
	var ids []string
	for _, c := range s.cards() {
		ids = append(ids, nmPortID(c.Name))
	}
	if s.Bond != "" {
		ids = append(ids, nmBondID(s))
	}
	ids = append(ids, nmBridgeID(s))
	for _, v := range s.VLANAddrs {
		ids = append(ids, nmVLANID(s, v))
	}
	return ids
}

func nmSetMTU(in *planInput) *netPlanned {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = []string{mtuSummary(n.MTUSet)}
	layers := mtuLayers(n)
	cur := mtuValues(in.Snap, layers)
	mod := func(v string) []netStep {
		var steps []netStep
		for _, id := range nmMTUProfiles(n) {
			steps = append(steps, stepRun("nmcli", "connection", "modify", "id", id, "802-3-ethernet.mtu", v))
		}
		return steps
	}
	pl.Steps = append(mod(strconv.Itoa(n.MTUSet)), mtuMoves(layers, cur, mtuTarget(layers, n.MTUSet))...)
	prev := "0"
	if old.mtu() != "" {
		prev = old.mtu()
	}
	pl.Restore = ignoreAll(append(mod(prev), mtuMoves(layers, mtuTarget(layers, n.MTUSet), cur)...))
	return pl
}

func nmSelfVLANSteps(s *bridgeSpec) netStep {
	v := nmSelfVLANs(s)
	return stepRun("nmcli", "connection", "modify", "id", nmBridgeID(s), "bridge.vlans", v)
}

func nmAddVLANAddr(in *planInput) *netPlanned {
	old, n, v := in.Old, in.New, *in.VLANAddr
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = vlanAddrPlanSummary(old, n, v)
	pl.Steps = []netStep{nmSelfVLANSteps(n)}
	if n.VLANs != old.VLANs {
		pl.Steps = append(pl.Steps, nmModVLANs(n)...)
	}
	pl.Steps = append(pl.Steps, vlanCommands(old, n)...)
	pl.Steps = append(pl.Steps, vlanSelfAdd(n, v.ID), nmVLANAdd(n, v), nmUp(nmVLANID(n, v)))
	if v.Method == nodeproto.AddrStatic {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: v.iface(n), Argv: []string{v.Address}, Secs: 20})
	}
	undo := []netStep{nmDel(nmVLANID(n, v)), stepRunIgnore("ip", "link", "del", v.iface(n)), vlanSelfDel(n, v.ID), nmSelfVLANSteps(old)}
	if n.VLANs != old.VLANs {
		undo = append(undo, nmModVLANs(old)...)
	}
	pl.Restore = ignoreAll(append(undo, vlanCommands(n, old)...))
	return pl
}

func nmDelVLANAddr(in *planInput) *netPlanned {
	old, n, v := in.Old, in.New, *in.VLANAddr
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = []string{fmt.Sprintf("Remove the node's address on VLAN %d (the interface %s). The VLAN stays allowed on the switch.", v.ID, v.iface(old))}
	pl.Steps = []netStep{nmDel(nmVLANID(old, v)), stepRunIgnore("ip", "link", "del", v.iface(old)), vlanSelfDel(old, v.ID), nmSelfVLANSteps(n)}
	pl.Restore = ignoreAll([]netStep{nmSelfVLANSteps(old), vlanSelfAdd(old, v.ID), nmVLANAdd(old, v), nmUp(nmVLANID(old, v))})
	return pl
}
