package nodecmd

import (
	"os"
	"strings"
	"testing"
)

// fakeQemuLogging installs a qemu-img that appends its arguments, one call per
// line, to a file and succeeds.
func (n *tnode) fakeQemuLogging() string {
	n.t.Helper()
	log := n.abs("qemu.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase $1 in info) echo '{\"format\":\"qcow2\"}';; esac\nexit 0\n"
	p := n.abs("fake-qemu-img")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		n.t.Fatal(err)
	}
	n.cfg.Tools["qemu-img"] = p
	return log
}

func TestReplVerbs(t *testing.T) {
	n := newNode(t)
	log := n.fakeQemuLogging()
	work := n.workDir(".virticle-rp.AAAAAA")
	base := n.put("pools/default/.virticle-rp.AAAAAA/b1-vda.qcow2", string(qcowHeader("")))
	n.put("pools/default/.virticle-rp.AAAAAA/p2-vda.qcow2", string(qcowHeader("b1-vda.qcow2")))
	n.put("pools/default/.virticle-rp.AAAAAA/in-vda.qcow2", string(qcowHeader("")))
	_ = base

	n.ok("", "repl-overlay", work, "p1-vda.qcow2", "b1-vda.qcow2")
	n.ok("", "repl-rebase", work, "in-vda.qcow2", "p2-vda.qcow2")
	n.ok("", "repl-commit", work, "p2-vda.qcow2")
	n.ok("", "repl-commit", work, "p2-vda.qcow2", "b1-vda.qcow2")
	got, _ := os.ReadFile(log)
	want := []string{
		"create -f qcow2 -b b1-vda.qcow2 -F qcow2 -- " + work + "/p1-vda.qcow2",
		"rebase -u -f qcow2 -F qcow2 -b p2-vda.qcow2 -- " + work + "/in-vda.qcow2",
		"commit -f qcow2 -t none -- " + work + "/p2-vda.qcow2",
		"commit -f qcow2 -t none -b b1-vda.qcow2 -- " + work + "/p2-vda.qcow2",
	}
	if g := strings.TrimSpace(string(got)); g != strings.Join(want, "\n") {
		t.Errorf("qemu-img was run as\n%s\nwant\n%s", g, strings.Join(want, "\n"))
	}

	// what is refused: other directories, odd names, missing files, links, an overlay that exists
	plain := n.workDir("plain")
	n.denied("not a work directory", "", "repl-rebase", plain, "in-vda.qcow2", "p2-vda.qcow2")
	n.denied("outside the pools", "", "repl-commit", n.abs("outside"), "x.qcow2")
	for _, bad := range []string{"../x", "a/b", ".hidden", "-r", "x y"} {
		n.denied("name "+bad, "", "repl-rebase", work, "in-vda.qcow2", bad)
		n.denied("name "+bad, "", "repl-commit", work, bad)
		n.denied("name "+bad, "", "repl-overlay", work, bad, "b1-vda.qcow2")
	}
	n.denied("missing image", "", "repl-commit", work, "none.qcow2")
	n.denied("missing backing", "", "repl-rebase", work, "in-vda.qcow2", "none.qcow2")
	n.put("pools/default/.virticle-rp.AAAAAA/p1-vda.qcow2", string(qcowHeader("b1-vda.qcow2")))
	n.denied("overlay exists", "", "repl-overlay", work, "p1-vda.qcow2", "b1-vda.qcow2")
	if err := os.Symlink(n.abs("outside"), work+"/link.qcow2"); err != nil {
		t.Fatal(err)
	}
	n.denied("a link", "", "repl-commit", work, "link.qcow2")
	// a chain that leaves the pools is not committed
	n.put("pools/default/.virticle-rp.AAAAAA/p3-vda.qcow2", string(qcowHeader("/etc/passwd")))
	n.denied("backing outside", "", "repl-commit", work, "p3-vda.qcow2")
	n.denied("too few arguments", "", "repl-commit", work)
	n.denied("too many arguments", "", "repl-commit", work, "a", "b", "c")
}

func TestMtime(t *testing.T) {
	n := newNode(t)
	f := n.put("pools/default/vm.qcow2", "data")
	out := n.ok("", "mtime", f, n.pool("none.qcow2"))
	l := strings.Split(out, "\n")
	if len(l) != 2 || !strings.HasSuffix(l[0], "|4") || l[1] != "MISSING" {
		t.Errorf("mtime: %q", out)
	}
	n.denied("outside", "", "mtime", n.abs("outside/x"))
	n.denied("relative", "", "mtime", "vm.qcow2")
}

func TestReplLs(t *testing.T) {
	n := newNode(t)
	work := n.workDir(".virticle-rp.AAAAAA")
	n.put("pools/default/.virticle-rp.AAAAAA/b1-vda.qcow2", "12345")
	n.put("pools/default/.virticle-rp.AAAAAA/p1.json", "{}")
	n.put("pools/default/.virticle-rp.AAAAAA/.hidden", "x")
	if err := os.Mkdir(work+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	got := n.ok("", "repl-ls", work)
	if got != "b1-vda.qcow2|5\np1.json|2" {
		t.Errorf("repl-ls: %q", got)
	}
	n.denied("not a work directory", "", "repl-ls", n.workDir("plain"))
}
