package nodecmd

import (
	"fmt"
	"net/netip"
	"strings"

	"virticle/internal/nodeproto"
)

// cardList is the cards a request names: Uplinks, or the one Uplink.
func cardList(req *nodeproto.NetRequest) ([]string, error) {
	if req.Uplink != "" && len(req.Uplinks) > 0 {
		return nil, Die("name the uplinks in one field: uplink or uplinks")
	}
	list := req.Uplinks
	if req.Uplink != "" {
		list = []string{req.Uplink}
	}
	seen := map[string]bool{}
	for _, n := range list {
		if n == "" || seen[n] {
			return nil, Die("the uplinks must be different interface names")
		}
		seen[n] = true
	}
	return list, nil
}

// checkCard looks at a card that is to become an uplink: it must exist, be physical and free.
func (c *Ctx) checkCard(snap *netSnap, name string) (*planCard, error) {
	l := snap.link(name)
	switch {
	case libvirtOwned(name):
		return nil, Fail("%s is managed by libvirt and is not touched", name)
	case l == nil:
		return nil, Fail("the node has no interface named %s", name)
	case l.SlaveKind == "bond":
		return nil, Fail("%s is already a member of the bond %s", name, l.Master)
	case l.Master != "":
		return nil, Fail("%s is already a port of %s", name, l.Master)
	}
	if ok, why := c.isPhysical(*l); !ok {
		return nil, Fail("%s cannot be an uplink: %s", name, why)
	}
	return &planCard{Link: l, Expect: uplinkExpect(*l)}, nil
}

func (c *Ctx) checkMTU(v int) error {
	if v != 0 && (v < 1280 || v > 9000) {
		return Die("the MTU must be between 1280 and 9000")
	}
	return nil
}

// checkVLANAddrRequest validates the address part of an add-vlan-address request and returns it.
func checkVLANAddrRequest(req *nodeproto.NetRequest, bridge string) (*vlanAddrSpec, error) {
	id := req.VLANID
	if id < 2 || id > 4094 {
		return nil, Die("the VLAN id must be between 2 and 4094 (VLAN 1 is the untagged network of the switch itself)")
	}
	if len(nodeproto.VLANInterface(bridge, id)) > 15 {
		return nil, Fail("the interface name %s would be longer than 15 characters; use a shorter switch name", nodeproto.VLANInterface(bridge, id))
	}
	v := &vlanAddrSpec{ID: id, Method: req.AddressMethod, DefaultRoute: req.DefaultRoute}
	switch req.AddressMethod {
	case nodeproto.AddrDHCP:
		if req.Address != "" || req.Gateway != "" {
			return nil, Die("with DHCP there is no address or gateway to enter")
		}
	case nodeproto.AddrStatic:
		p, err := netip.ParsePrefix(req.Address)
		if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 {
			return nil, Die("%q is not an IPv4 address with a prefix, such as 10.30.0.5/24", req.Address)
		}
		v.Address = p.String()
		if req.Gateway != "" {
			g, err := netip.ParseAddr(req.Gateway)
			if err != nil || !g.Is4() || !p.Contains(g) || g == p.Addr() {
				return nil, Die("the gateway %q must be another IPv4 address in the network %s", req.Gateway, p.Masked())
			}
			v.Gateway = g.String()
		}
		if req.DefaultRoute && v.Gateway == "" {
			return nil, Die("a default route over this VLAN needs a gateway")
		}
		if !req.DefaultRoute && req.Gateway != "" {
			return nil, Die("a gateway is only used when the VLAN carries the node's default route")
		}
	default:
		return nil, Die("the address method must be dhcp or static")
	}
	if req.AddressMethod == nodeproto.AddrDHCP && req.Gateway != "" {
		return nil, Die("with DHCP the gateway comes from the DHCP server")
	}
	return v, nil
}

// buildPlan validates req and computes the plan. Nothing on the system changes.
func (c *Ctx) buildPlan(req *nodeproto.NetRequest) (*planInput, *netPlanned, stackInfo, error) {
	st := c.detectStack()
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = nodeproto.NetDefaultTimeout
	}
	if req.TimeoutSeconds < nodeproto.NetMinTimeout || req.TimeoutSeconds > nodeproto.NetMaxTimeout {
		return nil, nil, st, Die("the time to confirm must be between %d and %d seconds", nodeproto.NetMinTimeout, nodeproto.NetMaxTimeout)
	}
	if !bridgeNameRE.MatchString(req.Bridge) || libvirtOwned(req.Bridge) {
		return nil, nil, st, Die("the bridge name must be 1 to 15 characters: lower-case letters, digits, dash and underscore, starting with a letter, and not one libvirt uses (virbr, vnet)")
	}
	if !st.Supported {
		return nil, nil, st, Fail("%s", st.Note)
	}
	if p := c.pendingMeta(); p != nil {
		return nil, nil, st, Fail("the network change %s is still pending (%s); confirm it or roll it back first", p.ID, p.State)
	}
	snap, err := c.readSnap()
	if err != nil {
		return nil, nil, st, err
	}
	managed := c.loadManaged()
	in := &planInput{Req: req, Snap: snap, Name: st.Name}
	mgmt := c.mgmtLink(snap, req)
	in.Mgmt = mgmt
	existing := snap.link(req.Bridge)
	if err := c.checkMTU(req.MTU); err != nil {
		return nil, nil, st, err
	}

	// VLAN options (create, set-vlans, delete)
	vlans := ""
	vlanAction := req.Action == nodeproto.NetCreate || req.Action == nodeproto.NetSetVLAN || req.Action == nodeproto.NetDelete
	if !vlanAction {
		if req.VLANAware || req.VLANs != "" {
			return nil, nil, st, Die("the VLAN settings are not part of this change")
		}
	} else if req.VLANAware {
		if req.Action != nodeproto.NetDelete && req.LibvirtVersion < nodeproto.LibvirtVLANMin {
			return nil, nil, st, Fail("VLANs on a Linux bridge need libvirt 11.0.0 or newer (this node has %s)", libvirtVersionString(req.LibvirtVersion))
		}
		if vlans, err = nodeproto.ParseVLANs(req.VLANs); err != nil {
			return nil, nil, st, Die("%v", err)
		}
	} else if req.VLANs != "" {
		return nil, nil, st, Die("VLAN ids need a VLAN-aware bridge")
	}

	switch req.Action {
	case nodeproto.NetCreate:
		if err := c.planCreate(in, existing, mgmt, vlans); err != nil {
			return nil, nil, st, err
		}
	case nodeproto.NetDelete, nodeproto.NetSetVLAN, nodeproto.NetSetUplinks, nodeproto.NetSetMTU, nodeproto.NetAddVLANAddr, nodeproto.NetDelVLANAddr:
		old := managed[req.Bridge]
		if old == nil {
			if existing != nil {
				return nil, nil, st, Fail("%s was not created by this feature and is only shown, not changed", req.Bridge)
			}
			return nil, nil, st, Fail("the node has no bridge named %s", req.Bridge)
		}
		in.Old = old
		if req.Action == nodeproto.NetDelete {
			if err := c.planDelete(in, existing, mgmt); err != nil {
				return nil, nil, st, err
			}
			break
		}
		if existing == nil {
			return nil, nil, st, Fail("the bridge %s does not exist right now", req.Bridge)
		}
		n := old.clone()
		in.New = n
		var err error
		switch req.Action {
		case nodeproto.NetSetVLAN:
			n.VLANAware, n.VLANs = req.VLANAware, vlans
			if n.VLANAware == old.VLANAware && n.VLANs == old.VLANs {
				return nil, nil, st, Fail("the VLAN settings of %s are already like this", req.Bridge)
			}
			for _, a := range old.VLANAddrs {
				if !n.VLANAware || !nodeproto.VLANAllowed(n.VLANs, a.ID) {
					return nil, nil, st, Fail("this node has an address on VLAN %d of %s; remove that address first, or keep the id in the list", a.ID, req.Bridge)
				}
			}
		case nodeproto.NetSetUplinks:
			err = c.planSetUplinks(in, mgmt)
		case nodeproto.NetSetMTU:
			if req.MTU == 0 {
				return nil, nil, st, Die("give the MTU, between 1280 and 9000")
			}
			if old.MTUSet == req.MTU {
				return nil, nil, st, Fail("the MTU of %s is already %d", req.Bridge, req.MTU)
			}
			n.MTUSet = req.MTU
		case nodeproto.NetAddVLANAddr:
			err = c.planAddVLANAddr(in, snap)
		case nodeproto.NetDelVLANAddr:
			err = c.planDelVLANAddr(in, mgmt)
		}
		if err != nil {
			return nil, nil, st, err
		}
	default:
		return nil, nil, st, Die("unknown action %q", req.Action)
	}
	pl, err := st.impl.plan(c, in)
	if err != nil {
		return nil, nil, st, err
	}
	return in, pl, st, nil
}

func (c *Ctx) planCreate(in *planInput, existing *netLink, mgmt, vlans string) error {
	req, snap := in.Req, in.Snap
	if existing != nil {
		return Fail("the node already has an interface named %s", req.Bridge)
	}
	names, err := cardList(req)
	if err != nil {
		return err
	}
	spec := &bridgeSpec{Bridge: req.Bridge, MoveAddresses: req.MoveAddresses, VLANAware: req.VLANAware, VLANs: vlans, MTUSet: req.MTU,
		Stack: in.Name, Created: c.netNow().UTC().Format("2006-01-02T15:04:05Z"), Data: map[string]string{}}
	in.New = spec
	if len(names) == 0 {
		if req.MoveAddresses {
			return Die("addresses can only move from an uplink: choose one")
		}
		if req.BondMode != "" {
			return Die("the bond mode needs two or more uplinks")
		}
		return nil
	}
	var cards []*planCard
	for _, n := range names {
		pc, err := c.checkCard(snap, n)
		if err != nil {
			return err
		}
		cards = append(cards, pc)
	}
	// The first card is the one whose configuration the bridge takes over: the card the controller uses, else the
	// first one that has addresses.
	first := -1
	for i, pc := range cards {
		if pc.Link.Name == mgmt {
			first = i
			break
		}
	}
	for i, pc := range cards {
		if first < 0 && len(pc.Link.globalAddrs()) > 0 {
			first = i
		}
	}
	if first < 0 {
		first = 0
	}
	cards[0], cards[first] = cards[first], cards[0]
	up := cards[0].Link
	in.Uplink, in.Expect = up, cards[0].Expect
	spec.Uplink, spec.MAC, spec.MTU, spec.UplinkExpect = up.Name, up.MAC, up.MTU, in.Expect
	for _, pc := range cards[1:] {
		in.Extra = append(in.Extra, *pc)
		spec.Members = append(spec.Members, cardSpec{Name: pc.Link.Name, MTU: pc.Link.MTU, Expect: pc.Expect})
		if len(pc.Link.globalAddrs()) > 0 && mgmt == "" {
			return Fail("%s has addresses and it is not known which interface the controller uses to reach this node; remove it from the list or choose another card", pc.Link.Name)
		}
	}
	if len(cards) >= 2 {
		spec.Bond, spec.BondMode = bondName(req.Bridge), req.BondMode
		if spec.BondMode == "" {
			spec.BondMode = nodeproto.BondFailover
		}
		if !bondModeOK(spec.BondMode) {
			return Die("the bond mode must be failover or lacp")
		}
		if snap.link(spec.Bond) != nil {
			return Fail("the node already has an interface named %s", spec.Bond)
		}
	} else if req.BondMode != "" {
		return Die("the bond mode needs two or more uplinks")
	}
	has := len(up.globalAddrs()) > 0
	switch {
	case mgmt == up.Name && !req.MoveAddresses:
		return Fail("%s carries the controller's connection to this node; turn on moving its addresses to the bridge, or choose another uplink", up.Name)
	case has && !req.MoveAddresses && mgmt == "":
		return Fail("%s has addresses and it is not known which interface the controller uses to reach this node; turn on moving its addresses to the bridge", up.Name)
	}
	return nil
}

func (c *Ctx) planDelete(in *planInput, existing *netLink, mgmt string) error {
	req, old := in.Req, in.Old
	if mgmt != "" {
		for _, a := range old.VLANAddrs {
			if a.iface(old) == mgmt {
				return Fail("the controller reaches this node through %s; it cannot be removed", mgmt)
			}
		}
	}
	if existing != nil {
		for _, p := range c.bridgePorts(req.Bridge) {
			switch {
			case p == old.port():
			case libvirtOwned(p) || strings.HasPrefix(p, "tap"):
				return Fail("virtual machines are attached to %s (port %s); detach them first", req.Bridge, p)
			default:
				return Fail("%s has another port, %s, which this feature did not add; remove it first", req.Bridge, p)
			}
		}
	}
	return nil
}

func (c *Ctx) planSetUplinks(in *planInput, mgmt string) error {
	req, snap, old, n := in.Req, in.Snap, in.Old, in.New
	if old.Uplink == "" {
		return Fail("%s has no uplink yet; this change needs one to start from", old.Bridge)
	}
	names, err := cardList(req)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return Die("name at least one uplink")
	}
	want := map[string]bool{}
	for _, nm := range names {
		want[nm] = true
	}
	var keep []cardSpec
	for _, oc := range old.cards() {
		if want[oc.Name] {
			keep = append(keep, oc)
			continue
		}
	}
	if len(keep) == 0 {
		return Fail("a switch cannot lose all of its uplinks at once; add the new card first, then remove the old one")
	}
	if old.MoveAddresses && !want[old.Uplink] {
		return Fail("%s is the card whose addresses the switch took over; it can only leave when the switch is deleted", old.Uplink)
	}
	for _, nm := range names {
		if old.card(nm) != nil {
			continue
		}
		pc, err := c.checkCard(snap, nm)
		if err != nil {
			return err
		}
		if pc.Link.Name == mgmt {
			return Fail("%s carries the controller's connection to this node and cannot become an uplink of this switch", nm)
		}
		if len(pc.Link.globalAddrs()) > 0 && mgmt == "" {
			return Fail("%s has addresses and it is not known which interface the controller uses to reach this node; choose another card", nm)
		}
		in.Added = append(in.Added, *pc)
		keep = append(keep, cardSpec{Name: nm, MTU: pc.Link.MTU, Expect: pc.Expect})
	}
	n.setCards(keep)
	mode := req.BondMode
	switch {
	case old.Bond == "" && len(keep) == 1:
		if mode != "" {
			return Die("the bond mode needs two or more uplinks")
		}
		return Fail("the uplink of %s is already %s", old.Bridge, keep[0].Name)
	case old.Bond == "":
		n.Bond, n.BondMode = bondName(old.Bridge), mode
		if n.BondMode == "" {
			n.BondMode = nodeproto.BondFailover
		}
		if snap.link(n.Bond) != nil {
			return Fail("the node already has an interface named %s", n.Bond)
		}
	default:
		if mode != "" {
			n.BondMode = mode
		}
	}
	if !bondModeOK(n.BondMode) {
		return Die("the bond mode must be failover or lacp")
	}
	if d := diffCards(old, n); len(d.Added) == 0 && len(d.Removed) == 0 && !d.ModeChanged {
		return Fail("the uplinks of %s are already like this", old.Bridge)
	}
	return nil
}

func (c *Ctx) planAddVLANAddr(in *planInput, snap *netSnap) error {
	req, old, n := in.Req, in.Old, in.New
	if !old.VLANAware {
		return Fail("%s does not filter VLANs; turn on VLANs for the switch first", old.Bridge)
	}
	v, err := checkVLANAddrRequest(req, old.Bridge)
	if err != nil {
		return err
	}
	if old.vlanAddr(v.ID) != nil {
		return Fail("this node already has an address on VLAN %d of %s", v.ID, old.Bridge)
	}
	if snap.link(v.iface(old)) != nil {
		return Fail("the node already has an interface named %s", v.iface(old))
	}
	if v.Method == nodeproto.AddrStatic {
		p, _ := netip.ParsePrefix(v.Address)
		for _, l := range snap.Links {
			for _, a := range l.globalAddrs() {
				if a.Local == p.Addr().String() {
					return Fail("the address %s is already used on %s", a.Local, l.Name)
				}
			}
		}
	}
	in.VLANAddr = v
	n.VLANAddrs = append(n.VLANAddrs, *v)
	if canon, err := nodeproto.ParseVLANs(old.VLANs + "," + fmt.Sprint(v.ID)); err == nil {
		n.VLANs = canon
	}
	return nil
}

func (c *Ctx) planDelVLANAddr(in *planInput, mgmt string) error {
	req, old, n := in.Req, in.Old, in.New
	v := old.vlanAddr(req.VLANID)
	if v == nil {
		return Fail("this node has no address on VLAN %d of %s", req.VLANID, old.Bridge)
	}
	if mgmt != "" && mgmt == v.iface(old) {
		return Fail("the controller reaches this node through %s; it cannot be removed", mgmt)
	}
	in.VLANAddr = v
	n.VLANAddrs = nil
	for _, a := range old.VLANAddrs {
		if a.ID != v.ID {
			n.VLANAddrs = append(n.VLANAddrs, a)
		}
	}
	return nil
}
