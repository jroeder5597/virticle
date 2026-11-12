package nodecmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A tnode is a fake node: a temp directory with two pools, an NVRAM directory
// and a service account home, and a Config that points at it. See doc.go.
type tnode struct {
	t    *testing.T
	root string
	cfg  *Config
}

// layout creates the directory tree of a fake node under root and returns its
// Config.
func layout(root string) *Config {
	for _, d := range []string{"pools/default", "pools/other", "nvram", "home/svc/.ssh", "outside", "etc/libvirt", "proc", "sys"} {
		p := filepath.Join(root, d)
		_ = os.MkdirAll(p, 0o755)
	}
	return &Config{
		Pools:        func(*Ctx) []string { return []string{filepath.Join(root, "pools/default")} },
		NVRAMDir:     filepath.Join(root, "nvram"),
		Prefix:       root,
		Unprivileged: true,
		Account:      &Account{Name: "svc", Home: filepath.Join(root, "home/svc"), UID: 1000, GID: 1000},
		SysDir:       filepath.Join(root, "sys"),
		ProcDir:      filepath.Join(root, "proc"),
		Tools:        map[string]string{},
	}
}

func newNode(t *testing.T) *tnode {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tnode{t: t, root: root, cfg: layout(root)}
}

func (n *tnode) abs(rel string) string { return filepath.Join(n.root, rel) }

// pool is the path of the default pool.
func (n *tnode) pool(rel string) string { return n.abs("pools/default/" + rel) }

func (n *tnode) runSplit(stdin string, args ...string) (stdout, stderr string, code int) {
	n.t.Helper()
	var o, e bytes.Buffer
	code = Run(n.cfg, args, strings.NewReader(stdin), &o, &e)
	return o.String(), e.String(), code
}

// run returns stdout and stderr together, trimmed, and the exit status.
func (n *tnode) run(stdin string, args ...string) (string, int) {
	n.t.Helper()
	o, e, code := n.runSplit(stdin, args...)
	return strings.TrimSpace(o + e), code
}

func (n *tnode) ok(stdin string, args ...string) string {
	n.t.Helper()
	out, code := n.run(stdin, args...)
	if code != 0 {
		n.t.Fatalf("%v: exit %d: %s", args, code, out)
	}
	return out
}

// refused: the arguments were refused (exit status 2).
func (n *tnode) refused(why, stdin string, args ...string) {
	n.t.Helper()
	out, code := n.run(stdin, args...)
	if code != 2 {
		n.t.Errorf("%s: %v: exit %d, want a refusal (2): %s", why, args, code, out)
	}
}

// denied: the verb failed and changed nothing: exit 2 (arguments refused) or 1 (the operation refused).
func (n *tnode) denied(why, stdin string, args ...string) {
	n.t.Helper()
	out, code := n.run(stdin, args...)
	if code != 1 && code != 2 {
		n.t.Errorf("%s: %v: exit %d, want a refusal: %s", why, args, code, out)
	}
}

func (n *tnode) put(rel, content string) string {
	n.t.Helper()
	p := n.abs(rel)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		n.t.Fatal(err)
	}
	return p
}

func (n *tnode) exists(rel string) bool {
	_, err := os.Lstat(n.abs(rel))
	return err == nil
}

func (n *tnode) glob(pat string) []string {
	m, err := filepath.Glob(n.abs(pat))
	if err != nil {
		n.t.Fatal(err)
	}
	return m
}

// TestMain lets the test binary stand in for other programs:
//   - NODECMD_TEST_RECV_ROOT: act as the peer-recv end of a fake node (used by
//     the fake ssh of the peer-send test);
//   - NODECMD_TEST_MAIN: run Main with the real signal handling, to test it.
func TestMain(m *testing.M) {
	if root := os.Getenv("NODECMD_TEST_RECV_ROOT"); root != "" {
		os.Exit(Run(layout(root), []string{"peer-recv"}, os.Stdin, os.Stdout, os.Stderr))
	}
	if os.Getenv("NODECMD_TEST_MAIN") != "" {
		os.Exit(Main(append([]string{"virticle-node"}, os.Args[1:]...), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func init() {
	// A verb that exists in the test binary only: makes a file that a signal
	// must remove, then waits for stdin.
	Register("zz-test-block", func(c *Ctx) error {
		f := c.Args[0]
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			return err
		}
		c.Cleanup(func() { os.Remove(f) })
		if err := os.WriteFile(f+".ready", nil, 0o600); err != nil {
			return err
		}
		buf := make([]byte, 1)
		c.Stdin.Read(buf)
		return nil
	})
}

func TestSignalRemovesRegisteredFiles(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "partfile")
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		cmd := exec.Command(os.Args[0], "zz-test-block", victim)
		cmd.Env = append(os.Environ(), "NODECMD_TEST_MAIN=1")
		w, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(victim + ".ready"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				t.Fatal("the verb did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = cmd.Process.Signal(sig)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
				t.Errorf("%v: exit %v, want status 1", sig, err)
			}
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("%v: the process did not end", sig)
		}
		w.Close()
		if _, err := os.Stat(victim); err == nil {
			t.Errorf("%v: the registered file was left behind", sig)
		}
		os.Remove(victim + ".ready")
	}
}
