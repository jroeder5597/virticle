package api

import (
	"strings"
	"testing"
	"time"
)

func nic(name, typ, state, master string, addrs ...string) hostNIC {
	n := hostNIC{Name: name, Type: typ, State: state, Master: master, Addresses: addrs}
	if typ == "bridge" || typ == "bond" || typ == "vlan" {
		n.Kind = typ
	}
	return n
}

func alertByID(a []Alert) map[string]Alert {
	m := map[string]Alert{}
	for _, x := range a {
		m[x.ID] = x
	}
	return m
}

func TestLinkAlertsUnusedCardsStayQuiet(t *testing.T) {
	f := linkFacts{NICs: []hostNIC{
		nic("eno1", "physical", "up", "br0"),
		nic("br0", "bridge", "up", "", "192.0.2.10/24"),
		nic("eno2", "physical", "down", ""), // never plugged in, nothing uses it
		nic("eno3", "physical", "down", ""),
	}}
	if got := linkAlerts(1, "n1", f, []string{"192.0.2.10"}); len(got) != 0 {
		t.Fatalf("alerts for unused cards: %+v", got)
	}
}

func TestLinkAlertsUplinkDownSeverity(t *testing.T) {
	// Only uplink of the bridge with the node's address: critical.
	f := linkFacts{NICs: []hostNIC{
		nic("eno1", "physical", "down", "br0"),
		nic("br0", "bridge", "up", "", "192.0.2.10/24"),
	}}
	got := alertByID(linkAlerts(1, "n1", f, []string{"192.0.2.10"}))
	a, ok := got["link:1:eno1"]
	if !ok || a.Severity != "critical" || a.Title != "Network card eno1 on n1 has no link." || a.Kind != "net-link" || a.Node != 1 {
		t.Fatalf("got %+v", got)
	}
	// With running VMs (tap ports) the text says so.
	f.NICs = append(f.NICs, nic("vnet0", "tap", "up", "br0"))
	a = alertByID(linkAlerts(1, "n1", f, nil))["link:1:eno1"]
	if a.Severity != "critical" || !strings.Contains(a.Message, "Its virtual machines have no network.") {
		t.Fatalf("got %+v", a)
	}
	// A second uplink with link: warning.
	f.NICs = append(f.NICs, nic("eno2", "physical", "up", "br0"))
	a = alertByID(linkAlerts(1, "n1", f, nil))["link:1:eno1"]
	if a.Severity != "warning" || strings.Contains(a.Message, "virtual machines") {
		t.Fatalf("got %+v", a)
	}
	// An empty bridge without the node's address: warning.
	f = linkFacts{NICs: []hostNIC{nic("eno1", "physical", "down", "br1"), nic("br1", "bridge", "up", "")}}
	if a = alertByID(linkAlerts(1, "n1", f, nil))["link:1:eno1"]; a.Severity != "warning" {
		t.Fatalf("got %+v", a)
	}
}

func TestLinkAlertsAddressedCardAndVLAN(t *testing.T) {
	f := linkFacts{NICs: []hostNIC{nic("eno1", "physical", "down", "", "198.51.100.5/24")}}
	if a := alertByID(linkAlerts(2, "n2", f, nil))["link:2:eno1"]; a.Severity != "warning" {
		t.Fatalf("addressed card: %+v", a)
	}
	// A card under a VLAN device that is a bridge port is in use through it.
	f = linkFacts{
		NICs: []hostNIC{
			nic("eno1", "physical", "down", ""),
			nic("eno1.20", "vlan", "down", "br20"),
			nic("br20", "bridge", "up", ""),
			nic("vnet3", "tap", "up", "br20"),
		},
		VLANParent: map[string]string{"eno1.20": "eno1"},
	}
	if a := alertByID(linkAlerts(2, "n2", f, nil))["link:2:eno1"]; a.Severity != "critical" {
		t.Fatalf("vlan parent: %+v", a)
	}
}

func TestLinkAlertsBond(t *testing.T) {
	nics := []hostNIC{
		nic("bond0", "bond", "up", "br0"),
		nic("br0", "bridge", "up", "", "192.0.2.10/24"),
		nic("eno1", "physical", "up", "bond0"),
		nic("eno2", "physical", "down", "bond0"),
	}
	got := alertByID(linkAlerts(1, "n1", linkFacts{NICs: nics}, []string{"192.0.2.10"}))
	b := got["bond:1:bond0"]
	if b.Title != "bond0 on n1 runs on 1 of 2 cards." || b.Severity != "warning" || b.Kind != "net-bond" {
		t.Fatalf("bond: %+v", got)
	}
	// The card is down, but the bond still has link: warning, not critical.
	if c := got["link:1:eno2"]; c.Severity != "warning" {
		t.Fatalf("member: %+v", c)
	}
	// Both members down: no "runs on" alert; the cards alert, and the bridge is cut off.
	nics[2].State = "down"
	nics[0].State = "down"
	got = alertByID(linkAlerts(1, "n1", linkFacts{NICs: nics}, []string{"192.0.2.10"}))
	if _, ok := got["bond:1:bond0"]; ok {
		t.Fatalf("bond with no card up reported as degraded")
	}
	if got["link:1:eno1"].Severity != "critical" || got["link:1:eno2"].Severity != "critical" {
		t.Fatalf("members of a dead bond: %+v", got)
	}
}

func TestLinkAlertsLACP(t *testing.T) {
	nics := []hostNIC{
		nic("bond0", "bond", "up", "br0"), nic("br0", "bridge", "up", ""),
		nic("eno1", "physical", "up", "bond0"), nic("eno2", "physical", "up", "bond0"),
	}
	f := linkFacts{NICs: nics, Bonds: map[string]bondStatus{"bond0": {LACP: true, PartnerMissing: true}}}
	got := alertByID(linkAlerts(1, "n1", f, nil))
	a := got["lacp:1:bond0"]
	if a.Title != "LACP is not answered by the switch for bond0 on n1." || a.Severity != "warning" {
		t.Fatalf("got %+v", got)
	}
	f.Bonds["bond0"] = bondStatus{LACP: true}
	if got = alertByID(linkAlerts(1, "n1", f, nil)); len(got) != 0 {
		t.Fatalf("answering partner alerted: %+v", got)
	}
	// An unused bond is not reported.
	f = linkFacts{NICs: []hostNIC{nic("bond0", "bond", "up", ""), nic("eno1", "physical", "up", "bond0")},
		Bonds: map[string]bondStatus{"bond0": {LACP: true, PartnerMissing: true}}}
	if got = alertByID(linkAlerts(1, "n1", f, nil)); len(got) != 0 {
		t.Fatalf("unused bond alerted: %+v", got)
	}
}

const bondNoPartner = `Ethernet Channel Bonding Driver: v6.8.0

Bonding Mode: IEEE 802.3ad Dynamic link aggregation
Transmit Hash Policy: layer2 (0)
MII Status: up
802.3ad info
LACP active: on
LACP rate: slow
Active Aggregator Info:
	Aggregator ID: 1
	Number of ports: 1
	Actor Key: 9
	Partner Key: 1
	Partner Mac Address: 00:00:00:00:00:00

Slave Interface: eno1
MII Status: up
Speed: 1000 Mbps
details partner lacp pdu:
    system priority: 65535
    system mac address: 00:00:00:00:00:00
`

func TestParseBondStatus(t *testing.T) {
	if st := parseBondStatus(bondNoPartner); !st.LACP || !st.PartnerMissing {
		t.Errorf("no partner: %+v", st)
	}
	ok := strings.Replace(bondNoPartner, "Partner Mac Address: 00:00:00:00:00:00", "Partner Mac Address: 52:54:00:12:34:56", 1)
	if st := parseBondStatus(ok); !st.LACP || st.PartnerMissing {
		t.Errorf("partner answers: %+v", st)
	}
	active := "Bonding Mode: fault-tolerance (active-backup)\nMII Status: up\nPrimary Slave: None\n"
	if st := parseBondStatus(active); st.LACP || st.PartnerMissing {
		t.Errorf("active-backup: %+v", st)
	}
}

func TestParseLinkFacts(t *testing.T) {
	out := `Welcome banner
[{"ifname":"lo","link_type":"loopback","operstate":"UNKNOWN","addr_info":[]},{"ifname":"eno1","address":"aa:bb:cc:dd:ee:01","master":"bond0","mtu":1500,"operstate":"UP","addr_info":[]},{"ifname":"bond0","operstate":"UP","linkinfo":{"info_kind":"bond"},"addr_info":[]},{"ifname":"bond0.5","link":"bond0","operstate":"UP","linkinfo":{"info_kind":"vlan"},"addr_info":[{"local":"192.0.2.9","prefixlen":24}]}]
SPEED eno1 1000
BONDSTART bond0
` + bondNoPartner + `BONDEND
`
	f, err := parseLinkFacts(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.NICs) != 3 || f.VLANParent["bond0.5"] != "bond0" || !f.Bonds["bond0"].PartnerMissing {
		t.Fatalf("%+v", f)
	}
}

func TestDebouncedAlerts(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	a := Alert{ID: "link:1:eno1", Kind: "net-link", Severity: "warning", Node: 1}
	step := func(in alertInput) (raised, cleared []Alert, all []Alert) {
		now = now.Add(time.Minute)
		in.Fresh = map[int64]bool{1: true}
		return tr.step(in, now)
	}
	// Seen once: nothing. A gap resets the count.
	if r, _, all := step(alertInput{Debounced: []Alert{a}}); len(r)+len(all) != 0 {
		t.Fatalf("raised on first sight: %v", all)
	}
	if r, _, all := step(alertInput{}); len(r)+len(all) != 0 {
		t.Fatalf("%v", all)
	}
	if _, _, all := step(alertInput{Debounced: []Alert{a}}); len(all) != 0 {
		t.Fatalf("raised after a gap: %v", all)
	}
	// Seen twice in a row: raised.
	if r, _, all := step(alertInput{Debounced: []Alert{a}}); len(r) != 1 || len(all) != 1 {
		t.Fatalf("not raised on second sight: %v", all)
	}
	// Missing once: still there. Back: no flap. Missing twice: cleared.
	if r, c, all := step(alertInput{}); len(r)+len(c) != 0 || len(all) != 1 {
		t.Fatalf("cleared at once: %v %v", c, all)
	}
	if r, c, all := step(alertInput{Debounced: []Alert{a}}); len(r)+len(c) != 0 || len(all) != 1 {
		t.Fatalf("flapped: %v %v", r, c)
	}
	step(alertInput{})
	if _, c, all := step(alertInput{}); len(c) != 1 || len(all) != 0 {
		t.Fatalf("not cleared after two misses: %v %v", c, all)
	}
	// A node that could not be read keeps its alert and is not counted as missing.
	step(alertInput{Debounced: []Alert{a}})
	step(alertInput{Debounced: []Alert{a}})
	unread := func(Alert) bool { return true }
	for i := 0; i < 4; i++ {
		if _, c, all := step(alertInput{Unread: unread}); len(c) != 0 || len(all) != 1 {
			t.Fatalf("unread node cleared its alert: %v %v", c, all)
		}
	}
}
