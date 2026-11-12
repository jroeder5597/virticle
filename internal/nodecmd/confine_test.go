package nodecmd

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestPlainPath(t *testing.T) {
	good := []string{"/a", "/pools/default/a.img", "/x/y-z_1+2=3,4@5.qcow2", "/a/..b", "/a/b..c", "/" + strings.Repeat("a", 199)}
	for _, p := range good {
		if !PlainPath(p) {
			t.Errorf("%q should be plain", p)
		}
	}
	bad := []string{
		"", "relative", "a/b", "/", "/a/", "/a//b", "/a/./b", "/a/../b", "/a/..", "/a/.", "/..", "/.", "//a",
		"/a b", "/a;b", "/a$b", "/a`b", "/a\nb", "/a'b", "/a*", "/a?b", "/a\\b", "/a\x00b", "-rf", "/ä", "/a:b", "/a(b)",
		"/" + strings.Repeat("a", 200),
	}
	for _, p := range bad {
		if PlainPath(p) {
			t.Errorf("%q should be refused", p)
		}
	}
}

func TestDeniedRoot(t *testing.T) {
	denied := []string{
		"/", "/etc", "/etc/x", "/home", "/var", "/var/lib", "/var/lib/libvirt", "/var/lib/libvirt/qemu", "/var/lib/libvirt/qemu/nvram",
		"/var/lib/libvirt/secrets/x", "/usr/local", "/root", "/root/x", "/proc", "/sys/x", "/run/x", "/boot", "/dev/shm", "/bin", "/sbin", "/lib64",
		"/var/log", "/var/log/x", "/var/spool/x", "/var/lib/dpkg", "/var/lib/rpm/x", "/var/lib/sudo", "/var/lib/virticle/x",
		"/var/lib/libvirt/swtpm", "/var/lib/libvirt/network", "/var/lib/libvirt/dnsmasq", "/var/lib/libvirt/lockd", "/var/lib/libvirt/sanlock",
	}
	for _, p := range denied {
		if !DeniedRoot(p) {
			t.Errorf("%s should be denied", p)
		}
	}
	allowed := []string{"/var/lib/libvirt/images", "/var/lib/libvirt/images/x", "/srv/pool", "/mnt/nfs", "/home/user", "/tmp/x", "/data", "/var/lib/other", "/etcetera", "/rootfs"}
	for _, p := range allowed {
		if DeniedRoot(p) {
			t.Errorf("%s should be allowed", p)
		}
	}
}

func TestRootsFilterAndOrder(t *testing.T) {
	n := newNode(t)
	link := n.abs("outside/pool-link")
	if err := os.Symlink(n.abs("pools/other"), link); err != nil {
		t.Fatal(err)
	}
	n.cfg.Pools = func(*Ctx) []string {
		return []string{"/etc", "/etc/secret", n.abs("pools/other"), "/nonexistent", link, n.abs("pools/default"), "relative", n.abs("pools/other") + "/", "/usr"}
	}
	got := strings.Fields(n.ok("", "roots"))
	want := []string{n.abs("pools/other"), n.abs("pools/default"), n.abs("nvram")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roots %v, want %v", got, want)
	}
}

func TestRootsAreOnlyThePoolsAndNVRAM(t *testing.T) {
	n := newNode(t)
	got := strings.Fields(n.ok("", "roots"))
	if strings.Join(got, ",") != n.abs("pools/default")+","+n.abs("nvram") {
		t.Errorf("roots %v", got)
	}
	// A missing NVRAM directory is no root.
	n.cfg.NVRAMDir = n.abs("no-such-dir")
	if got := strings.Fields(n.ok("", "roots")); len(got) != 1 {
		t.Errorf("roots %v", got)
	}
}

func TestInRootsAndAbove(t *testing.T) {
	n := newNode(t)
	c := &Ctx{Cfg: n.cfg, sh: &shared{}}
	pool := n.abs("pools/default")
	for p, want := range map[string]bool{pool: true, pool + "/x": true, pool + "x": false, n.abs("pools"): false, n.abs("pools/other/x"): false, "/": false} {
		if c.InRoots(p) != want {
			t.Errorf("InRoots(%s) = %v", p, !want)
		}
	}
	for p, want := range map[string]bool{pool: true, n.abs("pools"): true, n.root: true, "/": true, pool + "/x": false, n.abs("pools/other"): false} {
		if c.RootOrAbove(p) != want {
			t.Errorf("RootOrAbove(%s) = %v", p, !want)
		}
	}
}

func TestResolveKinds(t *testing.T) {
	n := newNode(t)
	c := &Ctx{Cfg: n.cfg, sh: &shared{}}
	n.put("pools/default/a", "x")
	if err := os.Symlink(n.pool("a"), n.pool("link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.abs("outside"), n.pool("outdir")); err != nil {
		t.Fatal(err)
	}
	// Read: existing, missing, through an inside link.
	r, err := c.ResolveRead(n.pool("link"))
	if err != nil || r.Path != n.pool("a") {
		t.Fatalf("read through an inside link: %v %+v", err, r)
	}
	r.Close()
	if r, err = c.ResolveRead(n.pool("missing")); err != nil {
		t.Errorf("a missing name in a pool: %v", err)
	} else {
		if _, exists, _ := r.Lstat(); exists {
			t.Error("missing name exists")
		}
		r.Close()
	}
	if _, err = c.ResolveRead(n.pool("outdir/x")); err == nil {
		t.Error("read below a link out of the pool")
	}
	if _, err = c.ResolveRead(n.abs("pools/other/x")); err == nil {
		t.Error("read in another directory")
	}
	// New: refuses what exists, also a link and a dangling link.
	if _, err = c.ResolveNew(n.pool("a")); err == nil {
		t.Error("new over a file")
	}
	if _, err = c.ResolveNew(n.pool("link")); err == nil {
		t.Error("new over a link")
	}
	if err := os.Symlink(n.abs("nothing"), n.pool("dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ResolveNew(n.pool("dangling")); err == nil {
		t.Error("new over a dangling link")
	}
	if _, err = c.ResolveNew(n.pool("outdir/new")); err == nil {
		t.Error("new below a link out of the pool")
	}
	r, err = c.ResolveNew(n.pool("fresh"))
	if err != nil {
		t.Errorf("new: %v", err)
	} else {
		r.Close()
	}
	// Mod: refuses a final link and the pool itself.
	if _, err = c.ResolveMod(n.pool("link")); err == nil {
		t.Error("mod of a link")
	}
	if _, err = c.ResolveMod(n.abs("pools/default")); err == nil {
		t.Error("mod of the pool")
	}
	if r, err = c.ResolveMod(n.pool("a")); err != nil {
		t.Errorf("mod: %v", err)
	} else {
		r.Close()
	}
}

func TestResolveRejectsAMovedDirectory(t *testing.T) {
	// openVerifiedDir compares the descriptor with the place it was meant to
	// be: a path that reaches a directory through a link is not that place.
	n := newNode(t)
	if err := os.Symlink(n.abs("outside"), n.pool("sneaky")); err != nil {
		t.Fatal(err)
	}
	if fd, err := openVerifiedDir(n.abs("pools/default") + "/sneaky/.."); err == nil {
		t.Errorf("a path that was not canonical was accepted (fd %d)", fd)
	}
	if _, err := openVerifiedDir(n.pool("sneaky")); err == nil {
		t.Error("a final symbolic link was followed")
	}
	if fd, err := openVerifiedDir(n.abs("pools/default")); err != nil {
		t.Errorf("a canonical directory: %v", err)
	} else {
		_ = fd
	}
}

func TestCreateNewLeavesNoPartFile(t *testing.T) {
	n := newNode(t)
	c := &Ctx{Cfg: n.cfg, sh: &shared{}}
	r, err := c.ResolveNew(n.pool("new"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// A failing fill removes the part file.
	err = c.CreateNew(r, 0o600, func(f *os.File) error { f.WriteString("abc"); return Fail("nope") })
	if err == nil || len(n.glob("pools/default/*part*")) != 0 || n.exists("pools/default/new") {
		t.Errorf("failed fill: %v, left %v", err, n.glob("pools/default/*"))
	}
	// A name that appears meanwhile is not replaced.
	err = c.CreateNew(r, 0o600, func(f *os.File) error {
		f.WriteString("mine")
		return os.WriteFile(n.pool("new"), []byte("theirs"), 0o600)
	})
	if err == nil {
		t.Error("an appearing file was replaced")
	}
	if b, _ := os.ReadFile(n.pool("new")); string(b) != "theirs" {
		t.Errorf("content %q", b)
	}
	if len(n.glob("pools/default/*part*")) != 0 {
		t.Error("part file left")
	}
	// Killed in the middle: the cleanup removes the part file.
	os.Remove(n.pool("new"))
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- c.CreateNew(r, 0o600, func(f *os.File) error { close(started); <-release; return Fail("killed") })
	}()
	<-started
	if len(n.glob("pools/default/*part*")) != 1 {
		t.Error("no part file while writing")
	}
	c.sh.runCleanups()
	if len(n.glob("pools/default/*part*")) != 0 {
		t.Error("the cleanup left the part file")
	}
	close(release)
	<-finished
}

func TestCopySparseKeepsHoles(t *testing.T) {
	n := newNode(t)
	src, err := os.Create(n.abs("outside/src"))
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	if err := src.Truncate(size); err != nil {
		t.Fatal(err)
	}
	src.WriteAt([]byte("head"), 0)
	src.WriteAt([]byte("middle"), 20<<20)
	src.WriteAt([]byte("tail"), size-4)
	src.Sync()
	dst, err := os.Create(n.abs("outside/dst"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CopySparse(dst, src, size); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst.Close()
	a, _ := os.ReadFile(n.abs("outside/src"))
	b, _ := os.ReadFile(n.abs("outside/dst"))
	if string(a) != string(b) {
		t.Fatal("the copy differs")
	}
	fi, _ := os.Stat(n.abs("outside/dst"))
	if fi.Size() != size {
		t.Errorf("size %d", fi.Size())
	}
	if blocks := blocksOf(t, n.abs("outside/dst")); blocks*512 > 16<<20 {
		t.Errorf("the copy uses %d bytes; it should stay sparse", blocks*512)
	}
}

func TestHeaderBackingAndChain(t *testing.T) {
	n := newNode(t)
	c := &Ctx{Cfg: n.cfg, sh: &shared{}}
	w := func(rel string, b []byte) string {
		p := n.abs(rel)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	w("pools/default/base.qcow2", qcowHeader(""))
	top := w("pools/default/top.qcow2", qcowHeader(n.pool("base.qcow2")))
	if b, err := HeaderBacking(top); err != nil || b != n.pool("base.qcow2") {
		t.Errorf("backing %q %v", b, err)
	}
	if b, err := HeaderBacking(w("pools/default/raw", []byte("not qcow"))); err != nil || b != "" {
		t.Errorf("raw backing %q %v", b, err)
	}
	if err := c.CheckChain(top); err != nil {
		t.Errorf("chain inside the pool: %v", err)
	}
	for name, backing := range map[string]string{
		"evil": n.abs("outside/secret"), "rel": "../../outside/x", "proto": "nbd://127.0.0.1/x", "json": `json:{"driver":"file"}`, "space": n.pool("a b"),
	} {
		w("outside/secret", []byte("s"))
		p := w("pools/default/"+name+".qcow2", qcowHeader(backing))
		if err := c.CheckChain(p); err == nil {
			t.Errorf("%s: backing %q accepted", name, backing)
		}
	}
	// A loop is cut off.
	loop := n.pool("loop.qcow2")
	w("pools/default/loop.qcow2", qcowHeader(loop))
	if err := c.CheckChain(loop); err == nil {
		t.Error("a backing loop was accepted")
	}
	// Oversized name length.
	h := qcowHeader("x")
	h[16+2] = 9
	if err := c.CheckChain(w("pools/default/big.qcow2", h)); err == nil {
		t.Error("oversize backing name")
	}
}

// qcowHeader returns the first bytes of a qcow2 file that names a backing file.
func qcowHeader(backing string) []byte {
	h := make([]byte, 512)
	copy(h, []byte{'Q', 'F', 'I', 0xfb, 0, 0, 0, 3})
	if backing == "" {
		return h
	}
	off := 200
	h[8+7] = byte(off) // backing_file_offset (big endian u64 at 8)
	h[16+3] = byte(len(backing))
	copy(h[off:], backing)
	return h
}

func blocksOf(t *testing.T, p string) int64 {
	t.Helper()
	out, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return statBlocks(out)
}

func statBlocks(fi os.FileInfo) int64 { return fi.Sys().(*syscall.Stat_t).Blocks }
