package nodecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var badDirs = []string{
	"", "relative/path", "/", "/etc", "/etc/", "/etc/pki/qemu/", "/etc/pki/../shadow", "/etc/pki/qemu/..",
	"/etc//pki/qemu", "/etc/pki/./qemu", "/etc/pki/qemu x", "/etc/pki/qemu;id", "/etc/pki/$(id)", "/etc/pki/`id`",
	"/tmp/x", "/var/lib/libvirt/images", "/etc/pki/other", "/etc/pki/qemu\n/etc/shadow", "/etc/pki/qemu'", "-rf",
}

func TestMigtlsDirectoryValidation(t *testing.T) {
	n := newNode(t)
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
	if n.exists("etc/pki") {
		t.Error("a refused directory was created")
	}
}

func TestMigtlsDirectoryMustBeTheConfiguredOne(t *testing.T) {
	n := newNode(t)
	conf := "# comment\n#migrate_tls_x509_cert_dir = \"/etc/pki/commented\"\nmigrate_tls_x509_cert_dir = \"/etc/pki/mig\"\ndefault_tls_x509_cert_dir=\"/etc/pki/def\"\n"
	n.put("etc/libvirt/qemu.conf", conf)
	n.ok("", "migtls-state", "/etc/pki/mig")
	n.refused("default is shadowed by the migrate setting", "", "migtls-state", "/etc/pki/def")
	n.refused("the built-in default is not configured", "", "migtls-state", "/etc/pki/qemu")
	n.put("etc/libvirt/qemu.conf", "default_tls_x509_cert_dir = /etc/pki/def\n")
	n.ok("", "migtls-state", "/etc/pki/def")
	n.put("etc/libvirt/qemu.conf", "default_tls_x509_cert_dir = /etc/pki/one # note\ndefault_tls_x509_cert_dir = /etc/pki/two\n")
	n.ok("", "migtls-state", "/etc/pki/two")
	n.refused("the earlier value", "", "migtls-state", "/etc/pki/one")
	if err := os.Remove(n.abs("etc/libvirt/qemu.conf")); err != nil {
		t.Fatal(err)
	}
	n.ok("", "migtls-state", "/etc/pki/qemu")
}

func TestMigtlsSymlinksAreRefused(t *testing.T) {
	n := newNode(t)
	target := n.abs("elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
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
	// The certificate directory itself as a link.
	os.Remove(n.abs("etc/pki/qemu/ca-cert.pem.virticle-tmp"))
	os.RemoveAll(n.abs("etc/pki/qemu"))
	if err := os.Symlink(target, n.abs("etc/pki/qemu")); err != nil {
		t.Fatal(err)
	}
	n.refused("the directory is a link", "x", "migtls-put", "/etc/pki/qemu", "ca-cert.pem", "0:0", "0644")
	n.refused("state of a link", "", "migtls-state", "/etc/pki/qemu")
	n.refused("rmdir of a link", "", "migtls-rmdir", "/etc/pki/qemu", "/etc/pki/qemu")
	if _, err := os.Stat(target); err != nil {
		t.Error("rmdir followed the link")
	}
}

func TestMigtlsPutValidation(t *testing.T) {
	n := newNode(t)
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
	if n.exists("etc/pki/qemu/ca-cert.pem.virticle-tmp") {
		t.Error("an oversized temporary file was left behind")
	}
	n.ok("cert", "migtls-put", dir, "ca-cert.pem", "0:0", "0644")
	n.ok("key", "migtls-put", dir, "server-key.pem", "libvirt-qemu:0", "0400")
	n.ok("key", "migtls-put", dir, "client-key.pem", "107:0", "0400")
	n.ok(strings.Repeat("a", 65536), "migtls-put", dir, "ca-cert.pem", "0:0", "0644")
	fi, err := os.Stat(n.abs("etc/pki/qemu/server-key.pem.virticle-tmp"))
	if err != nil || fi.Mode().Perm() != 0o400 {
		t.Errorf("key temp file: %v %v", fi, err)
	}
	if fi, _ = os.Stat(n.abs("etc/pki/qemu/ca-cert.pem.virticle-tmp")); fi.Size() != 65536 {
		t.Errorf("a replaced temporary file: %d", fi.Size())
	}
}

func putAll(n *tnode, dir string) {
	n.t.Helper()
	for _, f := range migFiles {
		mode := "0644"
		if strings.HasSuffix(f, "-key.pem") {
			mode = "0400"
		}
		n.ok("content of "+f, "migtls-put", dir, f, "0:0", mode)
	}
}

func TestMigtlsInstallRoundTrip(t *testing.T) {
	n := newNode(t)
	dir := "/etc/pki/qemu"
	n.ok("", "migtls-mkdir", dir)
	st := n.ok("", "migtls-state", dir)
	if !strings.Contains(st, "@@dir /etc/pki/qemu 1") || !strings.HasSuffix(st, "@@end") || strings.Contains(st, "@@file") {
		t.Errorf("empty state: %s", st)
	}
	if !strings.HasPrefix(st, "@@dir /etc 1\n@@dir /etc/pki 1\n@@dir /etc/pki/qemu 1\n@@content ca\n") {
		t.Errorf("state layout: %s", st)
	}
	n.refused("commit without the temporary files", "", "migtls-commit", dir)
	putAll(n, dir)
	// An existing foreign file is kept as a backup, never overwritten.
	n.put("etc/pki/qemu/ca-cert.pem", "old ca")
	n.refused("unknown backup name", "", "migtls-commit", dir, "backup:shadow")
	n.refused("unknown option", "", "migtls-commit", dir, "--force")
	n.ok("", "migtls-commit", dir, "backup:ca-cert.pem", "restorecon")
	for _, f := range []string{"ca-cert.pem", "server-key.pem", ".virticle-migration.json", "ca-cert.pem.virticle-orig"} {
		if !n.exists("etc/pki/qemu/" + f) {
			t.Errorf("missing %s", f)
		}
	}
	if b, _ := os.ReadFile(n.abs("etc/pki/qemu/ca-cert.pem")); string(b) != "content of ca-cert.pem" {
		t.Errorf("ca-cert.pem = %q", b)
	}
	if n.exists("etc/pki/qemu/ca-cert.pem.virticle-tmp") {
		t.Error("temporary file left behind")
	}
	st = n.ok("", "migtls-state", dir)
	if !strings.Contains(st, "@@file ca-cert.pem ") || !strings.Contains(st, "@@content ca\ncontent of ca-cert.pem") || !strings.Contains(st, "@@content ls\n.virticle-migration.json\nca-cert.pem\n") {
		t.Errorf("state after install: %s", st)
	}
	// A second commit would overwrite the backup: refused.
	putAll(n, dir)
	n.put("etc/pki/qemu/ca-cert.pem", "again")
	n.refused("backup exists", "", "migtls-commit", dir, "backup:ca-cert.pem")
	n.ok("", "migtls-discard", dir)
	if n.exists("etc/pki/qemu/server-key.pem.virticle-tmp") {
		t.Error("discard left a temporary file")
	}
	// Removal: only the allow-listed names, restore needs the backup.
	n.refused("path in rm", "", "migtls-remove", dir, "rm:../../shadow")
	n.refused("unknown op", "", "migtls-remove", dir, "chmod:ca-cert.pem")
	n.refused("restore without backup", "", "migtls-remove", dir, "restore:server-key.pem")
	n.ok("", "migtls-remove", dir, "rm:ca-cert.pem", "restore:ca-cert.pem", "rm:server-key.pem", "rm:.virticle-migration.json", "rm:client-cert.pem", "rm:server-cert.pem")
	if b, _ := os.ReadFile(n.abs("etc/pki/qemu/ca-cert.pem")); string(b) != "old ca" {
		t.Errorf("restored ca-cert.pem = %q", b)
	}
	if n.exists("etc/pki/qemu/server-key.pem") {
		t.Error("server-key.pem not removed")
	}
	// The legacy marker is removed by commit when asked.
	n.put("etc/pki/qemu/.virt-proj-migration.json", "{}")
	putAll(n, dir)
	n.ok("", "migtls-commit", dir, "rm-legacy")
	if n.exists("etc/pki/qemu/.virt-proj-migration.json") {
		t.Error("rm-legacy kept the legacy marker")
	}
}

func TestMigtlsRmdir(t *testing.T) {
	n := newNode(t)
	dir := "/etc/pki/qemu"
	n.ok("", "migtls-mkdir", dir)
	n.refused("not a parent", "", "migtls-rmdir", dir, "/etc/ssh")
	n.refused("a sibling", "", "migtls-rmdir", dir, "/etc/pki/qemu2")
	n.refused("/etc itself", "", "migtls-rmdir", dir, "/etc")
	n.refused("a child", "", "migtls-rmdir", dir, "/etc/pki/qemu/sub")
	n.refused("dot dot", "", "migtls-rmdir", dir, "/etc/pki/qemu/..")
	n.put("etc/pki/qemu/keep", "")
	n.refused("not empty", "", "migtls-rmdir", dir, dir)
	os.Remove(n.abs("etc/pki/qemu/keep"))
	n.ok("", "migtls-rmdir", dir, dir)
	n.ok("", "migtls-rmdir", dir, "/etc/pki")
	if n.exists("etc/pki") {
		t.Error("/etc/pki still there")
	}
	n.refused("already gone", "", "migtls-rmdir", dir, dir)
}

func TestGPUUsersValidation(t *testing.T) {
	n := newNode(t)
	for _, a := range []string{"", "0000:01:00", "0000:01:00.8", "0000:01:00.0;id", "../../x", "0000:01:00.0 ", "0000:0g:00.0", "$(id)", "0000:01:00.0/../.."} {
		n.refused("gpu "+a, "", "gpu-users", a)
	}
	if out := n.ok("", "gpu-users", "0000:01:00.0"); out != "ok" {
		t.Errorf("gpu-users on a missing card: %q", out)
	}
}

func TestGPUUsersFindsHolders(t *testing.T) {
	n := newNode(t)
	addr := "0000:01:00.0"
	dev := n.abs("sys/bus/pci/devices/" + addr)
	if err := os.MkdirAll(dev+"/drm/card1", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dev+"/drm/renderD128", 0o755); err != nil {
		t.Fatal(err)
	}
	proc := func(pid, comm string, fds ...string) {
		d := n.abs("proc/" + pid)
		os.MkdirAll(d+"/fd", 0o755)
		os.WriteFile(d+"/comm", []byte(comm+"\n"), 0o644)
		for i, t := range fds {
			os.Symlink(t, d+"/fd/"+string(rune('0'+i)))
		}
	}
	proc("100", "qemu-system-x86", "/dev/null", "/dev/dri/card1")
	proc("200", "Xorg", "/dev/dri/renderD128")
	proc("300", "bash", "/dev/dri/card10", "/dev/null")
	proc("400", "other", "/dev/dri/card0")
	os.MkdirAll(n.abs("proc/self"), 0o755)
	if got := n.ok("", "gpu-users", addr); got != "qemu-system-x86\nXorg\nok" {
		t.Errorf("holders: %q", got)
	}
	// nvidia: every /dev/nvidia* is a hit
	os.RemoveAll(dev + "/drm")
	drv := n.abs("sys/bus/pci/drivers/nvidia")
	os.MkdirAll(drv, 0o755)
	os.Symlink(drv, dev+"/driver")
	proc("500", "cuda-app", "/dev/nvidia0")
	if got := n.ok("", "gpu-users", addr); got != "cuda-app\nok" {
		t.Errorf("nvidia holders: %q", got)
	}
}
