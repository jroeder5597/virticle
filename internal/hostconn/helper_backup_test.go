package hostconn

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A backup directory in the fake pool.
func (n *fakeNode) backupDir(name string) string {
	n.t.Helper()
	d := "pools/default/" + name
	if err := os.MkdirAll(n.abs(d), 0o777); err != nil {
		n.t.Fatal(err)
	}
	_ = os.Chmod(n.abs(d), 0o777)
	n.put(d+"/manifest.json", `{"format":"x"}`)
	n.put(d+"/domain.xml", "<domain/>")
	n.put(d+"/vda.qcow2", "0123456789")
	return "/pools/default/" + name
}

func (n *fakeNode) workDir(name string) string {
	n.t.Helper()
	d := "pools/default/" + name
	if err := os.MkdirAll(n.abs(d), 0o777); err != nil {
		n.t.Fatal(err)
	}
	_ = os.Chmod(n.abs(d), 0o777)
	return "/pools/default/" + name
}

func TestBackupVerbsRejectBadPaths(t *testing.T) {
	n := poolNode(t)
	n.backupDir("a.vmbackup")
	for _, p := range badPaths {
		n.denied("df "+p, "", "df", p)
		n.denied("files-size "+p, "", "files-size", p, "manifest.json")
		n.denied("backup-scan "+p, "", "backup-scan", p, "@@@0123456789abcdef")
		n.denied("tar-pack "+p, "", "tar-pack", p, "manifest.json")
		n.denied("tar-unpack "+p, "", "tar-unpack", p)
		n.denied("convert-start work "+p, "", "convert-start", p, "/pools/default/a.vmbackup/vda.qcow2", "qcow2")
		n.denied("convert-start src "+p, "", "convert-start", "/pools/default/.virticle-bk.AAAAAA", p, "qcow2")
		n.denied("convert-poll "+p, "", "convert-poll", p)
		n.denied("convert-kill "+p, "", "convert-kill", p)
		n.denied("convert-finish work "+p, "", "convert-finish", p, "/pools/default/new")
		n.denied("convert-finish dest "+p, "", "convert-finish", "/pools/default/.virticle-bk.AAAAAA", p)
	}
}

func TestBackupVerbsStayInsideThePools(t *testing.T) {
	n := poolNode(t)
	n.put("etc/passwd", "root:x:0:0\n")
	_ = os.MkdirAll(n.abs("pools/other/x.vmbackup"), 0o777)
	n.put("pools/other/x.vmbackup/manifest.json", "{}")
	n.put("pools/other/x", "x")
	work := n.workDir(".virticle-bk.AAAAAA")
	for _, p := range []string{"/etc", "/etc/secret", "/pools/other", "/tmp", "/var/lib/libvirt/images", "/pools/other/x.vmbackup"} {
		n.denied("df "+p, "", "df", p)
		n.denied("files-size "+p, "", "files-size", p, "manifest.json")
		n.denied("backup-scan "+p, "", "backup-scan", p, "@@@0123456789abcdef")
		n.denied("tar-pack "+p, "", "tar-pack", p, "manifest.json")
		n.denied("tar-unpack "+p, "", "tar-unpack", p)
		n.denied("convert-start work "+p, "", "convert-start", p, "/pools/default/x", "qcow2")
		n.denied("convert-poll "+p, "", "convert-poll", p)
		n.denied("convert-kill "+p, "", "convert-kill", p)
		n.denied("convert-start src "+p, "", "convert-start", work, p, "raw")
		n.denied("convert-finish dest "+p, "", "convert-finish", work, p)
	}
	n.denied("convert-start of /etc/passwd", "", "convert-start", work, "/etc/passwd", "raw")
}

func TestBackupDf(t *testing.T) {
	n := poolNode(t)
	out := n.ok("", "df", "/pools/default")
	if out == "" || strings.Trim(out, "0123456789") != "" {
		t.Errorf("df printed %q", out)
	}
	n.denied("two arguments", "", "df", "/pools/default", "/pools/default")
}

func TestBackupFilesSize(t *testing.T) {
	n := poolNode(t)
	d := n.backupDir("a.vmbackup")
	if err := os.Symlink("/etc/passwd", n.abs("pools/default/a.vmbackup/link.qcow2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(n.abs("pools/default/a.vmbackup/sub"), 0o777); err != nil {
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
	n := poolNode(t)
	n.backupDir("a.vmbackup")
	n.backupDir("b.vmbackup")
	n.backupDir("notabackup") // no .vmbackup suffix
	n.put("pools/default/notabackup/manifest.json", "{}")
	// A link to a directory in the pool and a deeper manifest are not listed.
	if err := os.Symlink("a.vmbackup", n.abs("pools/default/c.vmbackup")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.abs("pools/default/a.vmbackup/d.vmbackup"), 0o777); err != nil {
		t.Fatal(err)
	}
	n.put("pools/default/a.vmbackup/d.vmbackup/manifest.json", "{}")
	m := "@@@0123456789abcdef"
	out := n.ok("", "backup-scan", "/pools/default", m)
	for _, want := range []string{m + " /pools/default/a.vmbackup\n", m + " /pools/default/b.vmbackup\n", `{"format":"x"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("scan lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, m) != 2 {
		t.Errorf("want exactly two backups:\n%s", out)
	}
	for _, bad := range []string{"", "@@@", "@@@0123456789abcde", "@@@0123456789abcdeg", "@@@0123456789ABCDEF", "x@@0123456789abcdef", "@@@0123456789abcdef;id", "@@@0123456789abcdef\n"} {
		n.refused("marker "+bad, "", "backup-scan", "/pools/default", bad)
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
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	n := poolNode(t)
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
	}
	if strings.Join(names, ",") != "manifest.json,vda.qcow2" {
		t.Errorf("members %v", names)
	}
	if err := os.Symlink("/etc/passwd", n.abs("pools/default/a.vmbackup/link")); err != nil {
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
}

func TestBackupTarUnpack(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	n := poolNode(t)
	good := tarOf(t, tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Size: 5}, tar.Header{Name: "vda.qcow2", Typeflag: tar.TypeReg, Size: 7})
	w := n.workDir(".virticle-imp.AAAAAA")
	n.ok(good, "tar-unpack", w)
	if b, _ := os.ReadFile(n.abs("pools/default/.virticle-imp.AAAAAA/vda.qcow2")); len(b) != 7 {
		t.Errorf("unpacked file has %d bytes", len(b))
	}
	n.denied("a directory that is not empty", good, "tar-unpack", w)
	n.denied("a directory that is not a work directory", good, "tar-unpack", n.workDir("plain"))
	n.denied("the pool itself", good, "tar-unpack", "/pools/default")
	empty := func(w string) {
		t.Helper()
		ents, err := os.ReadDir(n.abs("pools" + strings.TrimPrefix(w, "/pools")))
		if err != nil || len(ents) != 0 {
			t.Errorf("%s was not emptied: %v %v", w, ents, err)
		}
	}
	for name, archive := range map[string]string{
		"symlink":  tarOf(t, tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"hardlink": tarOf(t, tar.Header{Name: "evil", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}),
		"dir":      tarOf(t, tar.Header{Name: "sub", Typeflag: tar.TypeDir}),
		"fifo":     tarOf(t, tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}),
		"through a link": tarOf(t,
			tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
			tar.Header{Name: "evil/passwd", Typeflag: tar.TypeReg, Size: 3}),
	} {
		w := n.workDir(".virticle-imp." + strings.ToUpper(strings.ReplaceAll(name[:3], " ", "")) + "XXX")
		n.denied(name, archive, "tar-unpack", w)
		empty(w)
	}
	n.put("etc/passwd", "root:x:0:0\n")
	// A name with a slash is skipped, never written.
	w2 := n.workDir(".virticle-imp.BBBBBB")
	n.ok(tarOf(t, tar.Header{Name: "a/b", Typeflag: tar.TypeReg, Size: 3}, tar.Header{Name: "ok", Typeflag: tar.TypeReg, Size: 3}), "tar-unpack", w2)
	if n.exists("pools/default/.virticle-imp.BBBBBB/a") || !n.exists("pools/default/.virticle-imp.BBBBBB/ok") {
		t.Error("a member with a slash was written, or the plain one was not")
	}
	if b, _ := os.ReadFile(n.abs("etc/passwd")); string(b) != "root:x:0:0\n" {
		t.Errorf("/etc/passwd changed: %q", b)
	}
	n.denied("not a tar", "this is not a tar archive", "tar-unpack", n.workDir(".virticle-imp.CCCCCC"))
}

func TestBackupConvertValidation(t *testing.T) {
	n := poolNode(t)
	work := n.workDir(".virticle-bk.AAAAAA")
	n.put("pools/default/src.img", "RAW")
	plain := n.workDir("plain")
	for _, f := range []string{"", "vmdk", "qcow", "raw;id", "-f", "QCOW2", "qcow2 "} {
		n.denied("format "+f, "", "convert-start", work, "/pools/default/src.img", f)
	}
	n.denied("not a work dir", "", "convert-start", plain, "/pools/default/src.img", "raw")
	n.denied("source is a directory", "", "convert-start", work, plain, "raw")
	n.denied("source missing", "", "convert-start", work, "/pools/default/none", "raw")
	n.denied("poll of a plain dir", "", "convert-poll", plain)
	n.denied("kill in a plain dir", "", "convert-kill", plain)
	n.denied("finish in a plain dir", "", "convert-finish", plain, "/pools/default/new")
	n.denied("finish without a result", "", "convert-finish", work, "/pools/default/new")
	n.put("pools/default/.virticle-bk.AAAAAA/out.part", "converted")
	n.put("pools/default/exists", "x")
	n.denied("finish onto an existing file", "", "convert-finish", work, "/pools/default/exists")
	n.denied("finish outside the pool", "", "convert-finish", work, "/etc/new")
	if !n.exists("pools/default/.virticle-bk.AAAAAA/out.part") {
		t.Error("the result was moved although the target was refused")
	}
	n.ok("", "convert-finish", work, "/pools/default/new")
	if !n.exists("pools/default/new") || n.exists("pools/default/.virticle-bk.AAAAAA/out.part") {
		t.Error("convert-finish did not move the result")
	}
	// poll and kill on a quiet work directory
	if out := n.ok("", "convert-poll", work); !strings.HasPrefix(out, "---") {
		t.Errorf("poll: %q", out)
	}
	n.put("pools/default/.virticle-bk.AAAAAA/qpid", "1; id")
	n.ok("", "convert-kill", work)
	n.put("pools/default/.virticle-bk.AAAAAA/qpid", "999999")
	n.ok("", "convert-kill", work)
}

func TestBackupConvertRuns(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("no qemu-img")
	}
	n := poolNode(t)
	work := n.workDir(".virticle-bk.AAAAAA")
	n.put("pools/default/src.img", strings.Repeat("x", 4096))
	n.ok("", "convert-start", work, "/pools/default/src.img", "raw")
	for i := 0; i < 50; i++ {
		if out := n.ok("", "convert-poll", work); !strings.HasPrefix(out, "---") {
			if !strings.HasPrefix(out, "0") {
				t.Fatalf("conversion failed: %s", out)
			}
			break
		}
		exec.Command("sleep", "0.2").Run()
	}
	n.ok("", "convert-finish", work, "/pools/default/out.qcow2")
	if !n.exists("pools/default/out.qcow2") {
		t.Error("no result")
	}
}
