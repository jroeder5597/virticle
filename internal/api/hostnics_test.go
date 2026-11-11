package api

import (
	"os"
	"testing"
)

// The two fixtures are real output captured from the lab nodes (Debian 13 and
// Rocky Linux); the kinds test is hand-written because the lab has no bond,
// VLAN or tap device at rest.
func TestParseHostNICsDebianAndRocky(t *testing.T) {
	for file, mac := range map[string]string{"ip-addr-debian.txt": "52:54:00:5a:00:01", "ip-addr-rocky.txt": "52:54:00:5a:00:03"} {
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		nics, err := parseHostNICs(string(b))
		if err != nil {
			t.Fatal(file, err)
		}
		if len(nics) != 2 {
			t.Fatalf("%s: want 2 interfaces without lo, got %+v", file, nics)
		}
		e := nics[0]
		if e.Name != "enp1s0" || e.Type != "physical" || e.State != "up" || e.MAC != mac || e.MTU != 1500 || len(e.Addresses) == 0 {
			t.Errorf("%s: unexpected %+v", file, e)
		}
		if b := nics[1]; b.Name != "virbr0" || b.Type != "bridge" || b.State != "down" || b.Addresses[0] != "192.168.122.1/24" {
			t.Errorf("%s: unexpected bridge %+v", file, b)
		}
	}
}

func TestParseHostNICsKinds(t *testing.T) {
	out := `Welcome
[{"ifname":"lo","link_type":"loopback","operstate":"UNKNOWN","mtu":65536},{"ifname":"bond0","operstate":"UP","mtu":9000,"master":"br0","linkinfo":{"info_kind":"bond"}},{"ifname":"bond0.20","operstate":"LOWERLAYERDOWN","linkinfo":{"info_kind":"vlan"}},{"ifname":"vnet3","operstate":"UNKNOWN","master":"virbr0","linkinfo":{"info_kind":"tun"}},{"ifname":"veth1","operstate":"UP","linkinfo":{"info_kind":"veth"}}]
SPEED bond0 10000
SPEED vnet3 -1
SPEED veth1
`
	nics, err := parseHostNICs(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name, typ, state, master string
		speed                    int
	}{
		{"bond0", "bond", "up", "br0", 10000}, {"bond0.20", "vlan", "down", "", 0}, {"vnet3", "tap", "unknown", "virbr0", 0}, {"veth1", "other", "up", "", 0},
	}
	if len(nics) != len(want) {
		t.Fatalf("got %+v", nics)
	}
	for i, w := range want {
		g := nics[i]
		if g.Name != w.name || g.Type != w.typ || g.State != w.state || g.Master != w.master || g.SpeedMbps != w.speed {
			t.Errorf("%d: got %+v want %+v", i, g, w)
		}
	}
}

func TestParseHostNICsGarbage(t *testing.T) {
	if _, err := parseHostNICs("command not found"); err == nil {
		t.Error("expected an error")
	}
}
