package api

import (
	"strings"
	"testing"
)

func okAll(evacVM, evacTarget) []string { return nil }

func TestPlanBalancesByRemainingMemory(t *testing.T) {
	vms := []evacVM{
		{UUID: "a", Name: "a", State: "running", MemoryKiB: 300},
		{UUID: "b", Name: "b", State: "running", MemoryKiB: 300},
		{UUID: "c", Name: "c", State: "running", MemoryKiB: 100},
	}
	targets := []evacTarget{
		{ID: 1, Name: "t1", FreeKiB: 700, Online: true},
		{ID: 2, Name: "t2", FreeKiB: 500, Online: true},
	}
	plan, blocked := planEvacuation(vms, targets, okAll)
	if len(blocked) != 0 || len(plan) != 3 {
		t.Fatalf("plan=%+v blocked=%+v", plan, blocked)
	}
	// a -> t1 (700 left 400), b -> t1 again? t1 has 400 vs t2 500 -> t2.
	got := map[string]int64{}
	for _, p := range plan {
		got[p.VM.UUID] = p.Target
	}
	if got["a"] != 1 || got["b"] != 2 {
		t.Errorf("assignment %v: want a on t1 and b on t2", got)
	}
	// c: t1 has 400, t2 has 200 -> t1.
	if got["c"] != 1 {
		t.Errorf("c went to %d, want 1", got["c"])
	}
}

func TestPlanDoesNotOvercommitFreeMemory(t *testing.T) {
	vms := []evacVM{
		{UUID: "a", Name: "a", State: "running", MemoryKiB: 600},
		{UUID: "b", Name: "b", State: "running", MemoryKiB: 600},
	}
	targets := []evacTarget{{ID: 1, Name: "t1", FreeKiB: 1000, Online: true}}
	plan, blocked := planEvacuation(vms, targets, okAll)
	if len(plan) != 1 || len(blocked) != 1 {
		t.Fatalf("plan=%+v blocked=%+v", plan, blocked)
	}
	if !strings.Contains(strings.Join(blocked[0].Reasons, " "), "not enough free memory") {
		t.Errorf("reasons: %v", blocked[0].Reasons)
	}
}

func TestPlanSkipsMaintenanceAndOffline(t *testing.T) {
	vms := []evacVM{{UUID: "a", Name: "a", State: "running", MemoryKiB: 10}}
	targets := []evacTarget{
		{ID: 1, Name: "m", FreeKiB: 9999, Online: true, Maintenance: true},
		{ID: 2, Name: "off", FreeKiB: 9999, Online: false},
	}
	plan, blocked := planEvacuation(vms, targets, okAll)
	if len(plan) != 0 || len(blocked) != 1 {
		t.Fatalf("plan=%+v blocked=%+v", plan, blocked)
	}
	targets = append(targets, evacTarget{ID: 3, Name: "ok", FreeKiB: 20, Online: true})
	plan, _ = planEvacuation(vms, targets, okAll)
	if len(plan) != 1 || plan[0].Target != 3 {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanUsesPreflightAndFallsBack(t *testing.T) {
	vms := []evacVM{
		{UUID: "net", Name: "net", State: "running", MemoryKiB: 10},
		{UUID: "plain", Name: "plain", State: "running", MemoryKiB: 10},
	}
	targets := []evacTarget{
		{ID: 1, Name: "t1", FreeKiB: 100, Online: true},
		{ID: 2, Name: "t2", FreeKiB: 50, Online: true},
	}
	check := func(vm evacVM, t evacTarget) []string {
		if vm.UUID == "net" {
			return []string{"Network wk-net: does not exist"}
		}
		if t.ID == 1 {
			return []string{"Name on target: exists"}
		}
		return nil
	}
	plan, blocked := planEvacuation(vms, targets, check)
	if len(plan) != 1 || plan[0].VM.UUID != "plain" || plan[0].Target != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	if len(blocked) != 1 || blocked[0].VM.UUID != "net" || len(blocked[0].Reasons) != 2 {
		t.Fatalf("blocked=%+v", blocked)
	}
	if !strings.HasPrefix(blocked[0].Reasons[0], "t1: ") {
		t.Errorf("reasons should name the target: %v", blocked[0].Reasons)
	}
}

func TestPlanShutOffVMsNeedNoMemory(t *testing.T) {
	vms := []evacVM{{UUID: "off", Name: "off", State: "shutoff", MemoryKiB: 1 << 30}}
	targets := []evacTarget{{ID: 1, Name: "t1", FreeKiB: 1, Online: true}}
	plan, blocked := planEvacuation(vms, targets, okAll)
	if len(plan) != 1 || len(blocked) != 0 {
		t.Fatalf("plan=%+v blocked=%+v", plan, blocked)
	}
}

func TestPlanCheckNotCalledWithoutMemory(t *testing.T) {
	vms := []evacVM{{UUID: "a", Name: "a", State: "running", MemoryKiB: 500}}
	targets := []evacTarget{{ID: 1, Name: "t1", FreeKiB: 100, Online: true}}
	called := false
	planEvacuation(vms, targets, func(evacVM, evacTarget) []string { called = true; return nil })
	if called {
		t.Error("the expensive pre-flight ran for a target without enough memory")
	}
}

func TestSelectEvacVMsFilterIsExact(t *testing.T) {
	all := []evacVM{
		{UUID: "u1", Name: "mine-1", State: "running"},
		{UUID: "u2", Name: "mine-2", State: "shutoff"},
		{UUID: "u3", Name: "someone-else", State: "running"},
		{UUID: "u4", Name: "also-else", State: "running"},
	}
	// Regression: matching the first listed VM must not widen the selection.
	got, err := selectEvacVMs(all, []string{"U1"}, false)
	if err != nil || len(got) != 1 || got[0].UUID != "u1" {
		t.Fatalf("got %+v %v", got, err)
	}
	got, err = selectEvacVMs(all, []string{"u1", "u2"}, false)
	if err != nil || len(got) != 2 || got[1].UUID != "u2" {
		t.Fatalf("shut-off VM named explicitly must be taken: %+v %v", got, err)
	}
	if _, err := selectEvacVMs(all, []string{"u1", "nope"}, false); err == nil {
		t.Error("unknown UUID must be an error")
	}
	got, _ = selectEvacVMs(all, nil, false)
	if len(got) != 3 {
		t.Errorf("no filter, without shut-off: %+v", got)
	}
	got, _ = selectEvacVMs(all, nil, true)
	if len(got) != 4 {
		t.Errorf("no filter, with shut-off: %+v", got)
	}
}
