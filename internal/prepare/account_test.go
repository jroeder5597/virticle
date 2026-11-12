package prepare

import (
	"strings"
	"testing"
)

// The service account: names, the controller package's account, upgrade and
// idempotence.

const (
	ctlAcct    = "acct=virticle:x:990:990:Virticle controller:/var/lib/virticle:/sbin/nologin\nacct_groups=virticle\nacct_home_owner=virticle\n"
	oldAcct    = "acct=virticle:x:990:990:" + AccountComment + ":/var/lib/virticle:/bin/sh\nacct_groups=virticle libvirt kvm\nacct_home_owner=virticle\ngroup_libvirt=yes\ngroup_kvm=yes\n"
	nodeAcct   = "acct=virticle-node:x:991:991:" + AccountComment + ":/var/lib/virticle-node:/bin/sh\nacct_groups=virticle-node libvirt kvm\nacct_home_owner=virticle-node\ngroup_libvirt=yes\ngroup_kvm=yes\n"
	accountSet = "Account virticle-node is already set up\n@@unchanged"
)

// q is how a name appears inside the quoted command the runner sends.
func q(name string) string { return `'\''` + name + `'\''` }

// hostWithAccounts answers the account probe from a table (account name to
// the probe's output); an account not in the table does not exist.
func hostWithAccounts(changed bool, accts map[string]string) *fakeNode {
	return hostOnDebian(changed, func(cmd, stdin string) (string, bool, error) {
		if !strings.Contains(cmd, "acct_groups") {
			return "", false, nil
		}
		for name, out := range accts {
			if strings.Contains(cmd, "set -- "+q(name)) {
				return out, true, nil
			}
		}
		return "acct=\n", true, nil
	})
}

func accountCommands(n *fakeNode) []string {
	var out []string
	for _, c := range n.log {
		if strings.Contains(c, "useradd") || strings.Contains(c, "usermod") {
			out = append(out, c)
		}
	}
	return out
}

func logOf(n *fakeNode) string { return strings.Join(n.log, "\n") }

// (a) a fresh host gets virticle-node
func TestFreshHostCreatesTheNodeAccount(t *testing.T) {
	n := hostWithAccounts(true, nil)
	r := &Runner{N: n}
	rows := r.Prepare(Options{ControllerKey: testKey}, nil, func() []Row { return nil })
	if r.User != "virticle-node" {
		t.Errorf("resolved user %q", r.User)
	}
	if a := find(rows, "account"); a.Status != Info || !strings.Contains(a.Detail, "The account virticle-node will be created") {
		t.Errorf("check row: %+v", a)
	}
	cmds := strings.Join(accountCommands(n), "\n")
	for _, w := range []string{"u=" + q("virticle-node"), "home=" + q("/var/lib/virticle-node"), "useradd --system"} {
		if !strings.Contains(cmds, w) {
			t.Errorf("the account step lacks %q:\n%s", w, cmds)
		}
	}
	if strings.Contains(cmds, "u="+q("virticle")) {
		t.Error("the controller's account was addressed")
	}
	if !strings.Contains(logOf(n), "virticle-node ALL=(root) NOPASSWD:") {
		t.Error("the sudo rule is not for virticle-node")
	}
}

// (b) the controller package's account is never touched
func TestControllerPackageAccountIsLeftAlone(t *testing.T) {
	n := hostWithAccounts(true, map[string]string{"virticle": ctlAcct})
	r := &Runner{N: n}
	rows := r.Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r.User != "virticle-node" || HasError(rows) {
		t.Errorf("user %q, rows %+v", r.User, rows)
	}
	for _, c := range n.log {
		if strings.Contains(c, "u="+q("virticle")+"\n") || strings.Contains(c, "virticle ALL=(root)") {
			t.Errorf("a step touched the controller's account:\n%s", c)
		}
	}
	// Asking for that name explicitly is refused with the fix.
	rows, _, _ = (&Runner{N: hostWithAccounts(true, map[string]string{"virticle": ctlAcct})}).Check(Options{ControllerKey: testKey, User: "virticle"})
	if a := find(rows, "account"); a.Status != Error || !strings.Contains(a.Detail, "controller") || !strings.Contains(a.Detail, DefaultUser) {
		t.Errorf("explicit virticle: %+v", a)
	}
}

// (c) a marked virticle from an earlier version is reused
func TestAccountFromAnEarlierVersionIsReused(t *testing.T) {
	n := hostWithAccounts(false, map[string]string{"virticle": oldAcct})
	r := &Runner{N: n}
	rows := r.Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r.User != "virticle" {
		t.Errorf("resolved user %q, want the marked virticle", r.User)
	}
	if a := find(rows, "account"); a.Status != OK || !strings.Contains(a.Detail, "earlier version") {
		t.Errorf("row: %+v", a)
	}
	if strings.Contains(logOf(n), "virticle-node ALL=") || strings.Contains(logOf(n), "u="+q("virticle-node")) {
		t.Error("a second account was made")
	}
	if !strings.Contains(logOf(n), "virticle ALL=(root) NOPASSWD:") {
		t.Error("the sudo rule is not for the kept account")
	}
}

// (d) a marked virticle-node changes nothing; a broken one is repaired
func TestMarkedNodeAccountIsIdempotentAndRepaired(t *testing.T) {
	n := hostWithAccounts(false, map[string]string{"virticle-node": nodeAcct, "virticle": ctlAcct})
	n.handlers = append([]func(string, string) (string, bool, error){func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "useradd --system") {
			return accountSet, true, nil
		}
		return "", false, nil
	}}, n.handlers...)
	r := &Runner{N: n}
	rows := r.Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r.User != "virticle-node" {
		t.Errorf("user %q", r.User)
	}
	if a := find(rows, "account"); a.Status != OK || !strings.Contains(a.Detail, "exists and is set up") {
		t.Errorf("check row: %+v", a)
	}
	if a := find(rows, "do-account"); a.Status != OK || !strings.HasPrefix(a.Detail, "Already in place") {
		t.Errorf("step row: %+v", a)
	}
	if s := find(rows, "summary"); !strings.Contains(s.Detail, "Nothing changed") {
		t.Errorf("summary: %+v", s)
	}
	// Marked, but nologin and not in libvirt: the check says what will be fixed.
	broken := strings.NewReplacer(":/bin/sh\n", ":/sbin/nologin\n", "virticle-node libvirt kvm", "virticle-node").Replace(nodeAcct)
	rows, _, _ = (&Runner{N: hostWithAccounts(false, map[string]string{"virticle-node": broken})}).Check(Options{ControllerKey: testKey})
	a := find(rows, "account")
	if a.Status != Info || !strings.Contains(a.Detail, "Prepare will add it to the group libvirt") || !strings.Contains(a.Detail, "login shell /bin/sh") {
		t.Errorf("repair row: %+v", a)
	}
	if HasError(rows) {
		t.Errorf("errors: %+v", rows)
	}
	// The script repairs shell and groups only for an account that carries the marker.
	for _, w := range []string{`usermod -s /bin/sh`, `usermod -aG`, `[ "$marked" = 1 ] || [ "$confirm" = 1 ]`} {
		if !strings.Contains(accountCreateScript, w) {
			t.Errorf("account script lacks %q", w)
		}
	}
}

func TestUnmarkedNodeAccountIsAnErrorWithTheFix(t *testing.T) {
	foreign := strings.Replace(nodeAcct, AccountComment, "someone else", 1)
	n := hostWithAccounts(true, map[string]string{"virticle-node": foreign})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	a := find(rows, "account")
	if a.Status != Error || !strings.Contains(a.Detail, "--user OTHERNAME") {
		t.Errorf("row: %+v", a)
	}
	if len(accountCommands(n)) != 0 || find(rows, "do-packages").Status != Skipped {
		t.Error("something ran although the check failed")
	}
	// --use-existing-user keeps working, and a nologin shell says what to do.
	nologin := strings.Replace(foreign, ":/bin/sh\n", ":/sbin/nologin\n", 1)
	rows, _, _ = (&Runner{N: hostWithAccounts(true, map[string]string{"virticle-node": nologin})}).Check(Options{ControllerKey: testKey, UseExisting: true})
	if s := find(rows, "shell"); s.Status != Error || !strings.Contains(s.Detail, "login shell such as /bin/sh") || !strings.Contains(s.Detail, "let prepare create its own account") {
		t.Errorf("shell row: %+v", s)
	}
}

// (e) --minimal
func TestMinimalLeavesTheGuestToolsOut(t *testing.T) {
	for _, minimal := range []bool{false, true} {
		rows, plan, err := (&Runner{N: hostOnDebian(true, nil)}).Check(Options{ControllerKey: testKey, Minimal: minimal})
		if err != nil || HasError(rows) {
			t.Fatalf("%v %+v", err, rows)
		}
		want := strings.Join(plan.Want, " ")
		if has := strings.Contains(want, "libguestfs-tools"); has == minimal {
			t.Errorf("minimal=%v but the guest disk tools are wanted=%v", minimal, has)
		}
		if strings.Contains(want, "virtiofsd") {
			t.Error("virtiofsd is wanted")
		}
	}
}

// (f) unprepare picks the right account
func TestUnprepareRemovesTheAccountThatIsOurs(t *testing.T) {
	run := func(accts map[string]string, o Options) (*fakeNode, string) {
		n := hostWithAccounts(true, accts)
		n.handlers = append([]func(string, string) (string, bool, error){func(cmd, stdin string) (string, bool, error) {
			if strings.Contains(cmd, "userdel") {
				return "account removed\n@@changed", true, nil
			}
			return "", false, nil
		}}, n.handlers...)
		o.ControllerKey = testKey
		r := &Runner{N: n}
		r.Unprepare(o)
		return n, r.User
	}
	if _, u := run(map[string]string{"virticle-node": nodeAcct, "virticle": ctlAcct}, Options{}); u != "virticle-node" {
		t.Errorf("both exist, the node account is meant: %q", u)
	}
	n, u := run(map[string]string{"virticle": oldAcct}, Options{})
	if u != "virticle" || !strings.Contains(logOf(n), "u="+q("virticle")+"\n") {
		t.Errorf("a marked virticle from an earlier version: %q", u)
	}
	n, u = run(map[string]string{"virticle": ctlAcct}, Options{})
	if u != "virticle-node" || strings.Contains(logOf(n), "u="+q("virticle")+"\n") {
		t.Errorf("the controller's account must never be chosen: %q", u)
	}
	if _, u := run(map[string]string{"virticle": oldAcct}, Options{User: "other"}); u != "other" {
		t.Errorf("a given name is used as it is: %q", u)
	}
	if !strings.Contains(unprepareScript, `getent passwd "$u" | grep -q "`+AccountComment+`"`) {
		t.Error("the removal script must check the marker before userdel")
	}
}
