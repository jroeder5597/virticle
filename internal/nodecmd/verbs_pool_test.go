package nodecmd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Tests of the verbs that write into the storage pools: download, image
// import (info, convert, tar) and clone preparation (sysprep).

// A child process of the test binary can stand in for the node binary:
// import-convert-start runs Config.Self again, and the signal test runs a
// verb in a process it can kill. The environment says which fake node to use.
func init() {
	root := os.Getenv("NODECMD_TEST_POOL_ROOT")
	if root == "" {
		return
	}
	cfg := layout(root)
	if q := os.Getenv("NODECMD_TEST_QEMU"); q != "" {
		cfg.Tools["qemu-img"] = q
	}
	if os.Getenv("NODECMD_TEST_POOL_SIGNALS") == "" {
		os.Exit(Run(cfg, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	sh := &shared{}
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
	go func() {
		<-sigs
		cancel()
		sh.runCleanups()
		time.Sleep(100 * time.Millisecond)
		os.Exit(1)
	}()
	code := run(ctx, sh, cfg, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	sh.runCleanups()
	os.Exit(code)
}

func serveBytes(t *testing.T, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/img", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/img", http.StatusFound)
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 404) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func hexSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (n *tnode) hidden() []string { return n.glob("pools/default/.*") }

func (n *tnode) runFiles() []string { return n.glob("run/virticle-node/*") }

func TestDownloadRejectsBadURLs(t *testing.T) {
	n := newNode(t)
	bad := []string{
		"", "file:///etc/passwd", "ftp://example.org/x", "gopher://x/", "http://", "https://", "http:/x", "HTTP://example.org/x",
		"-o/pools/default/x", "--config=/etc/shadow", "example.org/x", "http://a b/", "http://a\tb/", "http://a/\nb",
		"http://example.org/x'", "http://example.org/x\"", "http://example.org/x`id`", "http://example.org/x\\y",
		"http://example.org/x<y", "http://example.org/x|y", "http://example.org/x{y}", "http://example.org/\x01",
		"http://example.org/" + strings.Repeat("a", 2100),
	}
	for _, u := range bad {
		n.refused("url "+u, "", "download", u, n.pool("x"))
	}
	if n.exists("pools/default/x") || len(n.hidden()) != 0 || len(n.runFiles()) != 0 {
		t.Error("a refused URL left something behind")
	}
}

func TestDownloadRejectsBadArguments(t *testing.T) {
	n := newNode(t)
	srv := serveBytes(t, "x")
	u := srv.URL + "/img"
	x := n.pool("x")
	for _, p := range badPaths(n.abs("pools/default")) {
		n.refused("dest "+p, "", "download", u, p)
	}
	n.refused("dest outside the pools", "", "download", u, n.abs("outside/x"))
	n.refused("a dot name", "", "download", u, n.pool(".hidden"))
	n.refused("no such directory", "", "download", u, n.pool("nodir/x"))
	n.refused("one argument", "", "download", u)
	n.refused("three arguments", "", "download", u, x, "sha256")
	n.refused("unknown digest", "", "download", u, x, "md5", strings.Repeat("0", 32))
	n.refused("short digest", "", "download", u, x, "sha256", "abcd")
	n.refused("upper case digest", "", "download", u, x, "sha256", strings.ToUpper(hexSum("x")))
	n.refused("digest with a shell word", "", "download", u, x, "sha256", strings.Repeat("0", 63)+";")
	n.refused("sha512 digest of the wrong length", "", "download", u, x, "sha512", hexSum("x"))
	if n.exists("pools/default/x") || len(n.hidden()) != 0 {
		t.Error("a refused download created a file")
	}
}

func TestDownload(t *testing.T) {
	n := newNode(t)
	body := strings.Repeat("image data\n", 1000)
	srv := serveBytes(t, body)
	u := srv.URL + "/img"

	out := n.ok("", "download", u, n.pool("ok.img"))
	if out != "@@done "+strconv.Itoa(len(body)) {
		t.Errorf("answer %q", out)
	}
	got, err := os.ReadFile(n.pool("ok.img"))
	if err != nil || string(got) != body {
		t.Fatalf("content: %v", err)
	}
	if st, _ := os.Stat(n.pool("ok.img")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if len(n.hidden()) != 0 || len(n.runFiles()) != 0 {
		t.Errorf("state left: %v %v", n.hidden(), n.runFiles())
	}
	// A redirect inside http is followed.
	n.ok("", "download", srv.URL+"/hop", n.pool("hop.img"))
	if g, _ := os.ReadFile(n.pool("hop.img")); string(g) != body {
		t.Error("the redirected download differs")
	}

	// An existing file is never replaced.
	n.denied("second download", "", "download", u, n.pool("ok.img"))
	if g, _ := os.ReadFile(n.pool("ok.img")); string(g) != body {
		t.Error("the existing file changed")
	}

	// A leftover part file, or a symbolic link in its place, stops the download.
	n.put("pools/default/.p.img.part", "left over")
	n.denied("leftover part", "", "download", u, n.pool("p.img"))
	if g, _ := os.ReadFile(n.pool(".p.img.part")); string(g) != "left over" {
		t.Error("the leftover part file was touched")
	}
	n.put("outside/victim", "keep")
	if err := os.Symlink(n.abs("outside/victim"), n.pool(".q.img.part")); err != nil {
		t.Fatal(err)
	}
	n.denied("part is a symlink", "", "download", u, n.pool("q.img"))
	if g, _ := os.ReadFile(n.abs("outside/victim")); string(g) != "keep" {
		t.Error("the download wrote through a symbolic link")
	}
	// A symbolic link as the destination name is an existing name too.
	if err := os.Symlink(n.abs("outside/victim"), n.pool("lnk.img")); err != nil {
		t.Fatal(err)
	}
	n.denied("dest is a symlink", "", "download", u, n.pool("lnk.img"))
	if g, _ := os.ReadFile(n.abs("outside/victim")); string(g) != "keep" {
		t.Error("the download wrote through a destination symbolic link")
	}

	// HTTP errors, redirects to local files, endless redirects leave nothing behind.
	n.denied("404", "", "download", srv.URL+"/gone", n.pool("g.img"))
	n.denied("redirect to a local file", "", "download", srv.URL+"/redir", n.pool("r.img"))
	n.denied("redirect loop", "", "download", srv.URL+"/loop", n.pool("l.img"))
	n.denied("connection refused", "", "download", "http://127.0.0.1:1/x", n.pool("c.img"))
	for _, f := range []string{"g.img", "r.img", "l.img", "c.img"} {
		if n.exists("pools/default/" + f) {
			t.Errorf("a failed download left %s", f)
		}
	}
	for _, f := range n.hidden() {
		if !strings.HasSuffix(f, ".p.img.part") && !strings.HasSuffix(f, ".q.img.part") {
			t.Errorf("leftover %s", f)
		}
	}
	if len(n.runFiles()) != 0 {
		t.Errorf("state left: %v", n.runFiles())
	}
}

func TestDownloadChecksTLSCertificates(t *testing.T) {
	n := newNode(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }))
	t.Cleanup(srv.Close)
	out, code := n.run("", "download", srv.URL+"/", n.pool("tls.img"))
	if code != 1 || !strings.Contains(out, "certificate") {
		t.Errorf("a certificate that is not trusted: exit %d: %s", code, out)
	}
	if n.exists("pools/default/tls.img") || len(n.hidden()) != 0 {
		t.Error("a refused TLS download left a file")
	}
}

func TestDownloadDigest(t *testing.T) {
	n := newNode(t)
	body := "some image\n"
	srv := serveBytes(t, body)
	u := srv.URL + "/img"
	n.ok("", "download", u, n.pool("good.img"), "sha256", hexSum(body))
	if !n.exists("pools/default/good.img") {
		t.Fatal("a matching digest did not install the file")
	}
	h512 := sha512.Sum512([]byte(body))
	n.ok("", "download", u, n.pool("good512.img"), "sha512", hex.EncodeToString(h512[:]))
	if !n.exists("pools/default/good512.img") {
		t.Fatal("a matching sha512 did not install the file")
	}
	out, code := n.run("", "download", u, n.pool("bad.img"), "sha256", hexSum("other"))
	if code != 1 || !strings.Contains(out, "checksum mismatch: got "+hexSum(body)) {
		t.Errorf("mismatch: exit %d: %s", code, out)
	}
	_, code = n.run("", "download", u, n.pool("bad512.img"), "sha512", strings.Repeat("a", 128))
	if code != 1 {
		t.Errorf("sha512 mismatch: exit %d", code)
	}
	if n.exists("pools/default/bad.img") || n.exists("pools/default/bad512.img") || len(n.hidden()) != 0 {
		t.Error("a mismatching download left a file")
	}
}

func slowServer(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var started atomic.Bool
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		started.Store(true)
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	return slow, &started
}

func TestDownloadCancel(t *testing.T) {
	n := newNode(t)
	slow, started := slowServer(t)

	n.denied("nothing runs", "", "download-cancel", n.pool("slow.img"))
	n.refused("bad path", "", "download-cancel", n.pool("../x"))
	n.refused("outside the pools", "", "download-cancel", n.abs("outside/x"))

	type res struct {
		out  string
		code int
	}
	done := make(chan res, 1)
	go func() {
		out, code := n.run("", "download", slow.URL+"/", n.pool("slow.img"))
		done <- res{out, code}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !started.Load() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !started.Load() {
		t.Fatal("the download did not start")
	}
	time.Sleep(300 * time.Millisecond)
	n.ok("", "download-cancel", n.pool("slow.img"))
	select {
	case r := <-done:
		if r.code != 1 || !strings.Contains(r.out, "cancelled") {
			t.Errorf("a cancelled download: exit %d: %s", r.code, r.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not stop")
	}
	if n.exists("pools/default/slow.img") || len(n.hidden()) != 0 || len(n.runFiles()) != 0 {
		t.Errorf("a cancelled download left %v %v", n.hidden(), n.runFiles())
	}
	n.denied("it is over", "", "download-cancel", n.pool("slow.img"))
}

// selfNode makes the fake node usable by child processes of the test binary.
func (n *tnode) selfNode(qemu string, signals bool) (self string, env []string) {
	n.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		n.t.Fatal(err)
	}
	env = []string{"NODECMD_TEST_POOL_ROOT=" + n.root, "NODECMD_TEST_QEMU=" + qemu}
	if signals {
		env = append(env, "NODECMD_TEST_POOL_SIGNALS=1")
	}
	script := "#!/bin/sh\n"
	for _, e := range env {
		script += "export " + e + "\n"
	}
	script += "exec " + exe + " \"$@\"\n"
	p := filepath.Join(n.t.TempDir(), "node-self")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		n.t.Fatal(err)
	}
	return p, env
}

// A download that is signalled (the SSH session ends) must not leave its part file.
func TestDownloadKilledLeavesNothing(t *testing.T) {
	n := newNode(t)
	slow, started := slowServer(t)
	exe, _ := os.Executable()
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		started.Store(false)
		_, env := n.selfNode("", true)
		cmd := exec.Command(exe, "download", slow.URL+"/", n.pool("killed.img"))
		cmd.Env = append(os.Environ(), env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for (!started.Load() || len(n.hidden()) == 0) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if len(n.hidden()) == 0 {
			cmd.Process.Kill()
			t.Fatal("the download never started writing")
		}
		_ = cmd.Process.Signal(sig)
		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()
		select {
		case err := <-waited:
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
				t.Errorf("%v: %v", sig, err)
			}
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("%v: the download did not end", sig)
		}
		if left := n.glob("pools/default/*"); len(left) != 0 || len(n.hidden()) != 0 || len(n.runFiles()) != 0 {
			t.Errorf("%v: a killed download left %v %v %v", sig, left, n.hidden(), n.runFiles())
		}
	}
}

func TestRealpathFreeSpace(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/a.img", "x")
	n.put("outside/passwd", "root")
	if err := os.Symlink(n.pool("a.img"), n.pool("in-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.abs("outside/passwd"), n.pool("out-link")); err != nil {
		t.Fatal(err)
	}
	if got := n.ok("", "realpath", "file", n.pool("a.img")); got != n.pool("a.img") {
		t.Errorf("realpath %q", got)
	}
	if got, _, _ := n.runSplit("", "realpath", "file", n.pool("a.img")); strings.HasSuffix(got, "\n") {
		t.Errorf("realpath prints no newline: %q", got)
	}
	if got := n.ok("", "realpath", "file", n.pool("in-link")); got != n.pool("a.img") {
		t.Errorf("realpath of a link inside the pool %q", got)
	}
	if got := n.ok("", "realpath", "dir", n.abs("pools/default")); got != n.abs("pools/default") {
		t.Errorf("realpath dir %q", got)
	}
	n.denied("link out of the pool", "", "realpath", "file", n.pool("out-link"))
	n.denied("outside", "", "realpath", "file", n.abs("outside/passwd"))
	n.denied("dir as file", "", "realpath", "file", n.abs("pools/default"))
	n.denied("file as dir", "", "realpath", "dir", n.pool("a.img"))
	n.denied("missing", "", "realpath", "file", n.pool("none"))
	n.refused("kind", "", "realpath", "any", n.pool("a.img"))
	n.refused("one argument", "", "realpath", "file")
	for _, p := range badPaths(n.abs("pools/default")) {
		n.refused("realpath "+p, "", "realpath", "file", p)
		n.refused("free-space "+p, "", "free-space", p)
		n.refused("tar-list "+p, "", "tar-list", p)
		n.refused("image-info "+p, "", "image-info", p)
	}
	if got := n.ok("", "free-space", n.abs("pools/default")); got == "" || strings.Trim(got, "0123456789") != "" {
		t.Errorf("free-space %q", got)
	}
	n.denied("free-space outside", "", "free-space", n.abs("outside"))
	n.denied("free-space of a file", "", "free-space", n.pool("a.img"))
}

type tarMember struct {
	hdr  tar.Header
	body string
}

func regular(name, body string) tarMember {
	return tarMember{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Uname: "root", Gname: "root",
		ModTime: time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)}, body}
}

func tarFile(t *testing.T, n *tnode, name string, ms ...tarMember) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range ms {
		if err := tw.WriteHeader(&m.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.pool(name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTarVerbs(t *testing.T) {
	n := newNode(t)
	tarFile(t, n, "good.ova", regular("vm.ovf", "ovf\n"), regular("vm-disk1.vmdk", "disk\n"))
	tarFile(t, n, "link.ova", regular("vm.ovf", "ovf\n"),
		tarMember{hdr: tar.Header{Name: "evil.vmdk", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}})
	tarFile(t, n, "hard.ova", regular("vm.ovf", "ovf\n"),
		tarMember{hdr: tar.Header{Name: "evil.vmdk", Typeflag: tar.TypeLink, Linkname: "vm.ovf", Mode: 0o644}})
	tarFile(t, n, "dir.ova", regular("vm.ovf", "ovf\n"),
		tarMember{hdr: tar.Header{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755}}, regular("sub/f", "x"))
	tarFile(t, n, "dots.ova", regular("vm.ovf", "ovf\n"), regular(".hidden", "x"))
	tarFile(t, n, "dotslash.ova", regular("./vm.ovf", "ovf\n"))
	tarFile(t, n, "abs.ova", regular("/etc/vm.ovf", "ovf\n"))
	tarFile(t, n, "up.ova", regular("../vm.ovf", "ovf\n"))
	tarFile(t, n, "dup.ova", regular("vm.ovf", "one"), regular("vm.ovf", "two"))
	tarFile(t, n, "empty.ova")
	n.put("pools/default/notatar", "plain text")
	n.put("pools/default/zero", "")

	list := n.ok("", "tar-list", n.pool("good.ova"))
	lines := strings.Split(list, "\n")
	if len(lines) != 2 || lines[0] != "-rw-r--r-- root/root         4 2026-03-04 05:06 vm.ovf" {
		t.Errorf("tar-list: %q", list)
	}
	if out := n.ok("", "tar-list", n.pool("link.ova")); !strings.Contains(out, "lrwxrwxrwx") || !strings.Contains(out, "evil.vmdk -> /etc/passwd") {
		t.Errorf("tar-list of links: %q", out)
	}
	if out := n.ok("", "tar-list", n.pool("dir.ova")); !strings.Contains(out, "drwxr-xr-x") {
		t.Errorf("tar-list of a directory: %q", out)
	}
	n.denied("not an archive", "", "tar-list", n.pool("notatar"))
	n.denied("outside the pools", "", "tar-list", n.abs("outside/x"))
	n.denied("a directory", "", "tar-list", n.abs("pools/default"))
	if out := n.ok("", "tar-list", n.pool("zero")); out != "" {
		t.Errorf("tar-list of an empty file: %q", out)
	}

	wd := n.ok("", "mkworkdir", n.abs("pools/default"), "import")
	ex := func(file string, names ...string) []string {
		return append([]string{"tar-extract", n.pool(file), wd}, names...)
	}
	n.refused("plain dir as work dir", "", "tar-extract", n.pool("good.ova"), n.abs("pools/default"), "vm.ovf")
	for _, bad := range []string{"../vm.ovf", "/etc/passwd", "a/b", ".hidden", "-x", "vm.ovf;id", "a b", "$(id)", "", strings.Repeat("a", 122)} {
		n.refused("member "+bad, "", ex("good.ova", bad)...)
	}
	n.refused("too few arguments", "", "tar-extract", n.pool("good.ova"), wd)
	n.refused("outside work dir", "", "tar-extract", n.pool("good.ova"), n.abs("outside"), "vm.ovf")
	n.refused("not a .virticle dir", "", "tar-extract", n.pool("good.ova"), n.abs("pools/other"), "vm.ovf")
	n.denied("archive outside the pools", "", "tar-extract", n.abs("outside/x"), wd, "vm.ovf")
	n.denied("a link member anywhere in the archive", "", ex("link.ova", "vm.ovf")...)
	n.denied("a hard link member", "", ex("hard.ova", "vm.ovf")...)
	n.denied("a directory member", "", ex("dir.ova", "vm.ovf")...)
	n.denied("a hidden member", "", ex("dots.ova", "vm.ovf")...)
	n.denied("a ./ member", "", ex("dotslash.ova", "vm.ovf")...)
	n.denied("an absolute member", "", ex("abs.ova", "vm.ovf")...)
	n.denied("a .. member", "", ex("up.ova", "vm.ovf")...)
	n.denied("a duplicate member", "", ex("dup.ova", "vm.ovf")...)
	n.denied("an empty archive", "", ex("empty.ova", "vm.ovf")...)
	n.denied("not an archive", "", ex("notatar", "vm.ovf")...)
	n.denied("a member that is not there", "", ex("good.ova", "nothere.ovf")...)
	if left := n.glob(strings.TrimPrefix(wd, n.root+"/") + "/*"); len(left) != 0 {
		t.Errorf("refused extractions wrote files: %v", left)
	}
	n.ok("", ex("good.ova", "vm.ovf", "vm-disk1.vmdk", "vm.ovf")...)
	if g, _ := os.ReadFile(wd + "/vm.ovf"); string(g) != "ovf\n" {
		t.Errorf("extracted content %q", g)
	}
	if g, _ := os.ReadFile(wd + "/vm-disk1.vmdk"); string(g) != "disk\n" {
		t.Errorf("extracted content %q", g)
	}
	if st, _ := os.Stat(wd + "/vm.ovf"); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	// A link already in the work directory is not written through.
	n.put("outside/victim", "keep")
	if err := os.Symlink(n.abs("outside/victim"), wd+"/vm-disk1.vmdk.x"); err != nil {
		t.Fatal(err)
	}
	tarFile(t, n, "x.ova", regular("vm-disk1.vmdk.x", "overwritten"))
	n.denied("a link in the work directory", "", ex("x.ova", "vm-disk1.vmdk.x")...)
	if g, _ := os.ReadFile(n.abs("outside/victim")); string(g) != "keep" {
		t.Error("extraction wrote through a symbolic link")
	}
}

// vmdkHeader is a sparse VMDK header with an embedded descriptor naming extent.
func vmdkHeader(extent string) []byte {
	b := make([]byte, 4096)
	copy(b, "KDMV")
	b[28] = 1 // descriptor at sector 1
	b[36] = 8 // 8 sectors long
	copy(b[512:], "# Disk DescriptorFile\nversion=1\ncreateType=\"monolithicSparse\"\nRW 2048 SPARSE \""+extent+"\"\n")
	return b
}

// fakeTool writes an executable shell script into a temp directory.
func fakeTool(t *testing.T, name, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// infoQemu answers `qemu-img info` the way the real one would for a plain image.
const infoQemu = `case "$1" in
info) echo '{"virtual-size": 4096, "format": "'"$4"'", "args": "'"$*"'"}' ;;
*) echo unexpected "$@" >&2; exit 3 ;;
esac
`

func TestSniffFormat(t *testing.T) {
	n := newNode(t)
	cases := map[string][]byte{
		"qcow2": {0x51, 0x46, 0x49, 0xfb, 0, 0, 0, 3},
		"vmdk":  []byte("KDMV\x01\x00\x00\x00"),
		"vpc":   []byte("conectix"),
		"vhdx":  []byte("vhdxfile"),
		"vdi":   []byte("<<< Oracle VM VirtualBox Disk Image >>>\n"),
		"raw":   []byte("<<< something else >>>\n"),
	}
	for want, b := range cases {
		p := n.put("pools/default/"+want+".img", string(b))
		f, _ := os.Open(p)
		got, err := sniffFormat(f)
		f.Close()
		if err != nil || got != want {
			t.Errorf("%s: sniffed %q (%v)", want, got, err)
		}
	}
	for _, b := range [][]byte{nil, {0x51}, []byte("conect"), make([]byte, 100)} {
		p := n.put("pools/default/short.img", string(b))
		f, _ := os.Open(p)
		if got, _ := sniffFormat(f); got != "raw" {
			t.Errorf("%q sniffed %q", b, got)
		}
		f.Close()
	}
}

func TestImageInfoRefusesForeignExtents(t *testing.T) {
	n := newNode(t)
	n.cfg.Tools["qemu-img"] = fakeTool(t, "qemu-img", infoQemu)
	write := func(rel string, b []byte) {
		if err := os.WriteFile(n.abs(rel), b, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	write("pools/default/other.vmdk", vmdkHeader("/etc/shadow"))
	write("pools/default/rel.vmdk", vmdkHeader("../../etc/shadow"))
	write("pools/default/two.vmdk", append(vmdkHeader("two.vmdk"), []byte("")...))
	// With a capacity in the header qemu does not follow the descriptor: a copy under another name is fine.
	write("pools/default/renamed.vmdk", func() []byte { b := vmdkHeader("original-name.vmdk"); b[12] = 1; return b }())
	write("pools/default/bad-offset.vmdk", func() []byte { b := vmdkHeader("bad-offset.vmdk"); b[28] = 0; return b }())
	write("pools/default/big-desc.vmdk", func() []byte { b := vmdkHeader("big-desc.vmdk"); b[37] = 8; return b }())
	write("pools/default/short.vmdk", []byte("KDMV"))
	write("pools/default/evil.qcow2", qcowHeader("/etc/shadow"))
	write("pools/default/inside.qcow2", qcowHeader(n.pool("base.img")))
	write("pools/default/base.img", make([]byte, 512))
	for _, f := range []string{"other.vmdk", "rel.vmdk", "bad-offset.vmdk", "big-desc.vmdk", "short.vmdk", "evil.qcow2"} {
		out, code := n.run("", "image-info", n.pool(f))
		if code != 1 && code != 2 {
			t.Errorf("%s: exit %d: %s", f, code, out)
		}
		if strings.Contains(out, "virtual-size") {
			t.Errorf("%s: qemu-img ran: %s", f, out)
		}
	}
	for f, format := range map[string]string{"renamed.vmdk": "vmdk", "two.vmdk": "vmdk", "inside.qcow2": "qcow2", "base.img": "raw"} {
		out := n.ok("", "image-info", n.pool(f))
		if !strings.Contains(out, `"args": "info -U -f `+format+` --output=json -- `+n.pool(f)+`"`) {
			t.Errorf("%s: %s", f, out)
		}
	}
	n.denied("a directory", "", "image-info", n.abs("pools/default"))
	n.denied("missing", "", "image-info", n.pool("none"))
	n.denied("outside", "", "image-info", n.abs("outside/x"))
	// import-convert-start takes the same decision.
	wd := n.ok("", "mkworkdir", n.abs("pools/default"), "import")
	n.denied("convert foreign vmdk", "", "import-convert-start", wd, n.pool("other.vmdk"))
	n.denied("convert qcow2 with outside backing", "", "import-convert-start", wd, n.pool("evil.qcow2"))
	if left := n.glob(strings.TrimPrefix(wd, n.root+"/") + "/*"); len(left) != 0 {
		t.Errorf("refused conversions left files: %v", left)
	}
	// qemu-img missing: the answer is a failure, not a crash.
	n.cfg.Tools["qemu-img"] = ""
	out, code := n.run("", "image-info", n.pool("base.img"))
	if code != 1 || !strings.Contains(out, "not installed") {
		t.Errorf("no qemu-img: exit %d: %s", code, out)
	}
}

func TestConvertArguments(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	wd := n.ok("", "mkworkdir", n.abs("pools/default"), "import")
	raw := n.pool("raw.img")
	for _, p := range badPaths(n.abs("pools/default")) {
		n.refused("import-convert-start dir "+p, "", "import-convert-start", p, raw)
		n.refused("import-convert-start src "+p, "", "import-convert-start", wd, p)
		n.refused("import-convert-status "+p, "", "import-convert-status", p)
		n.refused("import-convert-kill "+p, "", "import-convert-kill", p)
	}
	pool := n.abs("pools/default")
	n.refused("pool dir as work dir", "", "import-convert-start", pool, raw)
	n.refused("status of a pool dir", "", "import-convert-status", pool)
	n.refused("kill in a pool dir", "", "import-convert-kill", pool)
	n.refused("a plain subdirectory as work dir", "", "import-convert-status", n.abs("pools/other"))
	n.denied("source outside the pools", "", "import-convert-start", wd, n.abs("outside/x"))
	n.refused("extra argument", "", "import-convert-start", wd, raw, "qcow2")
	n.refused("run takes the same", "", "import-convert-run", wd)

	// A work directory without a conversion: status is empty, kill does nothing.
	if out := n.ok("", "import-convert-status", wd); out != "---" {
		t.Errorf("status %q", out)
	}
	n.ok("", "import-convert-kill", wd)
	// import-convert-kill only signals a process that is qemu-img.
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sleeper.Process.Kill(); sleeper.Wait() })
	if err := os.WriteFile(wd+"/qpid", []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	n.ok("", "import-convert-kill", wd)
	if err := sleeper.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("import-convert-kill signalled a process that is not qemu-img: %v", err)
	}
	for _, junk := range []string{"abc\n", "-1\n", "1 2\n", "\n", "1234567890123456789\n"} {
		os.WriteFile(wd+"/qpid", []byte(junk), 0o666)
		n.ok("", "import-convert-kill", wd)
	}
}

// convertQemu is a stand-in for qemu-img that "converts" by copying.
const convertQemu = `case "$1" in
info) echo '{"virtual-size": 4096, "format": "'"$4"'"}' ;;
convert)
  printf '    (0.00/100%%)\r    (50.00/100%%)\r' >&2
  case "$9" in *fail*) echo "qemu-img: Could not open" >&2; exit 5 ;; esac
  cp "$9" "${10}"
  printf '    (100.00/100%%)\r' >&2
  ;;
*) echo unexpected "$@" >&2; exit 3 ;;
esac
`

func waitStatus(t *testing.T, n *tnode, wd string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out := n.ok("", "import-convert-status", wd)
		if !strings.HasPrefix(out, "---") {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the conversion did not finish")
	return ""
}

func TestConvertRuns(t *testing.T) {
	n := newNode(t)
	q := fakeTool(t, "qemu-img", convertQemu)
	n.cfg.Tools["qemu-img"] = q
	n.cfg.Self, _ = n.selfNode(q, false)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 1<<16))
	n.put("pools/default/fail.img", "x")
	wd := n.ok("", "mkworkdir", n.abs("pools/default"), "import")
	// Old state is removed by a new start.
	for _, f := range []string{"rc", "qpid", "prog.log", "out.part"} {
		os.WriteFile(wd+"/"+f, []byte("9\n"), 0o600)
	}
	if got := n.ok("", "import-convert-start", wd, n.pool("raw.img")); got != "started" {
		t.Fatalf("start: %q", got)
	}
	out := waitStatus(t, n, wd)
	if !strings.HasPrefix(out, "0\n---\n") || !strings.Contains(out, "100.00/100%") || strings.Contains(out, "\r") {
		t.Errorf("status: %q", out)
	}
	if st, err := os.Stat(wd + "/out.part"); err != nil || st.Size() != 1<<16 {
		t.Errorf("out.part: %v", err)
	}
	if st, _ := os.Stat(wd + "/prog.log"); st.Mode().Perm() != 0o600 {
		t.Errorf("prog.log mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(wd + "/qpid"); !numOK(strings.TrimSpace(string(b))) {
		t.Errorf("qpid %q", b)
	}
	n.ok("", "move", wd+"/out.part", n.pool("out.qcow2"))

	// A failing conversion reports its exit status and message.
	wd2 := n.ok("", "mkworkdir", n.abs("pools/default"), "import")
	n.ok("", "import-convert-start", wd2, n.pool("fail.img"))
	out = waitStatus(t, n, wd2)
	if !strings.HasPrefix(out, "5\n---\n") || !strings.Contains(out, "Could not open") {
		t.Errorf("failed status: %q", out)
	}
}

func TestConvertKill(t *testing.T) {
	n := newNode(t)
	// The process name is what import-convert-kill checks: a script called qemu-img.
	slow := fakeTool(t, "qemu-img", "printf '    (1.00/100%%)\\r' >&2\nsleep 20 &\nwait\n")
	n.cfg.Tools["qemu-img"] = slow
	n.cfg.Self, _ = n.selfNode(slow, false)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	wd := n.ok("", "mkworkdir", n.abs("pools/default"), "import")
	n.ok("", "import-convert-start", wd, n.pool("raw.img"))
	deadline := time.Now().Add(10 * time.Second)
	var pid int
	for time.Now().Before(deadline) && pid == 0 {
		if b, err := os.ReadFile(wd + "/qpid"); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the conversion did not start")
	}
	if out := n.ok("", "import-convert-status", wd); !strings.HasPrefix(out, "---") {
		t.Errorf("a running conversion has no exit status: %q", out)
	}
	n.ok("", "import-convert-kill", wd)
	out := waitStatus(t, n, wd)
	if !strings.HasPrefix(out, "143\n---") {
		t.Errorf("killed conversion: %q", out)
	}
}

func TestSysprep(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	n.put("pools/default/disk.vmdk", "KDMV"+strings.Repeat("\x00", 4096))
	n.put("outside/outside.img", "x")
	if err := os.Symlink(n.pool("raw.img"), n.pool("link.img")); err != nil {
		t.Fatal(err)
	}
	for _, p := range badPaths(n.abs("pools/default")) {
		n.refused("sysprep "+p, "", "sysprep", p)
	}
	raw := n.pool("raw.img")
	n.refused("no disk", "", "sysprep")
	n.refused("too many disks", "", append([]string{"sysprep"}, []string{raw, raw, raw, raw, raw, raw, raw, raw, raw}...)...)
	n.refused("option as disk", "", "sysprep", "--run-command")
	n.refused("outside the pools", "", "sysprep", n.abs("outside/outside.img"))
	n.refused("a symbolic link", "", "sysprep", n.pool("link.img"))
	n.refused("a pool directory", "", "sysprep", n.abs("pools/default"))
	// vmdk and friends are not prepared: the format is recognised from the
	// content and only qcow2 and raw are accepted.
	out, code := n.run("", "sysprep", n.pool("disk.vmdk"))
	if code == 0 || !strings.Contains(out, "neither qcow2 nor raw") {
		t.Errorf("vmdk: exit %d: %s", code, out)
	}
	n.put("pools/default/evil.qcow2", string(qcowHeader("/etc/shadow")))
	n.denied("backing file outside the pools", "", "sysprep", n.pool("evil.qcow2"))
	n.denied("missing", "", "sysprep", n.pool("none"))
	n.denied("a directory", "", "sysprep", n.abs("pools/other"))
	// A plain image passes validation; without virt-sysprep the run stops there.
	n.cfg.Tools["virt-sysprep"] = ""
	out, code = n.run("", "sysprep", raw)
	if code != 1 || !strings.Contains(out, "virt-sysprep is not installed") {
		t.Errorf("valid image, no tool: exit %d: %s", code, out)
	}

	// With a stand-in for virt-sysprep: a fixed option set, formats from the content.
	rec := filepath.Join(t.TempDir(), "args")
	n.put("pools/default/disk.qcow2", string(qcowHeader("")))
	n.cfg.Tools["virt-sysprep"] = fakeTool(t, "virt-sysprep", `for a in "$@"; do printf '%s\n' "$a"; done > `+rec+`
echo "backend=$LIBGUESTFS_BACKEND"
echo "sysprep done"
case "$*" in *failme*) exit 4 ;; esac
`)
	out = n.ok("", "sysprep", raw, n.pool("disk.qcow2"))
	if !strings.Contains(out, "backend=direct") || !strings.HasSuffix(out, "sysprep done") {
		t.Errorf("output %q", out)
	}
	got, _ := os.ReadFile(rec)
	want := "--run-command\n" + SysprepFixups + "\n--format\nraw\n-a\n" + raw + "\n--format\nqcow2\n-a\n" + n.pool("disk.qcow2") + "\n"
	if string(got) != want {
		t.Errorf("virt-sysprep arguments:\n%s\nwant:\n%s", got, want)
	}
	n.put("pools/default/failme.img", "x")
	if o, code := n.run("", "sysprep", n.pool("failme.img")); code != 4 || !strings.Contains(o, "sysprep done") {
		t.Errorf("the status of virt-sysprep is passed on: exit %d: %s", code, o)
	}
}
