package nodecmd

import (
	"strings"
	"testing"
)

func TestWorkLs(t *testing.T) {
	n := newNode(t)
	n.workDir(".virticle-rp.AAAAAA")
	n.workDir(".virticle-bk.BBBBBB") // another kind of work directory: not listed
	n.put("pools/default/.virticle-rp.CCCCCC", "a file, not a directory")
	n.put("pools/default/plain.qcow2", "x")
	got := strings.TrimSpace(n.ok("", "work-ls", n.abs("pools/default")))
	lines := strings.Split(got, "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], ".virticle-rp.AAAAAA|") {
		t.Errorf("work-ls: %q", got)
	}
	n.denied("outside the pools", "", "work-ls", n.abs("outside"))
}
