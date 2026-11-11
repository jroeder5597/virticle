package hostconn

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The helper runs under sh against a fake root directory (VIRTICLE_HELPER_ROOT,
// honoured only when not running as root). Tests that are root drop to an
// unprivileged user with setpriv, so the real /etc is never touched.
type fakeNode struct {
	t    *testing.T
	root string
	sh   string
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	root, err := os.MkdirTemp("", "virticle-helper-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "virticle-helper.sh")
	if err := os.WriteFile(script, []byte(HelperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if _, err := exec.LookPath("setpriv"); err != nil {
			t.Skip("running as root and setpriv is missing: the helper would act on the real /etc")
		}
	}
	// A tree the unprivileged user may change.
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(root, p), 0o777)
	}
	mk("etc/libvirt")
	_ = os.Chmod(filepath.Join(root, "etc"), 0o777)
	return &fakeNode{t: t, root: root, sh: script}
}

func (n *fakeNode) abs(p string) string { return filepath.Join(n.root, p) }

func (n *fakeNode) run(stdin string, args ...string) (string, int) {
	n.t.Helper()
	argv := append([]string{"sh", n.sh}, args...)
	if os.Geteuid() == 0 {
		argv = append([]string{"setpriv", "--reuid=65534", "--regid=65534", "--clear-groups"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"VIRTICLE_HELPER_ROOT=" + n.root, "PATH=/usr/bin:/bin", "HOME=" + n.root}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		n.t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), code
}

func (n *fakeNode) ok(stdin string, args ...string) string {
	n.t.Helper()
	out, code := n.run(stdin, args...)
	if code != 0 {
		n.t.Fatalf("%v: exit %d: %s", args, code, out)
	}
	return out
}

func (n *fakeNode) refused(why string, stdin string, args ...string) {
	n.t.Helper()
	out, code := n.run(stdin, args...)
	if code != 2 {
		n.t.Errorf("%s: %v: exit %d, want a refusal (2): %s", why, args, code, out)
	}
}

func TestHelperVersionMatchesEmbedded(t *testing.T) {
	n := newFakeNode(t)
	if got := n.ok("", "version"); got != HelperVersion {
		t.Errorf("version %q, embedded %q", got, HelperVersion)
	}
}

func TestHelperUnknownAndMissing(t *testing.T) {
	n := newFakeNode(t)
	n.refused("no subcommand", "")
	n.refused("unknown", "", "run")
	n.refused("a generic exec is not offered", "", "exec", "id")
	n.refused("a self-update is not offered", "", "self-update")
	n.refused("shell metacharacters as subcommand", "", "version; id")
}

var badDirs = []string{
	"", "relative/path", "/", "/etc", "/etc/", "/etc/pki/qemu/", "/etc/pki/../shadow", "/etc/pki/qemu/..",
	"/etc//pki/qemu", "/etc/pki/./qemu", "/etc/pki/qemu x", "/etc/pki/qemu;id", "/etc/pki/$(id)", "/etc/pki/`id`",
	"/tmp/x", "/var/lib/libvirt/images", "/etc/pki/other", "/etc/pki/qemu\n/etc/shadow", "/etc/pki/qemu'", "-rf",
}

func TestHelperDirectoryValidation(t *testing.T) {
	n := newFakeNode(t)
	for _, d := range badDirs {
		n.refused("state "+d, "", "migtls-state", d)
		n.refused("mkdir "+d, "", "migtls-mkdir", d)
		n.refused("discard "+d, "", "migtls-discard", d)
		n.refused("put "+d, "x", "migtls-put", d, "ca-cert.pem", "0:0", "0644")
		n.refused("commit "+d, "", "migtls-commit", d)
		n.refused("remove "+d, "", "migtls-remove", d, "rm:ca-cert.pem")
		n.refused("rmdir "+d, "", "migtls-rmdir", d, "/etc/pki")
	}
	n.refused("missing argument", "", "migtls-state")
	n.refused("extra argument", "", "migtls-state", "/etc/pki/qemu", "/etc/pki/qemu")
}

func TestHelperDirectoryMustBeTheConfiguredOne(t *testing.T) {
	n := newFakeNode(t)
	conf := "# comment\n#migrate_tls_x509_cert_dir = \"/etc/pki/commented\"\nmigrate_tls_x509_cert_dir = \"/etc/pki/mig\"\ndefault_tls_x509_cert_dir=\"/etc/pki/def\"\n"
	if err := os.WriteFile(n.abs("etc/libvirt/qemu.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	n.ok("", "migtls-state", "/etc/pki/mig")
	n.refused("default is shadowed by the migrate setting", "", "migtls-state", "/etc/pki/def")
	n.refused("the built-in default is not configured", "", "migtls-state", "/etc/pki/qemu")
	if err := os.WriteFile(n.abs("etc/libvirt/qemu.conf"), []byte("default_tls_x509_cert_dir = /etc/pki/def\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.ok("", "migtls-state", "/etc/pki/def")
	if err := os.Remove(n.abs("etc/libvirt/qemu.conf")); err != nil {
		t.Fatal(err)
	}
	n.ok("", "migtls-state", "/etc/pki/qemu")
}

func TestHelperSymlinksAreRefused(t *testing.T) {
	n := newFakeNode(t)
	if err := os.MkdirAll(n.abs("etc"), 0o777); err != nil {
		t.Fatal(err)
	}
	target := n.abs("elsewhere")
	if err := os.MkdirAll(target, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, n.abs("etc/pki")); err != nil {
		t.Fatal(err)
	}
	n.refused("directory below a symlink", "", "migtls-state", "/etc/pki/qemu")
	n.refused("mkdir below a symlink", "", "migtls-mkdir", "/etc/pki/qemu")
	if _, err := os.Stat(filepath.Join(target, "qemu")); err == nil {
		t.Error("mkdir followed the symlink")
	}
	os.Remove(n.abs("etc/pki"))
	n.ok("", "migtls-mkdir", "/etc/pki/qemu")
	// A temporary file that is a symlink is not written through.
	victim := n.abs("victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, n.abs("etc/pki/qemu/ca-cert.pem.virticle-tmp")); err != nil {
		t.Fatal(err)
	}
	n.refused("temporary file is a symlink", "evil", "migtls-put", "/etc/pki/qemu", "ca-cert.pem", "0:0", "0644")
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("wrote through a symlink: %q", b)
	}
}

func TestHelperPutValidation(t *testing.T) {
	n := newFakeNode(t)
	dir := "/etc/pki/qemu"
	n.refused("directory missing", "x", "migtls-put", dir, "ca-cert.pem", "0:0", "0644")
	n.ok("", "migtls-mkdir", dir)
	bad := [][]string{
		{"passwd", "0:0", "0644"},
		{"../ca-cert.pem", "0:0", "0644"},
		{"ca-cert.pem/x", "0:0", "0644"},
		{"ca-cert.pem.virticle-tmp", "0:0", "0644"},
		{".virt-proj-migration.json", "0:0", "0644"},
		{"ca-cert.pem", "root", "0644"},
		{"ca-cert.pem", "0:0;id", "0644"},
		{"ca-cert.pem", "$(id):0", "0644"},
		{"ca-cert.pem", "a:b", "0644"},
		{"ca-cert.pem", "0:0", "4755"},
		{"ca-cert.pem", "0:0", "0400"},
		{"server-key.pem", "0:0", "0644"},
		{"server-key.pem", "0:0", "0777"},
		{"ca-cert.pem", "", "0644"},
	}
	for _, b := range bad {
		n.refused(strings.Join(b, " "), "x", append([]string{"migtls-put", dir}, b...)...)
	}
	n.refused("too few", "x", "migtls-put", dir, "ca-cert.pem", "0:0")
	n.refused("oversized input", strings.Repeat("a", 70000), "migtls-put", dir, "ca-cert.pem", "0:0", "0644")
	if _, err := os.Stat(n.abs("etc/pki/qemu/ca-cert.pem.virticle-tmp")); err == nil {
		t.Error("an oversized temporary file was left behind")
	}
	n.ok("cert", "migtls-put", dir, "ca-cert.pem", "0:0", "0644")
	n.ok("key", "migtls-put", dir, "server-key.pem", "libvirt-qemu:0", "0400")
	n.ok("key", "migtls-put", dir, "client-key.pem", "107:0", "0400")
	fi, err := os.Stat(n.abs("etc/pki/qemu/server-key.pem.virticle-tmp"))
	if err != nil || fi.Mode().Perm() != 0o400 {
		t.Errorf("key temp file: %v %v", fi, err)
	}
}

func put(n *fakeNode, dir string) {
	n.t.Helper()
	for _, f := range strings.Fields(strings.ReplaceAll("server-key.pem client-key.pem ca-cert.pem server-cert.pem client-cert.pem .virticle-migration.json", "\n", " ")) {
		mode := "0644"
		if strings.HasSuffix(f, "-key.pem") {
			mode = "0400"
		}
		n.ok("content of "+f, "migtls-put", dir, f, "0:0", mode)
	}
}

func TestHelperInstallRoundTrip(t *testing.T) {
	n := newFakeNode(t)
	dir := "/etc/pki/qemu"
	n.ok("", "migtls-mkdir", dir)
	st := n.ok("", "migtls-state", dir)
	if !strings.Contains(st, "@@dir /etc/pki/qemu 1") || !strings.HasSuffix(st, "@@end") || strings.Contains(st, "@@file") {
		t.Errorf("empty state: %s", st)
	}
	n.refused("commit without the temporary files", "", "migtls-commit", dir)
	put(n, dir)
	// An existing foreign file is kept as a backup, never overwritten.
	if err := os.WriteFile(n.abs("etc/pki/qemu/ca-cert.pem"), []byte("old ca"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.refused("unknown backup name", "", "migtls-commit", dir, "backup:shadow")
	n.refused("unknown option", "", "migtls-commit", dir, "--force")
	n.ok("", "migtls-commit", dir, "backup:ca-cert.pem", "restorecon")
	for _, f := range []string{"ca-cert.pem", "server-key.pem", ".virticle-migration.json", "ca-cert.pem.virticle-orig"} {
		if _, err := os.Stat(n.abs("etc/pki/qemu/" + f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(n.abs("etc/pki/qemu/ca-cert.pem")); string(b) != "content of ca-cert.pem" {
		t.Errorf("ca-cert.pem = %q", b)
	}
	if _, err := os.Stat(n.abs("etc/pki/qemu/ca-cert.pem.virticle-tmp")); err == nil {
		t.Error("temporary file left behind")
	}
	st = n.ok("", "migtls-state", dir)
	if !strings.Contains(st, "@@file ca-cert.pem ") || !strings.Contains(st, "@@content ca\ncontent of ca-cert.pem") {
		t.Errorf("state after install: %s", st)
	}
	// A second commit would overwrite the backup: refused.
	put(n, dir)
	if err := os.WriteFile(n.abs("etc/pki/qemu/ca-cert.pem"), []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.refused("backup exists", "", "migtls-commit", dir, "backup:ca-cert.pem")
	n.ok("", "migtls-discard", dir)
	if _, err := os.Stat(n.abs("etc/pki/qemu/server-key.pem.virticle-tmp")); err == nil {
		t.Error("discard left a temporary file")
	}
	// Removal: only the allow-listed names, restore needs the backup.
	n.refused("path in rm", "", "migtls-remove", dir, "rm:../../shadow")
	n.refused("unknown op", "", "migtls-remove", dir, "chmod:ca-cert.pem")
	n.refused("restore without backup", "", "migtls-remove", dir, "restore:server-key.pem")
	n.ok("", "migtls-remove", dir, "rm:ca-cert.pem", "restore:ca-cert.pem", "rm:server-key.pem", "rm:.virticle-migration.json")
	if b, _ := os.ReadFile(n.abs("etc/pki/qemu/ca-cert.pem")); string(b) != "old ca" {
		t.Errorf("restored ca-cert.pem = %q", b)
	}
	if _, err := os.Stat(n.abs("etc/pki/qemu/server-key.pem")); err == nil {
		t.Error("server-key.pem not removed")
	}
}

func TestHelperRmdir(t *testing.T) {
	n := newFakeNode(t)
	dir := "/etc/pki/qemu"
	n.ok("", "migtls-mkdir", dir)
	n.refused("not a parent", "", "migtls-rmdir", dir, "/etc/ssh")
	n.refused("a sibling", "", "migtls-rmdir", dir, "/etc/pki/qemu2")
	n.refused("/etc itself", "", "migtls-rmdir", dir, "/etc")
	n.refused("a child", "", "migtls-rmdir", dir, "/etc/pki/qemu/sub")
	n.refused("dot dot", "", "migtls-rmdir", dir, "/etc/pki/qemu/..")
	if err := os.WriteFile(n.abs("etc/pki/qemu/keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	n.refused("not empty", "", "migtls-rmdir", dir, dir)
	os.Remove(n.abs("etc/pki/qemu/keep"))
	n.ok("", "migtls-rmdir", dir, dir)
	n.ok("", "migtls-rmdir", dir, "/etc/pki")
	if _, err := os.Stat(n.abs("etc/pki")); err == nil {
		t.Error("/etc/pki still there")
	}
}

func TestHelperGPUUsersValidation(t *testing.T) {
	n := newFakeNode(t)
	for _, a := range []string{"", "0000:01:00", "0000:01:00.8", "0000:01:00.0;id", "../../x", "0000:01:00.0 ", "0000:0g:00.0", "$(id)", "0000:01:00.0/../.."} {
		n.refused("gpu "+a, "", "gpu-users", a)
	}
	if out := n.ok("", "gpu-users", "0000:01:00.0"); out != "ok" {
		t.Errorf("gpu-users on a missing card: %q", out)
	}
}

func TestSudoersLine(t *testing.T) {
	got := SudoersLine("virticle")
	if !strings.Contains(got, "virticle ALL=(root) NOPASSWD: "+HelperPath+"\n") {
		t.Errorf("sudoers: %q", got)
	}
	// One rule only: the account may run exactly the helper.
	rules := 0
	for _, l := range strings.Split(got, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			rules++
		}
	}
	if rules != 1 || strings.Contains(got, "ALL=(ALL)") || strings.Contains(got, "NOPASSWD: ALL") {
		t.Errorf("sudoers must hold a single helper-only rule: %q", got)
	}
}

func TestHelperScriptShape(t *testing.T) {
	// No generic execution or self-replacement in the helper.
	for _, bad := range []string{"eval ", "exec \"$", "sh -c", "bash -c", "xargs", "$(cat)"} {
		if strings.Contains(HelperScript, bad) {
			t.Errorf("the helper contains %q", bad)
		}
	}
	if !strings.HasPrefix(HelperScript, "#!/bin/sh\n") {
		t.Error("missing shebang")
	}
}

func TestHelperCmdQuotes(t *testing.T) {
	got := HelperCmd("migtls-put", "/etc/pki/qemu", "it's", "$(id)")
	want := `sudo -n /usr/local/libexec/virticle-helper migtls-put '/etc/pki/qemu' 'it'\''s' '$(id)'`
	if got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
}

func TestPrivilegeDetection(t *testing.T) {
	v := HelperVersion
	cases := []struct {
		name, in string
		want     Privilege
		note     string
	}{
		{"root", "uid=0\nsudo=no\nuser=root", PrivRoot, "root"},
		{"root with a helper installed", "uid=0\nhelper=" + v + "\nsudo=yes\nuser=root", PrivRoot, "root"},
		{"helper", "uid=998\nhelper=" + v + "\nsudo=no\nuser=virticle", PrivHelper, "helper"},
		{"helper beats broad sudo", "uid=998\nhelper=" + v + "\nsudo=yes\nuser=virticle", PrivHelper, "broad sudo works too"},
		{"old helper, no broad sudo", "uid=998\nhelper=0\nsudo=no\nuser=virticle", PrivNone, "Prepare node"},
		{"old helper, broad sudo", "uid=998\nhelper=0\nsudo=yes\nuser=virticle", PrivSudo, "ships"},
		{"helper does not answer", "uid=998\nhelper=\nsudo=no\nuser=virticle", PrivNone, "sudoers"},
		{"broad sudo", "uid=1000\nsudo=yes\nuser=admin", PrivSudo, "sudo"},
		{"nothing", "uid=1000\nsudo=no\nuser=admin", PrivNone, "fails"},
		{"empty", "", PrivNone, ""},
	}
	for _, c := range cases {
		got, note := ParseDetect(c.in)
		if got != c.want || !strings.Contains(note, c.note) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, got, note, c.want, c.note)
		}
	}
	if !strings.Contains(DetectScript, HelperPath+" version") {
		t.Error("DetectScript does not ask the helper for its version")
	}
	if _, err := WrapRoot(PrivHelper, "id"); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("a free-form command under the helper: %v", err)
	}
	if !PrivHelper.Valid() {
		t.Error("helper is not a valid setting")
	}
}
