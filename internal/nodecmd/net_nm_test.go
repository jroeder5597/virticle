package nodecmd

import (
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

const nmUUID = "6c0f3a52-0000-4000-8000-000000000001"

const nmShowKey = "nmcli -t -f connection.id,connection.type,connection.autoconnect,802-3-ethernet.mtu,ipv4,ipv6 connection show uuid " + nmUUID

const nmDHCPProfile = `connection.id:Wired connection 1
connection.type:802-3-ethernet
connection.autoconnect:yes
802-3-ethernet.mtu:auto
ipv4.method:auto
ipv4.addresses:
ipv4.gateway:
ipv4.dns:
ipv4.dhcp-client-id:
ipv4.never-default:no
ipv6.method:auto
ipv6.addr-gen-mode:eui64
`

const nmStaticProfile = `connection.id:Wired connection 1
connection.type:802-3-ethernet
connection.autoconnect:yes
802-3-ethernet.mtu:1500
ipv4.method:manual
ipv4.addresses:192.0.2.10/24
ipv4.gateway:192.0.2.1
ipv4.dns:192.0.2.53, 192.0.2.54
ipv4.dns-search:lab.example
ipv6.method:ignore
`

// newNMNode is a node run by NetworkManager.
func newNMNode(t *testing.T, profile string, dynamic bool, nmVersion string) *netNode {
	n := newNetNode(t, dhcpNetwork, dynamic)
	f := n.f
	f.active["systemd-networkd"] = false
	f.active["NetworkManager"] = true
	n.cfg.Tools["nmcli"] = "/usr/bin/nmcli"
	f.outputs["nmcli --version"] = "nmcli tool, version " + nmVersion + "\n"
	f.outputs["nmcli -t -f NAME connection show"] = "Wired connection 1\nlo\n"
	f.outputs["nmcli -t -g GENERAL.CON-UUID device show eth0"] = nmUUID + "\n"
	f.outputs[nmShowKey] = profile
	f.onRun = func(f *fakeNet, argv []string) {
		key := strings.Join(argv, " ")
		switch {
		case strings.HasPrefix(key, "nmcli --wait 0 connection up id virticle-br0"):
			if f.link("br0") == nil {
				f.links = append(f.links, fLink{name: "br0", kind: "bridge", mtu: 1500, mac: "52:54:00:aa:bb:cc"})
			}
			f.link("br0").addrs = append([]fAddr(nil), f.link("eth0").addrs...)
			f.link("eth0").addrs = nil
			f.link("eth0").master = "br0"
		case strings.HasPrefix(key, "nmcli connection delete id virticle-br0"), key == "ip link del br0":
			for i := range f.links {
				if f.links[i].name == "br0" {
					f.links = append(f.links[:i], f.links[i+1:]...)
					break
				}
			}
			f.link("eth0").master = ""
		case strings.HasPrefix(key, "nmcli --wait 0 connection up uuid "+nmUUID):
			f.link("eth0").addrs = []fAddr{{"192.0.2.10/24", dynamic}}
		}
	}
	return n
}

func TestNMPlanDHCP(t *testing.T) {
	n := newNMNode(t, nmDHCPProfile, true, "1.56.0")
	p := n.plan(createReq())
	if p.Stack != "networkmanager" {
		t.Fatalf("stack %s", p.Stack)
	}
	cmds := strings.Join(p.Commands, "\n")
	for _, w := range []string{
		"nmcli connection add type bridge ifname br0 con-name virticle-br0 connection.autoconnect yes bridge.stp no bridge.mac-address 52:54:00:aa:bb:cc",
		"ipv4.method auto",
		"ipv6.addr-gen-mode eui64",
		"nmcli connection add type ethernet ifname eth0 con-name virticle-port-eth0 controller br0 port-type bridge connection.autoconnect yes",
		"nmcli connection modify uuid " + nmUUID + " connection.autoconnect no",
		"nmcli --wait 0 connection up id virticle-br0",
		"nmcli --wait 0 connection up id virticle-port-eth0",
		"until br0 has dhcp4",
	} {
		has(t, "commands", cmds, w)
	}
	hasNot(t, "commands", cmds, "bridge.vlan-filtering")
	hasNot(t, "commands", cmds, "ipv4.addresses") // empty values are not copied
	if p.AddressMove == nil || p.AddressMove.Method != "dhcp" {
		t.Errorf("move %+v", p.AddressMove)
	}
	if n.f.ran("nmcli connection add") || n.f.ran("nmcli connection modify") {
		t.Error("the plan changed NetworkManager")
	}
}

func TestNMPlanStaticAndOldNM(t *testing.T) {
	n := newNMNode(t, nmStaticProfile, false, "1.40.2")
	p := n.plan(createReq())
	cmds := strings.Join(p.Commands, "\n")
	for _, w := range []string{
		"ipv4.addresses 192.0.2.10/24", "ipv4.gateway 192.0.2.1", "ipv4.dns '192.0.2.53, 192.0.2.54'", "ipv4.dns-search lab.example",
		"ipv4.method manual", "802-3-ethernet.mtu 1500",
		"master br0 slave-type bridge", "until br0 has 192.0.2.10/24",
	} {
		has(t, "commands", cmds, w)
	}
	hasNot(t, "commands", cmds, "controller br0")
	if p.AddressMove == nil || p.AddressMove.Method != "static" {
		t.Errorf("move %+v", p.AddressMove)
	}
}

func TestNMPlanVLANAndNoMove(t *testing.T) {
	n := newNMNode(t, nmStaticProfile, false, "1.56.0")
	r := createReq()
	r.VLANAware, r.VLANs = true, "10-20"
	cmds := strings.Join(n.plan(r).Commands, "\n")
	has(t, "commands", cmds, "bridge.vlan-filtering yes bridge.vlan-default-pvid 1")
	has(t, "commands", cmds, "bridge-port.vlans '1 pvid untagged,10-20'")
	// no uplink move, the controller reaches the node over eth1: the bridge is a plain L2 bridge
	n.f.addAddr("eth1", "198.51.100.7/24", false)
	r = createReq()
	r.MoveAddresses, r.ControllerAddrs = false, []string{"198.51.100.7"}
	p := n.plan(r)
	cmds = strings.Join(p.Commands, "\n")
	has(t, "commands", cmds, "ipv4.method disabled ipv6.method disabled")
	hasNot(t, "commands", cmds, "bridge.mac-address")
	if p.AddressMove != nil || len(p.Warnings) == 0 {
		t.Errorf("plan %+v", p)
	}
}

func TestNMRefusals(t *testing.T) {
	n := newNMNode(t, nmDHCPProfile, true, "1.56.0")
	n.f.outputs[nmShowKey] = strings.Replace(nmDHCPProfile, "802-3-ethernet", "wifi", 1)
	n.refusedPlan("not ethernet", createReq(), "not a plain ethernet")
	n.f.outputs[nmShowKey] = nmDHCPProfile
	n.f.outputs["nmcli -t -f NAME connection show"] = "virticle-br0\n"
	n.refusedPlan("name taken", createReq(), "already has a connection named virticle-br0")
	n.f.outputs["nmcli -t -f NAME connection show"] = ""
	n.f.outputs["nmcli -t -g GENERAL.CON-UUID device show eth0"] = "\n"
	n.refusedPlan("no profile", createReq(), "no active connection")
}

func TestNMApplyRollbackAndDelete(t *testing.T) {
	n := newNMNode(t, nmStaticProfile, false, "1.56.0")
	f := n.f
	id := n.apply(createReq())
	if f.index("arm "+id) < 0 || f.ran("nmcli connection add") {
		t.Fatalf("armed late: %v", f.events)
	}
	n.ok("", "net-run", id)
	if got := n.state(id); got != nodeproto.NetApplied {
		t.Fatalf("state %s", got)
	}
	// the unconfirmed change rolls back: profiles deleted, the original profile active again
	n.ok("", "net-rollback", id, "timer")
	for _, w := range []string{
		"nmcli connection delete id virticle-port-eth0", "nmcli connection delete id virticle-br0",
		"nmcli connection modify uuid " + nmUUID + " connection.autoconnect yes",
		"nmcli --wait 0 connection up uuid " + nmUUID,
	} {
		if !f.ran(w) {
			t.Errorf("rollback did not run %q", w)
		}
	}
	if f.link("br0") != nil || len(f.link("eth0").addrs) == 0 {
		t.Errorf("runtime %+v", f.links)
	}
	if n.status().Recent[0].Reason != "timer" {
		t.Errorf("status %+v", n.status().Recent[0])
	}
	// apply again, confirm, then delete the managed bridge
	id2 := n.apply(createReq())
	n.ok("", "net-run", id2)
	n.ok("", "net-confirm", id2)
	del := nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0", LibvirtVersion: libvirt113}
	p := n.plan(del)
	has(t, "delete", strings.Join(p.Commands, "\n"), "nmcli connection delete id virticle-br0")
	id3 := n.apply(del)
	n.ok("", "net-run", id3)
	n.ok("", "net-confirm", id3)
	if f.link("br0") != nil || len(f.link("eth0").addrs) == 0 {
		t.Errorf("runtime %+v", f.links)
	}
}

func TestNMSetVLANs(t *testing.T) {
	n := newNMNode(t, nmStaticProfile, false, "1.56.0")
	r := createReq()
	r.VLANAware, r.VLANs = true, "10-20"
	id := n.apply(r)
	n.ok("", "net-run", id)
	n.ok("", "net-confirm", id)
	sv := nodeproto.NetRequest{Action: nodeproto.NetSetVLAN, Bridge: "br0", VLANAware: true, VLANs: "10-40", LibvirtVersion: libvirt113}
	cmds := strings.Join(n.plan(sv).Commands, "\n")
	has(t, "set-vlans", cmds, "nmcli connection modify id virticle-port-eth0 bridge-port.vlans '1 pvid untagged,10-40'")
	has(t, "set-vlans", cmds, "bridge vlan add dev eth0 vid 21-40")
}

func TestNMVersionAndTerse(t *testing.T) {
	if !nmVersionAtLeast("nmcli tool, version 1.46.0\n", 1, 46) || nmVersionAtLeast("nmcli tool, version 1.44.2", 1, 46) || !nmVersionAtLeast("nmcli tool, version 2.0.0", 1, 46) {
		t.Error("nmVersionAtLeast")
	}
	m := nmTerse("ipv4.dns:a\\:b\nipv4.method:auto\nnocolon\n")
	if m["ipv4.dns"] != "a:b" || m["ipv4.method"] != "auto" || len(m) != 2 {
		t.Errorf("%v", m)
	}
}
