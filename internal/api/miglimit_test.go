package api

import (
	"strings"
	"testing"
	"time"

	"virticle/internal/store"
)

func side(name string, shared bool, speed int, lim string) linkSide {
	return linkSide{Name: name, Iface: "eth0", Addr: "10.0.0.1", Shared: shared, SpeedMbps: speed, Limit: storedLimit(lim)}
}

func TestDecideLimit(t *testing.T) {
	cases := []struct {
		name     string
		a, b     linkSide
		mbit     int
		applies  bool
		contains string
	}{
		{"both dedicated", side("n1", false, 1000, ""), side("n2", false, 1000, ""), 0, false, ""},
		{"both dedicated, custom ignored", side("n1", false, 1000, "custom:50"), side("n2", false, 0, ""), 0, false, ""},
		{"shared, 1 Gb/s, both alike", side("n1", true, 1000, ""), side("n2", true, 1000, ""), 800, true, "Shared with management: limited to 800 Mbit/s (80% of 1 Gb/s)."},
		{"shared, speed unknown", side("n1", true, 0, ""), side("n2", true, 0, ""), 0, true, "Link speed unknown, so the transfer is not limited."},
		{"source shared only", side("n1", true, 10000, ""), side("n2", false, 1000, ""), 8000, true, "limited to 8000 Mbit/s (80% of 10 Gb/s)"},
		{"target shared only", side("n1", false, 10000, ""), side("n2", true, 2500, ""), 2000, true, "80% of 2.5 Gb/s"},
		{"different links, the lower wins", side("n1", true, 10000, ""), side("n2", true, 1000, ""), 800, true, "Shared with management on n2: limited to 800 Mbit/s"},
		{"100 Mb/s card", side("n1", true, 100, ""), side("n2", false, 0, ""), 80, true, "80% of 100 Mb/s"},
		{"custom", side("n1", true, 1000, "custom:80"), side("n2", true, 1000, ""), 80, true, "set for n1"},
		{"custom on a link of unknown speed", side("n1", true, 0, "custom:80"), side("n2", true, 0, ""), 80, true, "limited to 80 Mbit/s"},
		{"custom higher than the other's 80%", side("n1", true, 1000, "custom:900"), side("n2", true, 1000, ""), 800, true, "limited to 800 Mbit/s"},
		{"no limit on both", side("n1", true, 1000, "none"), side("n2", true, 1000, "none"), 0, true, "limit is turned off"},
		{"no limit on one, auto on the other", side("n1", true, 1000, "none"), side("n2", true, 1000, ""), 800, true, "Shared with management on n2"},
		{"no limit on one, unknown speed on the other", side("n1", true, 1000, "none"), side("n2", true, 0, ""), 0, true, "Link speed unknown"},
		{"custom on a dedicated end does not apply, other shared unknown", side("n1", false, 1000, "custom:50"), side("n2", true, 0, ""), 0, true, "Link speed unknown"},
	}
	for _, c := range cases {
		d := decideLimit(c.a, c.b)
		if d.Mbit != c.mbit || d.Applies != c.applies {
			t.Errorf("%s: cap %d applies %v, want %d %v (%q)", c.name, d.Mbit, d.Applies, c.mbit, c.applies, d.Text)
		}
		if !strings.Contains(d.Text, c.contains) {
			t.Errorf("%s: text %q lacks %q", c.name, d.Text, c.contains)
		}
		// The order of the two nodes does not change the cap.
		if r := decideLimit(c.b, c.a); r.Mbit != c.mbit || r.Applies != c.applies {
			t.Errorf("%s reversed: cap %d applies %v", c.name, r.Mbit, r.Applies)
		}
	}
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]string{"": "", "auto": "", "none": "none", "custom:80": "custom:80", " custom:1000 ": "custom:1000"} {
		l, err := parseLimit(in)
		if err != nil || l.String() != want {
			t.Errorf("parseLimit(%q) = %q, %v; want %q", in, l.String(), err, want)
		}
	}
	for _, in := range []string{"custom:0", "custom:-3", "custom:x", "custom", "fast", "custom:2000000"} {
		if _, err := parseLimit(in); err == nil {
			t.Errorf("parseLimit(%q) accepted", in)
		}
	}
	if storedLimit("garbage").Mode != limitAuto {
		t.Error("an unreadable stored value must count as the default")
	}
}

func TestLimitUnits(t *testing.T) {
	if mbitToBytes(800) != 100_000_000 {
		t.Errorf("800 Mbit/s = %d B/s", mbitToBytes(800))
	}
	if mbitToMiB(800) != 95 || mbitToMiB(80) != 9 || mbitToMiB(1) != 1 {
		t.Errorf("MiB/s: %d %d %d", mbitToMiB(800), mbitToMiB(80), mbitToMiB(1))
	}
	for in, want := range map[int]string{100: "100 Mb/s", 1000: "1 Gb/s", 2500: "2.5 Gb/s", 10000: "10 Gb/s", 25000: "25 Gb/s"} {
		if fmtLinkSpeed(in) != want {
			t.Errorf("fmtLinkSpeed(%d) = %q", in, fmtLinkSpeed(in))
		}
	}
}

func TestRequestBandwidthWins(t *testing.T) {
	p := &migPlan{LimitMbit: 800}
	if p.bandwidthMiB(migReq{}) != 95 || p.rateBps(migReq{}) != 100_000_000 {
		t.Errorf("automatic: %d MiB/s, %d B/s", p.bandwidthMiB(migReq{}), p.rateBps(migReq{}))
	}
	r := migReq{BandwidthMiB: 20}
	if p.bandwidthMiB(r) != 20 || p.rateBps(r) != 20<<20 {
		t.Errorf("request: %d MiB/s, %d B/s", p.bandwidthMiB(r), p.rateBps(r))
	}
	if (&migPlan{}).bandwidthMiB(migReq{}) != 0 || (&migPlan{}).rateBps(migReq{}) != 0 {
		t.Error("no cap and no request must be unlimited")
	}
}

func nic(name, ip string, speed int) hostNIC {
	return hostNIC{Name: name, Type: "physical", State: "up", SpeedMbps: speed, Addresses: []string{ip + "/24"}}
}

func TestBuildTransferRows(t *testing.T) {
	nicsA := []hostNIC{nic("enp1s0", "10.0.0.5", 1000), nic("enp2s0", "10.0.1.5", 10000)}
	nicsB := []hostNIC{nic("enp1s0", "10.0.0.6", 1000), nic("enp2s0", "10.0.1.6", 10000)}
	mk := func(name, mgmt, mig string, nics []hostNIC, lim string) nodeNet {
		return buildNodeNet(name, store.Node{Addr: mgmt, MigrationAddr: mig, MigrationLimit: lim}, nics)
	}
	// Both on the second card: no cap, one row.
	tr := buildTransfer(mk("n1", "10.0.0.5", "10.0.1.5", nicsA, ""), mk("n2", "10.0.0.6", "10.0.1.6", nicsB, ""), "enp2s0", "10.0.1.5")
	if tr.Row != "enp2s0 on both nodes (10.0.1.5 → 10.0.1.6)" || tr.Limit.Applies || tr.RateBps() != 0 {
		t.Errorf("dedicated: %q %+v", tr.Row, tr.Limit)
	}
	// Default: the management network, 1 Gb/s each: 800 Mbit/s.
	tr = buildTransfer(mk("n1", "10.0.0.5", "", nicsA, ""), mk("n2", "10.0.0.6", "", nicsB, ""), "enp1s0", "10.0.0.5")
	if tr.Row != "Migration uses the management network on both nodes (10.0.0.5 → 10.0.0.6)" || tr.Limit.Mbit != 800 || tr.RateBps() != 100_000_000 {
		t.Errorf("shared: %q %+v", tr.Row, tr.Limit)
	}
	// Only the target has a migration network: the cap comes from the source's shared link.
	tr = buildTransfer(mk("n1", "10.0.0.5", "", nicsA, ""), mk("n2", "10.0.0.6", "10.0.1.6", nicsB, ""), "enp1s0", "10.0.0.5")
	if !strings.Contains(tr.Row, "management network on n1 and enp2s0 on n2") || tr.Limit.Mbit != 800 {
		t.Errorf("mixed: %q %+v", tr.Row, tr.Limit)
	}
	// The source's egress is not known: its own migration interface stands in.
	tr = buildTransfer(mk("n1", "10.0.0.5", "10.0.1.5", nicsA, ""), mk("n2", "10.0.0.6", "10.0.1.6", nicsB, ""), "", "")
	if tr.Src.Iface != "enp2s0" || tr.Limit.Applies {
		t.Errorf("fallback: %+v", tr.Src)
	}
	// Virtual cards report no speed.
	nicsV := []hostNIC{nic("ens3", "10.0.0.7", 0)}
	tr = buildTransfer(mk("n1", "10.0.0.7", "", nicsV, ""), mk("n2", "10.0.0.7", "", nicsV, ""), "ens3", "10.0.0.7")
	if tr.Limit.Mbit != 0 || !strings.Contains(tr.Limit.Text, "Link speed unknown") {
		t.Errorf("unknown speed: %+v", tr.Limit)
	}
}

func TestBridgeTakesSpeedOfItsCard(t *testing.T) {
	nics := []hostNIC{
		{Name: "br0", Type: "bridge", Kind: "bridge", State: "up", Addresses: []string{"10.0.0.5/24"}},
		{Name: "enp1s0", Type: "physical", State: "up", SpeedMbps: 2500, Master: "br0"},
		{Name: "vnet0", Type: "tap", State: "up", Master: "br0"},
	}
	if nicSpeed(nics, "br0") != 2500 || nicSpeed(nics, "vnet0") != 0 || nicSpeed(nics, "nope") != 0 {
		t.Errorf("speeds: %d %d", nicSpeed(nics, "br0"), nicSpeed(nics, "vnet0"))
	}
}

func TestMigrationOptionsAndDescribe(t *testing.T) {
	nics := []hostNIC{
		nic("enp1s0", "10.0.0.5", 1000),
		nic("enp2s0", "10.0.1.5", 10000),
		{Name: "virbr0", Type: "bridge", Kind: "bridge", State: "up", Addresses: []string{"192.168.122.1/24"}},
		{Name: "enp3s0", Type: "physical", State: "down", Addresses: []string{"10.0.2.5/24"}},
		{Name: "enp4s0", Type: "physical", State: "up", Addresses: []string{"fe80::1/64", "10.0.3.5/24"}},
	}
	opts := migrationOptions(nics, []string{"10.0.0.5"})
	if len(opts) != 2 || opts[0].Label != "enp2s0 (10.0.1.5), 10 Gb/s" || opts[1].Label != "enp4s0 (10.0.3.5)" {
		t.Fatalf("options: %+v", opts)
	}

	n := store.Node{Addr: "10.0.0.5"}
	info := describeMigNet(n, nics)
	if !info.Show || info.Current != "Same as management (10.0.0.5)" || info.Custom != "" || !info.Shared || info.Interface != "enp1s0" || info.SpeedMbps != 1000 {
		t.Errorf("default: %+v", info)
	}
	if info.LimitText != "80% of the link (default)" {
		t.Errorf("limit text %q", info.LimitText)
	}
	n.MigrationAddr = "10.0.1.5"
	info = describeMigNet(n, nics)
	if info.Current != "enp2s0 (10.0.1.5), 10 Gb/s" || info.Shared || info.Custom != "" {
		t.Errorf("chosen: %+v", info)
	}
	n.MigrationAddr = "172.16.0.9"
	info = describeMigNet(n, nics)
	if info.Custom != "172.16.0.9" || info.Current != "Custom (172.16.0.9)" {
		t.Errorf("custom: %+v", info)
	}
	// The management address stored as the migration address is the same as the default.
	n.MigrationAddr = "10.0.0.5"
	if info = describeMigNet(n, nics); info.Custom != "" || info.Current != "Same as management (10.0.0.5)" {
		t.Errorf("management stored: %+v", info)
	}
	// A single usable address: the field is not shown.
	info = describeMigNet(store.Node{Addr: "10.0.0.5"}, nics[:1])
	if info.Show {
		t.Errorf("single address must not be shown: %+v", info)
	}
	// But a stored custom value stays visible so it can be reset.
	info = describeMigNet(store.Node{Addr: "10.0.0.5", MigrationAddr: "172.16.0.9", MigrationLimit: "custom:80"}, nics[:1])
	if !info.Show || info.LimitText != "Custom (80 Mbit/s)" {
		t.Errorf("custom on a single address: %+v", info)
	}
	// Offline: only the stored settings.
	info = describeMigNet(store.Node{Addr: "10.0.0.5", MigrationAddr: "10.0.1.5"}, nil)
	if info.Online || info.Current != "Custom (10.0.1.5)" {
		t.Errorf("offline: %+v", info)
	}
}

func TestNormalizeMigAddr(t *testing.T) {
	cur := store.Node{Addr: "10.0.0.5:2222"}
	for in, want := range map[string]string{"": "", " ": "", "10.0.0.5": "", "10.0.1.5": "10.0.1.5", "m.example": "m.example"} {
		got, err := normalizeMigAddr(cur, in)
		if err != nil || got != want {
			t.Errorf("normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := normalizeMigAddr(cur, "bad host;x"); err == nil {
		t.Error("unsafe address accepted")
	}
}

// File copies (peer-send) go to the migration network of the target, with the SSH port of the node unless the
// migration address has its own; the host key is recorded under that same name.
func TestFileCopiesFollowTheMigrationNetwork(t *testing.T) {
	n := store.Node{Addr: "10.0.0.6:2200", User: "root", MigrationAddr: "10.0.1.6"}
	ep, err := endpointFor(n)
	if err != nil || ep.Host != "10.0.1.6" || ep.port() != "2200" {
		t.Fatalf("endpoint %+v, %v", ep, err)
	}
	n.MigrationAddr = "10.0.1.6:22022"
	if ep, _ = endpointFor(n); ep.port() != "22022" {
		t.Errorf("own port: %+v", ep)
	}
	n.MigrationAddr = ""
	if ep, _ = endpointFor(n); ep.Host != "10.0.0.6" || ep.port() != "2200" {
		t.Errorf("default: %+v", ep)
	}
	msg := sshBlockedMsg("n1", migEndpoint{Host: "10.0.1.6", Port: "22"}, store.Node{Addr: "10.0.0.6", MigrationAddr: "10.0.1.6"})
	if !strings.Contains(msg, "migration network of the target") || !strings.Contains(msg, "Same as management") {
		t.Errorf("message: %s", msg)
	}
	msg = sshBlockedMsg("n1", migEndpoint{Host: "10.0.0.6"}, store.Node{Addr: "10.0.0.6"})
	if strings.Contains(msg, "migration network") {
		t.Errorf("message for the default: %s", msg)
	}
}

// Changing the address renews the certificate at once, even inside the retry interval of the automatic upkeep.
func TestAddressChangeRenewsCertificateAtOnce(t *testing.T) {
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeEnsurer{}
	a, _ := newTestAuto(f, &clock)
	a.Try(1) // the upkeep just ran
	clock = clock.Add(time.Minute)
	if a.Try(1) || f.calls != 1 {
		t.Fatal("the upkeep must still be waiting")
	}
	if e := renewNow(a, 1); e != "" || f.calls != 2 {
		t.Fatalf("renewNow: %q, %d calls", e, f.calls)
	}
	f.err = errString("no space left")
	if e := renewNow(a, 1); e != "no space left" || f.calls != 3 {
		t.Fatalf("a failure must be reported: %q, %d calls", e, f.calls)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestMarkMigration(t *testing.T) {
	sw := []topoSwitch{{
		HostAddresses: []topoAddr{{Address: "10.0.0.5/24", Management: true}},
		Uplinks:       []topoUplink{{Name: "enp1s0", Addresses: []topoAddr{}}},
	}}
	unused := []topoUplink{{Name: "enp2s0", Addresses: []topoAddr{{Address: "10.0.1.5/24"}}}, {Name: "enp3s0", Addresses: []topoAddr{{Address: "10.0.2.5/24"}}}}
	markMigration(sw, unused, []string{"10.0.1.5"})
	if !unused[0].Migration || !unused[0].Addresses[0].Migration || unused[1].Migration || sw[0].HostAddresses[0].Migration {
		t.Errorf("marks: %+v %+v", sw, unused)
	}
}
