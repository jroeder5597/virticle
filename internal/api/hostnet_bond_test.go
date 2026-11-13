package api

import (
	"testing"

	"virticle/internal/nodeproto"
)

const bondProcSample = `Ethernet Channel Bonding Driver: v6.12.0

Bonding Mode: fault-tolerance (active-backup)
Currently Active Slave: eth1
MII Status: up

Slave Interface: eth0
MII Status: down
Speed: 1000 Mbps
Link Failure Count: 1

Slave Interface: eth1
MII Status: up
Speed: 1000 Mbps
Link Failure Count: 0
`

func TestParseHostNICsBondAndVLAN(t *testing.T) {
	out := `[{"ifname":"vswbond0","operstate":"UP","mtu":1500,"master":"vsw0","linkinfo":{"info_kind":"bond"}},{"ifname":"vsw0","operstate":"UP","mtu":9000,"linkinfo":{"info_kind":"bridge"}},{"ifname":"vsw0.30","link":"vsw0","operstate":"UP","mtu":9000,"linkinfo":{"info_kind":"vlan","info_data":{"protocol":"802.1Q","id":30}},"addr_info":[{"local":"10.30.0.5","prefixlen":24}]},{"ifname":"eth0","operstate":"DOWN","master":"vswbond0"},{"ifname":"eth1","operstate":"UP","master":"vswbond0"}]
SPEED eth0 1000
BOND vswbond0
` + bondProcSample + `ENDBOND
`
	nics, err := parseHostNICs(out)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]hostNIC{}
	for _, n := range nics {
		by[n.Name] = n
	}
	if b := by["vswbond0"].Bond; b == nil || b.Mode != "active-backup" || b.ActiveMember != "eth1" || len(b.Members) != 2 {
		t.Errorf("bond: %+v", by["vswbond0"].Bond)
	}
	if v := by["vsw0.30"]; v.Parent != "vsw0" || v.VLANID != 30 || v.Type != "vlan" {
		t.Errorf("vlan: %+v", v)
	}
	if by["eth0"].Bond != nil || by["vsw0"].Bond != nil {
		t.Error("a bond status on a device that is no bond")
	}

	// the topology shows the bond as one uplink with its cards, and the VLAN address of the node on the switch
	sw, _ := composeTopology(topoInput{NICs: nics, Mgmt: []string{"10.30.0.5"}})
	if len(sw) != 1 || sw[0].Name != "vsw0" || sw[0].MTU != 9000 {
		t.Fatalf("switches: %+v", sw)
	}
	up := sw[0].Uplinks
	if len(up) != 1 || up[0].Type != "bond" || up[0].Mode != "failover" || len(up[0].Members) != 2 || len(up[0].MemberStates) != 2 {
		t.Fatalf("uplinks: %+v", up)
	}
	if m := up[0].MemberStates; m[0].Name != "eth0" || m[0].State != "down" || m[0].Active || m[1].Name != "eth1" || !m[1].Active || m[1].State != "up" {
		t.Errorf("members: %+v", m)
	}
	if up[0].LACPAnswered != nil {
		t.Error("failover has no LACP state")
	}
	var found bool
	for _, a := range sw[0].HostAddresses {
		if a.VLAN == 30 && a.Iface == "vsw0.30" && a.Address == "10.30.0.5/24" && a.Management {
			found = true
		}
	}
	if !found {
		t.Errorf("host addresses: %+v", sw[0].HostAddresses)
	}
}

func TestTopologyLACPWithoutPartner(t *testing.T) {
	bond := nodeproto.ParseBonding("bond0", "Bonding Mode: IEEE 802.3ad Dynamic link aggregation\nActive Aggregator Info:\n\tPartner Mac Address: 00:00:00:00:00:00\nSlave Interface: eth0\nMII Status: up\n")
	nics := []hostNIC{{Name: "br0", Type: "bridge", Addresses: []string{}}, {Name: "bond0", Type: "bond", Master: "br0", Bond: &bond, Addresses: []string{}},
		{Name: "eth0", Type: "physical", Master: "bond0", Addresses: []string{}}}
	sw, _ := composeTopology(topoInput{NICs: nics})
	u := sw[0].Uplinks[0]
	if u.Mode != "lacp" || u.LACPAnswered == nil || *u.LACPAnswered {
		t.Errorf("uplink: %+v", u)
	}
}

func TestCheckHostnetRequestNewActions(t *testing.T) {
	for _, a := range []string{nodeproto.NetSetUplinks, nodeproto.NetSetMTU, nodeproto.NetAddVLANAddr, nodeproto.NetDelVLANAddr} {
		if err := checkHostnetRequest(nodeproto.NetRequest{Action: a, Bridge: "vsw0"}); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	if err := checkHostnetRequest(nodeproto.NetRequest{Action: "set-bond", Bridge: "vsw0"}); err == nil {
		t.Error("an unknown action was accepted")
	}
}
