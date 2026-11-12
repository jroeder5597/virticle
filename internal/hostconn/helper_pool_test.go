package hostconn

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Tests of the verbs that write into the storage pools: download, image
// import (info, convert, tar) and clone preparation (sysprep).

func serveBytes(t *testing.T, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/img", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
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

func TestHelperDownloadRejectsBadURLs(t *testing.T) {
	n := poolNode(t)
	bad := []string{
		"", "file:///etc/passwd", "ftp://example.org/x", "gopher://x/", "http://", "https://", "http:/x", "HTTP://example.org/x",
		"-o/pools/default/x", "--config=/etc/shadow", "example.org/x", "http://a b/", "http://a\tb/", "http://a/\nb",
		"http://example.org/x'", "http://example.org/x\"", "http://example.org/x`id`", "http://example.org/x\\y",
		"http://example.org/x<y", "http://example.org/x|y", "http://example.org/x{y}", "http://example.org/\x01",
		"http://example.org/" + strings.Repeat("a", 2100),
	}
	for _, u := range bad {
		n.refused("url "+u, "", "download", u, "/pools/default/x")
	}
	if n.exists("pools/default/x") || len(globAll(t, n, "pools/default/.*")) != 0 {
		t.Error("a refused URL left something behind")
	}
}

func TestHelperDownloadRejectsBadArguments(t *testing.T) {
	n := poolNode(t)
	srv := serveBytes(t, "x")
	u := srv.URL + "/img"
	for _, p := range badPaths {
		n.refused("dest "+p, "", "download", u, p)
	}
	n.refused("dest outside the pools", "", "download", u, "/etc/x")
	n.refused("dest in the NVRAM dir is fine, but a dot name is not", "", "download", u, "/pools/default/.hidden")
	n.refused("no such directory", "", "download", u, "/pools/default/nodir/x")
	n.refused("one argument", "", "download", u)
	n.refused("three arguments", "", "download", u, "/pools/default/x", "sha256")
	n.refused("unknown digest", "", "download", u, "/pools/default/x", "md5", strings.Repeat("0", 32))
	n.refused("short digest", "", "download", u, "/pools/default/x", "sha256", "abcd")
	n.refused("upper case digest", "", "download", u, "/pools/default/x", "sha256", strings.ToUpper(hexSum("x")))
	n.refused("digest with a shell word", "", "download", u, "/pools/default/x", "sha256", strings.Repeat("0", 63)+";")
	n.refused("sha512 digest of the wrong length", "", "download", u, "/pools/default/x", "sha512", hexSum("x"))
	if n.exists("pools/default/x") {
		t.Error("a refused download created the file")
	}
}

func TestHelperDownload(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is missing")
	}
	n := poolNode(t)
	body := strings.Repeat("image data\n", 1000)
	srv := serveBytes(t, body)
	u := srv.URL + "/img"

	out := n.ok("", "download", u, "/pools/default/ok.img")
	if !strings.HasSuffix(out, "@@done "+itoa(len(body))) {
		t.Errorf("answer %q", out)
	}
	got, err := os.ReadFile(n.abs("pools/default/ok.img"))
	if err != nil || string(got) != body {
		t.Fatalf("content: %v", err)
	}
	if st, _ := os.Stat(n.abs("pools/default/ok.img")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if len(globAll(t, n, "pools/default/.*")) != 0 {
		t.Errorf("part file left: %v", globAll(t, n, "pools/default/.*"))
	}

	// An existing file is never replaced.
	n.denied("second download", "", "download", u, "/pools/default/ok.img")
	if g, _ := os.ReadFile(n.abs("pools/default/ok.img")); string(g) != body {
		t.Error("the existing file changed")
	}

	// A leftover part file, or a symbolic link in its place, stops the download.
	n.put("pools/default/.p.img.part", "left over")
	n.denied("leftover part", "", "download", u, "/pools/default/p.img")
	if g, _ := os.ReadFile(n.abs("pools/default/.p.img.part")); string(g) != "left over" {
		t.Error("the leftover part file was touched")
	}
	n.put("etc/secret/victim", "keep")
	if err := os.Symlink(n.abs("etc/secret/victim"), n.abs("pools/default/.q.img.part")); err != nil {
		t.Fatal(err)
	}
	n.denied("part is a symlink", "", "download", u, "/pools/default/q.img")
	if g, _ := os.ReadFile(n.abs("etc/secret/victim")); string(g) != "keep" {
		t.Error("the download wrote through a symbolic link")
	}
	// A symbolic link as the destination name is an existing name too.
	if err := os.Symlink(n.abs("etc/secret/victim"), n.abs("pools/default/lnk.img")); err != nil {
		t.Fatal(err)
	}
	n.denied("dest is a symlink", "", "download", u, "/pools/default/lnk.img")
	if g, _ := os.ReadFile(n.abs("etc/secret/victim")); string(g) != "keep" {
		t.Error("the download wrote through a destination symbolic link")
	}

	// HTTP errors and redirects to local files leave nothing behind.
	n.denied("404", "", "download", srv.URL+"/gone", "/pools/default/g.img")
	n.denied("redirect to a local file", "", "download", srv.URL+"/redir", "/pools/default/r.img")
	if n.exists("pools/default/g.img") || n.exists("pools/default/r.img") {
		t.Error("a failed download left a file")
	}
	for _, f := range globAll(t, n, "pools/default/.*") {
		if !strings.HasSuffix(f, ".p.img.part") && !strings.HasSuffix(f, ".q.img.part") {
			t.Errorf("leftover %s", f)
		}
	}
}

func TestHelperDownloadDigest(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is missing")
	}
	n := poolNode(t)
	body := "some image\n"
	srv := serveBytes(t, body)
	u := srv.URL + "/img"
	n.ok("", "download", u, "/pools/default/good.img", "sha256", hexSum(body))
	if !n.exists("pools/default/good.img") {
		t.Fatal("a matching digest did not install the file")
	}
	wrong := hexSum("other")
	out, code := n.run("", "download", u, "/pools/default/bad.img", "sha256", wrong)
	if code != 1 || !strings.Contains(out, "checksum mismatch: got "+hexSum(body)) {
		t.Errorf("mismatch: exit %d: %s", code, out)
	}
	if n.exists("pools/default/bad.img") || len(globAll(t, n, "pools/default/.bad*")) != 0 {
		t.Error("a mismatching download left a file")
	}
}

func TestHelperDownloadCancel(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is missing")
	}
	n := poolNode(t)
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

	n.denied("nothing runs", "", "download-cancel", "/pools/default/slow.img")
	n.refused("bad path", "", "download-cancel", "/pools/default/../x")

	type res struct {
		out  string
		code int
	}
	done := make(chan res, 1)
	go func() {
		out, code := n.run("", "download", slow.URL+"/", "/pools/default/slow.img")
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
	n.ok("", "download-cancel", "/pools/default/slow.img")
	select {
	case r := <-done:
		if r.code != 1 {
			t.Errorf("a cancelled download: exit %d: %s", r.code, r.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not stop")
	}
	if n.exists("pools/default/slow.img") || len(globAll(t, n, "pools/default/.slow*")) != 0 {
		t.Error("a cancelled download left a file")
	}
}

func TestHelperRealpathFreeSpace(t *testing.T) {
	n := poolNode(t)
	n.put("pools/default/a.img", "x")
	n.put("etc/passwd", "root")
	if err := os.Symlink(n.abs("pools/default/a.img"), n.abs("pools/default/in-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.abs("etc/passwd"), n.abs("pools/default/out-link")); err != nil {
		t.Fatal(err)
	}
	if got := n.ok("", "realpath", "file", "/pools/default/a.img"); got != "/pools/default/a.img" {
		t.Errorf("realpath %q", got)
	}
	if got := n.ok("", "realpath", "file", "/pools/default/in-link"); got != "/pools/default/a.img" {
		t.Errorf("realpath of a link inside the pool %q", got)
	}
	if got := n.ok("", "realpath", "dir", "/pools/default"); got != "/pools/default" {
		t.Errorf("realpath dir %q", got)
	}
	n.denied("link out of the pool", "", "realpath", "file", "/pools/default/out-link")
	n.denied("outside", "", "realpath", "file", "/etc/passwd")
	n.denied("dir as file", "", "realpath", "file", "/pools/default")
	n.denied("file as dir", "", "realpath", "dir", "/pools/default/a.img")
	n.denied("missing", "", "realpath", "file", "/pools/default/none")
	n.refused("kind", "", "realpath", "any", "/pools/default/a.img")
	for _, p := range badPaths {
		n.refused("realpath "+p, "", "realpath", "file", p)
		n.refused("free-space "+p, "", "free-space", p)
		n.refused("tar-list "+p, "", "tar-list", p)
		n.refused("image-info "+p, "", "image-info", p)
	}
	if got := strings.TrimSpace(n.ok("", "free-space", "/pools/default")); got == "" || strings.Trim(got, "0123456789") != "" {
		t.Errorf("free-space %q", got)
	}
	n.denied("free-space outside", "", "free-space", "/etc")
	n.denied("free-space of a file", "", "free-space", "/pools/default/a.img")
}

// tarFile creates an archive in the pool with the given shell commands run in a scratch directory.
func tarFile(t *testing.T, n *fakeNode, name, script string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	cmd = exec.Command("sh", "-c", "tar -cf "+n.abs("pools/default/"+name)+" $(ls -A)")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	_ = os.Chmod(n.abs("pools/default/"+name), 0o666)
}

func TestHelperTarVerbs(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is missing")
	}
	n := poolNode(t)
	tarFile(t, n, "good.ova", "echo ovf > vm.ovf; echo disk > vm-disk1.vmdk")
	tarFile(t, n, "link.ova", "echo ovf > vm.ovf; ln -s /etc/passwd evil.vmdk")
	tarFile(t, n, "dir.ova", "echo ovf > vm.ovf; mkdir sub; echo x > sub/f")
	tarFile(t, n, "dots.ova", "echo ovf > vm.ovf; echo x > .hidden")
	n.put("pools/default/notatar", "plain text")

	if out := n.ok("", "tar-list", "/pools/default/good.ova"); !strings.Contains(out, "vm.ovf") || !strings.Contains(out, "vm-disk1.vmdk") {
		t.Errorf("tar-list: %q", out)
	}
	n.denied("not an archive", "", "tar-list", "/pools/default/notatar")
	n.denied("outside", "", "tar-list", "/etc/passwd")

	wd := n.ok("", "mkworkdir", "/pools/default", "import")
	n.refused("plain file as work dir", "", "tar-extract", "/pools/default/good.ova", "/pools/default", "vm.ovf")
	for _, bad := range []string{"../vm.ovf", "/etc/passwd", "a/b", ".hidden", "-x", "vm.ovf;id", "a b", "$(id)", ""} {
		n.refused("member "+bad, "", "tar-extract", "/pools/default/good.ova", wd, bad)
	}
	n.refused("outside work dir", "", "tar-extract", "/pools/default/good.ova", "/etc/secret", "vm.ovf")
	n.refused("not a .virticle dir", "", "tar-extract", "/pools/default/good.ova", "/pools/other", "vm.ovf")
	n.denied("archive outside the pools", "", "tar-extract", "/etc/passwd", wd, "vm.ovf")
	n.denied("a link member anywhere in the archive", "", "tar-extract", "/pools/default/link.ova", wd, "vm.ovf")
	n.denied("a directory member", "", "tar-extract", "/pools/default/dir.ova", wd, "vm.ovf")
	n.denied("a hidden member", "", "tar-extract", "/pools/default/dots.ova", wd, "vm.ovf")
	if left := globAll(t, n, strings.TrimPrefix(wd, "/")+"/*"); len(left) != 0 {
		t.Errorf("refused extractions wrote files: %v", left)
	}
	n.ok("", "tar-extract", "/pools/default/good.ova", wd, "vm.ovf", "vm-disk1.vmdk")
	if g, _ := os.ReadFile(n.abs(strings.TrimPrefix(wd, "/") + "/vm.ovf")); string(g) != "ovf\n" {
		t.Errorf("extracted content %q", g)
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

func TestHelperImageInfoRefusesForeignExtents(t *testing.T) {
	n := poolNode(t)
	write := func(rel string, b []byte) {
		if err := os.WriteFile(n.abs(rel), b, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	write("pools/default/other.vmdk", vmdkHeader("/etc/shadow"))
	write("pools/default/rel.vmdk", vmdkHeader("../../etc/shadow"))
	// With a capacity in the header qemu does not follow the descriptor: a copy under another name is fine.
	write("pools/default/renamed.vmdk", func() []byte { b := vmdkHeader("original-name.vmdk"); b[12] = 1; return b }())
	write("pools/default/bad-offset.vmdk", func() []byte { b := vmdkHeader("bad-offset.vmdk"); b[28] = 0; return b }())
	write("pools/default/evil.qcow2", qcowHeader("/etc/shadow"))
	for _, f := range []string{"other.vmdk", "rel.vmdk", "bad-offset.vmdk", "evil.qcow2"} {
		out, code := n.run("", "image-info", "/pools/default/"+f)
		if code != 1 && code != 2 {
			t.Errorf("%s: exit %d: %s", f, code, out)
		}
		if strings.Contains(out, "virtual-size") {
			t.Errorf("%s: qemu-img ran: %s", f, out)
		}
	}
	if out, _ := n.run("", "image-info", "/pools/default/renamed.vmdk"); strings.Contains(out, "refusing VMDK") {
		t.Errorf("a self-contained VMDK under another name was refused: %s", out)
	}
	// convert-start takes the same decision.
	wd := n.ok("", "mkworkdir", "/pools/default", "import")
	n.denied("convert foreign vmdk", "", "convert-start", wd, "/pools/default/other.vmdk")
	n.denied("convert qcow2 with outside backing", "", "convert-start", wd, "/pools/default/evil.qcow2")
	if left := globAll(t, n, strings.TrimPrefix(wd, "/")+"/*"); len(left) != 0 {
		t.Errorf("refused conversions left files: %v", left)
	}
}

func TestHelperConvertArguments(t *testing.T) {
	n := poolNode(t)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	wd := n.ok("", "mkworkdir", "/pools/default", "import")
	for _, p := range badPaths {
		n.refused("convert-start dir "+p, "", "convert-start", p, "/pools/default/raw.img")
		n.refused("convert-start src "+p, "", "convert-start", wd, p)
		n.refused("convert-status "+p, "", "convert-status", p)
		n.refused("convert-kill "+p, "", "convert-kill", p)
	}
	n.refused("pool dir as work dir", "", "convert-start", "/pools/default", "/pools/default/raw.img")
	n.refused("status of a pool dir", "", "convert-status", "/pools/default")
	n.refused("kill in a pool dir", "", "convert-kill", "/pools/default")
	n.denied("source outside the pools", "", "convert-start", wd, "/etc/passwd")
	n.refused("extra argument", "", "convert-start", wd, "/pools/default/raw.img", "qcow2")

	// A work directory without a conversion: status is empty, kill does nothing.
	if out := n.ok("", "convert-status", wd); out != "---" {
		t.Errorf("status %q", out)
	}
	n.ok("", "convert-kill", wd)
	// convert-kill only signals a process that is qemu-img.
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sleeper.Process.Kill(); sleeper.Wait() })
	if err := os.WriteFile(n.abs(strings.TrimPrefix(wd, "/")+"/qpid"), []byte(itoa(sleeper.Process.Pid)+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	n.ok("", "convert-kill", wd)
	if err := sleeper.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("convert-kill signalled a process that is not qemu-img: %v", err)
	}
}

func TestHelperConvertRuns(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is missing")
	}
	n := poolNode(t)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 1<<20))
	if out := n.ok("", "image-info", "/pools/default/raw.img"); !strings.Contains(out, `"format": "raw"`) {
		t.Errorf("image-info: %s", out)
	}
	wd := n.ok("", "mkworkdir", "/pools/default", "import")
	n.ok("", "convert-start", wd, "/pools/default/raw.img")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if out := n.ok("", "convert-status", wd); strings.HasPrefix(out, "0\n---") {
			n.ok("", "move", wd+"/out.part", "/pools/default/out.qcow2")
			if out := n.ok("", "image-info", "/pools/default/out.qcow2"); !strings.Contains(out, `"format": "qcow2"`) {
				t.Errorf("result: %s", out)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("conversion did not finish")
}

func TestHelperSysprepValidation(t *testing.T) {
	n := poolNode(t)
	n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	n.put("pools/default/disk.vmdk", "KDMV"+strings.Repeat("\x00", 4096))
	n.put("etc/secret/outside.img", "x")
	if err := os.Symlink(n.abs("pools/default/raw.img"), n.abs("pools/default/link.img")); err != nil {
		t.Fatal(err)
	}
	for _, p := range badPaths {
		n.refused("sysprep "+p, "", "sysprep", p)
	}
	n.refused("no disk", "", "sysprep")
	n.refused("too many disks", "", "sysprep", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img", "/pools/default/raw.img")
	n.refused("option as disk", "", "sysprep", "--run-command")
	n.refused("outside the pools", "", "sysprep", "/etc/secret/outside.img")
	n.refused("a symbolic link", "", "sysprep", "/pools/default/link.img")
	n.refused("a pool directory", "", "sysprep", "/pools/default")
	// vmdk and friends are not prepared: the format is recognised from the
	// content and only qcow2 and raw are accepted.
	out, code := n.run("", "sysprep", "/pools/default/disk.vmdk")
	if code == 0 || !strings.Contains(out, "neither qcow2 nor raw") {
		t.Errorf("vmdk: exit %d: %s", code, out)
	}
	// A qcow2 with a backing file outside the pools.
	if err := os.WriteFile(n.abs("pools/default/evil.qcow2"), qcowHeader("/etc/shadow"), 0o666); err != nil {
		t.Fatal(err)
	}
	n.denied("backing file outside the pools", "", "sysprep", "/pools/default/evil.qcow2")
	// A plain image passes validation; without virt-sysprep installed the run stops there.
	out, code = n.run("", "sysprep", "/pools/default/raw.img")
	if _, err := exec.LookPath("virt-sysprep"); err != nil && (code != 1 || !strings.Contains(out, "not installed")) {
		t.Errorf("valid image: exit %d: %s", code, out)
	}
}

// A receiver that is signalled in the middle of a transfer (the sender's SSH
// session ends) must not leave its partial file in the pool.
func TestHelperPeerRecvCleansUpWhenKilled(t *testing.T) {
	n := poolNode(t)
	argv := []string{"sh", n.sh, "peer-recv"}
	if os.Geteuid() == 0 {
		argv = append([]string{"setpriv", "--reuid=65534", "--regid=65534", "--clear-groups"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{"VIRTICLE_HELPER_ROOT=" + n.root, "PATH=/usr/bin:/bin", "HOME=" + n.root}, n.env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	in.Write([]byte("virticle-peer 1 recv 600 1000000 /pools/default/big.img\n" + strings.Repeat("x", 1000)))
	deadline := time.Now().Add(5 * time.Second)
	for len(globAll(t, n, "pools/default/*part*")) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(globAll(t, n, "pools/default/*part*")) == 0 {
		t.Fatal("the receiver never started writing")
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP) // the whole group, as when a session ends
	cmd.Wait()
	if left := globAll(t, n, "pools/default/*"); len(left) != 0 {
		t.Errorf("a terminated receiver left %v", left)
	}
}
