package hostconn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// poolNode is a fake node with one storage pool, /pools/default, and the UEFI
// NVRAM directory.
func poolNode(t *testing.T) *fakeNode {
	t.Helper()
	n := newFakeNode(t)
	for _, d := range []string{"pools/default", "pools/other", "var/lib/libvirt/qemu/nvram", "home/svc/.ssh", "etc/secret"} {
		p := n.abs(d)
		if err := os.MkdirAll(p, 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, 0o777)
	}
	n.env = []string{
		"VIRTICLE_HELPER_POOLS=/pools/default",
		"VIRTICLE_HELPER_HOME=" + n.abs("home/svc"),
		"VIRTICLE_HELPER_USER=svc",
	}
	return n
}

func (n *fakeNode) put(rel, content string) string {
	n.t.Helper()
	p := n.abs(rel)
	if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
		n.t.Fatal(err)
	}
	_ = os.Chmod(p, 0o666)
	return p
}

func (n *fakeNode) exists(rel string) bool {
	_, err := os.Lstat(n.abs(rel))
	return err == nil
}

func TestHelperRoots(t *testing.T) {
	n := poolNode(t)
	got := strings.Fields(n.ok("", "roots"))
	if strings.Join(got, ",") != "/pools/default,/var/lib/libvirt/qemu/nvram" {
		t.Errorf("roots %v", got)
	}
	// A pool that points at a system directory is not a root.
	n.env = append(n.env, "VIRTICLE_HELPER_POOLS=/etc\n/etc/secret\n/pools/other\n/nonexistent")
	got = strings.Fields(n.ok("", "roots"))
	if strings.Join(got, ",") != "/pools/other,/var/lib/libvirt/qemu/nvram" {
		t.Errorf("roots with a hostile pool list: %v", got)
	}
}

var badPaths = []string{
	"", "relative", "/", "/pools/default/", "/pools/default/../other/x", "/pools/default/./x", "/pools/default//x",
	"/pools/default/x y", "/pools/default/x;id", "/pools/default/$(id)", "/pools/default/`id`", "/pools/default/x\ny",
	"/pools/default/x'", "/pools/default/*", "-rf", "/pools/default/..",
}

func TestHelperFileVerbsRejectBadPaths(t *testing.T) {
	n := poolNode(t)
	n.put("pools/default/a", "x")
	for _, p := range badPaths {
		n.denied("stat "+p, "", "stat", p)
		n.denied("remove "+p, "", "remove", p)
		n.denied("rmtree "+p, "", "rmtree", p)
		n.denied("rmdir "+p, "", "rmdir", p)
		n.denied("move src "+p, "", "move", p, "/pools/default/new")
		n.denied("move dst "+p, "", "move", "/pools/default/a", p)
		n.denied("copy src "+p, "", "copy", p, "/pools/default/new")
		n.denied("copy dst "+p, "", "copy", "/pools/default/a", p)
		n.denied("read "+p, "", "read", p, "10")
		n.denied("write "+p, "x", "write", p, "10")
		n.denied("mkworkdir "+p, "", "mkworkdir", p, "bk")
		n.denied("chmod-work "+p, "", "chmod-work", "700", p)
		n.denied("sha256 "+p, "", "sha256", p)
		n.denied("qemu-img-info "+p, "", "qemu-img-info", p)
	}
}

func TestHelperFileVerbsStayInsideThePools(t *testing.T) {
	n := poolNode(t)
	n.put("etc/passwd", "root:x:0:0\n")
	n.put("pools/other/x", "x")
	n.put("pools/default/a", "x")
	for _, p := range []string{"/etc/passwd", "/pools/other/x", "/pools/other/new", "/etc/new", "/tmp/new", "/var/lib/libvirt/images/x"} {
		n.denied("stat "+p, "", "stat", p)
		n.denied("read "+p, "", "read", p, "100")
		n.denied("sha256 "+p, "", "sha256", p)
		n.denied("write "+p, "x", "write", p, "100")
		n.denied("remove "+p, "", "remove", p)
		n.denied("copy from "+p, "", "copy", p, "/pools/default/c")
		n.denied("copy to "+p, "", "copy", "/pools/default/a", p)
		n.denied("move to "+p, "", "move", "/pools/default/a", p)
		n.denied("move from "+p, "", "move", p, "/pools/default/c")
		n.denied("send "+p, "", "peer-send", "10.0.0.2", "22", "virticle", p, "/pools/default/c", "600")
	}
	if !n.exists("etc/passwd") || !n.exists("pools/other/x") || n.exists("pools/default/c") {
		t.Error("something outside the pool was changed or leaked in")
	}
	// A pool directory itself is not removable or movable.
	n.denied("remove the pool", "", "remove", "/pools/default")
	n.denied("rmdir the pool", "", "rmdir", "/pools/default")
	n.denied("rmtree the pool", "", "rmtree", "/pools/default")
	n.denied("move the pool", "", "move", "/pools/default", "/pools/default2")
	n.denied("remove the nvram dir", "", "remove", "/var/lib/libvirt/qemu/nvram")
}

func TestHelperFileVerbsSymlinks(t *testing.T) {
	n := poolNode(t)
	n.put("etc/passwd", "root:x:0:0\n")
	n.put("pools/default/real", "data")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("/etc/passwd", n.abs("pools/default/out-file")))
	must(os.Symlink("/etc", n.abs("pools/default/out-dir")))
	must(os.Symlink("real", n.abs("pools/default/in-file")))
	must(os.Symlink(n.abs("pools/default/real"), n.abs("pools/default/in-abs")))
	must(os.Symlink("/nonexistent", n.abs("pools/default/dangling")))
	// The symlink targets are resolved under the fake root, so make the links point there.
	must(os.Remove(n.abs("pools/default/out-file")))
	must(os.Symlink(n.abs("etc/passwd"), n.abs("pools/default/out-file")))
	must(os.Remove(n.abs("pools/default/out-dir")))
	must(os.Symlink(n.abs("etc"), n.abs("pools/default/out-dir")))

	n.denied("read through a link out of the pool", "", "read", "/pools/default/out-file", "100")
	n.denied("sha256 through a link out of the pool", "", "sha256", "/pools/default/out-file")
	n.denied("stat through a directory link out", "", "stat", "/pools/default/out-dir/passwd")
	n.denied("write below a directory link out", "x", "write", "/pools/default/out-dir/new", "10")
	n.denied("copy to below a directory link out", "", "copy", "/pools/default/real", "/pools/default/out-dir/new")
	n.denied("copy from a link out", "", "copy", "/pools/default/out-file", "/pools/default/c")
	n.denied("remove a link", "", "remove", "/pools/default/in-file")
	n.denied("remove a link out", "", "remove", "/pools/default/out-file")
	n.denied("move a link", "", "move", "/pools/default/in-file", "/pools/default/m")
	n.denied("write over a link", "x", "write", "/pools/default/in-file", "10")
	n.denied("write over a dangling link", "x", "write", "/pools/default/dangling", "10")
	n.denied("write over a link to a file outside", "x", "write", "/pools/default/out-file", "10")
	n.denied("copy over a link", "", "copy", "/pools/default/real", "/pools/default/in-file")
	n.denied("move over a link", "", "move", "/pools/default/real", "/pools/default/dangling")
	if b, _ := os.ReadFile(n.abs("etc/passwd")); string(b) != "root:x:0:0\n" {
		t.Errorf("the file behind a link was changed: %q", b)
	}
	if n.ok("", "read", "/pools/default/in-file", "100") != "data" {
		t.Error("a link that stays in the pool should be readable")
	}
	if n.ok("", "sha256", "/pools/default/in-abs") == "" {
		t.Error("an absolute link inside the pool should be readable")
	}
	if !n.exists("pools/default/real") {
		t.Error("the real file vanished")
	}
}

func TestHelperFileVerbsHappyPath(t *testing.T) {
	n := poolNode(t)
	n.ok("hello\n", "write", "/pools/default/a.txt", "100")
	if st := n.ok("", "stat", "/pools/default/a.txt", "/pools/default/none"); !strings.HasPrefix(st, "regular file|") || !strings.HasSuffix(st, "|600|6\nMISSING") {
		t.Errorf("stat: %q", st)
	}
	if got := n.ok("", "read", "/pools/default/a.txt", "100"); got != "hello" {
		t.Errorf("read: %q", got)
	}
	n.denied("a second write", "x", "write", "/pools/default/a.txt", "100")
	n.denied("write above the limit", "0123456789", "write", "/pools/default/big", "5")
	if n.exists("pools/default/big") || len(globAll(t, n, "pools/default/*.virticle-part.*")) != 0 {
		t.Error("a refused write left a file behind")
	}
	n.ok("x", "write", "/pools/default/pub", "10", "644")
	n.denied("odd mode", "x", "write", "/pools/default/m", "10", "4755")
	n.ok("", "copy", "/pools/default/a.txt", "/pools/default/b.txt")
	n.denied("copy over", "", "copy", "/pools/default/a.txt", "/pools/default/b.txt")
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got := n.ok("", "sha256", "/pools/default/b.txt"); got != want {
		t.Errorf("sha256 %q", got)
	}
	n.denied("move onto an existing file", "", "move", "/pools/default/a.txt", "/pools/default/b.txt")
	n.ok("", "move", "/pools/default/a.txt", "/pools/default/c.txt")
	if n.exists("pools/default/a.txt") || !n.exists("pools/default/c.txt") {
		t.Error("move")
	}
	n.ok("", "remove", "/pools/default/c.txt", "/pools/default/b.txt", "/pools/default/never-existed")
	if n.exists("pools/default/c.txt") || n.exists("pools/default/b.txt") {
		t.Error("remove")
	}
	n.denied("read above the limit", "", "read", "/pools/default/pub", "0")
	n.denied("read a directory", "", "read", "/pools/default", "10")
	n.ok("", "stat", "/var/lib/libvirt/qemu/nvram")
	n.ok("nvram", "write", "/var/lib/libvirt/qemu/nvram/vm_VARS.fd", "100")
	n.ok("", "copy", "/var/lib/libvirt/qemu/nvram/vm_VARS.fd", "/pools/default/nv")
}

func globAll(t *testing.T, n *fakeNode, pat string) []string {
	t.Helper()
	m, err := filepath.Glob(n.abs(pat))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHelperWorkDirectories(t *testing.T) {
	n := poolNode(t)
	wd := n.ok("", "mkworkdir", "/pools/default", "bk")
	if !strings.HasPrefix(wd, "/pools/default/.virticle-bk.") || len(wd) != len("/pools/default/.virticle-bk.")+6 {
		t.Fatalf("work directory %q", wd)
	}
	fi, err := os.Stat(n.abs(wd))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("work directory mode: %v %v", fi, err)
	}
	n.denied("tag with a dot", "", "mkworkdir", "/pools/default", "a.b")
	n.denied("empty tag", "", "mkworkdir", "/pools/default", "")
	n.denied("long tag", "", "mkworkdir", "/pools/default", "abcdefghijklmnop")
	n.denied("outside", "", "mkworkdir", "/etc", "bk")
	n.ok("", "chmod-work", "711", wd)
	if fi, _ = os.Stat(n.abs(wd)); fi.Mode().Perm() != 0o711 {
		t.Errorf("mode %v", fi.Mode())
	}
	n.denied("mode 777", "", "chmod-work", "777", wd)
	n.denied("mode 4755", "", "chmod-work", "4755", wd)
	n.ok("{}", "write", wd+"/manifest.json", "100")
	n.denied("chmod of a file", "", "chmod-work", "700", wd+"/manifest.json")
	n.put("pools/default/ordinary", "x")
	n.denied("chmod of something that is not a work directory", "", "chmod-work", "700", "/pools/default")
	if err := os.Mkdir(n.abs("pools/default/plain"), 0o777); err != nil {
		t.Fatal(err)
	}
	n.denied("chmod of a plain directory", "", "chmod-work", "700", "/pools/default/plain")
	n.denied("rmtree of a plain directory", "", "rmtree", "/pools/default/plain/")
	n.ok("", "rmdir", "/pools/default/plain")
	// A backup directory (with a manifest) is removable, a work directory too.
	n.ok("", "move", wd, "/pools/default/2026-final")
	n.ok("", "rmtree", "/pools/default/2026-final")
	if n.exists("pools/default/2026-final") {
		t.Error("rmtree left the directory")
	}
	if err := os.Mkdir(n.abs("pools/default/stuff"), 0o777); err != nil {
		t.Fatal(err)
	}
	n.put("pools/default/stuff/a", "x")
	n.denied("rmtree of a directory without manifest", "", "rmtree", "/pools/default/stuff")
	n.denied("rmdir of a non-empty directory", "", "rmdir", "/pools/default/stuff")
	n.denied("rmtree of a file", "", "rmtree", "/pools/default/ordinary")
}

// qcowHeader returns the first bytes of a qcow2 file that names a backing file.
func qcowHeader(backing string) []byte {
	h := make([]byte, 512)
	copy(h, []byte{'Q', 'F', 'I', 0xfb, 0, 0, 0, 3})
	off := 200
	h[8+7] = byte(off) // backing_file_offset (big endian u64 at 8)
	h[16+3] = byte(len(backing))
	copy(h[off:], backing)
	return h
}

func TestHelperQemuImgInfoChecksTheChain(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Log("qemu-img is missing; only the refusals are tested")
	}
	n := poolNode(t)
	n.put("etc/shadow", "root:!:\n")
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	write := func(rel string, b []byte) {
		if err := os.WriteFile(n.abs(rel), b, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	write("pools/default/evil.qcow2", qcowHeader("/etc/shadow"))
	write("pools/default/rel-evil.qcow2", qcowHeader("../../etc/shadow"))
	write("pools/default/proto.qcow2", qcowHeader("nbd://127.0.0.1/x"))
	write("pools/default/json.qcow2", qcowHeader(`json:{"driver":"file"}`))
	write("pools/default/oversize.qcow2", func() []byte { h := qcowHeader("x"); h[16+3] = 0; h[16+2] = 9; return h }())
	write("pools/default/missing.qcow2", qcowHeader("/pools/default/nothere.qcow2"))
	for _, f := range []string{"evil", "rel-evil", "proto", "json", "oversize", "missing"} {
		n.denied("chain of "+f, "", "qemu-img-info", "/pools/default/"+f+".qcow2")
	}
	// Even when the chain is not asked for, the check runs: qemu-img opens backing files anyway.
	n.denied("without chain", "", "qemu-img-info", "/pools/default/evil.qcow2", "chain")
	n.denied("unknown format", "", "qemu-img-info", "/pools/default/raw.img", "vmdk")
	n.denied("unknown argument", "", "qemu-img-info", "/pools/default/raw.img", "--help")
	n.denied("outside", "", "qemu-img-info", "/etc/shadow")
	// A chain that stays inside the pool passes the check (qemu-img itself may then complain
	// about the fake image, which is not what is tested).
	write("pools/default/base.qcow2", qcowHeader(""))
	write("pools/default/top.qcow2", qcowHeader("/pools/default/base.qcow2"))
	write("pools/default/top-rel.qcow2", qcowHeader("base.qcow2"))
	for _, f := range []string{"top", "top-rel"} {
		out, code := n.run("", "qemu-img-info", "/pools/default/"+f+".qcow2", "chain")
		if code == 2 {
			t.Errorf("%s: a chain inside the pool was refused: %s", f, out)
		}
	}
}

func TestHelperToolsMissing(t *testing.T) {
	n := poolNode(t)
	if out := n.ok("", "tools-missing", "ssh", "tar"); out != "" && !strings.Contains("ssh tar", out) {
		t.Errorf("tools: %q", out)
	}
	n.denied("arbitrary name", "", "tools-missing", "rm")
	n.denied("arbitrary name 2", "", "tools-missing", "ssh;id")
	n.denied("none", "", "tools-missing")
}

// --- peers ---

const peerKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPe virticle-peer@n1"
const ctrlLine = `restrict,pty,port-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIControllerControllerControllerContro virticle-controller` + "\n"

func TestHelperPeerKeyAllowDisallow(t *testing.T) {
	n := poolNode(t)
	n.denied("no key yet", "", "peer-key")
	n.put("home/svc/.ssh/virticle_peer_ed25519.pub", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwn virticle-peer@n2\n")
	if got := n.ok("", "peer-key"); got != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwn" {
		t.Errorf("peer-key: %q", got)
	}
	ak := n.put("home/svc/.ssh/authorized_keys", ctrlLine)
	n.ok(peerKey+"\n", "peer-allow")
	n.ok(peerKey+"\n", "peer-allow") // idempotent
	b, _ := os.ReadFile(ak)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || lines[0]+"\n" != ctrlLine {
		t.Fatalf("authorized_keys:\n%s", b)
	}
	want := `restrict,command="sudo -n /usr/local/libexec/virticle-helper peer-recv" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPe virticle-peer`
	if lines[1] != want {
		t.Errorf("peer line %q", lines[1])
	}
	if got := n.ok("", "peer-list"); !strings.Contains(got, "allow AAAAC3NzaC1lZDI1NTE5AAAAIPeer") {
		t.Errorf("peer-list: %q", got)
	}
	// The controller's own key cannot be removed through peer-disallow.
	n.ok("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIControllerControllerControllerContro x\n", "peer-disallow")
	if b, _ = os.ReadFile(ak); !strings.Contains(string(b), "virticle-controller") {
		t.Error("peer-disallow removed the controller key")
	}
	n.ok(peerKey+"\n", "peer-disallow")
	if b, _ = os.ReadFile(ak); string(b) != ctrlLine {
		t.Errorf("after disallow:\n%s", b)
	}
	for _, bad := range []string{
		"", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ", "ssh-ed25519 short", "ssh-ed25519 AAAA;id;AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPe\n",
		"command=\"id\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPe\n",
		"ssh-ed25519 " + strings.Repeat("A", 200) + "\n",
	} {
		n.denied("peer-allow "+bad, bad, "peer-allow")
		n.denied("peer-disallow "+bad, bad, "peer-disallow")
	}
	if b, _ = os.ReadFile(ak); string(b) != ctrlLine {
		t.Errorf("a refused key changed the file:\n%s", b)
	}
	// A symbolic link in place of authorized_keys is not followed.
	_ = os.Remove(ak)
	if err := os.Symlink(n.abs("etc/secret/x"), ak); err != nil {
		t.Fatal(err)
	}
	n.run(peerKey+"\n", "peer-allow")
	if n.exists("etc/secret/x") {
		t.Error("peer-allow wrote through a symbolic link")
	}
}

const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostHostHostHostHostHostHostHostHostHostHostHostH"

func TestHelperPeerHostKeys(t *testing.T) {
	n := poolNode(t)
	kh := n.abs("home/svc/.ssh/virticle_peer_known_hosts")
	n.ok(hostKey+"\n", "peer-hostkey-set", "192.168.150.12", "22")
	n.ok(hostKey+"\n", "peer-hostkey-set", "192.168.150.12", "22")
	n.ok(hostKey+" comment\n", "peer-hostkey-set", "node-2.example", "2222")
	b, _ := os.ReadFile(kh)
	if string(b) != "192.168.150.12 "+hostKey+"\n[node-2.example]:2222 "+hostKey+"\n" {
		t.Errorf("known hosts:\n%s", b)
	}
	// Replacing the key of a host drops the old one.
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOtherOtherOtherOth"
	n.ok(other+"\n", "peer-hostkey-set", "192.168.150.12", "22")
	b, _ = os.ReadFile(kh)
	if strings.Count(string(b), "192.168.150.12 ") != 1 || !strings.Contains(string(b), other) {
		t.Errorf("replace:\n%s", b)
	}
	if got := n.ok("", "peer-list"); !strings.Contains(got, "host 192.168.150.12 ssh-ed25519") {
		t.Errorf("peer-list: %q", got)
	}
	n.ok("", "peer-hostkey-unset", "192.168.150.12", "22")
	if b, _ = os.ReadFile(kh); strings.Contains(string(b), "192.168.150.12") {
		t.Errorf("unset:\n%s", b)
	}
	for _, c := range [][]string{
		{"", "22"}, {"-oProxyCommand=x", "22"}, {"host;id", "22"}, {"host name", "22"}, {"host", "0"}, {"host", "70000"}, {"host", "22x"}, {"host", ""},
	} {
		n.denied("hostkey-set "+strings.Join(c, ","), hostKey+"\n", "peer-hostkey-set", c[0], c[1])
		n.denied("hostkey-unset "+strings.Join(c, ","), "", "peer-hostkey-unset", c[0], c[1])
	}
	for _, bad := range []string{"", "ssh-ed25519 a;id\n", "bogus AAAAAAAA\n", "ssh-ed25519 " + strings.Repeat("A", 2000) + "\n", hostKey + "\nnot a key\n"} {
		n.denied("hostkey-set body "+bad, bad, "peer-hostkey-set", "h", "22")
	}
}

func TestHelperPeerRecv(t *testing.T) {
	n := poolNode(t)
	hdr := func(mode, size, path string) string {
		return "virticle-peer 1 recv " + mode + " " + size + " " + path + "\n"
	}
	if got := n.ok("virticle-peer 1 ping\n", "peer-recv"); got != "@@pong "+HelperVersion {
		t.Errorf("ping: %q", got)
	}
	if got := n.ok(hdr("600", "5", "/pools/default/new.img")+"hello", "peer-recv"); got != "@@received 5" {
		t.Errorf("recv: %q", got)
	}
	if b, _ := os.ReadFile(n.abs("pools/default/new.img")); string(b) != "hello" {
		t.Errorf("content %q", b)
	}
	if fi, _ := os.Stat(n.abs("pools/default/new.img")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	n.denied("overwrite", hdr("600", "3", "/pools/default/new.img")+"bad", "peer-recv")
	if b, _ := os.ReadFile(n.abs("pools/default/new.img")); string(b) != "hello" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
	n.denied("short data", hdr("600", "10", "/pools/default/short")+"abc", "peer-recv")
	if n.exists("pools/default/short") || len(globAll(t, n, "pools/default/*part*")) != 0 {
		t.Error("an incomplete file was left behind")
	}
	n.ok(hdr("644", "0", "/pools/default/empty"), "peer-recv")
	n.ok(hdr("600", "2", "/var/lib/libvirt/qemu/nvram/x_VARS.fd")+"ab", "peer-recv")
	for _, h := range []string{
		"", "\n", "hello\n", "virticle-peer 2 ping\n", "virticle-peer 1\n", "virticle-peer 1 exec id\n", "virticle-peer 1 ping extra\n",
		hdr("4755", "1", "/pools/default/x"), hdr("6000", "1", "/pools/default/x"), hdr("7", "1", "/pools/default/x"), hdr("600", "-1", "/pools/default/x"),
		hdr("600", "1x", "/pools/default/x"), hdr("600", "1", "/etc/evil"), hdr("600", "1", "/pools/other/evil"), hdr("600", "1", "/pools/default/../../etc/evil"),
		hdr("600", "1", "/pools/default/x;id"), hdr("600", "1", "/pools/default/$(id)"), hdr("600", "1", "relative"),
		hdr("600", "1", "/pools/default/a b"), hdr("600", "1", "/pools/default"), "virticle-peer 1 recv 600 1\n",
		hdr("600", "99999999999999999999", "/pools/default/x"), "virticle-peer 1 recv 600 1 /pools/default/x extra\n",
		strings.Repeat("a", 500) + "\n",
	} {
		n.denied("header "+h, h+"x", "peer-recv")
	}
	// a symlink planted at the destination is not followed
	if err := os.Symlink(n.abs("etc/secret/target"), n.abs("pools/default/planted")); err != nil {
		t.Fatal(err)
	}
	n.denied("planted link", hdr("600", "1", "/pools/default/planted")+"x", "peer-recv")
	if n.exists("etc/secret/target") {
		t.Error("peer-recv wrote through a planted link")
	}
	n.denied("arguments", "virticle-peer 1 ping\n", "peer-recv", "extra")
}

func TestHelperPeerSendReachesPeerRecv(t *testing.T) {
	n := poolNode(t)
	// The receiving node is a second fake node; the fake ssh hands the stream to its helper.
	r := poolNode(t)
	ssh := r.abs("fake-ssh")
	script := "#!/bin/sh\necho \"$@\" > " + r.abs("ssh-args") + "\nexec env VIRTICLE_HELPER_ROOT=" + r.root + " VIRTICLE_HELPER_POOLS=/pools/default PATH=/usr/bin:/bin sh " + r.sh + " peer-recv\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	n.env = append(n.env, "VIRTICLE_HELPER_SSH="+ssh)
	data := strings.Repeat("0123456789abcdef", 70000) // 1.1 MB
	n.put("pools/default/disk.img", data)
	out := n.ok("", "peer-send", "10.1.2.3", "22", "virticle", "/pools/default/disk.img", "/pools/default/disk.img", "600")
	if !strings.HasSuffix(out, "@@done "+itoa(len(data))) {
		t.Errorf("peer-send output: %q", out)
	}
	if b, _ := os.ReadFile(r.abs("pools/default/disk.img")); string(b) != data {
		t.Errorf("received %d bytes, want %d", len(b), len(data))
	}
	args, _ := os.ReadFile(r.abs("ssh-args"))
	for _, want := range []string{"-o StrictHostKeyChecking=yes", "-o BatchMode=yes", "-o IdentitiesOnly=yes", "-F /dev/null", "-p 22", "-- virticle@10.1.2.3 peer-recv", "UserKnownHostsFile=" + n.abs("home/svc/.ssh/virticle_peer_known_hosts")} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ssh was run without %q: %s", want, args)
		}
	}
	// Sending again does not overwrite: the receiver refuses.
	n.denied("send over an existing file", "", "peer-send", "10.1.2.3", "22", "virticle", "/pools/default/disk.img", "/pools/default/disk.img", "600")
	// The failure is a failure of the helper (not a refusal of the arguments).
	if _, code := n.run("", "peer-send", "10.1.2.3", "22", "virticle", "/pools/default/disk.img", "/pools/default/disk.img", "600"); code != 1 {
		t.Errorf("exit %d for a refused transfer, want 1", code)
	}
	// Bad arguments.
	for _, a := range [][]string{
		{"-oProxyCommand=x", "22", "virticle"}, {"host", "22", "-oX"}, {"host", "x", "virticle"}, {"host", "22", "Vir ticle"},
		{"host;id", "22", "virticle"}, {"host", "22", "root;id"}, {"", "22", "virticle"},
	} {
		n.denied("send args "+strings.Join(a, ","), "", append(append([]string{"peer-send"}, a...), "/pools/default/disk.img", "/pools/default/x", "600")...)
		n.denied("ping args "+strings.Join(a, ","), "", append([]string{"peer-ping"}, a...)...)
	}
	for _, m := range []string{"4755", "7777", "60", "rw", "", "-1"} {
		n.denied("send mode "+m, "", "peer-send", "h", "22", "virticle", "/pools/default/disk.img", "/pools/default/x", m)
	}
	n.denied("send destination", "", "peer-send", "h", "22", "virticle", "/pools/default/disk.img", "/pools/default/x y", "600")
	n.denied("send directory", "", "peer-send", "h", "22", "virticle", "/pools/default", "/pools/default/x", "600")
	// ping
	if got := n.ok("", "peer-ping", "10.1.2.3", "22", "virticle"); got != "@@pong "+HelperVersion {
		t.Errorf("ping: %q", got)
	}
}

// denied: the verb failed and changed nothing: exit 2 (arguments refused) or 1 (the operation refused).
func (n *fakeNode) denied(why string, stdin string, args ...string) {
	n.t.Helper()
	out, code := n.run(stdin, args...)
	if code != 1 && code != 2 {
		n.t.Errorf("%s: %v: exit %d, want a refusal: %s", why, args, code, out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
