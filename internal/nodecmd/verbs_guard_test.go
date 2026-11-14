package nodecmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"virticle/internal/nodeproto"
)

const gUUID = "79739dff-f4b6-4153-b593-13bcef947d90"

// domXML is the start of a domain definition as libvirt hands it to the hook.
func domXML(meta string) string {
	return `<domain type='kvm'>
  <name>web</name>
  <uuid>` + gUUID + `</uuid>
  <metadata>` + meta + `</metadata>
  <memory unit='KiB'>1048576</memory>
  <devices><disk type='file'/></devices>
</domain>`
}

const ns = `xmlns:virticle="https://virticle.invalid/ns/1"`

func replMeta(extra string) string {
	return `<virticle:meta ` + ns + `><virticle:replication enabled="yes" target-node="2" target-name="n4" pool="default" every-minutes="15" dir="/x" peer-host="10.0.0.4" peer-port="22" peer-user="virticle"/>` + extra + `</virticle:meta>`
}

// guardNode makes a node whose ssh is a script: it saves the header it was sent and prints the answer file.
func guardNode(t *testing.T, answer string, sshExit int) (*tnode, *[]string) {
	n := newNode(t)
	var notes []string
	n.cfg.Log = func(m string) { notes = append(notes, m) }
	ssh := n.abs("fake-ssh")
	script := "#!/bin/sh\nread h; echo \"$h\" > " + n.abs("header") + "\necho \"$@\" > " + n.abs("ssh-args") + "\n"
	if answer != "" {
		script += "echo '" + answer + "'\n"
	}
	script += "exit " + string(rune('0'+sshExit)) + "\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	n.cfg.Tools["ssh"] = ssh
	return n, &notes
}

func hook(n *tnode, xml string, args ...string) (string, string, int) {
	return n.runSplit(xml, append([]string{"hook-qemu", "svc", "web"}, args...)...)
}

func TestHookRefusesWhenTheStandbyTookOver(t *testing.T) {
	n, notes := guardNode(t, "@@took-over 1", 0)
	_, errOut, code := hook(n, domXML(replMeta("")), "prepare", "begin", "-")
	if code != nodeproto.HookRefuse {
		t.Fatalf("exit %d, want %d (stderr %q)", code, nodeproto.HookRefuse, errOut)
	}
	if !strings.Contains(errOut, "this VM was started on n4 from its copy; it is kept off here. Use Replicate back in Virticle.") {
		t.Errorf("message: %q", errOut)
	}
	hdr, _ := os.ReadFile(n.abs("header"))
	if strings.TrimSpace(string(hdr)) != "virticle-peer 1 took-over "+gUUID+" 0" {
		t.Errorf("header sent: %q", hdr)
	}
	if len(*notes) == 0 {
		t.Error("nothing in the journal")
	}
	args, _ := os.ReadFile(n.abs("ssh-args"))
	if !strings.Contains(string(args), "virticle@10.0.0.4") || !strings.Contains(string(args), "ConnectTimeout=3") {
		t.Errorf("ssh arguments: %q", args)
	}
}

func TestHookSendsTheVMsOwnGeneration(t *testing.T) {
	// A VM that took over itself (generation 2) asks with 2; the standby compares.
	n, _ := guardNode(t, "@@not-took-over", 0)
	_, _, code := hook(n, domXML(replMeta(`<virticle:failed-over from-node="1" from-name="n3" at="1" planned="no" gen="2"/>`)), "prepare", "begin", "-")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	hdr, _ := os.ReadFile(n.abs("header"))
	if strings.TrimSpace(string(hdr)) != "virticle-peer 1 took-over "+gUUID+" 2" {
		t.Errorf("header sent: %q", hdr)
	}
}

func TestHookStartsWhenNothingSaysOtherwise(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		exit   int
		xml    string
		args   []string
	}{
		{"not taken over", "@@not-took-over", 0, domXML(replMeta("")), []string{"prepare", "begin", "-"}},
		{"standby unreachable", "", 255, domXML(replMeta("")), []string{"prepare", "begin", "-"}},
		{"nonsense answer", "hello", 0, domXML(replMeta("")), []string{"prepare", "begin", "-"}},
		{"not replicated", "@@took-over 1", 0, domXML(""), []string{"prepare", "begin", "-"}},
		{"replicated but no standby address yet", "@@took-over 1", 0, domXML(`<virticle:meta ` + ns + `><virticle:replication enabled="yes" target-node="2" target-name="n4"/></virticle:meta>`), []string{"prepare", "begin", "-"}},
		{"other phase", "@@took-over 1", 0, domXML(replMeta("")), []string{"started", "begin", "-"}},
		{"other phase 2", "@@took-over 1", 0, domXML(replMeta("")), []string{"stopped", "end", "-"}},
		{"broken xml", "@@took-over 1", 0, "<domain><uuid>", []string{"prepare", "begin", "-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, _ := guardNode(t, tc.answer, tc.exit)
			if _, e, code := hook(n, tc.xml, tc.args...); code != 0 {
				t.Errorf("exit %d (%s): the VM must start", code, e)
			}
		})
	}
	// too few arguments from libvirt never block
	n, _ := guardNode(t, "@@took-over 1", 0)
	if _, code := n.run(domXML(replMeta("")), "hook-qemu", "svc"); code != 0 {
		t.Errorf("short call: exit %d", code)
	}
}

func TestHookNeverWaitsLongerThanTheLimit(t *testing.T) {
	n := newNode(t)
	n.cfg.Log = func(string) {}
	ssh := n.abs("slow-ssh")
	os.WriteFile(ssh, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	n.cfg.Tools["ssh"] = ssh
	start := time.Now()
	_, _, code := hook(n, domXML(replMeta("")), "prepare", "begin", "-")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if d := time.Since(start); d > guardHardLimit+2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestHookFastForOtherVMs(t *testing.T) {
	n, _ := guardNode(t, "", 0)
	n.cfg.Tools["ssh"] = "" // would fail if it were used
	start := time.Now()
	for i := 0; i < 20; i++ {
		if _, _, code := hook(n, domXML(`<virticle:meta `+ns+`><virticle:tag name="x"/></virticle:meta>`), "prepare", "begin", "-"); code != 0 {
			t.Fatal("exit", code)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("20 plain starts took %v", d)
	}
}

func TestHookSelftest(t *testing.T) {
	n, _ := guardNode(t, "", 0)
	n.cfg.Tools["ssh"] = "/bin/true"
	os.WriteFile(n.abs("home/svc/.ssh/virticle_peer_ed25519"), []byte("k"), 0o600)
	_, errOut, code := n.runSplit("", "hook-qemu", "svc", nodeproto.SelftestVM, "prepare", "begin", "-")
	if code != nodeproto.HookRefuse || !strings.Contains(errOut, nodeproto.SelftestMark+" exec=ok ssh=ok key=ok net=ok") {
		t.Errorf("exit %d, %q", code, errOut)
	}
}

// --- the answers of peer-recv ---

func fakeVirsh(t *testing.T, n *tnode, body string) {
	p := n.abs("fake-virsh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.cfg.Tools["virsh"] = p
}

func TestPeerTookOver(t *testing.T) {
	standby := func(meta string) string { return domXML(meta) }
	rec := func(gen string) string {
		return `<virticle:meta ` + ns + `><virticle:failed-over from-node="1" from-name="n3" at="5" planned="no" gen="` + gen + `"/></virticle:meta>`
	}
	cases := []struct {
		name  string
		xml   string
		asker string
		want  string
	}{
		{"took over, asker never did", standby(rec("1")), "0", "@@took-over 1"},
		{"same generation", standby(rec("1")), "1", "@@not-took-over"},
		{"asker took over later", standby(rec("1")), "2", "@@not-took-over"},
		{"asker is older", standby(rec("3")), "2", "@@took-over 3"},
		{"no record, a replica or the old copy", standby(`<virticle:meta ` + ns + `><virticle:replica dir="/x" src-gen="0"/></virticle:meta>`), "0", "@@not-took-over"},
		{"no metadata", standby(""), "0", "@@not-took-over"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newNode(t)
			os.WriteFile(n.abs("dom.xml"), []byte(tc.xml), 0o644)
			fakeVirsh(t, n, "cat '"+n.abs("dom.xml")+"'")
			got := n.ok("virticle-peer 1 took-over "+gUUID+" "+tc.asker+"\n", "peer-recv")
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
	// the VM is not there at all: nothing took over
	n := newNode(t)
	fakeVirsh(t, n, "echo 'error: failed to get domain' >&2; exit 1")
	if got := n.ok("virticle-peer 1 took-over "+gUUID+" 0\n", "peer-recv"); got != "@@not-took-over" {
		t.Errorf("absent VM: %q", got)
	}
	// libvirt broken: an error, which the asker treats as "start"
	fakeVirsh(t, n, "echo 'error: cannot connect' >&2; exit 1")
	n.denied("libvirt down", "virticle-peer 1 took-over "+gUUID+" 0\n", "peer-recv")
	// malformed headers are refused
	for _, h := range []string{"took-over nonsense 0", "took-over " + gUUID, "took-over " + gUUID + " x", "vm-state ;id", "vm-state " + gUUID + " 1"} {
		n.refused(h, "virticle-peer 1 "+h+"\n", "peer-recv")
	}
}

func TestPeerVMState(t *testing.T) {
	for virshOut, want := range map[string]string{"running": "@@vm running", "shut off": "@@vm shutoff", "paused": "@@vm paused", "weird; id": "@@vm unknown"} {
		n := newNode(t)
		fakeVirsh(t, n, "echo '"+virshOut+"'")
		if got := n.ok("virticle-peer 1 vm-state "+gUUID+"\n", "peer-recv"); got != want {
			t.Errorf("%q: got %q want %q", virshOut, got, want)
		}
	}
	n := newNode(t)
	fakeVirsh(t, n, "echo 'error: failed to get domain' >&2; exit 1")
	if got := n.ok("virticle-peer 1 vm-state "+gUUID+"\n", "peer-recv"); got != "@@vm absent" {
		t.Errorf("absent: %q", got)
	}
}

func TestPeerAskVerb(t *testing.T) {
	n, _ := guardNode(t, "@@vm running", 0)
	if got := n.ok("", "peer-ask", "10.0.0.3", "22", "virticle", "vm-state", gUUID); got != "@@vm running" {
		t.Errorf("got %q", got)
	}
	hdr, _ := os.ReadFile(n.abs("header"))
	if strings.TrimSpace(string(hdr)) != "virticle-peer 1 vm-state "+gUUID {
		t.Errorf("header %q", hdr)
	}
	n.ok("", "peer-ask", "10.0.0.3", "22", "virticle", "took-over", gUUID, "2")
	hdr, _ = os.ReadFile(n.abs("header"))
	if strings.TrimSpace(string(hdr)) != "virticle-peer 1 took-over "+gUUID+" 2" {
		t.Errorf("header %q", hdr)
	}
	for _, bad := range [][]string{
		{"-oProxyCommand=x", "22", "virticle", "vm-state", gUUID},
		{"10.0.0.3", "22", "virticle", "recv", gUUID},
		{"10.0.0.3", "22", "virticle", "vm-state", "x;y"},
		{"10.0.0.3", "22", "virticle", "took-over", gUUID},
		{"10.0.0.3", "22", "virticle", "took-over", gUUID, "x"},
	} {
		n.refused(strings.Join(bad, " "), "", append([]string{"peer-ask"}, bad...)...)
	}
	// the peer cannot be reached
	n2, _ := guardNode(t, "", 255)
	n2.denied("unreachable", "", "peer-ask", "10.0.0.3", "22", "virticle", "vm-state", gUUID)
}

// --- vm-probe ---

func TestVMProbe(t *testing.T) {
	n := newNode(t)
	os.MkdirAll(n.abs("sys/class/net/br0"), 0o755)
	arp := n.abs("fake-arping")
	os.WriteFile(arp, []byte(`#!/bin/sh
for a; do ip=$a; case "$a" in -D) echo "duplicate address detection must not be used" >&2; exit 2;; esac; done
if [ "$ip" = 10.0.0.5 ]; then echo "ARPING 10.0.0.5 from 0.0.0.0 br0"; echo "Unicast reply from 10.0.0.5 [52:54:00:AA:BB:CC]  0.7ms"; echo "Sent 1 probes (1 broadcast(s))"; exit 1; fi
echo "Sent 2 probes"; exit 0
`), 0o755)
	n.cfg.Tools["arping"] = arp
	// the bridge has no IPv4 address of this node: nothing to ask from
	noip := n.abs("fake-ip-none")
	os.WriteFile(noip, []byte("#!/bin/sh\ntrue\n"), 0o755)
	n.cfg.Tools["ip"] = noip
	if got := n.ok("", "vm-probe", "br0", "10.0.0.5"); got != "@@noaddr" {
		t.Errorf("no address on the bridge: %q", got)
	}
	hasip := n.abs("fake-ip-addr")
	os.WriteFile(hasip, []byte("#!/bin/sh\necho '5: br0    inet 10.0.0.2/24 scope global br0'\n"), 0o755)
	n.cfg.Tools["ip"] = hasip
	got := n.ok("", "vm-probe", "br0", "10.0.0.5", "10.0.0.6")
	want := "@@tool arping\n@@reply 10.0.0.5 52:54:00:aa:bb:cc\n@@silent 10.0.0.6"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := n.ok("", "vm-probe", "br9", "10.0.0.5"); got != "@@nobridge" {
		t.Errorf("no such bridge: %q", got)
	}
	// without arping: ping, then the neighbour table
	n.cfg.Tools["arping"] = ""
	n.cfg.Tools["ping"] = "/bin/true"
	ip := n.abs("fake-ip")
	os.WriteFile(ip, []byte("#!/bin/sh\ncase \"$1\" in -4) echo '5: br0    inet 10.0.0.2/24 scope global br0'; exit 0;; esac\ncase \"$3\" in 10.0.0.5) echo '10.0.0.5 dev br0 lladdr 52:54:00:aa:bb:cc REACHABLE';; 10.0.0.6) echo '10.0.0.6 dev br0  FAILED';; esac\n"), 0o755)
	n.cfg.Tools["ip"] = ip
	got = n.ok("", "vm-probe", "br0", "10.0.0.5", "10.0.0.6")
	want = "@@tool ip-neigh\n@@reply 10.0.0.5 52:54:00:aa:bb:cc\n@@silent 10.0.0.6"
	if got != want {
		t.Errorf("fallback: got\n%s\nwant\n%s", got, want)
	}
	n.cfg.Tools["ping"] = ""
	if got := n.ok("", "vm-probe", "br0", "10.0.0.5"); got != "@@tool none" {
		t.Errorf("no tool: %q", got)
	}
	for _, bad := range [][]string{{"br0;x", "10.0.0.5"}, {"br0", "10.0.0.5; id"}, {"br0", "fe80::1"}, {"br0"}, {"-x", "10.0.0.5"}} {
		n.refused(strings.Join(bad, " "), "", append([]string{"vm-probe"}, bad...)...)
	}
}
