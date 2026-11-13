package api

import "testing"

func findSwitch(t *testing.T, sw []topoSwitch, name string) topoSwitch {
	t.Helper()
	for _, s := range sw {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no switch %q in %+v", name, sw)
	return topoSwitch{}
}

func findGroup(t *testing.T, s topoSwitch, id string) topoPortGroup {
	t.Helper()
	for _, g := range s.PortGroups {
		if g.ID == id {
			return g
		}
	}
	t.Fatalf("no port group %q on %s", id, s.Name)
	return topoPortGroup{}
}

func testInput() topoInput {
	return topoInput{
		NICs: []hostNIC{
			{Name: "eno1", Type: "physical", State: "up", SpeedMbps: 1000, Master: "br0"},
			{Name: "eno2", Type: "physical", State: "down", Addresses: []string{"10.0.0.5/24"}},
			{Name: "eno3", Type: "physical", State: "up"},
			{Name: "br0", Type: "bridge", Kind: "bridge", State: "up", Addresses: []string{"192.0.2.10/24"}},
			{Name: "virbr0", Type: "bridge", Kind: "bridge", State: "up", Addresses: []string{"192.168.122.1/24"}},
			{Name: "vnet0", Type: "tap", Kind: "tun", Master: "virbr0"},
			{Name: "virbr0-nic", Type: "other", Kind: "dummy", Master: "virbr0"},
		},
		Networks: []networkInfo{
			{Name: "default", Active: true, Forward: "nat", Bridge: "virbr0", DHCP: true, DHCPStart: "192.168.122.2", DHCPEnd: "192.168.122.254",
				Leases: []netLease{{MAC: "52:54:00:00:00:01", IP: "192.168.122.50"}}},
			{Name: "lan", Active: true, Forward: "bridge", Bridge: "br0"},
			{Name: "iso", Active: true, Forward: "isolated", Bridge: "virbr5"},
			{Name: "tagged", Active: true, Forward: "bridge", Bridge: "br0", VLAN: 20},
		},
		ForwardDev: map[string]string{"default": "eno1"},
		VMs: []topoVM{
			{Name: "web", UUID: "u1", State: "running", NICs: []topoVMNIC{
				{MAC: "52:54:00:00:00:01", Type: "network", Network: "default"},
				{MAC: "52:54:00:00:00:02", Type: "network", Network: "iso", LinkDown: true},
			}},
			{Name: "db", UUID: "u2", State: "shutoff", NICs: []topoVMNIC{{MAC: "52:54:00:00:00:03", Type: "bridge", Bridge: "br0"}}},
			{Name: "v20", UUID: "u3", State: "running", NICs: []topoVMNIC{{MAC: "52:54:00:00:00:04", Type: "network", Network: "tagged"}}},
		},
		Mgmt: []string{"10.0.0.5"},
	}
}

func TestTopologyBridgeWithUplinkAndDirectVM(t *testing.T) {
	sw, _ := composeTopology(testInput())
	br := findSwitch(t, sw, "br0")
	if br.ID != "bridge:br0" || br.Kind != "linux-bridge" {
		t.Fatalf("br0 = %+v", br)
	}
	if len(br.Uplinks) != 1 || br.Uplinks[0].Name != "eno1" || br.Uplinks[0].SpeedMbps != 1000 {
		t.Fatalf("uplinks = %+v", br.Uplinks)
	}
	if len(br.HostAddresses) != 1 || br.HostAddresses[0].Address != "192.0.2.10/24" {
		t.Fatalf("host port = %+v", br.HostAddresses)
	}
	d := findGroup(t, br, "direct")
	if d.Kind != "direct" || len(d.Interfaces) != 1 || d.Interfaces[0].VM != "db" || d.Interfaces[0].State != "shutoff" {
		t.Fatalf("direct = %+v", d)
	}
	if br.PortGroups[len(br.PortGroups)-1].ID != "direct" {
		t.Fatalf("direct group should come last: %+v", br.PortGroups)
	}
	if br.UplinkNote != "" {
		t.Fatalf("a bridge with an uplink has no note: %q", br.UplinkNote)
	}
}

func TestTopologyNATNetwork(t *testing.T) {
	sw, _ := composeTopology(testInput())
	v := findSwitch(t, sw, "virbr0")
	if v.Kind != "libvirt" || v.UplinkNote != "NAT to eno1" || len(v.Uplinks) != 0 {
		t.Fatalf("virbr0 = %+v", v)
	}
	g := findGroup(t, v, "net:default")
	if g.Mode != "nat" || g.DHCP != "192.168.122.2 to 192.168.122.254" {
		t.Fatalf("group = %+v", g)
	}
	if len(g.Interfaces) != 1 || len(g.Interfaces[0].IPs) != 1 || g.Interfaces[0].IPs[0] != "192.168.122.50" {
		t.Fatalf("interfaces = %+v", g.Interfaces)
	}
}

func TestTopologyNATWithoutDevice(t *testing.T) {
	in := testInput()
	in.ForwardDev = nil
	sw, _ := composeTopology(in)
	if n := findSwitch(t, sw, "virbr0").UplinkNote; n != "NAT to any device" {
		t.Fatalf("note = %q", n)
	}
}

func TestTopologyIsolatedAndLinkDownAndTwoNetworks(t *testing.T) {
	sw, _ := composeTopology(testInput())
	iso := findSwitch(t, sw, "virbr5")
	if iso.UplinkNote != "Isolated (no uplink)" || iso.Kind != "libvirt" || iso.State != "up" {
		t.Fatalf("iso = %+v", iso)
	}
	g := findGroup(t, iso, "net:iso")
	if len(g.Interfaces) != 1 || !g.Interfaces[0].LinkDown || g.Interfaces[0].VM != "web" {
		t.Fatalf("iso interfaces = %+v", g.Interfaces)
	}
	// "web" is on two networks, so it shows up on two switches.
	n := 0
	for _, s := range sw {
		for _, pg := range s.PortGroups {
			for _, i := range pg.Interfaces {
				if i.VM == "web" {
					n++
				}
			}
		}
	}
	if n != 2 {
		t.Fatalf("web appears %d times", n)
	}
}

func TestTopologyVLANTaggedNetwork(t *testing.T) {
	sw, _ := composeTopology(testInput())
	br := findSwitch(t, sw, "br0")
	g := findGroup(t, br, "net:tagged")
	if g.VLAN != 20 || len(g.Interfaces) != 1 || g.Interfaces[0].VM != "v20" {
		t.Fatalf("tagged = %+v", g)
	}
	if br.Kind != "linux-bridge" {
		t.Fatalf("a bridge-mode network must not turn a host bridge into a libvirt switch: %s", br.Kind)
	}
}

func TestTopologyUnusedCardsAndManagement(t *testing.T) {
	sw, un := composeTopology(testInput())
	if len(un) != 2 || un[0].Name != "eno2" || un[1].Name != "eno3" {
		t.Fatalf("unused = %+v", un)
	}
	if !un[0].Management || !un[0].Addresses[0].Management || un[0].State != "down" {
		t.Fatalf("eno2 = %+v", un[0])
	}
	if un[1].Management {
		t.Fatal("eno3 carries no management address")
	}
	for _, s := range sw {
		for _, u := range s.Uplinks {
			if u.Management {
				t.Fatalf("uplink %s wrongly marked", u.Name)
			}
		}
	}
}

func TestTopologyManagementOnBridge(t *testing.T) {
	in := testInput()
	in.Mgmt = []string{"192.0.2.10"}
	sw, un := composeTopology(in)
	if !findSwitch(t, sw, "br0").HostAddresses[0].Management {
		t.Fatal("host port of br0 should be the management address")
	}
	for _, u := range un {
		if u.Management {
			t.Fatalf("unused %s wrongly marked", u.Name)
		}
	}
}

func TestTopologyBondAndOVSAndMissingNetwork(t *testing.T) {
	in := topoInput{
		NICs: []hostNIC{
			{Name: "ens1", Type: "physical", State: "up", Master: "bond0"},
			{Name: "ens2", Type: "physical", State: "up", Master: "bond0"},
			{Name: "bond0", Type: "bond", Kind: "bond", State: "up", Master: "ovsbr"},
			{Name: "ovsbr", Type: "other", Kind: "openvswitch", State: "up"},
		},
		VMs: []topoVM{{Name: "x", UUID: "u", State: "running", NICs: []topoVMNIC{
			{MAC: "aa", Type: "network", Network: "gone"},
			{MAC: "bb", Type: "direct", Dev: "eth9"},
		}}},
	}
	sw, un := composeTopology(in)
	o := findSwitch(t, sw, "ovsbr")
	if o.Kind != "openvswitch" || len(o.Uplinks) != 1 || o.Uplinks[0].Name != "bond0" || len(o.Uplinks[0].Members) != 2 {
		t.Fatalf("ovs = %+v", o)
	}
	if len(un) != 0 {
		t.Fatalf("bond members and enslaved bonds are not unused: %+v", un)
	}
	other := findSwitch(t, sw, "Other attachments")
	if other.Kind != "other" || len(other.PortGroups) != 2 || sw[len(sw)-1].Name != "Other attachments" {
		t.Fatalf("other = %+v", other)
	}
}

func TestTopologyBridgeWithNothingOnIt(t *testing.T) {
	sw, _ := composeTopology(topoInput{NICs: []hostNIC{{Name: "br9", Type: "bridge", Kind: "bridge", State: "down"}}})
	s := findSwitch(t, sw, "br9")
	if s.UplinkNote != "No uplink" || s.PortGroups == nil || s.Uplinks == nil || s.HostAddresses == nil {
		t.Fatalf("br9 = %+v", s)
	}
}

// A running VM on a bridge-mode libvirt network is "bridge" in the live XML, with the network name in source.
func TestTopologyRunningVMOnBridgeNetworkBelongsToTheNetwork(t *testing.T) {
	in := testInput()
	in.VMs = append(in.VMs, topoVM{Name: "live", UUID: "u4", State: "running", NICs: []topoVMNIC{{MAC: "52:54:00:00:00:05", Type: "bridge", Network: "tagged", Bridge: "br0"}}})
	sw, _ := composeTopology(in)
	br := findSwitch(t, sw, "br0")
	g := findGroup(t, br, "net:tagged")
	found := false
	for _, i := range g.Interfaces {
		found = found || i.VM == "live"
	}
	if !found {
		t.Fatalf("live VM not under network tagged: %+v", g)
	}
	for _, i := range findGroup(t, br, "direct").Interfaces {
		if i.VM == "live" {
			t.Fatalf("live VM must not be listed as direct")
		}
	}
}
