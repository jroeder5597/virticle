package nodecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNvramNew(t *testing.T) {
	n := newNode(t)
	share := n.abs("share")
	n.cfg.ShareDir = share
	if err := os.MkdirAll(share+"/OVMF", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(share+"/OVMF/VARS.fd", []byte("vars"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("VARS.fd", share+"/OVMF/link.fd"); err != nil {
		t.Fatal(err)
	}
	// A fake qemu-img: the last argument is the output file.
	n.fakeQemu(`{"format":"raw"}`, `cat "$src" > "$dst"`)
	dst := n.abs("nvram/a_VARS.qcow2")
	n.ok("", "nvram-new", share+"/OVMF/VARS.fd", dst)
	if b, err := os.ReadFile(dst); err != nil || string(b) != "vars" {
		t.Fatalf("result %q %v", b, err)
	}
	if out, code := n.run("", "nvram-new", share+"/OVMF/VARS.fd", dst); code == 0 {
		t.Errorf("replaced an existing file: %s", out)
	}
	n.ok("", "nvram-new", share+"/OVMF/link.fd", n.abs("nvram/b_VARS.qcow2")) // a link inside the share is fine
	if err := os.WriteFile(n.abs("outside/x.fd"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.abs("outside"), share+"/OVMF/out"); err != nil {
		t.Fatal(err)
	}
	n.refused("template outside", "", "nvram-new", n.abs("outside/x.fd"), n.abs("nvram/c.qcow2"))
	n.refused("link out of the share", "", "nvram-new", share+"/OVMF/out/x.fd", n.abs("nvram/d.qcow2"))
	n.refused("destination in a pool", "", "nvram-new", share+"/OVMF/VARS.fd", n.pool("e.qcow2"))
	n.refused("wrong suffix", "", "nvram-new", share+"/OVMF/VARS.fd", n.abs("nvram/f.fd"))
	n.refused("nested", "", "nvram-new", share+"/OVMF/VARS.fd", n.abs("nvram/sub/g.qcow2"))
	ents, _ := filepath.Glob(n.abs("nvram/*"))
	for _, e := range ents {
		if strings.Contains(e, ".part") {
			t.Errorf("leftover %s", e)
		}
	}
}
