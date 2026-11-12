package api

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNetworkBuild(t *testing.T) {
	existing := []existingNet{
		{Name: "default", Mode: "nat", Bridge: "virbr0", Subnet: netip.MustParsePrefix("192.168.122.1/24")},
		{Name: "br", Mode: "bridge", Bridge: "hostbr"},
	}
	cases := []struct {
		name string
		req  networkRequest
		want string // substring of the XML, or of the error when bad is set
		bad  bool
	}{
		{"nat default range", networkRequest{Name: "n1", Mode: "nat", Address: "10.77.1.1/24", DHCP: true}, `<range start='10.77.1.2' end='10.77.1.254'/>`, false},
		{"network address becomes first host", networkRequest{Name: "n1", Mode: "nat", Address: "10.77.1.0/24"}, `address='10.77.1.1' netmask='255.255.255.0'`, false},
		{"isolated without ip", networkRequest{Name: "n1", Mode: "isolated"}, `<name>n1</name>`, false},
		{"named bridge", networkRequest{Name: "n1", Mode: "isolated", BridgeName: "wfbr1"}, `<bridge name='wfbr1'`, false},
		{"explicit range", networkRequest{Name: "n1", Mode: "routed", Address: "10.77.3.1/24", DHCP: true, DHCPStart: "10.77.3.50", DHCPEnd: "10.77.3.60"}, `forward mode='route'`, false},
		{"bridge ovs vlan", networkRequest{Name: "n1", Mode: "bridge", BridgeName: "ovsbr0", OVS: true, VLAN: 20}, `<vlan><tag id='20'/></vlan>`, false},
		{"overlap", networkRequest{Name: "n1", Mode: "nat", Address: "192.168.122.5/25"}, "overlaps", true},
		{"range outside", networkRequest{Name: "n1", Mode: "nat", Address: "10.77.1.1/24", DHCP: true, DHCPStart: "10.77.2.5", DHCPEnd: "10.77.2.9"}, "not inside", true},
		{"range reversed", networkRequest{Name: "n1", Mode: "nat", Address: "10.77.1.1/24", DHCP: true, DHCPStart: "10.77.1.9", DHCPEnd: "10.77.1.5"}, "after its end", true},
		{"range with gateway", networkRequest{Name: "n1", Mode: "nat", Address: "10.77.1.1/24", DHCP: true, DHCPStart: "10.77.1.1", DHCPEnd: "10.77.1.9"}, "gateway", true},
		{"bad cidr", networkRequest{Name: "n1", Mode: "nat", Address: "10.77.1.1/33"}, "IPv4 address with prefix", true},
		{"ipv6", networkRequest{Name: "n1", Mode: "nat", Address: "fd00::1/64"}, "IPv4", true},
		{"nat without address takes next free /24", networkRequest{Name: "n1", Mode: "nat", DHCP: true}, `address='10.77.1.1' netmask='255.255.255.0'`, false},
		{"dup name", networkRequest{Name: "default", Mode: "isolated"}, "already exists", true},
		{"bad name", networkRequest{Name: "a b", Mode: "isolated"}, "name must", true},
		{"vlan on linux bridge", networkRequest{Name: "n1", Mode: "bridge", BridgeName: "hb", VLAN: 5}, "Open vSwitch", true},
		{"vlan range", networkRequest{Name: "n1", Mode: "bridge", BridgeName: "hb", OVS: true, VLAN: 5000}, "4094", true},
		{"libvirt's own bridge", networkRequest{Name: "n1", Mode: "bridge", BridgeName: "virbr0"}, "created by libvirt network default", true},
		{"unknown mode", networkRequest{Name: "n1", Mode: "weird"}, "mode must", true},
	}
	for _, c := range cases {
		got, err := c.req.build(existing)
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
			} else if _, ok := err.(badRequest); !ok {
				t.Errorf("%s: error is not a badRequest", c.name)
			}
			continue
		}
		if err != nil || !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q in XML, got err=%v\n%s", c.name, c.want, err, got)
		}
	}
}

func TestNetXMLInfo(t *testing.T) {
	x, err := parseNetXML(`<network><name>d</name><forward mode='nat'/><bridge name='virbr0'/>
<ip address='192.168.122.1' netmask='255.255.255.0'><dhcp><range start='192.168.122.2' end='192.168.122.254'/>
<host mac='52:54:00:00:00:01' ip='192.168.122.10' name='a'/></dhcp></ip></network>`)
	if err != nil {
		t.Fatal(err)
	}
	i := x.info()
	if i.Address != "192.168.122.1/24" || !i.DHCP || len(i.Hosts) != 1 || i.Forward != "nat" || i.DHCPEnd != "192.168.122.254" {
		t.Errorf("unexpected info %+v", i)
	}
	if _, idx, ok := x.dhcpIP(); !ok || idx != 0 {
		t.Error("dhcpIP not found")
	}
}

func TestNextFreeSubnet(t *testing.T) {
	p := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	got, ok := nextFreeSubnet([]existingNet{{Subnet: p("192.168.122.1/24")}, {}})
	if !ok || got != p("10.77.1.1/24") {
		t.Fatalf("empty node: %v %v", got, ok)
	}
	got, _ = nextFreeSubnet([]existingNet{{Subnet: p("10.77.1.1/24")}, {Subnet: p("10.77.2.1/25")}, {Subnet: p("10.77.4.1/24")}})
	if got != p("10.77.3.1/24") {
		t.Fatalf("with gaps: %v", got)
	}
	got, _ = nextFreeSubnet([]existingNet{{Subnet: p("10.77.0.1/16")}})
	if got.Addr().String() == "10.77.1.1" {
		t.Fatalf("a /16 covering the range must be skipped, got %v", got)
	}
}

func TestDefaultDHCPRange(t *testing.T) {
	for _, c := range []struct {
		addr, start, end string
		ok               bool
	}{
		{"10.77.1.1/24", "10.77.1.2", "10.77.1.254", true},
		{"10.77.1.0/24", "10.77.1.2", "10.77.1.254", true},
		{"10.77.1.254/24", "10.77.1.1", "10.77.1.253", true},
		{"10.77.1.100/24", "10.77.1.1", "10.77.1.254", false},
	} {
		s, e, ok := defaultDHCPRange(netip.MustParsePrefix(c.addr))
		if s.String() != c.start || e.String() != c.end || ok != c.ok {
			t.Errorf("%s: got %s-%s %v", c.addr, s, e, ok)
		}
	}
}
