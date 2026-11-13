package api

import (
	"strings"
	"testing"
)

func TestClaimReplExcludes(t *testing.T) {
	a := &replRun{uuid: "u-a", name: "a", srcID: 1}
	b := &replRun{uuid: "u-b", name: "b", srcID: 1}
	c := &replRun{uuid: "u-c", name: "c", srcID: 2}
	if err := claimRepl(a); err != nil {
		t.Fatal(err)
	}
	defer releaseRepl(a)
	if err := claimRepl(a); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("a second run of the same VM: %v", err)
	}
	if err := claimRepl(b); err == nil || !strings.Contains(err.Error(), "one run at a time per node") {
		t.Errorf("another VM from the same node: %v", err)
	}
	if err := claimRepl(c); err != nil {
		t.Errorf("another node is free: %v", err)
	}
	releaseRepl(c)
	// the last run of a planned failover is not held back by another VM's run on the node
	fin := &replRun{uuid: "u-b", name: "b", srcID: 1, final: true}
	if err := claimRepl(fin); err != nil {
		t.Errorf("final run: %v", err)
	}
	releaseRepl(fin)
	releaseRepl(a)
	if err := claimRepl(b); err != nil {
		t.Errorf("after release: %v", err)
	}
	releaseRepl(b)
}

func TestClaimReplAndBackupExcludeEachOther(t *testing.T) {
	r := &replRun{uuid: "u-x", name: "x", srcID: 9}
	bkRuns.Lock()
	bkRuns.m["u-x"] = &bkRun{}
	bkRuns.Unlock()
	if err := claimRepl(r); err == nil || !strings.Contains(err.Error(), "backup of this VM is in progress") {
		t.Errorf("a run while a backup runs: %v", err)
	}
	bkRuns.Lock()
	delete(bkRuns.m, "u-x")
	bkRuns.Unlock()
	if err := claimRepl(r); err != nil {
		t.Fatal(err)
	}
	defer releaseRepl(r)
	if !replRunning("u-x") {
		t.Error("replRunning")
	}
}
