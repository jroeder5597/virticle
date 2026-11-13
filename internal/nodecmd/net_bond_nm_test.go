package nodecmd

import (
	"os"
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

const nm2UUID = "6c0f3a52-0000-4000-8000-000000000002"

// newNMBondNode is a NetworkManager node with the extra cards eth2 (a DHCP profile of its own) and eth3.
func newNMBondNode(t *testing.T) *netNode {
	n := newNMNode(t, nmStaticProfile, false, "1.56.0")
	f := n.f
	for _, name := range []string{"eth2", "eth3"} {
		f.links = append(f.links, fLink{name: name, mac: "52:54:00:aa:bb:" + map[string]string{"eth2": "ee", "eth3": "ff"}[name], mtu: 1500})
		os.MkdirAll(n.abs("sys/class/net/"+name+"/device"), 0o755)
	}
	f.addAddr("eth2", "198.51.100.20/24", true)
	f.outputs["nmcli -t -g GENERAL.CON-UUID device show eth2"] = nm2UUID + "\n"
	f.outputs["nmcli -t -f connection.id,connection.type,connection.autoconnect,802-3-ethernet.mtu,ipv4,ipv6 connection show uuid "+nm2UUID] =
		"connection.id:Wired connection 3\nconnection.type:802-3-ethernet\nconnection.autoconnect:yes\n802-3-ethernet.mtu:auto\nipv4.method:auto\n"
	prev := f.onRun
	f.onRun = func(f *fakeNet, argv []string) {
		if prev != nil {
			prev(f, argv)
		}
		key := strings.Join(argv, " ")
		switch {
		case strings.HasPrefix(key, "nmcli --wait 0 connection up id virticle-bond-"):
			name := strings.TrimPrefix(key, "nmcli --wait 0 connection up id virticle-bond-")
			if f.link(name) == nil {
				f.links = append(f.links, fLink{name: name, kind: "bond", mtu: 1500})
			}
		case strings.HasPrefix(key, "nmcli --wait 0 connection up id virticle-vlan-"):
			name := strings.TrimPrefix(key, "nmcli --wait 0 connection up id virticle-vlan-")
			if f.link(name) == nil {
				f.links = append(f.links, fLink{name: name, kind: "vlan", mtu: 1500, addrs: []fAddr{{"10.30.0.5/24", false}}})
			}
		case strings.HasPrefix(key, "nmcli connection delete id virticle-bond-"), strings.HasPrefix(key, "ip link del "):
			name := strings.TrimPrefix(key, "nmcli connection delete id virticle-bond-")
			if strings.HasPrefix(key, "ip link del ") {
				name = argv[3]
			}
			for i := range f.links {
				if f.links[i].name == name && f.links[i].kind != "bridge" {
					f.links = append(f.links[:i], f.links[i+1:]...)
					break
				}
			}
		}
	}
	return n
}

func TestNMBondCreate(t *testing.T) {
	n := newNMBondNode(t)
	r := bondReq("", "eth2", "eth0")
	r.VLANAware, r.VLANs = true, "10-20"
	p := n.plan(r)
	cmds := cmdsOf(p)
	for _, w := range []string{
		"nmcli connection add type bond ifname b-br0 con-name virticle-bond-b-br0 controller br0 port-type bridge connection.autoconnect yes bond.options 'mode=active-backup,miimon=100' 802-3-ethernet.cloned-mac-address 52:54:00:aa:bb:cc",
		"bridge-port.vlans '1 pvid untagged,10-20'",
		"802-3-ethernet.cloned-mac-address 52:54:00:aa:bb:cc",
		"nmcli connection add type ethernet ifname eth0 con-name virticle-port-eth0 controller b-br0 port-type bond connection.autoconnect yes",
		"nmcli connection add type ethernet ifname eth2 con-name virticle-port-eth2 controller b-br0 port-type bond connection.autoconnect yes",
		"nmcli connection modify uuid " + nm2UUID + " connection.autoconnect no", // the extra card's own profile is switched off
		"nmcli --wait 0 connection up id virticle-bond-b-br0",
	} {
		has(t, "commands", cmds, w)
	}
	// the VLAN ids sit on the bond, not on the member profiles
	for _, c := range p.Commands {
		if strings.Contains(c, "virticle-port-") && strings.Contains(c, "bridge-port.vlans") {
			t.Errorf("a member has VLAN ids: %s", c)
		}
	}
	has(t, "summary", strings.Join(p.Summary, "\n"), "eth2 has the address 198.51.100.20/24")
	l := bondReq("lacp", "eth0", "eth3")
	has(t, "lacp", cmdsOf(n.plan(l)), "bond.options 'mode=802.3ad,miimon=100,lacp_rate=fast,xmit_hash_policy=layer2+3'")

	id := n.apply(r)
	n.ok("", "net-run", id)
	n.ok("", "net-rollback", id, "timer")
	for _, w := range []string{"nmcli connection delete id virticle-bond-b-br0", "nmcli connection delete id virticle-port-eth2",
		"nmcli connection modify uuid " + nm2UUID + " connection.autoconnect yes", "nmcli --wait 0 connection up uuid " + nm2UUID} {
		if !n.f.ran(w) {
			t.Errorf("rollback did not run %q", w)
		}
	}
	confirmed(n, r)
}

func TestNMSetUplinks(t *testing.T) {
	n := newNMBondNode(t)
	f := n.f
	confirmed(n, createReq())
	set := func(mode string, cards ...string) nodeproto.NetRequest {
		return nodeproto.NetRequest{Action: nodeproto.NetSetUplinks, Bridge: "br0", Uplinks: cards, BondMode: mode, LibvirtVersion: libvirt113, ControllerAddrs: []string{"192.0.2.10"}}
	}
	// 1 -> 2
	p := n.plan(set("", "eth0", "eth3"))
	cmds := cmdsOf(p)
	idx := func(s string) int { return strings.Index(cmds, s) }
	if !(idx("connection delete id virticle-port-eth0") >= 0 && idx("connection delete id virticle-port-eth0") < idx("type bond ifname b-br0") && idx("type bond ifname b-br0") < idx("connection up id virticle-bond-b-br0")) {
		t.Errorf("order:\n%s", cmds)
	}
	has(t, "port", cmds, "virticle-port-eth0 controller b-br0 port-type bond")
	id := n.apply(set("", "eth0", "eth3"))
	n.ok("", "net-run", id)
	n.ok("", "net-rollback", id, "timer")
	// the way back: bond gone, the card is a bridge port with the VLAN ids again
	for _, w := range []string{"nmcli connection delete id virticle-bond-b-br0", "nmcli connection add type ethernet ifname eth0 con-name virticle-port-eth0 controller br0 port-type bridge"} {
		if !f.ran(w) {
			t.Errorf("rollback did not run %q", w)
		}
	}
	confirmed(n, set("", "eth0", "eth3"))

	// add eth2 (its DHCP address is dropped, its profile is switched off)
	p = n.plan(set("", "eth0", "eth3", "eth2"))
	has(t, "add", cmdsOf(p), "nmcli connection modify uuid "+nm2UUID+" connection.autoconnect no")
	hasNot(t, "add", cmdsOf(p), "connection delete id virticle-bond-b-br0")
	has(t, "summary", strings.Join(p.Summary, "\n"), "eth2 joins br0 as a member of the bond b-br0")
	confirmed(n, set("", "eth0", "eth3", "eth2"))

	// remove eth2: its profile comes back
	p = n.plan(set("", "eth0", "eth3"))
	has(t, "remove", cmdsOf(p), "nmcli connection delete id virticle-port-eth2")
	has(t, "remove", cmdsOf(p), "nmcli --wait 0 connection up uuid "+nm2UUID)
	n.refusedPlan("first card", set("", "eth3", "eth2"), "can only leave when the switch is deleted")
	confirmed(n, set("", "eth0", "eth3"))

	// mode
	p = n.plan(set("lacp", "eth0", "eth3"))
	has(t, "mode", cmdsOf(p), "nmcli connection modify id virticle-bond-b-br0 bond.options 'mode=802.3ad,miimon=100,lacp_rate=fast,xmit_hash_policy=layer2+3'")
	confirmed(n, set("lacp", "eth0", "eth3"))

	// delete takes the bond and the members away
	os.MkdirAll(n.abs("sys/class/net/br0/brif/b-br0"), 0o755)
	del := nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0", LibvirtVersion: libvirt113}
	p = n.plan(del)
	for _, w := range []string{"nmcli connection delete id virticle-port-eth0", "nmcli connection delete id virticle-port-eth3", "nmcli connection delete id virticle-bond-b-br0", "nmcli connection delete id virticle-br0"} {
		has(t, "delete", cmdsOf(p), w)
	}
}

func TestNMVLANAddress(t *testing.T) {
	n := newNMBondNode(t)
	f := n.f
	r := createReq()
	r.VLANAware, r.VLANs = true, "10-40"
	confirmed(n, r)

	p := n.plan(vlanReq("static", "10.30.0.5/24"))
	cmds := cmdsOf(p)
	for _, w := range []string{
		"nmcli connection add type vlan ifname br0.30 con-name virticle-vlan-br0.30 dev br0 id 30 connection.autoconnect yes ipv6.method disabled ipv4.method manual ipv4.addresses 10.30.0.5/24 ipv4.never-default yes",
		"nmcli connection modify id virticle-br0 bridge.vlans 30",
		"bridge vlan add dev br0 vid 30 self",
	} {
		has(t, "commands", cmds, w)
	}
	hasNot(t, "ids already allowed", strings.Join(p.Summary, "\n"), "was not in the list")
	d := vlanReq("dhcp", "")
	d.VLANID = 31
	has(t, "dhcp", cmdsOf(n.plan(d)), "ipv4.method auto ipv4.ignore-auto-dns yes ipv4.never-default yes")
	_ = f
}
