package api

import "testing"

func TestRollStepCountsLeavesOutSkipped(t *testing.T) {
	nodes := []*rollNode{
		{Action: "update", Steps: []rollStep{{Status: "ok"}, {Status: "skipped"}, {Status: "running"}, {Status: "pending"}, {Status: "skipped"}}},
		{Action: "skip", Steps: []rollStep{{Status: "pending"}}},
		{Action: "update", Steps: []rollStep{{Status: "pending"}, {Status: "skipped"}}},
	}
	total, done := rollStepCounts(nodes)
	if total != 4 || done != 1 {
		t.Fatalf("got %d of %d, want 1 of 4", done, total)
	}
}
