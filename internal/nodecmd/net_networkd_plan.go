package nodecmd

import (
	"fmt"
	"path"
	"strings"

	"virticle/internal/nodeproto"
)

// The files and steps of the networkd backend (see net_networkd.go for the layout and the reasons).
//
//	05-virticle-BRIDGE.netdev / .network       the bridge; its .network also lists the VLAN interfaces (VLAN=)
//	                                           and the VLAN ids of the bridge itself ([BridgeVLAN], "self")
//	05-virticle-BOND.netdev / .network         the bond (two or more cards): Kind=bond, and the bridge's port
//	05-virticle-BRIDGE-CARD.network            a card: a port of the bridge (one card) or a member of the bond
//	05-virticle-BRIDGE.ID.netdev / .network    the node's address on a VLAN

func ndPathOf(name, ext string) string  { return networkdEtc + "/05-virticle-" + name + "." + ext }
func ndNetdevPath(s *bridgeSpec) string { return ndPathOf(s.Bridge, "netdev") }
func ndBridgePath(s *bridgeSpec) string { return ndPathOf(s.Bridge, "network") }
func ndCardPath(s *bridgeSpec, card string) string {
	return networkdEtc + "/05-virticle-" + s.Bridge + "-" + card + ".network"
}
func ndUplinkPath(s *bridgeSpec) string { return ndCardPath(s, s.Uplink) }

const ndHeader = "# Written by virticle. Changing it by hand is fine; deleting the bridge in virticle removes the file.\n"

func ndNetdev(s *bridgeSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[NetDev]\nName=" + s.Bridge + "\nKind=bridge\n")
	if s.MoveAddresses && s.MAC != "" {
		b.WriteString("MACAddress=" + s.MAC + "\n")
	}
	if s.VLANAware {
		b.WriteString("\n[Bridge]\nVLANFiltering=yes\nDefaultPVID=1\n")
	}
	return b.String()
}

func ndBridgeNetwork(s *bridgeSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[Match]\nName=" + s.Bridge + "\n\n")
	if s.Moved != "" {
		b.WriteString(s.Moved)
	} else {
		b.WriteString("[Network]\nLinkLocalAddressing=no\nIPv6AcceptRA=no\nConfigureWithoutCarrier=yes\n\n[Link]\nRequiredForOnline=no\n")
	}
	if s.MTUSet > 0 {
		b.WriteString("\n[Link]\nMTUBytes=" + s.mtu() + "\n")
	}
	if len(s.VLANAddrs) > 0 {
		b.WriteString("\n[Network]\n")
		for _, id := range s.vlanAddrIDs() {
			b.WriteString("VLAN=" + nodeproto.VLANInterface(s.Bridge, id) + "\n")
		}
		// The bridge itself is a member of the VLANs ("self"), or tagged frames do not reach its VLAN interfaces.
		for _, id := range s.vlanAddrIDs() {
			b.WriteString(fmt.Sprintf("\n[BridgeVLAN]\nVLAN=%d\n", id))
		}
	}
	return b.String()
}

// ndPortNetwork is the .network of a device that is a port of the bridge (master Bridge) or a member of the
// bond (master Bond). Only the bridge's port carries the VLAN ids.
func ndPortNetwork(s *bridgeSpec, name, masterKey, master string, vlans bool) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[Match]\nName=" + name + "\n\n[Network]\n" + masterKey + "=" + master + "\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\n")
	if m := s.mtu(); m != "" {
		b.WriteString("MTUBytes=" + m + "\n")
	}
	b.WriteString("RequiredForOnline=enslaved\n")
	if vlans && s.VLANAware {
		b.WriteString("\n[BridgeVLAN]\nPVID=1\nEgressUntagged=1\n")
		// networkd takes one id or one range M-N per VLAN= line, not a list.
		for _, r := range strings.Split(taggedVLANs(s.VLANs), ",") {
			if r != "" {
				b.WriteString("\n[BridgeVLAN]\nVLAN=" + r + "\n")
			}
		}
	}
	return b.String()
}

func ndCardNetwork(s *bridgeSpec, card string) string {
	if s.Bond != "" {
		return ndPortNetwork(s, card, "Bond", s.Bond, false)
	}
	return ndPortNetwork(s, card, "Bridge", s.Bridge, true)
}

func ndBondNetdev(s *bridgeSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[NetDev]\nName=" + s.Bond + "\nKind=bond\n")
	if s.MoveAddresses && s.MAC != "" {
		// The bond shows the MAC address of the card whose configuration moved, like the bridge does.
		b.WriteString("MACAddress=" + s.MAC + "\n")
	}
	if m := s.mtu(); m != "" {
		b.WriteString("MTUBytes=" + m + "\n")
	}
	if s.BondMode == nodeproto.BondLACP {
		b.WriteString("\n[Bond]\nMode=802.3ad\nLACPTransmitRate=fast\nTransmitHashPolicy=layer2+3\nMIIMonitorSec=100ms\n")
	} else {
		b.WriteString("\n[Bond]\nMode=active-backup\nMIIMonitorSec=100ms\n")
	}
	return b.String()
}

func ndVLANNetdev(s *bridgeSpec, v vlanAddrSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[NetDev]\nName=" + v.iface(s) + "\nKind=vlan\n")
	if m := s.mtu(); m != "" && s.MTUSet > 0 {
		b.WriteString("MTUBytes=" + m + "\n")
	}
	b.WriteString(fmt.Sprintf("\n[VLAN]\nId=%d\n", v.ID))
	return b.String()
}

func ndVLANNetwork(s *bridgeSpec, v vlanAddrSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[Match]\nName=" + v.iface(s) + "\n\n[Network]\n")
	if v.Method == nodeproto.AddrDHCP {
		b.WriteString("DHCP=ipv4\n")
	} else {
		b.WriteString("Address=" + v.Address + "\nConfigureWithoutCarrier=yes\n")
		if v.DefaultRoute && v.Gateway != "" {
			b.WriteString("Gateway=" + v.Gateway + "\n")
		}
	}
	b.WriteString("LinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\nRequiredForOnline=no\n")
	if v.Method == nodeproto.AddrDHCP {
		b.WriteString("\n[DHCPv4]\nUseDNS=no\nUseNTP=no\nUseDomains=no\n")
		if !v.DefaultRoute {
			b.WriteString("UseGateway=no\n")
		}
	}
	return b.String()
}

func ndWrites(s *bridgeSpec) []netStep {
	w := func(p, content string) netStep { return netStep{Kind: "write", Path: p, Content: content, Mode: 0o644} }
	steps := []netStep{w(ndNetdevPath(s), ndNetdev(s)), w(ndBridgePath(s), ndBridgeNetwork(s))}
	if s.Bond != "" {
		steps = append(steps, w(ndPathOf(s.Bond, "netdev"), ndBondNetdev(s)), w(ndPathOf(s.Bond, "network"), ndPortNetwork(s, s.Bond, "Bridge", s.Bridge, true)))
	}
	for _, c := range s.cardNames() {
		steps = append(steps, w(ndCardPath(s, c), ndCardNetwork(s, c)))
	}
	for _, v := range s.VLANAddrs {
		steps = append(steps, w(ndPathOf(v.iface(s), "netdev"), ndVLANNetdev(s, v)), w(ndPathOf(v.iface(s), "network"), ndVLANNetwork(s, v)))
	}
	return steps
}

func ndPaths(s *bridgeSpec) []string {
	var r []string
	for _, st := range ndWrites(s) {
		r = append(r, st.Path)
	}
	return r
}

func ndRemoves(s *bridgeSpec) []netStep {
	var steps []netStep
	for _, p := range ndPaths(s) {
		steps = append(steps, netStep{Kind: "remove", Path: p})
	}
	return steps
}

// ndFileChange writes the files of to and removes those of from that to does not have.
func ndFileChange(from, to *bridgeSpec) []netStep {
	steps := ndWrites(to)
	keep := map[string]bool{}
	for _, p := range ndPaths(to) {
		keep[p] = true
	}
	for _, p := range ndPaths(from) {
		if !keep[p] {
			steps = append(steps, netStep{Kind: "remove", Path: p})
		}
	}
	return steps
}

func ndIgn(ignore bool, argv ...string) netStep {
	return netStep{Kind: "run", Argv: argv, Ignore: ignore}
}

// ndTearDown puts the cards back: networkd forgets the files (already removed or restored by the caller), the
// VLAN interfaces, the bond and the bridge go, each card is configured again from its own file.
func ndTearDown(s *bridgeSpec, ignore bool) []netStep {
	steps := []netStep{ndIgn(ignore, "networkctl", "reload")}
	for _, v := range s.VLANAddrs {
		steps = append(steps, stepRunIgnore("ip", "link", "del", v.iface(s)))
	}
	for _, c := range s.cards() {
		steps = append(steps, ndIgn(ignore, "ip", "link", "set", c.Name, "nomaster"))
	}
	if s.Bond != "" {
		steps = append(steps, stepRunIgnore("ip", "link", "del", s.Bond))
	}
	steps = append(steps, stepRunIgnore("ip", "link", "del", s.Bridge))
	for i, c := range s.cards() {
		if s.MTUSet > 0 && c.MTU > 0 {
			steps = append(steps, stepRunIgnore("ip", "link", "set", "dev", c.Name, "mtu", fmt.Sprint(c.MTU)))
		}
		// Right after the bridge is gone networkd may not know the card yet ("Link 2 not known"): try again for a while.
		steps = append(steps, netStep{Kind: "run", Argv: []string{"networkctl", "reconfigure", c.Name}, Secs: 15, Ignore: ignore || i > 0})
		if i == 0 && s.MoveAddresses && len(c.Expect) > 0 {
			steps = append(steps, netStep{Kind: "wait-addr", Path: c.Name, Argv: c.Expect, Secs: 40, Ignore: ignore})
		}
	}
	return steps
}

// ndBringUp creates the bridge (and the bond) from the files and enslaves the cards.
func ndBringUp(s *bridgeSpec, ignore bool) []netStep {
	steps := []netStep{ndIgn(ignore, "networkctl", "reload"), {Kind: "wait-link", Path: s.Bridge, Secs: 20, Ignore: ignore}}
	re := []string{"networkctl", "reconfigure", s.Bridge}
	if s.Bond != "" {
		steps = append(steps, netStep{Kind: "wait-link", Path: s.Bond, Secs: 20, Ignore: ignore})
		re = append(re, s.Bond)
	}
	re = append(re, s.cardNames()...)
	steps = append(steps, ndIgn(ignore, re...))
	for _, v := range s.VLANAddrs {
		steps = append(steps, netStep{Kind: "wait-link", Path: v.iface(s), Secs: 20, Ignore: true})
	}
	return steps
}

// ndVLANUp creates the VLAN interface and makes the bridge a member of the VLAN, without touching the bridge's
// other configuration (a reconfigure would restart its DHCP client).
func ndVLANUp(s *bridgeSpec, v vlanAddrSpec, ignore bool) []netStep {
	steps := []netStep{ndIgn(ignore, "networkctl", "reload"),
		ndIgn(true, "ip", "link", "add", "link", s.Bridge, "name", v.iface(s), "type", "vlan", "id", fmt.Sprint(v.ID)),
		ndIgn(ignore, "bridge", "vlan", "add", "dev", s.Bridge, "vid", fmt.Sprint(v.ID), "self"),
		{Kind: "wait-link", Path: v.iface(s), Secs: 20, Ignore: ignore},
		{Kind: "run", Argv: []string{"networkctl", "reconfigure", v.iface(s)}, Secs: 15, Ignore: ignore}}
	return steps
}

func ndVLANDown(s *bridgeSpec, v vlanAddrSpec, ignore bool) []netStep {
	return []netStep{ndIgn(ignore, "networkctl", "reload"),
		ndIgn(ignore, "ip", "link", "del", v.iface(s)),
		ndIgn(ignore, "bridge", "vlan", "del", "dev", s.Bridge, "vid", fmt.Sprint(v.ID), "self")}
}

// ndCardsRuntime are the runtime steps that turn the cards and the bond of from into those of to. The files are
// written (or restored) before; the first step reads them.
func ndCardsRuntime(from, to *bridgeSpec, ignore bool) []netStep {
	d := diffCards(from, to)
	steps := []netStep{ndIgn(ignore, "networkctl", "reload")}
	flush := func(cs []cardSpec) {
		for _, c := range cs {
			steps = append(steps, stepRunIgnore("ip", "addr", "flush", "dev", c.Name))
		}
	}
	giveBack := func(cs []cardSpec) {
		for _, c := range cs {
			steps = append(steps, ndIgn(ignore, "ip", "link", "set", c.Name, "nomaster"))
			if from.MTUSet > 0 && c.MTU > 0 {
				steps = append(steps, stepRunIgnore("ip", "link", "set", "dev", c.Name, "mtu", fmt.Sprint(c.MTU)))
			}
			steps = append(steps, netStep{Kind: "run", Argv: []string{"networkctl", "reconfigure", c.Name}, Secs: 15, Ignore: true})
		}
	}
	switch {
	case d.BondDropped:
		// The bond goes, the remaining card is a port of the bridge again, the other cards are given back.
		for _, c := range from.cards() {
			steps = append(steps, ndIgn(true, "ip", "link", "set", c.Name, "nomaster"))
		}
		steps = append(steps, stepRunIgnore("ip", "link", "del", from.Bond))
		re := append([]string{"networkctl", "reconfigure"}, to.cardNames()...)
		steps = append(steps, ndIgn(ignore, re...))
		for _, c := range d.Removed {
			steps = append(steps, netStep{Kind: "run", Argv: []string{"networkctl", "reconfigure", c.Name}, Secs: 15, Ignore: true})
		}
	case d.BondCreated:
		// The card leaves the bridge and joins the bond, which joins the bridge.
		for _, c := range from.cards() {
			steps = append(steps, ndIgn(ignore, "ip", "link", "set", c.Name, "nomaster"))
		}
		steps = append(steps, netStep{Kind: "wait-link", Path: to.Bond, Secs: 20, Ignore: ignore})
		re := append([]string{"networkctl", "reconfigure", to.Bond}, to.cardNames()...)
		steps = append(steps, ndIgn(ignore, re...))
		flush(d.Added)
	default:
		giveBack(d.Removed)
		if d.ModeChanged {
			// A bond cannot change its mode while it has members or is up: the members are released and the bond is
			// set again in place (making it again confuses networkd, which then leaves the cards unconfigured).
			for _, c := range from.cards() {
				steps = append(steps, ndIgn(true, "ip", "link", "set", c.Name, "nomaster"))
			}
			steps = append(steps, ndIgn(ignore, "ip", "link", "set", to.Bond, "down"),
				ndIgn(ignore, append([]string{"ip", "link", "set", to.Bond, "type", "bond"}, bondModeArgs(to.BondMode)...)...))
			re := append([]string{"networkctl", "reconfigure", to.Bond}, to.cardNames()...)
			steps = append(steps, ndIgn(ignore, re...))
		} else if len(d.Added) > 0 {
			re := append([]string{"networkctl", "reconfigure"}, cardNamesOf(d.Added)...)
			steps = append(steps, ndIgn(ignore, re...))
		}
		flush(d.Added)
	}
	return steps
}

func (networkdStack) plan(c *Ctx, in *planInput) (*netPlanned, error) {
	switch in.Req.Action {
	case nodeproto.NetCreate:
		return ndCreate(c, in)
	case nodeproto.NetDelete:
		return ndDelete(in), nil
	case nodeproto.NetSetUplinks:
		return ndSetUplinks(c, in)
	case nodeproto.NetSetMTU:
		return ndSetMTU(in), nil
	case nodeproto.NetAddVLANAddr:
		return ndAddVLANAddr(in), nil
	case nodeproto.NetDelVLANAddr:
		return ndDelVLANAddr(in), nil
	}
	return ndSetVLANs(in), nil
}

// ndCheckOrder: networkd uses the first .network file that matches a card, by file name; ours must come first.
func ndCheckOrder(c *Ctx, s *bridgeSpec, l *netLink, has bool) (*ndFile, error) {
	f, err := c.findNetworkFile(*l)
	if err != nil && has {
		return nil, Fail("cannot tell which networkd configuration applies to %s: %v", l.Name, err)
	}
	if f == nil && has {
		return nil, Fail("%s has addresses, but no networkd file configures them (set by hand with ip?); they cannot be moved safely", l.Name)
	}
	if f != nil {
		if ours := path.Base(ndCardPath(s, l.Name)); path.Base(f.Path) <= ours {
			return nil, Fail("%s configures %s and sorts before the file this change writes (%s); rename it so that it sorts later, or choose another uplink", f.Path, l.Name, ours)
		}
	}
	return f, nil
}

func ndCreate(c *Ctx, in *planInput) (*netPlanned, error) {
	s := in.New
	s.Data = map[string]string{}
	pl := &netPlanned{Marker: "put", Spec: s}
	pl.Saves = []netSave{{"networkctl-list.txt", []string{"networkctl", "list", "--no-pager"}}}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Create the Linux bridge %s.", s.Bridge))
	if in.Uplink != nil {
		up := in.Uplink
		has := len(up.globalAddrs()) > 0
		f, err := c.findNetworkFile(*up)
		if err != nil && (s.MoveAddresses || has) {
			return nil, Fail("cannot tell which networkd configuration applies to %s: %v", up.Name, err)
		}
		if f == nil && has {
			return nil, Fail("%s has addresses, but no networkd file configures them (set by hand with ip?); they cannot be moved safely", up.Name)
		}
		if f != nil {
			if ours := path.Base(ndUplinkPath(s)); path.Base(f.Path) <= ours {
				return nil, Fail("%s configures %s and sorts before the file this change writes (%s); rename it so that it sorts later, or choose another uplink", f.Path, up.Name, ours)
			}
			secs := parseINI(f.Content)
			s.Data["mtu"] = linkMTU(secs)
			if s.MoveAddresses {
				text, method, gw := movedConfig(secs)
				if method == "dhcp" {
					text = withIAID(text, c.leaseIAID(*up))
				}
				s.Moved = text
				if has {
					mv, txt := moveSummary(in, s, method, gw)
					pl.Move = mv
					pl.Summary = append(pl.Summary, txt, fmt.Sprintf("%s becomes %s without addresses.", up.Name, portWords(s)))
				}
			}
		}
		if !s.MoveAddresses {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s becomes %s.", up.Name, portWords(s)))
			if has {
				pl.Warn = append(pl.Warn, fmt.Sprintf("The addresses of %s are removed and not moved to the bridge.", up.Name))
			}
		}
		for _, x := range in.Extra {
			if _, err := ndCheckOrder(c, s, x.Link, len(x.Link.globalAddrs()) > 0); err != nil {
				return nil, err
			}
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
	pl.Steps = append(ndWrites(s), ndBringUp(s, false)...)
	if s.Uplink != "" && len(in.Expect) > 0 && s.MoveAddresses {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: s.Bridge, Argv: in.Expect, Secs: 40})
		pl.Steps = append(pl.Steps, stepRunIgnore("ip", "addr", "flush", "dev", s.Uplink))
	}
	for _, x := range in.Extra {
		pl.Steps = append(pl.Steps, stepRunIgnore("ip", "addr", "flush", "dev", x.Link.Name))
	}
	// The undo: the written files are removed by the generic restore.
	pl.Restore = ndTearDown(s, true)
	return pl, nil
}

func ndDelete(in *planInput) *netPlanned {
	s := in.Old
	pl := &netPlanned{Marker: "remove", Spec: s}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Delete the Linux bridge %s, which virticle created.", s.Bridge))
	if s.Bond != "" {
		pl.Summary = append(pl.Summary, fmt.Sprintf("The bond %s goes with it.", s.Bond))
	}
	for _, v := range s.VLANAddrs {
		pl.Summary = append(pl.Summary, fmt.Sprintf("The node's address on VLAN %d (%s) is removed.", v.ID, v.iface(s)))
	}
	if s.Uplink != "" {
		if s.MoveAddresses && len(s.UplinkExpect) > 0 {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s gets its own configuration (%s) back.", s.Uplink, strings.Join(s.UplinkExpect, ", ")))
		} else {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s stops being a port and is configured by its own file again.", s.Uplink))
		}
		for _, m := range s.Members {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s is configured by its own file again.", m.Name))
		}
	}
	pl.Steps = append(ndRemoves(s), ndTearDown(s, false)...)
	pl.Restore = ndBringUp(s, true)
	return pl
}

func ndPortFiles(n *bridgeSpec) []netStep {
	steps := []netStep{{Kind: "write", Path: ndNetdevPath(n), Content: ndNetdev(n), Mode: 0o644}}
	if n.Bond != "" {
		steps = append(steps, netStep{Kind: "write", Path: ndPathOf(n.Bond, "network"), Content: ndPortNetwork(n, n.Bond, "Bridge", n.Bridge, true), Mode: 0o644})
	} else if n.Uplink != "" {
		steps = append(steps, netStep{Kind: "write", Path: ndUplinkPath(n), Content: ndCardNetwork(n, n.Uplink), Mode: 0o644})
	}
	return steps
}

func ndSetVLANs(in *planInput) *netPlanned {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = vlanChangeSummary(old, n)
	pl.Steps = ndPortFiles(n)
	pl.Steps = append(pl.Steps, stepRun("networkctl", "reload"))
	pl.Steps = append(pl.Steps, vlanCommands(old, n)...)
	pl.Restore = append([]netStep{stepRunIgnore("networkctl", "reload")}, ignoreAll(vlanCommands(n, old))...)
	return pl
}

func ndSetUplinks(c *Ctx, in *planInput) (*netPlanned, error) {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	d := diffCards(old, n)
	for _, x := range in.Added {
		if _, err := ndCheckOrder(c, n, x.Link, len(x.Link.globalAddrs()) > 0); err != nil {
			return nil, err
		}
	}
	describeUplinkChange(c, pl, in, d)
	pl.Steps = append(ndFileChange(old, n), ndCardsRuntime(old, n, false)...)
	if n.MoveAddresses && len(n.UplinkExpect) > 0 {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: n.Bridge, Argv: n.UplinkExpect, Secs: 40})
	}
	pl.Restore = append([]netStep(nil), ndCardsRuntime(n, old, true)...)
	return pl, nil
}

func ndSetMTU(in *planInput) *netPlanned {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = []string{mtuSummary(n.MTUSet)}
	layers := mtuLayers(n)
	cur := mtuValues(in.Snap, layers)
	pl.Steps = append(ndFileChange(old, n), stepRun("networkctl", "reload"))
	pl.Steps = append(pl.Steps, mtuMoves(layers, cur, mtuTarget(layers, n.MTUSet))...)
	pl.Restore = ignoreAll(mtuMoves(layers, mtuTarget(layers, n.MTUSet), cur))
	pl.Restore = append([]netStep{stepRunIgnore("networkctl", "reload")}, pl.Restore...)
	return pl
}

func ndAddVLANAddr(in *planInput) *netPlanned {
	old, n, v := in.Old, in.New, *in.VLANAddr
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = vlanAddrPlanSummary(old, n, v)
	pl.Steps = ndFileChange(old, n)
	pl.Steps = append(pl.Steps, stepRun("networkctl", "reload"))
	pl.Steps = append(pl.Steps, vlanCommands(old, n)...)
	pl.Steps = append(pl.Steps, ndVLANUp(n, v, false)...)
	if v.Method == nodeproto.AddrStatic {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: v.iface(n), Argv: []string{v.Address}, Secs: 20})
	}
	pl.Restore = append(ndVLANDown(n, v, true), ignoreAll(vlanCommands(n, old))...)
	return pl
}

func ndDelVLANAddr(in *planInput) *netPlanned {
	old, n, v := in.Old, in.New, *in.VLANAddr
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = []string{fmt.Sprintf("Remove the node's address on VLAN %d (the interface %s). The VLAN stays allowed on the switch.", v.ID, v.iface(old))}
	pl.Steps = append(ndFileChange(old, n), ndVLANDown(old, v, false)...)
	pl.Restore = ndVLANUp(old, v, true)
	return pl
}
