package nodecmd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestVersionAndUnknown(t *testing.T) {
	n := newNode(t)
	if got := n.ok("", "version"); got != Version {
		t.Errorf("version %q", got)
	}
	n.refused("no subcommand", "")
	n.refused("unknown", "", "run")
	n.refused("a generic exec is not offered", "", "exec", "id")
	n.refused("a self-update is not offered", "", "self-update")
	n.refused("shell metacharacters as subcommand", "", "version; id")
	n.refused("version takes no argument", "", "version", "x")
}

func badPaths(pool string) []string {
	return []string{
		"", "relative", "/", pool + "/", pool + "/../other/x", pool + "/./x", pool + "//x",
		pool + "/x y", pool + "/x;id", pool + "/$(id)", pool + "/`id`", pool + "/x\ny",
		pool + "/x'", pool + "/*", "-rf", pool + "/..", "/" + strings.Repeat("a", 250),
	}
}

func TestFileVerbsRejectBadPaths(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/a", "x")
	good := n.pool("a")
	for _, p := range badPaths(n.abs("pools/default")) {
		n.denied("stat "+p, "", "stat", p)
		n.denied("remove "+p, "", "remove", p)
		n.denied("rmtree "+p, "", "rmtree", p)
		n.denied("rmdir "+p, "", "rmdir", p)
		n.denied("move src "+p, "", "move", p, n.pool("new"))
		n.denied("move dst "+p, "", "move", good, p)
		n.denied("copy src "+p, "", "copy", p, n.pool("new"))
		n.denied("copy dst "+p, "", "copy", good, p)
		n.denied("read "+p, "", "read", p, "10")
		n.denied("write "+p, "x", "write", p, "10")
		n.denied("mkworkdir "+p, "", "mkworkdir", p, "bk")
		n.denied("chmod-work "+p, "", "chmod-work", "700", p)
		n.denied("sha256 "+p, "", "sha256", p)
		n.denied("qemu-img-info "+p, "", "qemu-img-info", p)
	}
	if got := n.glob("pools/default/*"); len(got) != 1 {
		t.Errorf("something was created: %v", got)
	}
}

func TestFileVerbsStayInsideThePools(t *testing.T) {
	n := newNode(t)
	n.put("outside/passwd", "root:x:0:0\n")
	n.put("pools/other/x", "x")
	n.put("pools/default/a", "x")
	out := n.abs("outside")
	for _, p := range []string{n.abs("outside/passwd"), n.abs("pools/other/x"), n.abs("pools/other/new"), n.abs("outside/new"), "/etc/passwd", "/tmp/virticle-never", "/var/lib/libvirt/images/x"} {
		n.denied("stat "+p, "", "stat", p)
		n.denied("read "+p, "", "read", p, "100")
		n.denied("sha256 "+p, "", "sha256", p)
		n.denied("write "+p, "x", "write", p, "100")
		n.denied("remove "+p, "", "remove", p)
		n.denied("copy from "+p, "", "copy", p, n.pool("c"))
		n.denied("copy to "+p, "", "copy", n.pool("a"), p)
		n.denied("move to "+p, "", "move", n.pool("a"), p)
		n.denied("move from "+p, "", "move", p, n.pool("c"))
		n.denied("send "+p, "", "peer-send", "10.0.0.2", "22", "virticle", p, n.pool("c"), "600")
	}
	if !n.exists("outside/passwd") || !n.exists("pools/other/x") || n.exists("pools/default/c") || n.exists("outside/new") || n.exists("pools/other/new") {
		t.Error("something outside the pool was changed or leaked in")
	}
	// A pool directory itself is not removable or movable, and neither is a directory above it.
	n.denied("remove the pool", "", "remove", n.abs("pools/default"))
	n.denied("rmdir the pool", "", "rmdir", n.abs("pools/default"))
	n.denied("rmtree the pool", "", "rmtree", n.abs("pools/default"))
	n.denied("move the pool", "", "move", n.abs("pools/default"), n.abs("pools/default2"))
	n.denied("remove the nvram dir", "", "remove", n.abs("nvram"))
	n.denied("rmtree above the pool", "", "rmtree", n.abs("pools"))
	_ = out
}

func TestFileVerbsSymlinks(t *testing.T) {
	n := newNode(t)
	n.put("outside/passwd", "root:x:0:0\n")
	n.put("pools/default/real", "data")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(n.abs("outside/passwd"), n.pool("out-file")))
	must(os.Symlink(n.abs("outside"), n.pool("out-dir")))
	must(os.Symlink("real", n.pool("in-file")))
	must(os.Symlink(n.pool("real"), n.pool("in-abs")))
	must(os.Symlink(n.abs("nonexistent"), n.pool("dangling")))

	n.denied("read through a link out of the pool", "", "read", n.pool("out-file"), "100")
	n.denied("sha256 through a link out of the pool", "", "sha256", n.pool("out-file"))
	n.denied("stat through a directory link out", "", "stat", n.pool("out-dir/passwd"))
	n.denied("write below a directory link out", "x", "write", n.pool("out-dir/new"), "10")
	n.denied("copy to below a directory link out", "", "copy", n.pool("real"), n.pool("out-dir/new"))
	n.denied("copy from a link out", "", "copy", n.pool("out-file"), n.pool("c"))
	n.denied("remove a link", "", "remove", n.pool("in-file"))
	n.denied("remove a link out", "", "remove", n.pool("out-file"))
	n.denied("move a link", "", "move", n.pool("in-file"), n.pool("m"))
	n.denied("write over a link", "x", "write", n.pool("in-file"), "10")
	n.denied("write over a dangling link", "x", "write", n.pool("dangling"), "10")
	n.denied("write over a link to a file outside", "x", "write", n.pool("out-file"), "10")
	n.denied("copy over a link", "", "copy", n.pool("real"), n.pool("in-file"))
	n.denied("move over a link", "", "move", n.pool("real"), n.pool("dangling"))
	n.denied("mkworkdir through a link out", "", "mkworkdir", n.pool("out-dir"), "bk")
	n.denied("rmtree of a link", "", "rmtree", n.pool("out-dir"))
	n.denied("chmod-work of a link", "", "chmod-work", "700", n.pool("out-dir"))
	if b, _ := os.ReadFile(n.abs("outside/passwd")); string(b) != "root:x:0:0\n" {
		t.Errorf("the file behind a link was changed: %q", b)
	}
	if len(n.glob("outside/*")) != 1 {
		t.Errorf("a file appeared outside: %v", n.glob("outside/*"))
	}
	if n.ok("", "read", n.pool("in-file"), "100") != "data" {
		t.Error("a link that stays in the pool should be readable")
	}
	if n.ok("", "sha256", n.pool("in-abs")) == "" {
		t.Error("an absolute link inside the pool should be readable")
	}
	if !n.exists("pools/default/real") || !n.exists("pools/default/in-file") {
		t.Error("the real file or a link vanished")
	}
}

func TestFileVerbsHappyPath(t *testing.T) {
	n := newNode(t)
	a, b, cpath := n.pool("a.txt"), n.pool("b.txt"), n.pool("c.txt")
	n.ok("hello\n", "write", a, "100")
	if st := n.ok("", "stat", a, n.pool("none")); !strings.HasPrefix(st, "regular file|") || !strings.HasSuffix(st, "|512|600|6\nMISSING") {
		t.Errorf("stat: %q", st)
	}
	if got := n.ok("", "read", a, "100"); got != "hello" {
		t.Errorf("read: %q", got)
	}
	n.denied("a second write", "x", "write", a, "100")
	n.denied("write above the limit", "0123456789", "write", n.pool("big"), "5")
	if n.exists("pools/default/big") || len(n.glob("pools/default/*.virticle-part.*")) != 0 {
		t.Error("a refused write left a file behind")
	}
	n.ok("x", "write", n.pool("pub"), "10", "644")
	if fi, _ := os.Stat(n.pool("pub")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode())
	}
	n.denied("odd mode", "x", "write", n.pool("m"), "10", "4755")
	for _, l := range []string{"0", "-1", "x", "", "67108865", "1000000000000000000"} {
		n.refused("limit "+l, "x", "write", n.pool("lim"), l)
		n.refused("read limit "+l, "", "read", a, l)
	}
	n.ok("", "copy", a, b)
	n.denied("copy over", "", "copy", a, b)
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got := n.ok("", "sha256", b); got != want {
		t.Errorf("sha256 %q", got)
	}
	n.denied("move onto an existing file", "", "move", a, b)
	n.denied("move a missing file", "", "move", n.pool("missing"), n.pool("x"))
	n.ok("", "move", a, cpath)
	if n.exists("pools/default/a.txt") || !n.exists("pools/default/c.txt") {
		t.Error("move")
	}
	n.ok("", "remove", cpath, b, n.pool("never-existed"))
	if n.exists("pools/default/c.txt") || n.exists("pools/default/b.txt") {
		t.Error("remove")
	}
	// read: one byte more than the limit comes back, so the caller sees the overflow.
	if got := n.ok("", "read", n.pool("pub"), "1"); got != "x" {
		t.Errorf("read of a one byte file: %q", got)
	}
	n.ok("abcdef", "write", n.pool("six"), "100")
	if out, _, _ := n.runSplit("", "read", n.pool("six"), "3"); out != "abcd" {
		t.Errorf("read above the limit: %q", out)
	}
	n.denied("read a directory", "", "read", n.abs("pools/default"), "10")
	n.ok("", "stat", n.abs("nvram"))
	n.ok("nvram", "write", n.abs("nvram/vm_VARS.fd"), "100")
	n.ok("", "copy", n.abs("nvram/vm_VARS.fd"), n.pool("nv"))
	if b, _ := os.ReadFile(n.pool("nv")); string(b) != "nvram" {
		t.Errorf("copy from nvram: %q", b)
	}
	// stat of a directory and an empty file
	if st := n.ok("", "stat", n.abs("pools/default")); !strings.HasPrefix(st, "directory|") {
		t.Errorf("stat dir: %q", st)
	}
	n.ok("", "write", n.pool("empty"), "10")
	if st := n.ok("", "stat", n.pool("empty")); !strings.HasPrefix(st, "regular empty file|") {
		t.Errorf("stat empty: %q", st)
	}
}

func TestCopyKeepsModeAndSparseness(t *testing.T) {
	n := newNode(t)
	src := n.pool("disk.img")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(32 << 20)
	f.WriteAt([]byte("boot"), 0)
	f.Close()
	os.Chmod(src, 0o640)
	n.ok("", "copy", src, n.pool("copy.img"))
	fi, _ := os.Stat(n.pool("copy.img"))
	if fi.Mode().Perm() != 0o640 || fi.Size() != 32<<20 {
		t.Errorf("copy: mode %v size %d", fi.Mode(), fi.Size())
	}
	if blocks := blocksOf(t, n.pool("copy.img")); blocks*512 > 8<<20 {
		t.Errorf("the copy is not sparse: %d bytes", blocks*512)
	}
	if len(n.glob("pools/default/*part*")) != 0 {
		t.Error("part file left")
	}
}

func TestWorkDirectories(t *testing.T) {
	n := newNode(t)
	pool := n.abs("pools/default")
	wd := n.ok("", "mkworkdir", pool, "bk")
	if !strings.HasPrefix(wd, pool+"/.virticle-bk.") || len(wd) != len(pool+"/.virticle-bk.")+6 {
		t.Fatalf("work directory %q", wd)
	}
	fi, err := os.Stat(wd)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("work directory mode: %v %v", fi, err)
	}
	n.denied("tag with a dot", "", "mkworkdir", pool, "a.b")
	n.denied("empty tag", "", "mkworkdir", pool, "")
	n.denied("long tag", "", "mkworkdir", pool, "abcdefghijklmnop")
	n.denied("upper case tag", "", "mkworkdir", pool, "AB")
	n.denied("outside", "", "mkworkdir", n.abs("outside"), "bk")
	n.denied("a file", "", "mkworkdir", n.put("pools/default/file", "x"), "bk")
	n.ok("", "chmod-work", "711", wd)
	if fi, _ = os.Stat(wd); fi.Mode().Perm() != 0o711 {
		t.Errorf("mode %v", fi.Mode())
	}
	n.denied("mode 777", "", "chmod-work", "777", wd)
	n.denied("mode 4755", "", "chmod-work", "4755", wd)
	n.ok("{}", "write", wd+"/manifest.json", "100")
	n.denied("chmod of a file", "", "chmod-work", "700", wd+"/manifest.json")
	n.denied("chmod of something that is not a work directory", "", "chmod-work", "700", pool)
	if err := os.Mkdir(n.pool("plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.denied("chmod of a plain directory", "", "chmod-work", "700", n.pool("plain"))
	n.denied("rmtree of a plain directory", "", "rmtree", n.pool("plain"))
	n.ok("", "rmdir", n.pool("plain"))
	// A backup directory (with a manifest) is removable, a work directory too.
	n.ok("", "move", wd, n.pool("2026-final"))
	n.put("pools/default/2026-final/sub-file", "x")
	if err := os.Mkdir(n.pool("2026-final/subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.put("pools/default/2026-final/subdir/y", "y")
	n.ok("", "rmtree", n.pool("2026-final"))
	if n.exists("pools/default/2026-final") {
		t.Error("rmtree left the directory")
	}
	wd2 := n.ok("", "mkworkdir", pool, "tmp")
	n.put(strings.TrimPrefix(wd2, n.root)+"/f", "x")
	n.ok("", "rmtree", wd2)
	if n.exists("pools/default/" + wd2[strings.LastIndex(wd2, "/")+1:]) {
		t.Error("rmtree left the work directory")
	}
	if err := os.Mkdir(n.pool("stuff"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.put("pools/default/stuff/a", "x")
	n.denied("rmtree of a directory without manifest", "", "rmtree", n.pool("stuff"))
	n.denied("rmdir of a non-empty directory", "", "rmdir", n.pool("stuff"))
	n.denied("rmtree of a file", "", "rmtree", n.pool("file"))
	n.denied("rmdir of a file", "", "rmdir", n.pool("file"))
	n.denied("remove of a directory", "", "remove", n.pool("stuff"))
	// A symbolic link inside a tree is removed, not followed.
	wd3 := n.ok("", "mkworkdir", pool, "lnk")
	n.put("outside/keep", "keep")
	if err := os.Symlink(n.abs("outside"), wd3+"/escape"); err != nil {
		t.Fatal(err)
	}
	n.ok("", "rmtree", wd3)
	if !n.exists("outside/keep") {
		t.Error("rmtree followed a link out of the tree")
	}
}

func TestQemuImgInfoChecksTheChain(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Log("qemu-img is missing; only the refusals are tested")
	}
	n := newNode(t)
	n.put("outside/shadow", "root:!:\n")
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	write := func(rel string, b []byte) {
		if err := os.WriteFile(n.abs(rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pools/default/evil.qcow2", qcowHeader(n.abs("outside/shadow")))
	write("pools/default/rel-evil.qcow2", qcowHeader("../../outside/shadow"))
	write("pools/default/proto.qcow2", qcowHeader("nbd://127.0.0.1/x"))
	write("pools/default/json.qcow2", qcowHeader(`json:{"driver":"file"}`))
	write("pools/default/oversize.qcow2", func() []byte { h := qcowHeader("x"); h[16+3] = 0; h[16+2] = 9; return h }())
	write("pools/default/missing.qcow2", qcowHeader(n.pool("nothere.qcow2")))
	for _, f := range []string{"evil", "rel-evil", "proto", "json", "oversize", "missing"} {
		n.denied("chain of "+f, "", "qemu-img-info", n.pool(f+".qcow2"))
	}
	// Even when the chain is not asked for, the check runs: qemu-img opens backing files anyway.
	n.denied("with chain", "", "qemu-img-info", n.pool("evil.qcow2"), "chain")
	n.denied("explicit format", "", "qemu-img-info", n.pool("evil.qcow2"), "qcow2")
	n.refused("unknown format", "", "qemu-img-info", n.pool("raw.img"), "vmdk")
	n.refused("unknown argument", "", "qemu-img-info", n.pool("raw.img"), "--help")
	n.denied("outside", "", "qemu-img-info", n.abs("outside/shadow"))
	// A chain that stays inside the pool passes the check (qemu-img itself may then complain
	// about the fake image, which is not what is tested).
	write("pools/default/base.qcow2", qcowHeader(""))
	write("pools/default/top.qcow2", qcowHeader(n.pool("base.qcow2")))
	write("pools/default/top-rel.qcow2", qcowHeader("base.qcow2"))
	for _, f := range []string{"top", "top-rel"} {
		out, code := n.run("", "qemu-img-info", n.pool(f+".qcow2"), "chain")
		if code == 2 {
			t.Errorf("%s: a chain inside the pool was refused: %s", f, out)
		}
	}
	// Without qemu-img on the node the verb fails cleanly.
	n.cfg.Tools["qemu-img"] = ""
	if out, code := n.run("", "qemu-img-info", n.pool("raw.img")); code != 1 || !strings.Contains(out, "not installed") {
		t.Errorf("missing qemu-img: %d %s", code, out)
	}
}

func TestQemuImgInfoRunsTheTool(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/raw.img", "xxxx")
	tool := n.abs("fake-qemu-img")
	n.put("fake-qemu-img", "#!/bin/sh\necho \"args: $@\"\nexit 3\n")
	os.Chmod(tool, 0o755)
	n.cfg.Tools["qemu-img"] = tool
	out, code := n.run("", "qemu-img-info", n.pool("raw.img"), "chain")
	if code != 3 || out != "args: info -U -f raw --backing-chain --output=json -- "+n.pool("raw.img") {
		t.Errorf("exit %d: %q", code, out)
	}
	out, _ = n.run("", "qemu-img-info", n.pool("raw.img"), "vdi")
	if !strings.Contains(out, "-f vdi") || strings.Contains(out, "backing-chain") {
		t.Errorf("%q", out)
	}
}

func TestToolsMissing(t *testing.T) {
	n := newNode(t)
	n.cfg.Tools["ssh"] = ""
	n.cfg.Tools["qemu-img"] = "/bin/true"
	n.cfg.Tools["scp"] = ""
	if out := n.ok("", "tools-missing", "ssh", "tar", "qemu-img", "scp", "sha256sum"); out != "ssh\nscp" {
		t.Errorf("tools: %q", out)
	}
	n.refused("arbitrary name", "", "tools-missing", "rm")
	n.refused("arbitrary name 2", "", "tools-missing", "ssh;id")
	n.refused("none", "", "tools-missing")
	n.refused("too many", "", "tools-missing", "ssh", "ssh", "ssh", "ssh", "ssh", "ssh", "ssh", "ssh", "ssh")
}

func TestWorkStale(t *testing.T) {
	n := newNode(t)
	pool := n.abs("pools/default")
	old := time.Now().Add(-3 * time.Hour)
	// an old, untouched work directory
	oldWd := n.ok("", "mkworkdir", pool, "bk")
	f := n.put(strings.TrimPrefix(oldWd, n.root+"/")+"/vda.qcow2", "x")
	for _, p := range []string{f, oldWd} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// an old directory in which a file was written a moment ago: a backup still running
	busy := n.ok("", "mkworkdir", pool, "bk")
	n.put(strings.TrimPrefix(busy, n.root+"/")+"/vda.qcow2", "y")
	if err := os.Chtimes(busy, old, old); err != nil {
		t.Fatal(err)
	}
	// a new one, an old restore directory (other tag), a wrong name and a plain directory
	fresh := n.ok("", "mkworkdir", pool, "bk")
	rs := n.ok("", "mkworkdir", pool, "rs")
	_ = os.Chtimes(rs, old, old)
	for _, name := range []string{".virticle-bk.link", "vm-backup"} {
		p := n.abs("pools/default/" + name)
		_ = os.Mkdir(p, 0o700)
		_ = os.Chtimes(p, old, old)
	}

	out := n.ok("", "work-stale", pool, "60")
	if out != oldWd {
		t.Errorf("stale directories %q, want only %q (not %q, %q, %q)", out, oldWd, busy, fresh, rs)
	}
	n.refused("minutes", "", "work-stale", pool, "0")
	n.refused("minutes text", "", "work-stale", pool, "abc")
	n.denied("outside", "", "work-stale", n.abs("outside"), "60")
	n.refused("arguments", "", "work-stale", pool)
}
