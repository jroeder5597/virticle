package api

import (
	"strings"
	"testing"

	"virticle/internal/hostconn"
	"virticle/internal/nodeproto"
)

func probeNIC(mac, bridge string, ips ...string) nicProbe {
	return nicProbe{MAC: mac, Bridge: bridge, IPs: ips}
}

func TestDecideFailoverChecks(t *testing.T) {
	const mac = "52:54:00:aa:bb:cc"
	base := foFacts{Source: "n4", Standby: "n3", Asked: true, AskError: "ssh: connect to host: Connection timed out"}
	with := func(mod func(*foFacts)) foFacts {
		f := base
		f.NICs = nil
		mod(&f)
		return f
	}
	answered := probeNIC(mac, "br0", "10.0.0.5")
	answered.Replies = []hostconn.ProbeReply{{IP: "10.0.0.5", MAC: mac}}
	other := probeNIC(mac, "br0", "10.0.0.5")
	other.Replies = []hostconn.ProbeReply{{IP: "10.0.0.5", MAC: "52:54:00:11:22:33"}} // somebody else holds the address
	nat := nicProbe{MAC: mac, Skip: "nat"}
	noaddr := nicProbe{MAC: mac, Bridge: "br0", Skip: "noaddr"}

	cases := []struct {
		name    string
		f       foFacts
		refuse  string
		warning bool
		checked string // a part that must be in the line
		answers []string
	}{
		{"source reachable from the standby, VM running: refused",
			with(func(f *foFacts) { f.AskError, f.VMState = "", "running" }),
			"n4 is reachable from n3 and the VM is running there. Use Planned failover when the controller can reach it again, or shut the VM down on n4.", false, "", nil},
		{"paused counts as running",
			with(func(f *foFacts) { f.AskError, f.VMState = "", "paused" }), "n4 is reachable from n3", false, "", nil},
		{"source reachable, VM shut off: good news, allowed",
			with(func(f *foFacts) {
				f.AskError, f.VMState = "", "shutoff"
				f.NICs = []nicProbe{probeNIC(mac, "br0", "10.0.0.5")}
			}),
			"", false, "n4 is reachable from n3 and the VM is not running there; nothing answers at 10.0.0.5", nil},
		{"source reachable, VM gone from it",
			with(func(f *foFacts) { f.AskError, f.VMState = "", "absent" }), "", false, "the VM is not running there", nil},
		{"dead node, nothing answers",
			with(func(f *foFacts) { f.NICs = []nicProbe{probeNIC(mac, "br0", "10.0.0.5")} }),
			"", false, "Checked: n4 does not answer from n3; nothing answers at 10.0.0.5.", nil},
		{"dead node, two addresses silent",
			with(func(f *foFacts) { f.NICs = []nicProbe{probeNIC(mac, "br0", "10.0.0.6", "10.0.0.5")} }),
			"", false, "nothing answers at 10.0.0.5 and 10.0.0.6", nil},
		{"the VM answers from its own MAC: warning, start anyway needed",
			with(func(f *foFacts) { f.NICs = []nicProbe{answered} }),
			"", true, "the VM answers at 10.0.0.5", []string{"10.0.0.5"}},
		{"another MAC at the address is not the VM",
			with(func(f *foFacts) { f.NICs = []nicProbe{other} }),
			"", false, "nothing answers at 10.0.0.5", nil},
		{"only NAT networks",
			with(func(f *foFacts) { f.NICs = []nicProbe{nat} }),
			"", false, "the network was not checked: the VM's networks (NAT) exist per node", nil},
		{"no known address",
			with(func(f *foFacts) { f.NICs = []nicProbe{noaddr} }),
			"", false, "the network was not checked: no address of the VM is known", nil},
		{"no network cards at all",
			with(func(f *foFacts) {}), "", false, "the network was not checked", nil},
		{"the standby has no such bridge",
			with(func(f *foFacts) { n := probeNIC(mac, "br0", "10.0.0.5"); n.Skip = "nobridge"; f.NICs = []nicProbe{n} }),
			"", false, "n3 has no bridge for the VM's network", nil},
		{"the standby has no address on the bridge to ask from",
			with(func(f *foFacts) { n := probeNIC(mac, "br0", "10.0.0.5"); n.Skip = "nobraddr"; f.NICs = []nicProbe{n} }),
			"", false, "n3 has no IPv4 address on that bridge to ask from", nil},
		{"no arping and no ping",
			with(func(f *foFacts) { n := probeNIC(mac, "br0", "10.0.0.5"); n.Skip = "notool"; f.NICs = []nicProbe{n} }),
			"", false, "arping is not installed on n3", nil},
		{"trust not set up: said so, allowed",
			with(func(f *foFacts) { f.AskError = "Permission denied (publickey)" }),
			"", false, "n4 could not be asked from n3 (the node-to-node access is not set up)", nil},
		{"source not known to the controller",
			with(func(f *foFacts) { f.Asked, f.AskError = false, "" }),
			"", false, "n4 could not be asked from n3", nil},
		{"one NAT card and one answering bridge card",
			with(func(f *foFacts) { f.NICs = []nicProbe{nat, answered} }),
			"", true, "the VM answers at 10.0.0.5", []string{"10.0.0.5"}},
		{"running there wins over a silent network",
			with(func(f *foFacts) {
				f.AskError, f.VMState = "", "running"
				f.NICs = []nicProbe{probeNIC(mac, "br0", "10.0.0.5")}
			}),
			"is reachable from n3", false, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideFailoverChecks(tc.f)
			if (tc.refuse == "") != (got.Refuse == "") || !strings.Contains(got.Refuse, tc.refuse) {
				t.Errorf("refuse %q, want %q", got.Refuse, tc.refuse)
			}
			if (got.Warning != "") != tc.warning {
				t.Errorf("warning %q, want %v", got.Warning, tc.warning)
			}
			if tc.warning && !strings.Contains(got.Warning, "This VM still answers on the network at 10.0.0.5. It is probably still running on n4. Starting it here gives two VMs with the same addresses.") {
				t.Errorf("warning text: %q", got.Warning)
			}
			if !strings.Contains(got.Checked, tc.checked) {
				t.Errorf("checked %q, want it to contain %q", got.Checked, tc.checked)
			}
			if strings.Join(got.Answers, ",") != strings.Join(tc.answers, ",") {
				t.Errorf("answers %v, want %v", got.Answers, tc.answers)
			}
		})
	}
}

func TestVMAddrsFormat(t *testing.T) {
	m := parseVMAddrs("52:54:00:aa:bb:cc=10.0.0.5,10.0.0.6;52:54:00:11:22:33=192.168.1.9;bad;=x")
	if strings.Join(m["52:54:00:aa:bb:cc"], ",") != "10.0.0.5,10.0.0.6" || strings.Join(m["52:54:00:11:22:33"], ",") != "192.168.1.9" || len(m) != 2 {
		t.Errorf("%v", m)
	}
	for ip, want := range map[string]bool{"10.0.0.5": true, "192.168.1.9/24": true, "127.0.0.1": false, "169.254.1.1": false, "fe80::1": false, "::1": false, "10.0.0": false, "": false, "0.0.0.0": false} {
		if isProbeIPv4(ip) != want {
			t.Errorf("isProbeIPv4(%q) = %v", ip, !want)
		}
	}
}

func TestReplicationRecordsTheStandbyAddressAndTheReplicaTheAddresses(t *testing.T) {
	c := replConfig{Enabled: true, TargetNode: 2, TargetName: "n4", Pool: "default", EveryMinutes: 15, Dir: "/x", PeerHost: "10.0.0.4", PeerPort: "22", PeerUser: "virticle"}
	back := parseReplication("<meta>" + c.element() + "</meta>")
	if back == nil || back.PeerHost != "10.0.0.4" || back.PeerPort != "22" || back.PeerUser != "virticle" {
		t.Fatalf("%+v", back)
	}
	// a record written before this version has no address: it parses, and the hook starts the VM without asking
	old := parseReplication(`<meta><replication enabled="yes" target-node="2" target-name="n4" pool="default" every-minutes="15" dir="/x"/></meta>`)
	if old == nil || old.PeerHost != "" {
		t.Fatalf("%+v", old)
	}
	rm := replicaMeta{Dir: "/d", SourceName: "n3", Addrs: "52:54:00:aa:bb:cc=10.0.0.5"}
	if got := parseReplicaMeta("<meta>" + rm.element() + "</meta>"); got == nil || got.Addrs != rm.Addrs {
		t.Fatalf("%+v", got)
	}
}

// The start guard on the old node and the controller's reconcile apply one rule: the VM that took over later
// (the higher generation) is the live one.
func TestTookOverIsTheRuleOfDecideOldCopy(t *testing.T) {
	for asker := 0; asker <= 3; asker++ {
		for standby := 1; standby <= 3; standby++ {
			meta := ""
			if asker > 0 {
				meta = failedOverMeta{Gen: asker}.element()
				meta = "<meta>" + meta + "</meta>"
			}
			act := decideOldCopy(failedOverMeta{Gen: standby}, oldVMFacts{Meta: meta, Running: true})
			if (act == oldPowerOff) != nodeproto.TookOver(true, standby, asker) {
				t.Errorf("standby %d, old VM %d: decideOldCopy %v, TookOver %v", standby, asker, act, nodeproto.TookOver(true, standby, asker))
			}
		}
	}
	if nodeproto.TookOver(false, 5, 0) {
		t.Error("no failover record cannot have taken over")
	}
}
