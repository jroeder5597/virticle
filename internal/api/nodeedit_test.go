package api

import (
	"testing"

	"virticle/internal/cluster"
	"virticle/internal/store"
)

func TestCheckWarningsAddsSteps(t *testing.T) {
	cur := store.Node{HostUUID: "aaa", Privilege: "root"}
	n := store.Node{Privilege: "root"}
	res := &cluster.TestResult{HostUUID: "bbb", Privilege: "none", PrivilegeNote: "not root", Steps: []cluster.TestStep{
		{ID: "libvirt", Status: "ok"}, {ID: "privilege", Status: "ok"},
	}}
	w := checkWarnings(cur, n, res)
	if len(w) != 2 {
		t.Fatalf("warnings: %v", w)
	}
	if len(res.Steps) != 3 || res.Steps[1].ID != "identity" || res.Steps[1].Status != "warning" || res.Steps[2].Status != "warning" {
		t.Errorf("steps: %+v", res.Steps)
	}
}
