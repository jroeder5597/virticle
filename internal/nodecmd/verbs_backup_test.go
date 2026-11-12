package nodecmd

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The detached half of convert-start runs this test binary again (through a
// wrapper script set as Config.Self) as the verb convert-run on the same fake
// node.
func init() {
	if root := os.Getenv("NODECMD_TEST_BACKUP_ROOT"); root != "" {
		cfg := layout(root)
		cfg.Tools["qemu-img"] = os.Getenv("NODECMD_TEST_QEMU")
		os.Exit(Run(cfg, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// A backup directory in the pool; returns its path.
func (n *tnode) backupDir(name string) string {
	n.t.Helper()
	d := "pools/default/" + name
	if err := os.MkdirAll(n.abs(d), 0o755); err != nil {
		n.t.Fatal(err)
	}
	n.put(d+"/manifest.json", `{"format":"x"}`)
	n.put(d+"/domain.xml", "<domain/>")
	n.put(d+"/vda.qcow2", "0123456789")
	return n.abs(d)
}

func (n *tnode) workDir(name string) string {
	n.t.Helper()
	d := "pools/default/" + name
	if err := os.MkdirAll(n.abs(d), 0o755); err != nil {
		n.t.Fatal(err)
	}
	return n.abs(d)
}

// fakeQemu installs a fake qemu-img. The info call answers with info; the
// convert call (source and destination are the last two arguments, $src and
// $dst) copies, or runs convertBody instead when it is not empty.
func (n *tnode) fakeQemu(info, convertBody string) {
	n.t.Helper()
	if convertBody == "" {
		convertBody = `printf '    (100.00/100%%)\r'; cp "$src" "$dst"`
	}
	script := "#!/bin/sh\nif [ \"$1\" = info ]; then echo '" + info + "'; exit 0; fi\n" +
		"for a; do src=$dst; dst=$a; done\n" + convertBody + "\n"
	p := n.abs("fake-qemu-img")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		n.t.Fatal(err)
	}
	n.cfg.Tools["qemu-img"] = p
	self := n.abs("fake-self")
	wrapper := fmt.Sprintf("#!/bin/sh\nNODECMD_TEST_BACKUP_ROOT=%s NODECMD_TEST_QEMU=%s exec %s \"$@\"\n", n.root, p, os.Args[0])
	if err := os.WriteFile(self, []byte(wrapper), 0o755); err != nil {
		n.t.Fatal(err)
	}
	n.cfg.Self = self
}

func TestBackupVerbsRejectBadPaths(t *testing.T) {
	n := newNode(t)
	bk := n.backupDir("a.vmbackup")
	work := n.workDir(".virticle-bk.AAAAAA")
	pool := n.abs("pools/default")
	for _, p := range badPaths(pool) {
		n.denied("df "+p, "", "df", p)
		n.denied("files-size "+p, "", "files-size", p, "manifest.json")
		n.denied("backup-scan "+p, "", "backup-scan", p, "@@@0123456789abcdef")
		n.denied("tar-pack "+p, "", "tar-pack", p, "manifest.json")
		n.denied("tar-unpack "+p, "", "tar-unpack", p)
		n.denied("convert-start work "+p, "", "convert-start", p, bk+"/vda.qcow2", "qcow2")
		n.denied("convert-start src "+p, "", "convert-start", work, p, "qcow2")
		n.denied("convert-poll "+p, "", "convert-poll", p)
		n.denied("convert-kill "+p, "", "convert-kill", p)
		n.denied("convert-finish work "+p, "", "convert-finish", p, pool+"/new")
		n.denied("convert-finish dest "+p, "", "convert-finish", work, p)
	}
}

func TestBackupVerbsStayInsideThePools(t *testing.T) {
	n := newNode(t)
	n.put("outside/passwd", "root:x:0:0\n")
	_ = os.MkdirAll(n.abs("pools/other/x.vmbackup"), 0o755)
	n.put("pools/other/x.vmbackup/manifest.json", "{}")
	n.put("pools/other/x", "x")
	work := n.workDir(".virticle-bk.AAAAAA")
	for _, p := range []string{n.abs("outside"), n.abs("outside/secret"), n.abs("pools/other"), n.abs("pools/other/x.vmbackup"), "/tmp", "/etc", "/var/lib/libvirt/images"} {
		n.denied("df "+p, "", "df", p)
		n.denied("files-size "+p, "", "files-size", p, "manifest.json")
		n.denied("backup-scan "+p, "", "backup-scan", p, "@@@0123456789abcdef")
		n.denied("tar-pack "+p, "", "tar-pack", p, "manifest.json")
		n.denied("tar-unpack "+p, "", "tar-unpack", p)
		n.denied("convert-start work "+p, "", "convert-start", p, n.pool("x"), "qcow2")
		n.denied("convert-poll "+p, "", "convert-poll", p)
		n.denied("convert-kill "+p, "", "convert-kill", p)
		n.denied("convert-start src "+p, "", "convert-start", work, p, "raw")
		n.denied("convert-finish dest "+p, "", "convert-finish", work, p)
	}
	n.denied("convert-start of a file outside", "", "convert-start", work, n.abs("outside/passwd"), "raw")
	// A link out of the pool does not lead out.
	if err := os.Symlink(n.abs("outside/passwd"), n.pool("link.img")); err != nil {
		t.Fatal(err)
	}
	n.denied("convert-start of a link out", "", "convert-start", work, n.pool("link.img"), "raw")
	if err := os.Symlink(n.abs("outside"), n.pool("linkdir")); err != nil {
		t.Fatal(err)
	}
	n.denied("df through a link out", "", "df", n.pool("linkdir"))
	n.denied("files-size through a link out", "", "files-size", n.pool("linkdir"), "passwd")
}

func TestBackupDf(t *testing.T) {
	n := newNode(t)
	out := n.ok("", "df", n.abs("pools/default"))
	if out == "" || strings.Trim(out, "0123456789") != "" {
		t.Errorf("df printed %q", out)
	}
	n.denied("two arguments", "", "df", n.abs("pools/default"), n.abs("pools/default"))
	n.put("pools/default/file", "x")
	n.denied("a file", "", "df", n.pool("file"))
	n.denied("a missing directory", "", "df", n.pool("nothere"))
}

func TestBackupFilesSize(t *testing.T) {
	n := newNode(t)
	d := n.backupDir("a.vmbackup")
	if err := os.Symlink(n.abs("outside"), d+"/link.qcow2"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(n.ok("", "files-size", d, "vda.qcow2", "nothere", "link.qcow2", "sub", "domain.xml"))
	want := []string{"10", "MISSING", "MISSING", "MISSING", "9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sizes %v, want %v", got, want)
	}
	for _, bad := range []string{"", "../x", "a/b", "/etc/passwd", "-x", ".hidden", "a b", "a;id", "a\nb", strings.Repeat("x", 121)} {
		n.denied("name "+bad, "", "files-size", d, bad)
	}
	n.denied("no names", "", "files-size", d)
	many := []string{"files-size", d}
	for i := 0; i < 41; i++ {
		many = append(many, "f")
	}
	n.denied("too many names", "", many...)
}

func TestBackupScan(t *testing.T) {
	n := newNode(t)
	n.backupDir("a.vmbackup")
	n.backupDir("b.vmbackup")
	n.backupDir("notabackup") // no .vmbackup suffix
	// A link to a directory in the pool and a deeper manifest are not listed.
	if err := os.Symlink("a.vmbackup", n.pool("c.vmbackup")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.pool("a.vmbackup/d.vmbackup"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.put("pools/default/a.vmbackup/d.vmbackup/manifest.json", "{}")
	// A manifest that is a link, and one that is too big, are not listed.
	n.backupDir("e.vmbackup")
	if err := os.Remove(n.pool("e.vmbackup/manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.abs("outside"), n.pool("e.vmbackup/manifest.json")); err != nil {
		t.Fatal(err)
	}
	n.backupDir("f.vmbackup")
	n.put("pools/default/f.vmbackup/manifest.json", strings.Repeat("x", 64*1024))
	m := "@@@0123456789abcdef"
	pool := n.abs("pools/default")
	out := n.ok("", "backup-scan", pool, m)
	for _, want := range []string{m + " " + pool + "/a.vmbackup\n", m + " " + pool + "/b.vmbackup\n", `{"format":"x"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("scan lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, m) != 2 {
		t.Errorf("want exactly two backups:\n%s", out)
	}
	// Section layout: marker line, size, manifest, newline.
	lines := strings.Split(out, "\n")
	if len(lines) < 3 || strings.Trim(lines[1], "0123456789") != "" || lines[1] == "" || lines[2] != `{"format":"x"}` {
		t.Errorf("section layout: %q", lines)
	}
	for _, bad := range []string{"", "@@@", "@@@0123456789abcde", "@@@0123456789abcdeg", "@@@0123456789ABCDEF", "x@@0123456789abcdef", "@@@0123456789abcdef;id", "@@@0123456789abcdef\n"} {
		n.refused("marker "+bad, "", "backup-scan", pool, bad)
	}
}

func TestBackupScanLimit(t *testing.T) {
	n := newNode(t)
	for i := 0; i < 503; i++ {
		d := fmt.Sprintf("pools/default/b%03d.vmbackup", i)
		_ = os.Mkdir(n.abs(d), 0o755)
		n.put(d+"/manifest.json", "{}")
	}
	m := "@@@0123456789abcdef"
	if got := strings.Count(n.ok("", "backup-scan", n.abs("pools/default"), m), m); got != 500 {
		t.Errorf("scan listed %d backups, want at most 500", got)
	}
}

func tarOf(t *testing.T, hs ...tar.Header) string {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range hs {
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = strings.Repeat("d", int(h.Size))
		}
		h.Mode = 0o644
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	return b.String()
}

func TestBackupTarPack(t *testing.T) {
	n := newNode(t)
	d := n.backupDir("a.vmbackup")
	out := n.ok("", "tar-pack", d, "manifest.json", "vda.qcow2")
	r := tar.NewReader(strings.NewReader(out))
	var names []string
	for {
		h, err := r.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		body := make([]byte, 64)
		k, _ := r.Read(body)
		if h.Name == "vda.qcow2" && string(body[:k]) != "0123456789" {
			t.Errorf("vda.qcow2 holds %q", body[:k])
		}
	}
	if strings.Join(names, ",") != "manifest.json,vda.qcow2" {
		t.Errorf("members %v", names)
	}
	// The real tar program reads what we write.
	if _, err := exec.LookPath("tar"); err == nil {
		cmd := exec.Command("tar", "-tf", "-")
		cmd.Stdin = strings.NewReader(out)
		if lst, err := cmd.Output(); err != nil || string(lst) != "manifest.json\nvda.qcow2\n" {
			t.Errorf("tar -t: %q %v", lst, err)
		}
	}
	if err := os.Symlink(n.abs("outside"), d+"/link"); err != nil {
		t.Fatal(err)
	}
	n.denied("a link is not packed", "", "tar-pack", d, "link")
	n.denied("a missing file", "", "tar-pack", d, "nothere")
	n.denied("a path as name", "", "tar-pack", d, "../a.vmbackup/vda.qcow2")
	n.denied("an option as name", "", "tar-pack", d, "--to-command=id")
	n.denied("no names", "", "tar-pack", d)
	plain := n.workDir("plain")
	n.put("pools/default/plain/f", "x")
	n.denied("a directory that is no backup", "", "tar-pack", plain, "f")
	// A manifest that is a link does not make a backup.
	l := n.workDir("linked")
	if err := os.Symlink(n.abs("outside"), l+"/manifest.json"); err != nil {
		t.Fatal(err)
	}
	n.put("pools/default/linked/f", "x")
	n.denied("a linked manifest", "", "tar-pack", l, "f")
}

func TestBackupTarUnpack(t *testing.T) {
	n := newNode(t)
	good := tarOf(t, tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Size: 5}, tar.Header{Name: "vda.qcow2", Typeflag: tar.TypeReg, Size: 7})
	w := n.workDir(".virticle-imp.AAAAAA")
	n.ok(good, "tar-unpack", w)
	if b, _ := os.ReadFile(w + "/vda.qcow2"); len(b) != 7 {
		t.Errorf("unpacked file has %d bytes", len(b))
	}
	if fi, _ := os.Stat(w + "/vda.qcow2"); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("unpacked file mode %v, want 0600", fi)
	}
	n.denied("a directory that is not empty", good, "tar-unpack", w)
	n.denied("a directory that is not a work directory", good, "tar-unpack", n.workDir("plain"))
	n.denied("the pool itself", good, "tar-unpack", n.abs("pools/default"))
	empty := func(w string) {
		t.Helper()
		ents, err := os.ReadDir(w)
		if err != nil || len(ents) != 0 {
			t.Errorf("%s was not emptied: %v %v", w, ents, err)
		}
	}
	for name, archive := range map[string]string{
		"symlink":  tarOf(t, tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"hardlink": tarOf(t, tar.Header{Name: "evil", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}),
		"dir":      tarOf(t, tar.Header{Name: "sub", Typeflag: tar.TypeDir}),
		"fifo":     tarOf(t, tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}),
		"chardev":  tarOf(t, tar.Header{Name: "dev", Typeflag: tar.TypeChar}),
		"dot":      tarOf(t, tar.Header{Name: ".", Typeflag: tar.TypeReg}),
		"dotdot":   tarOf(t, tar.Header{Name: "..", Typeflag: tar.TypeReg}),
		"duplicate": tarOf(t,
			tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 1},
			tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 1}),
		"good then bad": tarOf(t,
			tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 3},
			tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc"}),
		"through a link": tarOf(t,
			tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
			tar.Header{Name: "evil/passwd", Typeflag: tar.TypeReg, Size: 3}),
	} {
		w := n.workDir(".virticle-imp." + strings.ReplaceAll(name, " ", "") + "XXX")
		n.denied(name, archive, "tar-unpack", w)
		empty(w)
	}
	n.put("outside/passwd", "root:x:0:0\n")
	// A name with a slash is skipped, never written.
	w2 := n.workDir(".virticle-imp.BBBBBB")
	n.ok(tarOf(t,
		tar.Header{Name: "a/b", Typeflag: tar.TypeReg, Size: 3},
		tar.Header{Name: "../outside/passwd", Typeflag: tar.TypeReg, Size: 3},
		tar.Header{Name: "/abs", Typeflag: tar.TypeReg, Size: 3},
		tar.Header{Name: "ok", Typeflag: tar.TypeReg, Size: 3}), "tar-unpack", w2)
	if ents, _ := os.ReadDir(w2); len(ents) != 1 || ents[0].Name() != "ok" {
		t.Errorf("a member with a slash was written, or the plain one was not: %v", ents)
	}
	if b, _ := os.ReadFile(n.abs("outside/passwd")); string(b) != "root:x:0:0\n" {
		t.Errorf("outside/passwd changed: %q", b)
	}
	w3 := n.workDir(".virticle-imp.CCCCCC")
	n.denied("not a tar", "this is not a tar archive", "tar-unpack", w3)
	empty(w3)
	w4 := n.workDir(".virticle-imp.DDDDDD")
	n.denied("a cut archive", good[:600], "tar-unpack", w4)
	empty(w4)
	link := n.pool(".virticle-imp.LINK")
	if err := os.Symlink(w4, link); err != nil {
		t.Fatal(err)
	}
	n.denied("a work directory that is a link", good, "tar-unpack", link)
	empty(w4)
}

func TestBackupConvertValidation(t *testing.T) {
	n := newNode(t)
	n.fakeQemu(`{"format":"qcow2"}`, "")
	work := n.workDir(".virticle-bk.AAAAAA")
	src := n.put("pools/default/src.img", "RAW")
	plain := n.workDir("plain")
	for _, f := range []string{"", "vmdk", "qcow", "raw;id", "-f", "QCOW2", "qcow2 "} {
		n.denied("format "+f, "", "convert-start", work, src, f)
	}
	n.denied("not a work dir", "", "convert-start", plain, src, "raw")
	n.denied("source is a directory", "", "convert-start", work, plain, "raw")
	n.denied("source missing", "", "convert-start", work, n.pool("none"), "raw")
	n.denied("poll of a plain dir", "", "convert-poll", plain)
	n.denied("kill in a plain dir", "", "convert-kill", plain)
	n.denied("finish in a plain dir", "", "convert-finish", plain, n.pool("new"))
	n.denied("finish without a result", "", "convert-finish", work, n.pool("new"))
	n.denied("convert-run of a plain dir", "", "convert-run", plain, src, "raw")
	n.put("pools/default/.virticle-bk.AAAAAA/out.part", "converted")
	n.put("pools/default/exists", "x")
	n.denied("finish onto an existing file", "", "convert-finish", work, n.pool("exists"))
	n.denied("finish outside the pool", "", "convert-finish", work, n.abs("outside/new"))
	if !n.exists("pools/default/.virticle-bk.AAAAAA/out.part") {
		t.Error("the result was moved although the target was refused")
	}
	if err := os.Symlink(n.abs("outside"), n.pool("dangling")); err != nil {
		t.Fatal(err)
	}
	n.denied("finish onto a link", "", "convert-finish", work, n.pool("dangling"))
	n.put("pools/default/.virticle-bk.AAAAAA/rc", "0\n")
	n.ok("", "convert-finish", work, n.pool("new"))
	if b, _ := os.ReadFile(n.pool("new")); string(b) != "converted" || n.exists("pools/default/.virticle-bk.AAAAAA/out.part") || n.exists("pools/default/.virticle-bk.AAAAAA/rc") {
		t.Error("convert-finish did not move the result or drop the state")
	}
	// An out.part that is a link is not a result.
	if err := os.Symlink(n.abs("outside"), work+"/out.part"); err != nil {
		t.Fatal(err)
	}
	n.denied("finish with a linked result", "", "convert-finish", work, n.pool("new2"))
	// poll and kill on a quiet work directory
	if out := n.ok("", "convert-poll", work); !strings.HasPrefix(out, "---") {
		t.Errorf("poll: %q", out)
	}
	n.put("pools/default/.virticle-bk.AAAAAA/qpid", "1; id")
	n.ok("", "convert-kill", work)
	n.put("pools/default/.virticle-bk.AAAAAA/qpid", "999999")
	n.ok("", "convert-kill", work)
}

func TestBackupConvertPollFormat(t *testing.T) {
	n := newNode(t)
	work := n.workDir(".virticle-bk.AAAAAA")
	n.put("pools/default/.virticle-bk.AAAAAA/rc", "0\n")
	n.put("pools/default/.virticle-bk.AAAAAA/prog.log", strings.Repeat("z", 1000)+"    (10.00/100%)\r    (50.00/100%)\r")
	out := n.ok("", "convert-poll", work)
	rc, log, _ := strings.Cut(out, "---")
	if strings.TrimSpace(rc) != "0" {
		t.Errorf("rc part %q", rc)
	}
	if !strings.Contains(log, "(10.00/100%)\n") || !strings.Contains(log, "(50.00/100%)") || strings.Contains(log, "\r") {
		t.Errorf("log part %q", log)
	}
	if len(strings.TrimSpace(log)) > 400 {
		t.Errorf("log part has %d bytes, want at most 400", len(log))
	}
}

func TestBackupConvertKillStopsOnlyQemuImg(t *testing.T) {
	n := newNode(t)
	work := n.workDir(".virticle-bk.AAAAAA")
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	other, qemu := start(), start()
	n.put("pools/default/.virticle-bk.AAAAAA/qpid", strconv.Itoa(other.Process.Pid)+"\n")
	for _, p := range []struct {
		pid  int
		comm string
	}{{other.Process.Pid, "sleep"}, {qemu.Process.Pid, "qemu-img"}} {
		dir := filepath.Join(n.root, "proc", strconv.Itoa(p.pid))
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(dir+"/comm", []byte(p.comm+"\n"), 0o644)
	}
	n.ok("", "convert-kill", work)
	if other.Process.Signal(syscall.Signal(0)) != nil {
		t.Error("a process that is not qemu-img was killed")
	}
	n.put("pools/default/.virticle-bk.AAAAAA/qpid", strconv.Itoa(qemu.Process.Pid)+"\n")
	n.ok("", "convert-kill", work)
	done := make(chan error, 1)
	go func() { done <- qemu.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("qemu-img was not stopped")
	}
}

func waitRC(t *testing.T, n *tnode, work string) (rc, log string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out := n.ok("", "convert-poll", work)
		r, l, _ := strings.Cut(out, "---")
		if r = strings.TrimSpace(r); r != "" {
			return r, l
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the conversion did not finish")
	return
}

// The conversion runs with a fake qemu-img: this tests the mechanism (detach,
// pid, progress, exit code, finish), not qemu-img itself.
func TestBackupConvertRuns(t *testing.T) {
	n := newNode(t)
	n.fakeQemu(`{"format":"qcow2"}`, "")
	work := n.workDir(".virticle-bk.AAAAAA")
	src := n.put("pools/default/src.img", strings.Repeat("x", 4096))
	// stale state of an earlier run is removed by convert-start
	n.put("pools/default/.virticle-bk.AAAAAA/rc", "7\n")
	if got := n.ok("", "convert-start", work, src, "raw"); got != "started" {
		t.Fatalf("convert-start printed %q", got)
	}
	rc, log := waitRC(t, n, work)
	if rc != "0" {
		t.Fatalf("conversion failed: %s %s", rc, log)
	}
	if !strings.Contains(log, "100.00/100%") {
		t.Errorf("progress log %q", log)
	}
	if pid, _ := os.ReadFile(work + "/qpid"); len(pid) == 0 || strings.Trim(strings.TrimSpace(string(pid)), "0123456789") != "" {
		t.Errorf("qpid %q", pid)
	}
	n.denied("a second run in the same work directory", "", "convert-run", work, src, "raw")
	dest := n.pool("out.qcow2")
	n.ok("", "convert-finish", work, dest)
	if b, _ := os.ReadFile(dest); len(b) != 4096 {
		t.Errorf("result has %d bytes", len(b))
	}
	for _, f := range []string{"out.part", "rc", "qpid", "prog.log"} {
		if n.exists("pools/default/.virticle-bk.AAAAAA/" + f) {
			t.Errorf("%s was left in the work directory", f)
		}
	}
}

func TestBackupConvertQcow2Checks(t *testing.T) {
	n := newNode(t)
	work := n.workDir(".virticle-bk.AAAAAA")
	src := n.put("pools/default/src.qcow2", "not really a qcow2")
	n.fakeQemu(`{"format":"qcow2","data-file":"/etc/shadow"}`, "")
	n.denied("external data file", "", "convert-start", work, src, "qcow2")
	if n.exists("pools/default/.virticle-bk.AAAAAA/qpid") {
		t.Error("a conversion was started")
	}
	// A qcow2 header with a backing file outside the pools.
	name := "/etc/passwd"
	hdr := make([]byte, 104+len(name))
	binary.BigEndian.PutUint32(hdr[0:], qcow2Magic)
	binary.BigEndian.PutUint32(hdr[4:], 3)
	binary.BigEndian.PutUint64(hdr[8:], 104)
	binary.BigEndian.PutUint32(hdr[16:], uint32(len(name)))
	copy(hdr[104:], name)
	n.fakeQemu(`{"format":"qcow2"}`, "")
	if err := os.WriteFile(n.pool("chain.qcow2"), hdr, 0o644); err != nil {
		t.Fatal(err)
	}
	n.denied("backing file outside the pools", "", "convert-start", work, n.pool("chain.qcow2"), "qcow2")
	if n.exists("pools/default/.virticle-bk.AAAAAA/qpid") {
		t.Error("a conversion was started")
	}
	n.cfg.Tools["qemu-img"] = ""
	n.denied("qemu-img not installed", "", "convert-start", work, src, "raw")
}

func TestBackupConvertFailureLeavesNoPartFile(t *testing.T) {
	n := newNode(t)
	n.fakeQemu(`{}`, `echo partial >"$dst"; echo "qemu-img: broken" >&2; exit 3`)
	work := n.workDir(".virticle-bk.AAAAAA")
	src := n.put("pools/default/src.img", "x")
	n.ok("", "convert-start", work, src, "raw")
	rc, log := waitRC(t, n, work)
	if rc != "3" || !strings.Contains(log, "broken") {
		t.Errorf("rc %q log %q", rc, log)
	}
	if n.exists("pools/default/.virticle-bk.AAAAAA/out.part") {
		t.Error("out.part was left after a failed conversion")
	}
	n.denied("finish after a failure", "", "convert-finish", work, n.pool("new"))
}

func TestBackupConvertKillLeavesNoPartFile(t *testing.T) {
	n := newNode(t)
	n.fakeQemu(`{}`, `echo partial >"$dst"; exec sleep 60`)
	work := n.workDir(".virticle-bk.AAAAAA")
	src := n.put("pools/default/src.img", "x")
	n.ok("", "convert-start", work, src, "raw")
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pid == 0 {
		b, _ := os.ReadFile(work + "/qpid")
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("no qpid")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	for time.Now().Before(deadline) && !n.exists("pools/default/.virticle-bk.AAAAAA/out.part") {
		time.Sleep(20 * time.Millisecond)
	}
	dir := filepath.Join(n.root, "proc", strconv.Itoa(pid))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(dir+"/comm", []byte("qemu-img\n"), 0o644)
	n.ok("", "convert-kill", work)
	rc, _ := waitRC(t, n, work)
	if rc == "0" {
		t.Errorf("rc %q after a kill", rc)
	}
	if n.exists("pools/default/.virticle-bk.AAAAAA/out.part") {
		t.Error("out.part was left after the kill")
	}
}
