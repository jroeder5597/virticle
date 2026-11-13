package nodecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

const libvirt113 = 11003000

func createReq() nodeproto.NetRequest {
	return nodeproto.NetRequest{Action: nodeproto.NetCreate, Bridge: "br0", Uplink: "eth0", MoveAddresses: true,
		ControllerAddrs: []string{"192.0.2.10"}, LibvirtVersion: libvirt113}
}

func has(t *testing.T, what, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s does not contain %q:\n%s", what, want, text)
	}
}

func hasNot(t *testing.T, what, text, bad string) {
	t.Helper()
	if strings.Contains(text, bad) {
		t.Errorf("%s contains %q:\n%s", what, bad, text)
	}
}

func fileOf(p nodeproto.NetPlan, path string) string {
	for _, f := range p.Files {
		if f.Path == path {
			return f.Content
		}
	}
	return "<no such file in the plan>"
}

func TestNetPlanNetworkdDHCP(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	p := n.plan(createReq())
	if p.Stack != "netplan-networkd" {
		t.Errorf("stack %q", p.Stack)
	}
	br := fileOf(p, "/etc/systemd/network/05-virticle-br0.network")
	has(t, "bridge network", br, "Name=br0")
	has(t, "bridge network", br, "DHCP=ipv4")
	has(t, "bridge network", br, "RouteMetric=100")
	hasNot(t, "bridge network", br, "MACAddress=52") // the match of the uplink is not copied
	nd := fileOf(p, "/etc/systemd/network/05-virticle-br0.netdev")
	has(t, "netdev", nd, "Kind=bridge")
	has(t, "netdev", nd, "MACAddress=52:54:00:aa:bb:cc") // the lease follows the MAC address
	hasNot(t, "netdev", nd, "VLANFiltering")
	up := fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network")
	has(t, "uplink network", up, "Name=eth0")
	has(t, "uplink network", up, "Bridge=br0")
	hasNot(t, "uplink network", up, "DHCP")
	if p.AddressMove == nil || p.AddressMove.Method != "dhcp" || p.AddressMove.From != "eth0" || p.AddressMove.To != "br0" || p.AddressMove.Gateway != "192.0.2.1" {
		t.Errorf("address move: %+v", p.AddressMove)
	}
	if !p.ManagementAffected || p.ManagementInterface != "eth0" {
		t.Errorf("management: %q %v", p.ManagementInterface, p.ManagementAffected)
	}
	if p.TimeoutSeconds != 90 {
		t.Errorf("timeout %d", p.TimeoutSeconds)
	}
	cmds := strings.Join(p.Commands, "\n")
	for _, w := range []string{"networkctl reload", "networkctl reconfigure br0 eth0", "wait up to 40 s until br0 has dhcp4", "ip addr flush dev eth0"} {
		has(t, "commands", cmds, w)
	}
	hasNot(t, "commands", cmds, "firewall")
	// planning changes nothing
	if n.exists("etc/systemd/network/05-virticle-br0.netdev") || n.f.ran("networkctl") {
		t.Error("net-plan changed the node")
	}
	has(t, "summary", strings.Join(p.Summary, "\n"), "stored")
}

func TestNetPlanNetworkdStatic(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	p := n.plan(createReq())
	br := fileOf(p, "/etc/systemd/network/05-virticle-br0.network")
	for _, w := range []string{"Address=192.0.2.10/24", "Gateway=192.0.2.1", "DNS=192.0.2.53", "Domains=lab.example", "MTUBytes=1500"} {
		has(t, "bridge network", br, w)
	}
	if p.AddressMove == nil || p.AddressMove.Method != "static" || p.AddressMove.Addresses[0] != "192.0.2.10/24" {
		t.Errorf("address move: %+v", p.AddressMove)
	}
	has(t, "commands", strings.Join(p.Commands, "\n"), "until br0 has 192.0.2.10/24")
	has(t, "uplink network", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network"), "MTUBytes=1500")
}

func TestNetPlanVLANAware(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	r := createReq()
	r.VLANAware = true
	p := n.plan(r)
	has(t, "netdev", fileOf(p, "/etc/systemd/network/05-virticle-br0.netdev"), "VLANFiltering=yes")
	up := fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network")
	has(t, "uplink", up, "VLAN=2-4094")
	has(t, "uplink", up, "PVID=1")
	has(t, "uplink", up, "EgressUntagged=1")
	r.VLANs = "10, 20-30,1"
	up = fileOf(n.plan(r), "/etc/systemd/network/05-virticle-br0-eth0.network")
	has(t, "uplink", up, "VLAN=10\n")    // id 1 is the untagged default and is left out
	has(t, "uplink", up, "VLAN=20-30\n") // networkd takes one id or range per VLAN= line
	if strings.Contains(up, "VLAN=10,") {
		t.Errorf("a list in one VLAN= line is rejected by networkd:\n%s", up)
	}
	r.VLANs = "1-4094"
	up = fileOf(n.plan(r), "/etc/systemd/network/05-virticle-br0-eth0.network")
	has(t, "uplink", up, "VLAN=2-4094")
	r.VLANs = "10,20-30"
	up = fileOf(n.plan(r), "/etc/systemd/network/05-virticle-br0-eth0.network")
	has(t, "uplink", up, "VLAN=20-30\n")
}

func TestWithIAID(t *testing.T) {
	in := "[Network]\nDHCP=ipv4\n\n[DHCP]\nRouteMetric=100\n"
	out := withIAID(in, "0x5de26c15")
	has(t, "iaid", out, "[DHCP]\nRouteMetric=100\nIAID=0x5de26c15")
	out = withIAID("[Network]\nDHCP=ipv4\n", "0x1")
	has(t, "new section", out, "[DHCPv4]\nIAID=0x1")
	keep := "[Network]\nDHCP=ipv4\n\n[DHCPv4]\nClientIdentifier=mac\n"
	if withIAID(keep, "0x1") != keep {
		t.Error("an explicit client identifier must be left alone")
	}
	if withIAID(in, "") != in {
		t.Error("no IAID, no change")
	}
}

func TestNetApplyArmsBeforeApplyAndConfirmCancels(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	f := n.f
	id := n.apply(createReq())
	if f.armed[id] != 90 || len(f.detach) != 1 || f.detach[0] != id {
		t.Fatalf("armed %v detached %v", f.armed, f.detach)
	}
	// the rollback is armed before the first command that touches the network, and nothing is applied yet
	if f.index("arm "+id) < 0 || f.index("run networkctl reload") != -1 || n.exists("etc/systemd/network/05-virticle-br0.netdev") {
		t.Fatalf("the change started before the detached run: %v", f.events)
	}
	if got := n.state(id); got != nodeproto.NetArmed {
		t.Errorf("state %s", got)
	}
	// the saved state exists and is root only
	for _, p := range []string{"meta.json", "runtime.json"} {
		fi, err := os.Stat(n.abs("var/lib/virticle-node/net/changes/" + id + "/" + p))
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: %v %v", p, err, fi)
		}
	}
	n.ok("", "net-run", id)
	if got := n.state(id); got != nodeproto.NetApplied {
		t.Fatalf("state after run: %s", got)
	}
	if !strings.Contains(readFile(n.abs("etc/systemd/network/05-virticle-br0.network")), "DHCP=ipv4") {
		t.Error("bridge network not written")
	}
	if f.index("arm "+id) > f.index("run networkctl reload") {
		t.Error("armed after the apply")
	}
	st := n.status()
	if st.Pending == nil || st.Pending.ID != id || st.Pending.SecondsLeft <= 0 || st.Pending.SecondsLeft > 90 {
		t.Errorf("pending: %+v", st.Pending)
	}
	// a second change is refused while one is pending
	n.refusedPlan("pending", nodeproto.NetRequest{Action: nodeproto.NetCreate, Bridge: "br1", LibvirtVersion: libvirt113}, "still pending")
	n.ok("", "net-confirm", id)
	if f.index("cancel "+id) < 0 {
		t.Error("confirm did not cancel the rollback timer")
	}
	if got := n.state(id); got != nodeproto.NetConfirmed || n.status().Pending != nil {
		t.Errorf("state %s", got)
	}
	// the bridge is now managed, with its uplink
	var info nodeproto.NetInfo
	jsonInto(t, n.ok("", "net-info"), &info)
	if len(info.Bridges) != 1 || !info.Bridges[0].Managed || info.Bridges[0].Uplink != "eth0" || !info.Bridges[0].MovedAddresses {
		t.Errorf("info: %+v", info.Bridges)
	}
	// confirming again changes nothing; rolling back a confirmed change is refused
	n.ok("", "net-confirm", id)
	n.denied("rollback of a confirmed change", "", "net-rollback", id)
}

func TestNetFailingApplyRestores(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	f := n.f
	f.fail["networkctl reconfigure br0"] = os.ErrInvalid
	id := n.apply(createReq())
	n.ok("", "net-run", id)
	if got := n.state(id); got != nodeproto.NetRolledBack {
		t.Fatalf("state %s", got)
	}
	st := n.status().Recent[0]
	if st.Reason != "apply failed" || !strings.Contains(st.Message, "networkctl reconfigure") {
		t.Errorf("status: %+v", st)
	}
	for _, p := range []string{"05-virticle-br0.netdev", "05-virticle-br0.network", "05-virticle-br0-eth0.network"} {
		if n.exists("etc/systemd/network/" + p) {
			t.Errorf("%s is still there after the rollback", p)
		}
	}
	for _, w := range []string{"ip link del br0", "networkctl reconfigure eth0"} {
		if !f.ran(w) {
			t.Errorf("the restore did not run %q: %v", w, f.events)
		}
	}
	// the rollback cancelled the timer, and nothing is pending any more
	if f.index("cancel "+id) < 0 || n.status().Pending != nil {
		t.Error("timer not cancelled or change still pending")
	}
	n.denied("confirming a rolled back change", "", "net-confirm", id)
}

func TestNetUnconfirmedChangeRollsBackByTimer(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	f := n.f
	orig := readFile(n.abs("etc/systemd/network/10-netplan-eth0.network"))
	id := n.apply(createReq())
	n.ok("", "net-run", id)
	if f.link("eth0").master != "br0" || len(f.link("eth0").addrs) != 0 {
		t.Fatalf("eth0 after apply: %+v", f.link("eth0"))
	}
	// nobody confirms: the clock passes the deadline and the timer fires
	f.now = f.now.Add(91 * 1e9)
	if st := n.status(); st.Pending == nil || st.Pending.SecondsLeft != 0 {
		t.Errorf("pending after the deadline: %+v", st.Pending)
	}
	n.ok("", "net-rollback", id, "timer")
	st := n.status()
	if st.Pending != nil || st.Recent[0].State != nodeproto.NetRolledBack || st.Recent[0].Reason != "timer" {
		t.Fatalf("status: %+v", st)
	}
	if f.link("br0") != nil || f.link("eth0").master != "" || len(f.link("eth0").addrs) == 0 {
		t.Errorf("runtime after rollback: %+v", f.links)
	}
	if readFile(n.abs("etc/systemd/network/10-netplan-eth0.network")) != orig || n.exists("etc/systemd/network/05-virticle-br0.netdev") {
		t.Error("files not restored")
	}
	// a second run does nothing and says so
	before := len(f.events)
	if out := n.ok("", "net-rollback", id, "timer"); !strings.Contains(out, "already") {
		t.Errorf("output %q", out)
	}
	for _, e := range f.events[before:] {
		if strings.HasPrefix(e, "run networkctl") || strings.HasPrefix(e, "run ip link") {
			t.Errorf("a second rollback ran %s", e)
		}
	}
	// a late confirmation is refused, and a new change may start
	n.denied("late confirm", "", "net-confirm", id)
	n.plan(createReq())
}

func TestNetRollbackByHandWithoutID(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	id := n.apply(createReq())
	n.ok("", "net-run", id)
	n.ok("", "net-rollback")
	st := n.status().Recent[0]
	if st.State != nodeproto.NetRolledBack || st.Reason != "manual" {
		t.Errorf("status %+v", st)
	}
	n.denied("nothing pending", "", "net-rollback")
}

func TestNetCheckFailureRollsBackAtOnce(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	// the bridge never gets its address
	n.f.onRun = nil
	id := n.apply(createReq())
	n.f.links = append(n.f.links, fLink{name: "br0", kind: "bridge", mac: "52:54:00:aa:bb:cc"})
	n.ok("", "net-run", id)
	st := n.status().Recent[0]
	if st.State != nodeproto.NetRolledBack || st.Reason != "check failed" || !strings.Contains(st.Message, "did not get 192.0.2.10/24") {
		t.Errorf("status %+v", st)
	}
}

func TestNetRefusals(t *testing.T) {
	base := createReq()
	mod := func(f func(*nodeproto.NetRequest)) nodeproto.NetRequest { r := base; f(&r); return r }
	t.Run("bond member", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.f.link("eth0").master, n.f.link("eth0").slaveKind = "bond0", "bond"
		n.refusedPlan("bond member", base, "member of the bond")
	})
	t.Run("already a port", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.f.link("eth0").master = "br9"
		n.refusedPlan("port", base, "already a port of br9")
	})
	t.Run("not physical", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.f.link("eth0").kind = "veth"
		n.refusedPlan("veth", base, "not a physical")
		n.f.link("eth0").kind = ""
		os.Remove(n.abs("sys/class/net/eth0/device"))
		n.refusedPlan("no device", base, "not a physical")
	})
	t.Run("wireless", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		os.MkdirAll(n.abs("sys/class/net/eth0/wireless"), 0o755)
		n.refusedPlan("wireless", base, "wireless")
	})
	t.Run("libvirt interface", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.refusedPlan("virbr", mod(func(r *nodeproto.NetRequest) { r.Uplink = "virbr0" }), "managed by libvirt")
		n.refusedPlan("bridge named virbr", mod(func(r *nodeproto.NetRequest) { r.Bridge = "virbr7" }), "bridge name")
	})
	t.Run("name", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		for _, b := range []string{"", "Br0", "0br", "a b", "a;b", "averyveryverylongname"} {
			n.refusedPlan("name "+b, mod(func(r *nodeproto.NetRequest) { r.Bridge = b }), "bridge name")
		}
		n.refusedPlan("exists", mod(func(r *nodeproto.NetRequest) { r.Bridge = "eth1" }), "already has an interface")
	})
	t.Run("management path lost", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.refusedPlan("move off", mod(func(r *nodeproto.NetRequest) { r.MoveAddresses = false }), "carries the controller's connection")
		n.refusedPlan("unknown path", mod(func(r *nodeproto.NetRequest) { r.MoveAddresses, r.ControllerAddrs = false, nil }), "not known which interface")
		// the controller reaches the node over eth1: eth0 may lose its address
		n.f.addAddr("eth1", "198.51.100.7/24", false)
		p := n.plan(mod(func(r *nodeproto.NetRequest) { r.MoveAddresses, r.ControllerAddrs = false, []string{"198.51.100.7"} }))
		if p.ManagementAffected || len(p.Warnings) == 0 {
			t.Errorf("plan: %+v", p)
		}
	})
	t.Run("libvirt too old", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.refusedPlan("old", mod(func(r *nodeproto.NetRequest) { r.VLANAware, r.LibvirtVersion = true, 10009000 }), "libvirt 11.0.0")
		n.refusedPlan("unknown", mod(func(r *nodeproto.NetRequest) { r.VLANAware, r.LibvirtVersion = true, 0 }), "libvirt 11.0.0")
		n.plan(mod(func(r *nodeproto.NetRequest) { r.LibvirtVersion = 10009000 })) // without VLANs the version does not matter
	})
	t.Run("vlan options", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.refusedPlan("ids without vlan awareness", mod(func(r *nodeproto.NetRequest) { r.VLANs = "10" }), "VLAN-aware")
		for _, v := range []string{"0", "4095", "20-10", "a", "5,,6"} {
			n.refusedPlan("vlans "+v, mod(func(r *nodeproto.NetRequest) { r.VLANAware, r.VLANs = true, v }), "VLAN id")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.refusedPlan("short", mod(func(r *nodeproto.NetRequest) { r.TimeoutSeconds = 5 }), "between 30 and 600")
		n.refusedPlan("long", mod(func(r *nodeproto.NetRequest) { r.TimeoutSeconds = 601 }), "between 30 and 600")
	})
	t.Run("stack", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.f.active["systemd-networkd"] = false
		os.MkdirAll(n.abs("etc/network"), 0o755)
		n.put("etc/network/interfaces", "auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet dhcp\n")
		n.refusedPlan("ifupdown", base, "ifupdown")
		var info nodeproto.NetInfo
		jsonInto(t, n.ok("", "net-info"), &info)
		if info.Stack != "ifupdown" || info.Supported {
			t.Errorf("info %+v", info)
		}
		os.Remove(n.abs("etc/network/interfaces"))
		n.refusedPlan("nothing", base, "not supported")
	})
	t.Run("unmanaged bridge", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.f.links = append(n.f.links, fLink{name: "brx", kind: "bridge", mtu: 1500})
		for _, a := range []string{nodeproto.NetDelete, nodeproto.NetSetVLAN} {
			n.refusedPlan("by hand "+a, nodeproto.NetRequest{Action: a, Bridge: "brx", VLANAware: true, LibvirtVersion: libvirt113}, "not created by this feature")
		}
		var info nodeproto.NetInfo
		jsonInto(t, n.ok("", "net-info"), &info)
		if len(info.Bridges) != 1 || info.Bridges[0].Managed {
			t.Errorf("info %+v", info.Bridges)
		}
	})
	t.Run("networkd file sorts first", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.put("etc/systemd/network/00-first.network", "[Match]\nName=eth0\n\n[Network]\nDHCP=yes\n")
		n.refusedPlan("order", base, "sorts before")
	})
	t.Run("unknown match key", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		n.put("etc/systemd/network/10-netplan-eth0.network", "[Match]\nDriver=virtio_net\n\n[Network]\nDHCP=yes\n")
		n.refusedPlan("match", base, "cannot be evaluated")
	})
	t.Run("addresses nobody configures", func(t *testing.T) {
		n := newNetNode(t, dhcpNetwork, true)
		os.Remove(n.abs("etc/systemd/network/10-netplan-eth0.network"))
		n.refusedPlan("by hand", base, "no networkd file")
	})
}

func TestNetDeleteAndSetVLANs(t *testing.T) {
	n := newNetNode(t, staticNetwork, false)
	f := n.f
	r := createReq()
	r.VLANAware = true
	r.VLANs = "10-20"
	id := n.apply(r)
	n.ok("", "net-run", id)
	n.ok("", "net-confirm", id)

	// VMs attached: refused
	os.MkdirAll(n.abs("sys/class/net/br0/brif/eth0"), 0o755)
	os.MkdirAll(n.abs("sys/class/net/br0/brif/vnet3"), 0o755)
	del := nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0", LibvirtVersion: libvirt113}
	n.refusedPlan("vms", del, "virtual machines are attached")
	os.RemoveAll(n.abs("sys/class/net/br0/brif/vnet3"))
	os.MkdirAll(n.abs("sys/class/net/br0/brif/dummy9"), 0o755)
	n.refusedPlan("foreign port", del, "another port")
	os.RemoveAll(n.abs("sys/class/net/br0/brif/dummy9"))

	// change the allowed ids
	sv := nodeproto.NetRequest{Action: nodeproto.NetSetVLAN, Bridge: "br0", VLANAware: true, VLANs: "10-20", LibvirtVersion: libvirt113}
	n.refusedPlan("same", sv, "already like this")
	sv.VLANs = "15-30"
	p := n.plan(sv)
	cmds := strings.Join(p.Commands, "\n")
	has(t, "set-vlans", cmds, "bridge vlan add dev eth0 vid 21-30")
	has(t, "set-vlans", cmds, "bridge vlan del dev eth0 vid 10-14")
	has(t, "set-vlans", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth0.network"), "VLAN=15-30\n")
	id2 := n.apply(sv)
	n.ok("", "net-run", id2)
	n.ok("", "net-confirm", id2)
	var info nodeproto.NetInfo
	jsonInto(t, n.ok("", "net-info"), &info)
	if info.Bridges[0].VLANs != "15-30" {
		t.Errorf("info %+v", info.Bridges)
	}
	// turn filtering off
	off := nodeproto.NetRequest{Action: nodeproto.NetSetVLAN, Bridge: "br0", LibvirtVersion: libvirt113}
	cmds = strings.Join(n.plan(off).Commands, "\n")
	has(t, "off", cmds, "ip link set br0 type bridge vlan_filtering 0")

	// delete: the uplink gets its configuration back
	p = n.plan(del)
	has(t, "delete", strings.Join(p.Summary, "\n"), "192.0.2.10/24")
	id3 := n.apply(del)
	n.ok("", "net-run", id3)
	if f.link("br0") != nil || f.link("eth0").master != "" || len(f.link("eth0").addrs) == 0 {
		t.Errorf("runtime: %+v", f.links)
	}
	if n.exists("etc/systemd/network/05-virticle-br0.netdev") || n.exists("etc/systemd/network/05-virticle-br0-eth0.network") {
		t.Error("files remain")
	}
	n.ok("", "net-confirm", id3)
	jsonInto(t, n.ok("", "net-info"), &info)
	if len(info.Bridges) != 0 {
		t.Errorf("info %+v", info.Bridges)
	}
	if n.exists("var/lib/virticle-node/net/managed/br0.json") {
		t.Error("marker remains")
	}
}

func TestNetNoUplinkBridge(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	r := nodeproto.NetRequest{Action: nodeproto.NetCreate, Bridge: "isol0", LibvirtVersion: libvirt113}
	p := n.plan(r)
	if p.AddressMove != nil || p.ManagementAffected {
		t.Errorf("plan %+v", p)
	}
	has(t, "network", fileOf(p, "/etc/systemd/network/05-virticle-isol0.network"), "ConfigureWithoutCarrier=yes")
	n.refusedPlan("move without uplink", nodeproto.NetRequest{Action: nodeproto.NetCreate, Bridge: "isol0", MoveAddresses: true}, "uplink")
}

func TestNetOnlyOurFilesAreTouched(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	id := n.apply(createReq())
	n.ok("", "net-run", id)
	if got := readFile(n.abs("etc/systemd/network/10-netplan-eth0.network")); got != dhcpNetwork {
		t.Error("the administrator's file changed")
	}
	for _, e := range n.f.events {
		for _, bad := range []string{"iptables", "nft", "firewall", "systemctl restart", "netplan apply"} {
			if strings.Contains(e, bad) {
				t.Errorf("unexpected command %s", e)
			}
		}
	}
	// targeted reload only
	if !n.f.ran("networkctl reload") || n.f.ran("systemctl") && n.f.index("run systemctl restart") >= 0 {
		t.Error("the network service must be reloaded, not restarted")
	}
}

func TestSystemdTimerCommands(t *testing.T) {
	n := newNode(t)
	var calls []string
	n.cfg.Net = &NetHooks{Run: func(argv []string) (string, error) { calls = append(calls, strings.Join(argv, " ")); return "", nil }}
	n.cfg.Self = "/usr/local/libexec/virticle-node"
	c := &Ctx{Cfg: n.cfg}
	tm := c.netTimer()
	_ = tm.Arm("20261113-100000-abcd", 90)
	_ = tm.Detach("20261113-100000-abcd")
	_ = tm.Cancel("20261113-100000-abcd")
	joined := strings.Join(calls, "\n")
	has(t, "arm", joined, "systemd-run --quiet --collect --unit=virticle-net-rollback-20261113-100000-abcd")
	has(t, "arm", joined, "--on-active=90s")
	has(t, "arm", joined, "/usr/local/libexec/virticle-node net-rollback 20261113-100000-abcd timer")
	has(t, "detach", joined, "--unit=virticle-net-apply-20261113-100000-abcd")
	has(t, "detach", joined, "/usr/local/libexec/virticle-node net-run 20261113-100000-abcd")
	has(t, "cancel", joined, "systemctl stop virticle-net-rollback-20261113-100000-abcd.timer")
}

func TestNetIDsAreChecked(t *testing.T) {
	n := newNetNode(t, dhcpNetwork, true)
	for _, id := range []string{"../../etc", "x", "20261113-100000-ABCD", "20261113-100000-abcd/../x"} {
		n.refused("id "+id, "", "net-confirm", id)
		n.refused("id "+id, "", "net-run", id)
	}
	n.denied("unknown", "", "net-confirm", "20261113-100000-abcd")
}

func TestVLANParsing(t *testing.T) {
	good := map[string]string{"": "2-4094", "10": "10", "20-30, 10": "10,20-30", "1-5,3-8": "1-8", "4094": "4094", "5,6,7": "5-7"}
	for in, want := range good {
		if got, err := nodeproto.ParseVLANs(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if !nodeproto.VLANAllowed("10,20-30", 25) || nodeproto.VLANAllowed("10,20-30", 15) || !nodeproto.VLANAllowed("10", 1) {
		t.Error("VLANAllowed")
	}
}

func TestNetworkdMatch(t *testing.T) {
	l := netLink{Name: "eth0", MAC: "52:54:00:aa:bb:cc"}
	cases := []struct {
		ini  string
		want bool
		err  bool
	}{
		{"[Match]\nName=eth0\n", true, false},
		{"[Match]\nName=en*\n", false, false},
		{"[Match]\nName=eth* !eth1\n", true, false},
		{"[Match]\nName=!eth0\n", false, false},
		{"[Match]\nMACAddress=52:54:00:AA:BB:CC\nName=eth0\n", true, false},
		{"[Match]\nMACAddress=52:54:00:00:00:01\n", false, false},
		{"[Network]\nDHCP=yes\n", true, false},
		{"[Match]\nType=ether\n", false, true},
		{"[Match]\nDriver=virtio_net\n", false, true},
	}
	for _, c := range cases {
		got, err := matchNetwork(parseINI(c.ini), l)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%q: %v %v", c.ini, got, err)
		}
	}
}

func TestLeaseIAID(t *testing.T) {
	dir := t.TempDir()
	lease := filepath.Join(dir, "run/systemd/netif/leases")
	if err := os.MkdirAll(lease, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two links with a lease: the one whose ADDRESS the uplink has is the right one.
	os.WriteFile(filepath.Join(lease, "2"), []byte("ADDRESS=192.168.150.15\nCLIENTID=ff56504d9800020000ab11f6edaf3a7574a3aa\n"), 0o644)
	os.WriteFile(filepath.Join(lease, "5"), []byte("ADDRESS=192.168.150.141\nCLIENTID=ff5de26c1500020000ab11f6edaf3a7574a3aa\n"), 0o644)
	c := &Ctx{Cfg: &Config{Prefix: dir}}
	l := netLink{Name: "eth1", Addrs: []netAddr{{Family: "inet", Local: "192.168.150.141", Prefix: 24, Scope: "global"}}}
	if got := c.leaseIAID(l); got != "0x5de26c15" {
		t.Errorf("IAID = %q", got)
	}
	l.Addrs[0].Local = "10.0.0.1"
	if got := c.leaseIAID(l); got != "" {
		t.Errorf("no lease, IAID = %q", got)
	}
	// A MAC based identifier (type 01) has no IAID.
	os.WriteFile(filepath.Join(lease, "5"), []byte("ADDRESS=192.168.150.141\nCLIENTID=01525400977a1b\n"), 0o644)
	l.Addrs[0].Local = "192.168.150.141"
	if got := c.leaseIAID(l); got != "" {
		t.Errorf("mac identifier, IAID = %q", got)
	}
}

func TestHostPathIsNeverRelative(t *testing.T) {
	c := &Ctx{Cfg: &Config{}}
	if got := c.hostPath("etc/x"); got != "/etc/x" {
		t.Errorf("hostPath = %q", got)
	}
	if got := c.netDir(); got != "/var/lib/virticle-node/net" {
		t.Errorf("netDir = %q", got)
	}
	if got := c.poolRunDir(); got != "/run/virticle-node" {
		t.Errorf("poolRunDir = %q", got)
	}
}
