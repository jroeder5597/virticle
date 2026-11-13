package api

import (
	"strings"
	"testing"
)

func dn(id int64, name, group string, nets map[string]driftNet) driftNode {
	return driftNode{ID: id, Name: name, Group: group, Nets: nets, Used: map[string]int{}, Running: map[string]int{}, Bridges: map[string]driftBridge{}}
}

var natNet = driftNet{Mode: "nat", Bridge: "virbr1"}

func rowKinds(r driftReport) []string {
	var out []string
	for _, x := range r.Rows {
		out = append(out, x.Network+":"+x.Kind)
	}
	return out
}

func TestDriftNone(t *testing.T) {
	a := dn(1, "a", "apt/x86_64", map[string]driftNet{"default": natNet})
	b := dn(2, "b", "apt/x86_64", map[string]driftNet{"default": {Mode: "nat", Bridge: "virbr0"}})
	// A different bridge name only matters in bridge mode.
	r := compareNetworks([]driftNode{a, b})
	if r.Networks != 0 || len(r.Rows) != 0 || len(r.Blocked) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestDriftMissing(t *testing.T) {
	a := dn(1, "a", "apt/x86_64", map[string]driftNet{"lan": {Mode: "isolated"}, "default": natNet})
	b := dn(2, "b", "apt/x86_64", map[string]driftNet{"default": natNet})
	r := compareNetworks([]driftNode{a, b})
	if r.Networks != 1 || len(r.Rows) != 1 || r.Rows[0].Kind != "missing" || r.Rows[0].Nodes[0] != "b" {
		t.Fatalf("%+v", r)
	}
	if r.Rows[0].What != "Exists on a, missing on b." {
		t.Errorf("what: %q", r.Rows[0].What)
	}
	if r.Rows[0].Fix != nil {
		t.Errorf("an isolated network must not be copyable: %+v", r.Rows[0].Fix)
	}
	// Nobody uses it: listed, but no alert.
	if r.Rows[0].Used || len(r.Blocked) != 0 || driftAlert(r) != nil {
		t.Errorf("unused network alerted: %+v", r)
	}
	// A running VM uses it: blocked, and the alert exists.
	a.Used["lan"], a.Running["lan"] = 1, 1
	r = compareNetworks([]driftNode{a, b})
	if len(r.Blocked) != 1 || r.Blocked[0] != "lan" || !r.Rows[0].Used {
		t.Fatalf("%+v", r)
	}
	al := driftAlert(r)
	if len(al) != 1 || al[0].Title != "Networks differ between nodes; some virtual machines cannot migrate." || al[0].Severity != "warning" {
		t.Fatalf("%+v", al)
	}
	// A stopped VM does not make it an alert.
	a.Running["lan"] = 0
	if r = compareNetworks([]driftNode{a, b}); len(r.Blocked) != 0 || !r.Rows[0].Used {
		t.Fatalf("%+v", r)
	}
}

func TestDriftBlockedNeedsNoOtherNodeToHaveIt(t *testing.T) {
	br := driftNet{Mode: "bridge", Bridge: "br0"}
	a := dn(1, "a", "apt/x86_64", map[string]driftNet{"lan": br})
	b := dn(2, "b", "apt/x86_64", map[string]driftNet{"lan": br})
	c := dn(3, "c", "apt/x86_64", map[string]driftNet{})
	a.Running["lan"] = 1
	r := compareNetworks([]driftNode{a, b, c})
	if len(r.Blocked) != 0 || len(r.Rows) != 1 || r.Rows[0].Nodes[0] != "c" {
		t.Fatalf("b can take the VM: %+v", r)
	}
}

func TestDriftModeVLANBridge(t *testing.T) {
	a := dn(1, "a", "dnf/x86_64", map[string]driftNet{
		"m": {Mode: "nat"}, "v": {Mode: "bridge", Bridge: "br0", VLAN: 100}, "b": {Mode: "bridge", Bridge: "br0"}})
	b := dn(2, "b", "dnf/x86_64", map[string]driftNet{
		"m": {Mode: "bridge", Bridge: "br0"}, "v": {Mode: "bridge", Bridge: "br0"}, "b": {Mode: "bridge", Bridge: "br9"}})
	r := compareNetworks([]driftNode{a, b})
	got := strings.Join(rowKinds(r), " ")
	if got != "b:bridge m:mode v:vlan" {
		t.Fatalf("rows: %s", got)
	}
	for _, x := range r.Rows {
		switch x.Kind {
		case "mode":
			if x.What != "Kind: bridge on b; nat on a." {
				t.Errorf("mode: %q", x.What)
			}
		case "vlan":
			if x.What != "VLAN: 100 on a; none on b." {
				t.Errorf("vlan: %q", x.What)
			}
		case "bridge":
			if x.What != "Bridge: br0 on a; br9 on b." {
				t.Errorf("bridge: %q", x.What)
			}
		}
	}
	if r.Networks != 3 {
		t.Errorf("networks: %d", r.Networks)
	}
}

func TestDriftIncompatibleNodesIgnored(t *testing.T) {
	a := dn(1, "a", "apt/x86_64", map[string]driftNet{"lan": {Mode: "isolated"}})
	b := dn(2, "b", "dnf/x86_64", map[string]driftNet{})
	c := dn(3, "c", "apt/aarch64", map[string]driftNet{})
	d := dn(4, "d", "", map[string]driftNet{}) // system not recognised
	a.Running["lan"] = 1
	r := compareNetworks([]driftNode{a, b, c, d})
	if r.Networks != 0 || len(r.Rows) != 0 || len(r.Blocked) != 0 {
		t.Fatalf("%+v", r)
	}
	// A second node of a's group brings it back.
	e := dn(5, "e", "apt/x86_64", map[string]driftNet{})
	if r = compareNetworks([]driftNode{a, b, c, d, e}); len(r.Rows) != 1 || r.Rows[0].Nodes[0] != "e" || len(r.Blocked) != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestDriftCreateOffer(t *testing.T) {
	br := driftNet{Mode: "bridge", Bridge: "br0"}
	a := dn(1, "a", "apt/x86_64", map[string]driftNet{"lan": br})
	b := dn(2, "b", "apt/x86_64", map[string]driftNet{})
	// The bridge is missing on the target: no button.
	if r := compareNetworks([]driftNode{a, b}); r.Rows[0].Fix != nil {
		t.Fatalf("offered without a bridge: %+v", r.Rows[0].Fix)
	}
	b.Bridges["br0"] = driftBridge{}
	r := compareNetworks([]driftNode{a, b})
	f := r.Rows[0].Fix
	if f == nil || f.Label != "Create on b" || f.From != 1 || f.To != 2 || f.Network != "lan" {
		t.Fatalf("fix: %+v", f)
	}
	// A different kind of switch (Open vSwitch) is not the same bridge.
	b.Bridges["br0"] = driftBridge{OVS: true}
	if r = compareNetworks([]driftNode{a, b}); r.Rows[0].Fix != nil {
		t.Fatalf("offered on another switch kind")
	}
	// A VLAN tag needs a VLAN-aware bridge that allows it.
	a.Nets["lan"] = driftNet{Mode: "bridge", Bridge: "br0", VLAN: 100}
	b.Bridges["br0"] = driftBridge{}
	if r = compareNetworks([]driftNode{a, b}); r.Rows[0].Fix != nil {
		t.Fatalf("offered with unknown VLAN support")
	}
	b.Bridges["br0"] = driftBridge{VLANKnown: true, VLANFiltering: true, VLANs: "100,200"}
	if r = compareNetworks([]driftNode{a, b}); r.Rows[0].Fix == nil {
		t.Fatalf("not offered although the VLAN is allowed")
	}
	b.Bridges["br0"] = driftBridge{VLANKnown: true, VLANFiltering: true, VLANs: "200"}
	if r = compareNetworks([]driftNode{a, b}); r.Rows[0].Fix != nil {
		t.Fatalf("offered although the VLAN is not allowed")
	}
	// Nodes that define it differently: nothing to copy as is.
	c := dn(3, "c", "apt/x86_64", map[string]driftNet{"lan": {Mode: "bridge", Bridge: "br0", VLAN: 5}})
	b.Bridges["br0"] = driftBridge{VLANKnown: true, VLANFiltering: true, VLANs: "5,100"}
	for _, row := range compareNetworks([]driftNode{a, b, c}).Rows {
		if row.Kind == "missing" && row.Fix != nil {
			t.Fatalf("copy offered from inconsistent definitions")
		}
	}
}

func TestGroupOf(t *testing.T) {
	deb := "ID=debian\nPRETTY_NAME=\"Debian GNU/Linux 13\"\nVIRTICLE_ARCH=x86_64\n"
	if g := groupOf(deb); g != "apt/x86_64" {
		t.Errorf("debian: %q", g)
	}
	if g := groupOf("ID=rocky\nID_LIKE=\"rhel centos fedora\"\nVIRTICLE_ARCH=aarch64\n"); g != "dnf/aarch64" {
		t.Errorf("rocky: %q", g)
	}
	if g := groupOf("ID=arch\nVIRTICLE_ARCH=x86_64\n"); g != "" {
		t.Errorf("arch: %q", g)
	}
	if g := groupOf("ID=debian\n"); g != "" {
		t.Errorf("no arch: %q", g)
	}
}
