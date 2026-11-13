package nodecmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fakeQemuRecording installs a qemu-img that answers info, and writes the
// arguments of rebase and convert to files next to it.
func (n *tnode) fakeQemuRecording() (rebaseArgs, convertArgs string) {
	n.t.Helper()
	rebaseArgs, convertArgs = n.abs("rebase.args"), n.abs("convert.args")
	script := "#!/bin/sh\ncase $1 in\n" +
		"info) echo '{\"format\":\"qcow2\"}'; exit 0;;\n" +
		"rebase) printf '%s\\n' \"$@\" > " + rebaseArgs + "; exit 0;;\n" +
		"convert) printf '%s\\n' \"$@\" > " + convertArgs + "; for a; do dst=$a; done; echo done > \"$dst\"; exit 0;;\n" +
		"esac\nexit 1\n"
	p := n.abs("fake-qemu-img")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		n.t.Fatal(err)
	}
	n.cfg.Tools["qemu-img"] = p
	self := n.abs("fake-self")
	wrapper := "#!/bin/sh\nNODECMD_TEST_BACKUP_ROOT=" + n.root + " NODECMD_TEST_QEMU=" + p + " exec " + os.Args[0] + " \"$@\"\n"
	if err := os.WriteFile(self, []byte(wrapper), 0o755); err != nil {
		n.t.Fatal(err)
	}
	n.cfg.Self = self
	return
}

func TestBackupDetach(t *testing.T) {
	n := newNode(t)
	rebase, _ := n.fakeQemuRecording()
	work := n.workDir(".virticle-bk.AAAAAA")
	n.put("pools/default/.virticle-bk.AAAAAA/vda.qcow2", string(qcowHeader("/var/lib/libvirt/images/vm.qcow2")))
	n.ok("", "backup-detach", work, "vda.qcow2")
	got, _ := os.ReadFile(rebase)
	want := "rebase\n-u\n-f\nqcow2\n-b\n\n--\n" + work + "/vda.qcow2\n"
	if string(got) != want {
		t.Errorf("qemu-img was run as %q, want %q", got, want)
	}
	// what it refuses
	plain := n.workDir("plain")
	n.denied("not a work directory", "", "backup-detach", plain, "vda.qcow2")
	n.denied("outside the pools", "", "backup-detach", n.abs("outside"), "vda.qcow2")
	for _, name := range []string{"../vda.qcow2", "a/b", ".hidden", "-r", "", "x y"} {
		n.denied("name "+name, "", "backup-detach", work, name)
	}
	n.denied("a file that is not there", "", "backup-detach", work, "none.qcow2")
	if err := os.Symlink(n.abs("outside"), work+"/link.qcow2"); err != nil {
		t.Fatal(err)
	}
	n.denied("a link", "", "backup-detach", work, "link.qcow2")
	n.denied("too few arguments", "", "backup-detach", work)
	n.denied("too many arguments", "", "backup-detach", work, "a", "b")
}

func TestConvertChain(t *testing.T) {
	n := newNode(t)
	_, convert := n.fakeQemuRecording()
	work := n.workDir(".virticle-bk.AAAAAA")
	for _, d := range []string{"b1", "b2", "b3", "b4"} {
		n.workDir(d + ".vmbackup")
	}
	full := n.put("pools/default/b1.vmbackup/vda.qcow2", string(qcowHeader("")))
	i1 := n.put("pools/default/b2.vmbackup/vda.qcow2", string(qcowHeader("")))
	i2 := n.put("pools/default/b3.vmbackup/vda.qcow2", string(qcowHeader("")))
	n.ok("", "convert-start", work, full, "qcow2", i1, i2)
	rc, _ := waitRC(t, n, work)
	if rc != "0" {
		t.Fatalf("rc %s", rc)
	}
	raw, _ := os.ReadFile(convert)
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	src := args[len(args)-2]
	if !strings.HasPrefix(src, "json:") || args[len(args)-3] != "--" {
		t.Fatalf("convert was run as %q", args)
	}
	// the last overlay is on top, each one backs the one above it, the full backup is at the bottom
	var top map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(src, "json:")), &top); err != nil {
		t.Fatal(err)
	}
	var order []string
	for node := top; node != nil; {
		order = append(order, node["file"].(map[string]any)["filename"].(string))
		next, _ := node["backing"].(map[string]any)
		node = next
	}
	if strings.Join(order, ",") != i2+","+i1+","+full {
		t.Errorf("chain %v", order)
	}
	if !strings.Contains(strings.Join(args, " "), "-f qcow2 -O qcow2") {
		t.Errorf("formats: %v", args)
	}
	// a chain is qcow2 only, every image must stand alone and stay inside the pools
	work2 := n.workDir(".virticle-bk.BBBBBB")
	n.denied("raw chain", "", "convert-start", work2, full, "raw", i1)
	linked := n.put("pools/default/b4.vmbackup/vda.qcow2", string(qcowHeader(full)))
	n.denied("an overlay with a backing file", "", "convert-start", work2, full, "qcow2", linked)
	n.denied("a base with a backing file", "", "convert-start", work2, linked, "qcow2", i1)
	_ = os.MkdirAll(n.abs("outside"), 0o755)
	n.put("outside/x.qcow2", string(qcowHeader("")))
	n.denied("an overlay outside the pools", "", "convert-start", work2, full, "qcow2", n.abs("outside/x.qcow2"))
	n.denied("an overlay that is missing", "", "convert-start", work2, full, "qcow2", n.pool("none.qcow2"))
	var many []string
	for i := 0; i < convertMaxChain; i++ {
		many = append(many, i1)
	}
	n.denied("too long a chain", "", append([]string{"convert-start", work2, full, "qcow2"}, many...)...)
}

func TestChainSourceQuotesPaths(t *testing.T) {
	got := chainSource([]string{"/p/a.qcow2", "/p/b.qcow2"})
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got, "json:")), &v); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if one := chainSource([]string{"/p/a.qcow2"}); !strings.HasPrefix(one, "json:") || strings.Contains(one, "backing") {
		t.Errorf("single image: %s", one)
	}
}
