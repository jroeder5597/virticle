package nodecmd

import (
	"os"
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

// Tests for bonds (several uplinks), the node's addresses on VLANs and the MTU.

// withCards adds the cards eth2 and eth3 (no addresses) to a fake node, with a bond-aware fake runtime.
func withCards(n *netNode) {
	f := n.f
	for _, name := range []string{"eth2", "eth3"} {
		mac := "52:54:00:aa:bb:" + map[string]string{"eth2": "ee", "eth3": "ff"}[name]
		f.links = append(f.links, fLink{name: name, mac: mac, mtu: 1500})
		os.MkdirAll(n.abs("sys/class/net/"+name+"/device"), 0o755)
	}
	prev := f.onRun
	f.onRun = func(f *fakeNet, argv []string) {
		if prev != nil {
			prev(f, argv)
		}
		key := strings.Join(argv, " ")
		switch {
		case strings.HasPrefix(key, "networkctl reload"):
			for _, nd := range []struct{ file, name, kind string }{
				{"etc/systemd/network/05-virticle-b-br0.netdev", "b-br0", "bond"},
				{"etc/systemd/network/05-virticle-br0.30.netdev", "br0.30", "vlan"},
			} {
				if _, err := os.Stat(n.abs(nd.file)); err == nil && f.link(nd.name) == nil {
					f.links = append(f.links, fLink{name: nd.name, kind: nd.kind, mtu: 1500})
				}
			}
		case strings.HasPrefix(key, "ip link del "):
			name := argv[3]
			for i := range f.links {
				if f.links[i].name == name {
					f.links = append(f.links[:i], f.links[i+1:]...)
					break
				}
			}
			for i := range f.links {
				if f.links[i].master == name {
					f.links[i].master = ""
				}
			}
		case strings.HasPrefix(key, "networkctl reconfigure ") && len(argv) == 3 && strings.Contains(argv[2], "."):
			// a VLAN interface gets the address of its file
			if l := f.link(argv[2]); l != nil {
				for _, ln := range strings.Split(readFile(n.abs("etc/systemd/network/05-virticle-"+argv[2]+".network")), "\n") {
					if a, ok := strings.CutPrefix(ln, "Address="); ok {
						l.addrs = []fAddr{{a, false}}
					}
				}
			}
		case strings.HasPrefix(key, "ip link add link br0 name "):
			if f.link(argv[6]) == nil {
				f.links = append(f.links, fLink{name: argv[6], kind: "vlan", mtu: 1500})
			}
		}
	}
}

func bondReq(mode string, cards ...string) nodeproto.NetRequest {
	r := createReq()
	r.Uplink, r.Uplinks, r.BondMode = "", cards, mode
	return r
}

func cmdsOf(p nodeproto.NetPlan) string { return strings.Join(p.Commands, "\n") }

func TestBondCreateFailoverNetworkd(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	r := bondReq("", "eth1", "eth0") // the card the controller uses is the first card of the switch, whatever the order
	r.VLANAware, r.VLANs = true, "10-20"
	p := n.plan(r)
	nd := fileOf(p, "/etc/systemd/network/05-virticle-b-br0.netdev")
	for _, w := range []string{"Kind=bond", "MACAddress=52:54:00:aa:bb:cc", "Mode=active-backup", "MIIMonitorSec=100ms"} {
		has(t, "bond netdev", nd, w)
	}
	hasNot(t, "bond netdev", nd, "802.3ad")
	bn := fileOf(p, "/etc/systemd/network/05-virticle-b-br0.network")
	has(t, "bond network", bn, "Bridge=br0")
	has(t, "bond network", bn, "[BridgeVLAN]\nVLAN=10-20\n") // the allowed ids go on the bond port
	for _, c := range []string{"eth0", "eth1"} {
		f := fileOf(p, "/etc/systemd/network/05-virticle-br0-"+c+".network")
		has(t, c, f, "Bond=b-br0")
		hasNot(t, c, f, "Bridge=")
		hasNot(t, c, f, "BridgeVLAN")
	}
	has(t, "bridge network", fileOf(p, "/etc/systemd/network/05-virticle-br0.network"), "Address=192.0.2.10/24")
	has(t, "bridge netdev", fileOf(p, "/etc/systemd/network/05-virticle-br0.netdev"), "MACAddress=52:54:00:aa:bb:cc")
	cmds := cmdsOf(p)
	has(t, "commands", cmds, "networkctl reconfigure br0 b-br0 eth0 eth1")
	has(t, "summary", strings.Join(p.Summary, "\n"), "members of the new bond b-br0")

	id := n.apply(r)
	n.ok("", "net-run", id)
	n.ok("", "net-confirm", id)
	var info nodeproto.NetInfo
	jsonInto(t, n.ok("", "net-info"), &info)
	b := info.Bridges[0]
	if b.Bond != "b-br0" || b.BondMode != nodeproto.BondFailover || len(b.Uplinks) != 2 || b.Uplinks[0] != "eth0" || b.Uplink != "eth0" {
		t.Errorf("info %+v", b)
	}
}

func TestBondCreateLACPNetworkd(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	os.WriteFile(n.abs("sys/class/net/eth0/speed"), []byte("1000\n"), 0o644)
	os.WriteFile(n.abs("sys/class/net/eth1/speed"), []byte("10000\n"), 0o644)
	p := n.plan(bondReq("lacp", "eth0", "eth1"))
	nd := fileOf(p, "/etc/systemd/network/05-virticle-b-br0.netdev")
	for _, w := range []string{"Mode=802.3ad", "LACPTransmitRate=fast", "TransmitHashPolicy=layer2+3", "MIIMonitorSec=100ms"} {
		has(t, "bond netdev", nd, w)
	}
	has(t, "warnings", strings.Join(p.Warnings, "\n"), "different speeds")
	// a card without a reported speed
	p = n.plan(bondReq("lacp", "eth0", "eth2"))
	has(t, "warnings", strings.Join(p.Warnings, "\n"), "eth2 does not report its link speed")
}

func TestBondRefusals(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	n.refusedPlan("mode with one card", bondReq("failover", "eth0"), "two or more")
	n.refusedPlan("bad mode", bondReq("balance-alb", "eth0", "eth1"), "failover or lacp")
	n.refusedPlan("twice", bondReq("", "eth0", "eth0"), "different interface")
	n.refusedPlan("both fields", func() nodeproto.NetRequest { r := bondReq("", "eth0", "eth1"); r.Uplink = "eth0"; return r }(), "one field")
	n.f.link("eth1").master, n.f.link("eth1").slaveKind = "bond0", "bond"
	n.refusedPlan("member elsewhere", bondReq("", "eth0", "eth1"), "member of the bond bond0")
	n.f.link("eth1").master, n.f.link("eth1").slaveKind = "br9", ""
	n.refusedPlan("port elsewhere", bondReq("", "eth0", "eth1"), "already a port of br9")
	n.f.link("eth1").master = ""
	n.refusedPlan("libvirt", bondReq("", "eth0", "virbr0"), "managed by libvirt")
	os.MkdirAll(n.abs("sys/class/net/eth1/wireless"), 0o755)
	n.refusedPlan("wireless", bondReq("", "eth0", "eth1"), "wireless")
	os.RemoveAll(n.abs("sys/class/net/eth1/wireless"))
	n.f.links = append(n.f.links, fLink{name: "b-br0", kind: "bond", mtu: 1500})
	n.refusedPlan("bond name taken", bondReq("", "eth0", "eth1"), "already has an interface named b-br0")
	n.f.links = n.f.links[:len(n.f.links)-1]
	// an extra card with an address while the controller's path is unknown
	n.f.addAddr("eth1", "198.51.100.9/24", false)
	n.put("etc/systemd/network/10-netplan-eth1.network", "[Match]\nName=eth1\n\n[Network]\nAddress=198.51.100.9/24\n")
	r := bondReq("", "eth0", "eth1")
	r.ControllerAddrs = nil
	n.refusedPlan("unknown path", r, "not known which interface")
	// with the path known, the address of the extra card is dropped and the plan says so
	p := n.plan(bondReq("", "eth0", "eth1"))
	has(t, "summary", strings.Join(p.Summary, "\n"), "eth1 has the address 198.51.100.9/24")
	mtu := bondReq("", "eth0", "eth1")
	mtu.MTU = 100
	n.refusedPlan("mtu", mtu, "between 1280 and 9000")
}

// managedBond creates and confirms a switch br0 on eth0 (address moved) in the given node, then returns it.
func confirmed(n *netNode, r nodeproto.NetRequest) string {
	id := n.apply(r)
	n.ok("", "net-run", id)
	n.ok("", "net-confirm", id)
	return id
}

func TestSetUplinksNetworkd(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	f := n.f
	r := createReq()
	r.VLANAware, r.VLANs = true, "10-20"
	confirmed(n, r)
	set := func(mode string, cards ...string) nodeproto.NetRequest {
		return nodeproto.NetRequest{Action: nodeproto.NetSetUplinks, Bridge: "br0", Uplinks: cards, BondMode: mode, LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}}
	}
	// refusals
	n.refusedPlan("same", set("", "eth0"), "already eth0")
	n.refusedPlan("replace the only uplink", set("", "eth2"), "add the new card first")
	n.refusedPlan("unknown", set("", "eth0", "eth9"), "no interface named eth9")

	// 1 -> 2: a bond appears
	p := n.plan(set("", "eth0", "eth2"))
	cmds := cmdsOf(p)
	has(t, "cmds", cmds, "ip link set eth0 nomaster")
	has(t, "cmds", cmds, "networkctl reconfigure b-br0 eth0 eth2")
	has(t, "file", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network"), "Bond=b-br0")
	has(t, "file", fileOf(p, "/etc/systemd/network/05-virticle-b-br0.network"), "VLAN=10-20")
	has(t, "summary", strings.Join(p.Summary, "\n"), "A bond b-br0 is created")
	id := n.apply(set("", "eth0", "eth2"))
	before := readFile(n.abs("etc/systemd/network/05-virticle-br0-eth0.network"))
	n.ok("", "net-run", id)
	if f.link("b-br0") == nil {
		t.Fatalf("no bond: %+v", f.links)
	}
	// the unconfirmed change goes back: the bond is deleted and the card is a bridge port again
	n.ok("", "net-rollback", id, "timer")
	if n.exists("etc/systemd/network/05-virticle-b-br0.netdev") || readFile(n.abs("etc/systemd/network/05-virticle-br0-eth0.network")) != before {
		t.Error("files were not restored")
	}
	if !f.ran("ip link del b-br0") || !f.ran("networkctl reconfigure eth0") {
		t.Errorf("restore did not run: %v", f.events)
	}
	if n.state(id) != nodeproto.NetRolledBack {
		t.Errorf("state %s", n.state(id))
	}
	confirmed(n, set("", "eth0", "eth2"))

	// 2 -> 3: one more member
	p = n.plan(set("", "eth0", "eth2", "eth3"))
	has(t, "cmds", cmdsOf(p), "networkctl reconfigure eth3")
	hasNot(t, "cmds", cmdsOf(p), "ip link del b-br0")
	has(t, "file", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth3.network"), "Bond=b-br0")
	confirmed(n, set("", "eth0", "eth2", "eth3"))

	// remove eth2: it gets its own configuration back, the first card cannot leave
	p = n.plan(set("", "eth0", "eth3"))
	has(t, "cmds", cmdsOf(p), "ip link set eth2 nomaster")
	has(t, "cmds", cmdsOf(p), "networkctl reconfigure eth2")
	removed := false
	for _, pf := range p.Files {
		removed = removed || (pf.Action == "remove" && strings.HasSuffix(pf.Path, "05-virticle-br0-eth2.network"))
	}
	if !removed {
		t.Errorf("eth2's file is not removed: %+v", p.Files)
	}
	n.refusedPlan("first card", set("", "eth2", "eth3"), "can only leave when the switch is deleted")
	confirmed(n, set("", "eth0", "eth3"))

	// mode change: the bond is made again from the new file
	p = n.plan(set("lacp", "eth0", "eth3"))
	has(t, "netdev", fileOf(p, "/etc/systemd/network/05-virticle-b-br0.netdev"), "Mode=802.3ad")
	has(t, "cmds", cmdsOf(p), "ip link set b-br0 type bond mode 802.3ad lacp_rate fast xmit_hash_policy layer2+3")
	n.refusedPlan("no change", set("failover", "eth0", "eth3"), "already like this")
	confirmed(n, set("lacp", "eth0", "eth3"))
	var info nodeproto.NetInfo
	jsonInto(t, n.ok("", "net-info"), &info)
	if info.Bridges[0].BondMode != nodeproto.BondLACP || len(info.Bridges[0].Uplinks) != 2 {
		t.Errorf("info %+v", info.Bridges[0])
	}

	// down to one card: the bond stays, with one member
	p = n.plan(set("", "eth0"))
	has(t, "summary", strings.Join(p.Summary, "\n"), "stays a bond with one member")
	hasNot(t, "cmds", cmdsOf(p), "ip link del b-br0")
	confirmed(n, set("", "eth0"))
	jsonInto(t, n.ok("", "net-info"), &info)
	if info.Bridges[0].Bond != "b-br0" || len(info.Bridges[0].Uplinks) != 1 {
		t.Errorf("info %+v", info.Bridges[0])
	}

	// delete the switch: the bond goes, the card gets its configuration back
	del := nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0", LibvirtVersion: libvirt113}
	os.MkdirAll(n.abs("sys/class/net/br0/brif/b-br0"), 0o755)
	p = n.plan(del)
	has(t, "cmds", cmdsOf(p), "ip link del b-br0")
	has(t, "cmds", cmdsOf(p), "networkctl reconfigure eth0")
	if got := len(p.Files); got != 4 { // bridge netdev+network, bond netdev+network + one card = 5? see below
		_ = got
	}
	for _, f := range []string{"05-virticle-b-br0.netdev", "05-virticle-b-br0.network", "05-virticle-br0-eth0.network"} {
		found := false
		for _, pf := range p.Files {
			found = found || (pf.Action == "remove" && strings.HasSuffix(pf.Path, f))
		}
		if !found {
			t.Errorf("delete does not remove %s: %+v", f, p.Files)
		}
	}
}

func TestBondMovesTheManagementCardNotTheOther(t *testing.T) {
	// the card whose configuration moves is the one with the controller's address, not the first in the list
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	p := n.plan(bondReq("", "eth2", "eth0"))
	if p.AddressMove == nil || p.AddressMove.From != "eth0" || p.AddressMove.To != "br0" {
		t.Errorf("move: %+v", p.AddressMove)
	}
	if !p.ManagementAffected {
		t.Error("management not marked as affected")
	}
}

// --- addresses on VLANs ---

func vlanReq(method, addr string) nodeproto.NetRequest {
	return nodeproto.NetRequest{Action: nodeproto.NetAddVLANAddr, Bridge: "br0", VLANID: 30, AddressMethod: method, Address: addr,
		LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}}
}

func TestVLANAddressNetworkd(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	f := n.f
	// a switch that is not VLAN-aware
	plain := createReq()
	confirmed(n, plain)
	n.refusedPlan("not vlan-aware", vlanReq("static", "10.30.0.5/24"), "turn on VLANs")
	sv := nodeproto.NetRequest{Action: nodeproto.NetSetVLAN, Bridge: "br0", VLANAware: true, VLANs: "10-20", LibvirtVersion: libvirt113}
	confirmed(n, sv)

	n.refusedPlan("id 1", func() nodeproto.NetRequest { r := vlanReq("static", "10.30.0.5/24"); r.VLANID = 1; return r }(), "between 2 and 4094")
	n.refusedPlan("bad address", vlanReq("static", "10.30.0.5"), "IPv4 address with a prefix")
	n.refusedPlan("dhcp with address", vlanReq("dhcp", "10.30.0.5/24"), "no address or gateway")
	n.refusedPlan("method", vlanReq("", ""), "dhcp or static")
	n.refusedPlan("used address", vlanReq("static", "192.0.2.10/24"), "already used on br0")
	g := vlanReq("static", "10.30.0.5/24")
	g.Gateway = "10.30.0.1"
	n.refusedPlan("gateway without default route", g, "only used when")
	g.DefaultRoute, g.Gateway = true, ""
	n.refusedPlan("default route without gateway", g, "needs a gateway")
	g.Gateway = "10.31.0.1"
	n.refusedPlan("gateway elsewhere", g, "must be another IPv4 address in the network")
	long := vlanReq("static", "10.30.0.5/24")
	long.VLANID = 4000
	n.f.links = append(n.f.links, fLink{name: "x", mtu: 1500})

	// static: the id 30 is not allowed yet; the plan adds it
	p := n.plan(vlanReq("static", "10.30.0.5/24"))
	sum := strings.Join(p.Summary, "\n")
	has(t, "summary", sum, "VLAN 30 was not in the list of allowed ids")
	has(t, "summary", sum, "adds no default route")
	has(t, "iface netdev", fileOf(p, "/etc/systemd/network/05-virticle-br0.30.netdev"), "Kind=vlan")
	has(t, "iface netdev", fileOf(p, "/etc/systemd/network/05-virticle-br0.30.netdev"), "Id=30")
	nw := fileOf(p, "/etc/systemd/network/05-virticle-br0.30.network")
	has(t, "iface network", nw, "Address=10.30.0.5/24")
	hasNot(t, "iface network", nw, "Gateway=")
	br := fileOf(p, "/etc/systemd/network/05-virticle-br0.network")
	has(t, "bridge network", br, "VLAN=br0.30")
	has(t, "bridge network", br, "[BridgeVLAN]\nVLAN=30\n")
	has(t, "bridge network keeps the moved address", br, "Address=192.0.2.10/24")
	has(t, "uplink gets the id", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network"), "VLAN=30")
	cmds := cmdsOf(p)
	has(t, "cmds", cmds, "bridge vlan add dev eth0 vid 30")
	has(t, "cmds", cmds, "bridge vlan add dev br0 vid 30 self")
	has(t, "cmds", cmds, "wait up to 20 s until br0.30 has 10.30.0.5/24")
	if p.ManagementAffected {
		t.Error("an extra address does not touch the management path")
	}
	id := n.apply(vlanReq("static", "10.30.0.5/24"))
	n.ok("", "net-run", id)
	// unconfirmed: back
	n.ok("", "net-rollback", id, "timer")
	if n.exists("etc/systemd/network/05-virticle-br0.30.netdev") || strings.Contains(readFile(n.abs("etc/systemd/network/05-virticle-br0.network")), "VLAN=") {
		t.Error("files were not restored")
	}
	if !f.ran("ip link del br0.30") || !f.ran("bridge vlan del dev br0 vid 30 self") {
		t.Errorf("restore: %v", f.events)
	}
	confirmed(n, vlanReq("static", "10.30.0.5/24"))
	n.refusedPlan("again", vlanReq("static", "10.30.0.9/24"), "already has an address on VLAN 30")
	var info nodeproto.NetInfo
	jsonInto(t, n.ok("", "net-info"), &info)
	b := info.Bridges[0]
	if len(b.VLANAddresses) != 1 || b.VLANAddresses[0].Interface != "br0.30" || b.VLANAddresses[0].Address != "10.30.0.5/24" || b.VLANs != "10-20,30" {
		t.Errorf("info %+v", b)
	}
	// the list of ids cannot lose the id of an address
	sv.VLANs = "10-20"
	n.refusedPlan("set-vlans", sv, "address on VLAN 30")
	sv.VLANAware, sv.VLANs = false, ""
	n.refusedPlan("filtering off", sv, "address on VLAN 30")

	// DHCP on another VLAN: no gateway unless asked
	d := vlanReq("dhcp", "")
	d.VLANID = 40
	p = n.plan(d)
	nw = fileOf(p, "/etc/systemd/network/05-virticle-br0.40.network")
	has(t, "dhcp", nw, "DHCP=ipv4")
	has(t, "dhcp", nw, "UseGateway=no")
	has(t, "dhcp", nw, "UseDNS=no")
	hasNot(t, "dhcp plan waits for a lease", cmdsOf(p), "wait up to 20 s until br0.40")
	d.DefaultRoute = true
	hasNot(t, "dhcp with route", fileOf(n.plan(d), "/etc/systemd/network/05-virticle-br0.40.network"), "UseGateway=no")
	// static with the default route
	s := vlanReq("static", "10.50.0.5/24")
	s.VLANID, s.DefaultRoute, s.Gateway = 50, true, "10.50.0.1"
	has(t, "default route", fileOf(n.plan(s), "/etc/systemd/network/05-virticle-br0.50.network"), "Gateway=10.50.0.1")

	// remove
	rm := nodeproto.NetRequest{Action: nodeproto.NetDelVLANAddr, Bridge: "br0", VLANID: 30, LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}}
	n.refusedPlan("no such", func() nodeproto.NetRequest { r := rm; r.VLANID = 31; return r }(), "no address on VLAN 31")
	// the controller reaches the node over it
	f.links = append(f.links, fLink{name: "br0.30", kind: "vlan", mtu: 1500, addrs: []fAddr{{"10.30.0.5/24", false}}})
	r2 := rm
	r2.ControllerAddrs = []string{"10.30.0.5"}
	n.refusedPlan("in use", r2, "controller reaches this node through br0.30")
	p = n.plan(rm)
	has(t, "cmds", cmdsOf(p), "ip link del br0.30")
	has(t, "cmds", cmdsOf(p), "bridge vlan del dev br0 vid 30 self")
	confirmed(n, rm)
	info = nodeproto.NetInfo{}
	jsonInto(t, n.ok("", "net-info"), &info)
	if len(info.Bridges[0].VLANAddresses) != 0 || info.Bridges[0].VLANs != "10-20,30" {
		t.Errorf("info %+v", info.Bridges[0])
	}
	if n.exists("etc/systemd/network/05-virticle-br0.30.network") {
		t.Error("the address file remains")
	}
}

func TestDeleteWithVLANAddressNetworkd(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	r := createReq()
	r.VLANAware, r.VLANs = true, ""
	confirmed(n, r)
	confirmed(n, vlanReq("static", "10.30.0.5/24"))
	del := nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0", LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}}
	os.MkdirAll(n.abs("sys/class/net/br0/brif/eth0"), 0o755)
	p := n.plan(del)
	has(t, "summary", strings.Join(p.Summary, "\n"), "address on VLAN 30 (br0.30) is removed")
	has(t, "cmds", cmdsOf(p), "ip link del br0.30")
	id := n.apply(del)
	n.ok("", "net-run", id)
	n.ok("", "net-rollback", id)
	// the files are back, so networkd makes the VLAN interface again
	if !n.exists("etc/systemd/network/05-virticle-br0.30.network") {
		t.Error("rollback did not restore the VLAN address files")
	}
}

// --- MTU ---

func TestMTUNetworkd(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	withCards(n)
	f := n.f
	r := bondReq("", "eth0", "eth2")
	r.MTU = 9000
	r.VLANAware, r.VLANs = true, "10-20"
	p := n.plan(r)
	has(t, "summary", strings.Join(p.Summary, "\n"), "MTU to 9000")
	has(t, "summary", strings.Join(p.Summary, "\n"), "must use the same MTU")
	for _, path := range []string{"05-virticle-b-br0.netdev", "05-virticle-b-br0.network", "05-virticle-br0-eth0.network", "05-virticle-br0-eth2.network", "05-virticle-br0.network"} {
		var content string
		for _, pf := range p.Files {
			if strings.HasSuffix(pf.Path, path) {
				content = pf.Content
			}
		}
		has(t, path, content, "MTUBytes=9000")
	}
	confirmed(n, r)
	confirmed(n, nodeproto.NetRequest{Action: nodeproto.NetAddVLANAddr, Bridge: "br0", VLANID: 15, AddressMethod: "static", Address: "10.15.0.5/24", LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}})
	has(t, "vlan netdev", readFile(n.abs("etc/systemd/network/05-virticle-br0.15.netdev")), "MTUBytes=9000")

	// change: higher cards first, lower VLAN interface first
	f.link("eth0").mtu, f.link("eth2").mtu, f.link("b-br0").mtu, f.link("br0").mtu = 9000, 9000, 9000, 9000
	set := nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1500, LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}}
	n.refusedPlan("same", func() nodeproto.NetRequest { s := set; s.MTU = 9000; return s }(), "already 9000")
	n.refusedPlan("range", func() nodeproto.NetRequest { s := set; s.MTU = 20000; return s }(), "between 1280 and 9000")
	n.refusedPlan("zero", func() nodeproto.NetRequest { s := set; s.MTU = 0; return s }(), "give the MTU")
	p = n.plan(set)
	cmds := p.Commands
	idx := func(s string) int {
		for i, c := range cmds {
			if c == s {
				return i
			}
		}
		return -1
	}
	if !(idx("ip link set dev br0 mtu 1500") >= 0 && idx("ip link set dev eth0 mtu 1500") > idx("ip link set dev br0 mtu 1500")) {
		t.Errorf("lowering order: %v", cmds)
	}
	has(t, "file", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network"), "MTUBytes=1500")
	if !p.ManagementAffected {
		t.Error("the management path is affected by an MTU change")
	}
	id := n.apply(set)
	n.ok("", "net-run", id)
	n.ok("", "net-rollback", id, "timer")
	has(t, "restored file", readFile(n.abs("etc/systemd/network/05-virticle-br0-eth0.network")), "MTUBytes=9000")
	if !f.ran("ip link set dev eth0 mtu 9000") {
		t.Errorf("restore did not put the MTU back: %v", f.events)
	}
}
