package nodecmd

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

// fakeTools answers the tool calls of the update verbs by the first words of the command line.
type fakeTools struct {
	calls  []string
	answer func(argv []string) (string, error)
}

func (f *fakeTools) run(argv []string) (string, error) {
	f.calls = append(f.calls, strings.Join(argv, " "))
	return f.answer(argv)
}

func osRelease(t *testing.T, n *tnode, body string) {
	t.Helper()
	if err := os.MkdirAll(n.abs("etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.abs("etc/os-release"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, n *tnode) nodeproto.UpdateStatus {
	t.Helper()
	var st nodeproto.UpdateStatus
	if err := json.Unmarshal([]byte(n.ok("", "update-status")), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestUpdateStatusNone(t *testing.T) {
	n := newNode(t)
	n.cfg.Net = &NetHooks{Run: (&fakeTools{answer: func([]string) (string, error) { return "", nil }}).run}
	if st := status(t, n); st.State != nodeproto.UpdateNone {
		t.Fatalf("state %q", st.State)
	}
}

func TestUpdateStartRefusesWhileRunning(t *testing.T) {
	n := newNode(t)
	f := &fakeTools{answer: func(argv []string) (string, error) { return "", nil }} // is-active succeeds: a unit is running
	n.cfg.Net = &NetHooks{Run: f.run}
	out, code := n.run("", "update-start")
	if code == 0 || !strings.Contains(out, "already running") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestUpdateStartStartsUnit(t *testing.T) {
	n := newNode(t)
	f := &fakeTools{answer: func(argv []string) (string, error) {
		if argv[0] == "systemctl" {
			return "", errors.New("inactive")
		}
		return "", nil
	}}
	n.cfg.Net = &NetHooks{Run: f.run}
	n.cfg.Self = "/x/virticle-node"
	n.ok("", "update-start")
	last := f.calls[len(f.calls)-1]
	if !strings.HasPrefix(last, "systemd-run") || !strings.HasSuffix(last, "/x/virticle-node update-run") || !strings.Contains(last, "--unit=virticle-update") {
		t.Fatalf("last call %q", last)
	}
	// The unit is not running (is-active fails), so a status that still says running means it died.
	if st := status(t, n); st.State != nodeproto.UpdateFailed || !strings.Contains(st.Error, "stopped before it finished") {
		t.Fatalf("%+v", st)
	}
}

const dnfPending = "glibc.x86_64   2.39-2   baseos\nnano.x86_64   8.1-1   baseos\nqemu-kvm-core.x86_64 10.1-2 appstream\n"

func TestUpdateRunDnf(t *testing.T) {
	n := newNode(t)
	osRelease(t, n, "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n")
	checks := 0
	f := &fakeTools{}
	f.answer = func(argv []string) (string, error) {
		switch {
		case argv[0] == "dnf" && argv[2] == "check-update":
			checks++
			if checks == 1 {
				return dnfPending, &Error{Code: 100}
			}
			return "nano.x86_64   8.1-1   baseos\n", &Error{Code: 100} // held back
		case argv[0] == "uname":
			return "6.12.0-1.el10.x86_64\n", nil
		case argv[0] == "rpm":
			return "6.12.0-1.el10.x86_64\n", nil
		}
		return "", nil
	}
	n.cfg.Net = &NetHooks{Run: f.run}
	n.ok("", "update-run")
	st := status(t, n)
	if st.State != nodeproto.UpdateOK || st.Family != "dnf" {
		t.Fatalf("%+v", st)
	}
	if strings.Join(st.Updated, ",") != "glibc.x86_64,qemu-kvm-core.x86_64" || strings.Join(st.HeldBack, ",") != "nano.x86_64" {
		t.Fatalf("updated %v held %v", st.Updated, st.HeldBack)
	}
	if !st.RebootNeeded || !strings.Contains(st.RebootWhy, "core system library") {
		t.Fatalf("reboot %v %q", st.RebootNeeded, st.RebootWhy)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"dnf -q check-update --refresh", "dnf -y upgrade"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no call %q in\n%s", want, joined)
		}
	}
}

func TestUpdateRunDnfRepoFailure(t *testing.T) {
	n := newNode(t)
	osRelease(t, n, "ID=rocky\n")
	f := &fakeTools{answer: func(argv []string) (string, error) {
		if argv[0] == "dnf" {
			return "Error: Failed to download metadata for repo 'extras'\n", &Error{Code: 1}
		}
		return "", nil
	}}
	n.cfg.Net = &NetHooks{Run: f.run}
	_, code := n.run("", "update-run")
	if code == 0 {
		t.Fatal("a failing repository must fail the run")
	}
	st := status(t, n)
	if st.State != nodeproto.UpdateFailed || !strings.Contains(st.Error, "cannot read its repositories") {
		t.Fatalf("%+v", st)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "upgrade") {
			t.Fatalf("nothing may be installed when the repositories cannot be read: %v", f.calls)
		}
	}
}

func TestUpdateRunAptUsesNoQuestions(t *testing.T) {
	n := newNode(t)
	osRelease(t, n, "ID=debian\n")
	lists := 0
	f := &fakeTools{}
	f.answer = func(argv []string) (string, error) {
		switch {
		case argv[0] == "apt" && argv[1] == "list":
			lists++
			if lists == 1 {
				return "Listing...\nlibssl3/stable-security 3.5.1-1 amd64 [upgradable from: 3.5.0-1]\nvim/stable 9.1 amd64 [upgradable from: 9.0]\n", nil
			}
			return "Listing...\nvim/stable 9.1 amd64 [upgradable from: 9.0]\n", nil
		case argv[0] == "uname":
			return "6.12.43+deb13-amd64\n", nil
		case argv[0] == "sh":
			return "6.12.43+deb13-amd64\n", nil
		}
		return "", nil
	}
	n.cfg.Net = &NetHooks{Run: f.run}
	n.ok("", "update-run")
	st := status(t, n)
	if st.State != nodeproto.UpdateOK || st.Family != "apt" || strings.Join(st.Updated, ",") != "libssl3" || strings.Join(st.HeldBack, ",") != "vim" {
		t.Fatalf("%+v", st)
	}
	var up string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "apt-get -y") {
			up = c
		}
	}
	for _, want := range []string{"--with-new-pkgs", "--force-confold", "--force-confdef", "upgrade"} {
		if !strings.Contains(up, want) {
			t.Errorf("apt-get call lacks %q: %q", want, up)
		}
	}
	if st.RebootNeeded {
		t.Errorf("libssl3 alone must not ask for a restart: %q", st.RebootWhy)
	}
}

func TestUpdateRunAptFailedSource(t *testing.T) {
	n := newNode(t)
	osRelease(t, n, "ID=ubuntu\n")
	f := &fakeTools{answer: func(argv []string) (string, error) {
		if argv[0] == "apt-get" && argv[1] == "update" {
			return "Err:3 http://example.invalid/ubuntu noble InRelease\n  Could not resolve\nW: Failed to fetch http://example.invalid/ubuntu/dists/noble/InRelease\n", nil // exits 0, as apt does
		}
		return "", nil
	}}
	n.cfg.Net = &NetHooks{Run: f.run}
	if _, code := n.run("", "update-run"); code == 0 {
		t.Fatal("a source that cannot be read must fail the run")
	}
	st := status(t, n)
	if st.State != nodeproto.UpdateFailed || !strings.Contains(st.Error, "Err:3") {
		t.Fatalf("%+v", st)
	}
}

func TestUpdateRunUnsupportedSystem(t *testing.T) {
	n := newNode(t)
	osRelease(t, n, "ID=arch\n")
	n.cfg.Net = &NetHooks{Run: (&fakeTools{answer: func([]string) (string, error) { return "", nil }}).run}
	if _, code := n.run("", "update-run"); code == 0 {
		t.Fatal("must fail")
	}
	if st := status(t, n); !strings.Contains(st.Error, "not based on dnf or apt") {
		t.Fatalf("%+v", st)
	}
}

func TestRebootVerdictKernelNewerAndFlag(t *testing.T) {
	n := newNode(t)
	f := &fakeTools{answer: func(argv []string) (string, error) {
		if argv[0] == "uname" {
			return "6.12.0-1.el10.x86_64\n", nil
		}
		if argv[0] == "rpm" {
			return "6.12.0-1.el10.x86_64\n6.12.0-9.el10.x86_64\n", nil
		}
		return "", nil
	}}
	n.cfg.Net = &NetHooks{Run: f.run}
	c := &Ctx{Cfg: n.cfg}
	ok, why := c.rebootVerdict("dnf", nil)
	if !ok || !strings.Contains(why, "newer kernel") {
		t.Fatalf("%v %q", ok, why)
	}
	if err := os.MkdirAll(n.abs("var/run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.abs("var/run/reboot-required"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.answer = func(argv []string) (string, error) { return "6.12.43\n", nil }
	ok, why = c.rebootVerdict("apt", nil)
	if !ok || !strings.Contains(why, "reboot-required") {
		t.Fatalf("%v %q", ok, why)
	}
}

func TestNodeRebootSchedulesUnit(t *testing.T) {
	n := newNode(t)
	f := &fakeTools{answer: func(argv []string) (string, error) {
		if argv[0] == "systemctl" {
			return "", errors.New("inactive")
		}
		return "", nil
	}}
	n.cfg.Net = &NetHooks{Run: f.run}
	n.ok("", "node-reboot")
	last := f.calls[len(f.calls)-1]
	if !strings.Contains(last, "--on-active=5s") || !strings.HasSuffix(last, "systemctl reboot") {
		t.Fatalf("%q", last)
	}
	f.answer = func([]string) (string, error) { return "", nil } // an update is running
	if out, code := n.run("", "node-reboot"); code == 0 || !strings.Contains(out, "still running") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestSelfcheckRows(t *testing.T) {
	n := newNode(t)
	var modes []string
	f := &fakeTools{answer: func(argv []string) (string, error) {
		modes = append(modes, argv[len(argv)-1])
		switch {
		case strings.Contains(argv[2], "printf 'ok\\tlibvirt"):
			return "ok\tlibvirt\tlibvirt answers\n", nil
		case strings.Contains(argv[2], "virt_use_nfs"):
			return "fixed\tselinux\tturned on\n", nil
		case strings.Contains(argv[2], "svc=virticle-node"):
			return "problem\tfirewall\tgone\n", nil
		}
		return "", errors.New("did not run")
	}}
	n.cfg.Net = &NetHooks{Run: f.run}
	out := n.ok("", "selfcheck", "repair")
	rows := nodeproto.ParseCheckRows(out)
	by := map[string]string{}
	for _, r := range rows {
		by[r.ID] = r.Status
	}
	if by["libvirt"] != "ok" || by["selinux"] != "fixed" || by["firewall"] != "problem" || by["services"] != "problem" {
		t.Fatalf("rows %v\n%s", by, out)
	}
	for _, m := range modes {
		if m != "repair" {
			t.Fatalf("mode %q", m)
		}
	}
	n.refused("bad mode", "", "selfcheck", "everything")
}
