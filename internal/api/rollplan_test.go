package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"virticle/internal/store"
)

const gibKiB = 1 << 20

func vm(name string, memGiB uint64) evacVM {
	return evacVM{UUID: "uuid-" + name, Name: name, State: "running", MemoryKiB: memGiB * gibKiB}
}

// node makes a healthy online dnf node with 16 GiB of memory of which free GiB are free.
func node(id int64, name string, free uint64, pending ...string) rollNodeInfo {
	return rollNodeInfo{ID: id, Name: name, Online: true, Family: "dnf", FreeKiB: free * gibKiB, TotalKiB: 16 * gibKiB, Pending: pending}
}

func findNode(t *testing.T, p rollPlan, name string) rollNodePlan {
	t.Helper()
	for _, n := range p.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node %s in the plan", name)
	return rollNodePlan{}
}

func names(p rollPlan) string {
	var out []string
	for _, n := range p.Nodes {
		out = append(out, n.Name)
	}
	return strings.Join(out, ",")
}

func TestRollOrderIsByIDWithTheControllerMachineLast(t *testing.T) {
	a, b, c := node(1, "a", 8, "x"), node(2, "b", 8, "x"), node(3, "c", 8, "x")
	a.Local = true
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b, c}})
	if got := names(p); got != "b,c,a" {
		t.Fatalf("order %s", got)
	}
	c.Local, a.Local = true, false
	if got := names(planRolling(rollPlanIn{Nodes: []rollNodeInfo{c, b, a}})); got != "a,b,c" {
		t.Fatalf("order %s", got)
	}
}

func TestRollSelectionLimitsTheNodes(t *testing.T) {
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{node(1, "a", 8, "x"), node(2, "b", 8, "x")}, Select: map[int64]bool{2: true}})
	if names(p) != "b" || len(p.Refusals) != 0 {
		t.Fatalf("%s %v", names(p), p.Refusals)
	}
}

func TestRollRefusals(t *testing.T) {
	ok := func() []rollNodeInfo { return []rollNodeInfo{node(1, "a", 8, "x"), node(2, "b", 8, "x")} }
	cases := []struct {
		name string
		mod  func(n []rollNodeInfo) rollPlanIn
		want string
	}{
		{"offline node", func(n []rollNodeInfo) rollPlanIn { n[1].Online = false; return rollPlanIn{Nodes: n} }, "b is offline. No node is updated while another one is down"},
		{"offline node that is not selected still refuses", func(n []rollNodeInfo) rollPlanIn {
			n[1].Online = false
			return rollPlanIn{Nodes: n, Select: map[int64]bool{1: true}}
		}, "b is offline"},
		{"selected node in maintenance", func(n []rollNodeInfo) rollPlanIn { n[0].Maintenance = true; return rollPlanIn{Nodes: n} }, "a is in maintenance. End maintenance"},
		{"no node command", func(n []rollNodeInfo) rollPlanIn { n[0].NoNodeCmd = true; return rollPlanIn{Nodes: n} }, "virticle node prepare"},
		{"updates unreadable", func(n []rollNodeInfo) rollPlanIn {
			n[1].UpdateErr = "Failed to download metadata for repo 'x'"
			return rollPlanIn{Nodes: n}
		}, "b: the updates cannot be read: Failed to download"},
		{"unsupported system", func(n []rollNodeInfo) rollPlanIn { n[0].Family = "unsupported"; return rollPlanIn{Nodes: n} }, "not based on dnf or apt"},
		{"busy", func(n []rollNodeInfo) rollPlanIn { return rollPlanIn{Nodes: n, Busy: "A node evacuation is running."} }, "A node evacuation is running."},
		{"nothing selected", func(n []rollNodeInfo) rollPlanIn { return rollPlanIn{Nodes: n, Select: map[int64]bool{9: true}} }, "no node to update"},
	}
	for _, c := range cases {
		p := planRolling(c.mod(ok()))
		if len(p.Refusals) == 0 || !strings.Contains(strings.Join(p.Refusals, "\n"), c.want) {
			t.Errorf("%s: refusals %v, want %q", c.name, p.Refusals, c.want)
		}
	}
	if p := planRolling(rollPlanIn{Nodes: ok()}); len(p.Refusals) != 0 {
		t.Errorf("a healthy datacenter is refused: %v", p.Refusals)
	}
}

func TestRollMaintenanceOfANodeNotSelectedDoesNotRefuse(t *testing.T) {
	a, b := node(1, "a", 8, "x"), node(2, "b", 8, "x")
	b.Maintenance = true
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}, Select: map[int64]bool{1: true}})
	if len(p.Refusals) != 0 {
		t.Fatalf("%v", p.Refusals)
	}
	// and it is not a target either
	a.VMs = []evacVM{vm("v1", 1)}
	p = planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}, Select: map[int64]bool{1: true}})
	if len(findNode(t, p, "a").Stuck) != 1 {
		t.Fatalf("a node in maintenance took a VM: %+v", p.Nodes[0])
	}
}

func TestRollNodeWithoutUpdatesIsLeftAlone(t *testing.T) {
	a := node(1, "a", 8) // nothing pending
	a.VMs = []evacVM{vm("v1", 1)}
	b := node(2, "b", 8, "x")
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}})
	if n := findNode(t, p, "a"); n.Action != "current" || len(n.Moves) != 0 {
		t.Fatalf("%+v", n)
	}
	// A node that only needs its restart is still updated (its restart is the work).
	a.RebootFlag = true
	p = planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}})
	if n := findNode(t, p, "a"); n.Action != "update" || !n.Reboot || len(n.Moves) != 1 {
		t.Fatalf("%+v", n)
	}
}

func TestRollRebootExpectedPerPackage(t *testing.T) {
	for _, c := range []struct {
		pending []string
		reboot  bool
	}{
		{[]string{"nano.x86_64", "tmux.x86_64"}, false},
		{[]string{"kernel-core.x86_64"}, true},
		{[]string{"kernel-tools.x86_64"}, false},
		{[]string{"glibc.x86_64"}, true},
		{[]string{"libvirt-daemon.x86_64"}, true},
		{[]string{"qemu-kvm-core.x86_64"}, true},
		{[]string{"qemu-guest-agent.x86_64"}, false},
		{[]string{"linux-image-6.12.43-amd64"}, true},
		{[]string{"libc6"}, true},
		{[]string{"libssl3"}, false},
		{[]string{"systemd-libs.x86_64"}, false},
		{[]string{"systemd.x86_64"}, true},
	} {
		n := node(1, "a", 8, c.pending...)
		got, why := rollRebootExpected(n)
		if got != c.reboot {
			t.Errorf("%v: reboot %v (%q), want %v", c.pending, got, why, c.reboot)
		}
	}
}

func TestRollPlacementUsesMemoryAndReserve(t *testing.T) {
	a := node(1, "a", 2, "x")
	a.VMs = []evacVM{vm("big", 6), vm("mid", 4), vm("small", 1)}
	b := node(2, "b", 9, "x") // 16 GiB total: reserve is 0.8 GiB, so 8.2 GiB usable
	c := node(3, "c", 5, "x") // 4.2 GiB usable
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b, c}, Select: map[int64]bool{1: true}})
	n := findNode(t, p, "a")
	where := map[string]string{}
	for _, m := range n.Moves {
		where[m.VM.Name] = m.ToName
	}
	if where["big"] != "b" || where["mid"] != "c" {
		t.Fatalf("moves %v", where)
	}
	// small needs 1 GiB: b has 2.2 left, c has 0.2 left
	if where["small"] != "b" {
		t.Fatalf("small went to %q", where["small"])
	}
	if len(n.Stuck) != 0 {
		t.Fatalf("stuck %+v", n.Stuck)
	}
}

func TestRollNotEnoughMemoryLeavesTheVMStuckWithTheReason(t *testing.T) {
	a := node(1, "a", 2, "x")
	a.VMs = []evacVM{vm("huge", 12)}
	b := node(2, "b", 6, "x")
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}, Select: map[int64]bool{1: true}})
	n := findNode(t, p, "a")
	if len(n.Stuck) != 1 || !strings.Contains(strings.Join(n.Stuck[0].Reasons, " "), "not enough free memory") {
		t.Fatalf("%+v", n)
	}
	if !p.NeedsChoice {
		t.Fatal("a VM that cannot move needs a choice")
	}
}

func TestRollUpdatedNodesAreTriedFirst(t *testing.T) {
	// c has the most free memory, but b was updated earlier in the run, so a VM of the node after it goes to b.
	a, b, c := node(1, "a", 8, "x"), node(2, "b", 8, "x"), node(3, "c", 12, "x")
	c.VMs = []evacVM{vm("v", 1)}
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b, c}})
	if got := findNode(t, p, "c").Moves; len(got) != 1 || got[0].ToName == "c" {
		t.Fatalf("%+v", got)
	}
	// Without being updated, the one with most memory is chosen (a node later in the order).
	p = planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b, c}, Select: map[int64]bool{3: true}})
	if got := findNode(t, p, "c").Moves; len(got) != 1 || got[0].ToName == "c" {
		t.Fatalf("%+v", got)
	}
	// In a live plan, the nodes done so far are given.
	p = planRolling(rollPlanIn{Nodes: []rollNodeInfo{node(1, "a", 14, "x"), node(2, "b", 8, "x"), func() rollNodeInfo { n := node(3, "c", 8, "x"); n.VMs = []evacVM{vm("v", 1)}; return n }()},
		Select: map[int64]bool{3: true}, Done: map[int64]bool{2: true}})
	if got := findNode(t, p, "c").Moves; len(got) != 1 || got[0].ToName != "b" {
		t.Fatalf("an updated node must come first: %+v", got)
	}
}

func TestRollCheckBlocksATarget(t *testing.T) {
	a, b, c := node(1, "a", 8, "x"), node(2, "b", 12, "x"), node(3, "c", 8, "x")
	a.VMs = []evacVM{vm("v", 1)}
	check := func(v evacVM, from int64, to evacTarget) []string {
		if to.Name == "b" {
			return []string{"Network lan: does not exist on b"}
		}
		return nil
	}
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b, c}, Select: map[int64]bool{1: true}, Check: check})
	if got := findNode(t, p, "a").Moves; len(got) != 1 || got[0].ToName != "c" {
		t.Fatalf("%+v", got)
	}
	// nowhere to go: stuck, with the reasons of every target
	check = func(v evacVM, from int64, to evacTarget) []string {
		return []string{"Passthrough devices: 1 host device"}
	}
	p = planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b, c}, Select: map[int64]bool{1: true}, Check: check})
	n := findNode(t, p, "a")
	if len(n.Stuck) != 1 || len(n.Stuck[0].Reasons) != 2 {
		t.Fatalf("%+v", n.Stuck)
	}
}

func TestRollChoiceSkipAndShutdown(t *testing.T) {
	a, b := node(1, "a", 8, "x"), node(2, "b", 8, "x")
	a.VMs = []evacVM{vm("pci", 1), vm("ok", 1)}
	check := func(v evacVM, from int64, to evacTarget) []string {
		if v.Name == "pci" {
			return []string{"Passthrough devices: 1 host device"}
		}
		return nil
	}
	in := rollPlanIn{Nodes: []rollNodeInfo{a, b}, Check: check}
	p := planRolling(in)
	if !p.NeedsChoice || findNode(t, p, "a").Action != "update" {
		t.Fatalf("without a choice the node is planned as updated and the plan asks: %+v", p)
	}
	in.Choice = rollChoiceSkip
	p = planRolling(in)
	if n := findNode(t, p, "a"); n.Action != "skip" || !strings.Contains(n.SkipWhy, "pci cannot move") {
		t.Fatalf("%+v", n)
	}
	if findNode(t, p, "b").Action != "update" {
		t.Fatal("the other node is still updated")
	}
	in.Choice = rollChoiceShutdown
	p = planRolling(in)
	n := findNode(t, p, "a")
	if n.Action != "update" || len(n.Stuck) != 1 || len(n.Moves) != 1 {
		t.Fatalf("%+v", n)
	}
	steps := rollStepsFor(n, rollChoiceShutdown)
	for _, s := range steps {
		if (s.ID == stepStop || s.ID == stepStart) && s.Status != stepStatusPend {
			t.Errorf("step %s must run with the shutdown choice: %+v", s.ID, s)
		}
	}
	for _, s := range rollStepsFor(n, rollChoiceSkip) {
		if (s.ID == stepStop || s.ID == stepStart) && s.Status != "skipped" {
			t.Errorf("step %s must not run without it", s.ID)
		}
	}
}

func TestRollSingleNodeHasNowhereToMoveVMs(t *testing.T) {
	a := node(1, "a", 8, "x")
	a.VMs = []evacVM{vm("v", 1)}
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a}, Choice: rollChoiceShutdown})
	n := findNode(t, p, "a")
	if len(n.Stuck) != 1 || !strings.Contains(n.Stuck[0].Reasons[0], "no target node") || n.Action != "update" {
		t.Fatalf("%+v", n)
	}
}

func TestRollControllerMachineNote(t *testing.T) {
	a, b := node(1, "a", 8, "glibc.x86_64"), node(2, "b", 8, "x")
	a.Local = true
	p := planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}})
	n := findNode(t, p, "a")
	if !strings.Contains(n.Note, "web interface is not available") || !strings.Contains(n.Note, "carries on by itself") || !n.Local {
		t.Fatalf("note %q", n.Note)
	}
	if findNode(t, p, "b").Note != "" {
		t.Fatal("only the controller's machine gets the note")
	}
	a.Pending = []string{"nano.x86_64"}
	p = planRolling(rollPlanIn{Nodes: []rollNodeInfo{a, b}})
	if n := findNode(t, p, "a"); !strings.Contains(n.Note, "does not need a restart") {
		t.Fatalf("note %q", n.Note)
	}
}

func TestRollNextStepAndReopen(t *testing.T) {
	np := rollNodePlan{Node: 1, Name: "a", Moves: []rollMove{{VM: vm("v", 1), To: 2, ToName: "b"}}}
	steps := rollStepsFor(np, "")
	if i := rollNextStep(steps); i != 0 || steps[i].ID != stepMaint {
		t.Fatalf("%d", i)
	}
	for i := range steps {
		if steps[i].ID == stepUpdate {
			break
		}
		if steps[i].Status == stepStatusPend {
			steps[i].Status = "ok"
		}
	}
	if i := rollNextStep(steps); steps[i].ID != stepUpdate {
		t.Fatalf("next %s", steps[i].ID)
	}
	// a step that was running when the controller died runs again; so does a failed one
	steps[3].Status = "running"
	if rollNextStep(steps) != 3 {
		t.Fatal("a running step must be re-run")
	}
	steps[3].Status = "failed"
	if rollNextStep(steps) != 3 {
		t.Fatal("a failed step is next")
	}
	for i := range steps {
		if steps[i].Status != "skipped" {
			steps[i].Status = "ok"
		}
	}
	if rollNextStep(steps) != -1 {
		t.Fatal("all done")
	}
}

func stoppedRun() *rollRun {
	np := rollNodePlan{Node: 1, Name: "a", Moves: []rollMove{{VM: vm("v1", 1), To: 2, ToName: "b"}}}
	r := &rollRun{Status: "failed", Cur: 0, Nodes: []*rollNode{
		{Node: 1, Name: "a", Action: "update", Status: "failed", Steps: rollStepsFor(np, ""), MaintSet: true,
			Moved: []rollMoved{{UUID: "uuid-v1", Name: "v1", To: 2, ToName: "b", Status: "moved"}}},
		{Node: 2, Name: "b", Action: "update", Status: "pending", Steps: rollStepsFor(rollNodePlan{Node: 2, Name: "b"}, "")},
	}}
	set := func(id, st string) {
		for i := range r.Nodes[0].Steps {
			if r.Nodes[0].Steps[i].ID == id {
				r.Nodes[0].Steps[i].Status = st
			}
		}
	}
	set(stepMaint, "ok")
	set(stepEvacuate, "ok")
	set(stepUpdate, "failed")
	return r
}

func TestRollConditionFailedDuringUpdate(t *testing.T) {
	c := rollCondition(stoppedRun())
	for _, want := range []string{"The updates are not installed on a.", "a is in maintenance.", "v1 (on b)", "Not touched: b."} {
		if !strings.Contains(c, want) {
			t.Errorf("condition lacks %q: %s", want, c)
		}
	}
}

func TestRollConditionAfterRestartNotConfirmed(t *testing.T) {
	r := stoppedRun()
	n := r.Nodes[0]
	for i := range n.Steps {
		switch n.Steps[i].ID {
		case stepUpdate, stepReboot:
			n.Steps[i].Status = "ok"
		case stepWait:
			n.Steps[i].Status = "failed"
		}
	}
	n.RebootSent = true
	c := rollCondition(r)
	if !strings.Contains(c, "The updates are installed on a.") || !strings.Contains(c, "was restarted and has not been confirmed healthy") {
		t.Fatalf("%s", c)
	}
}

func TestRollConditionMentionsShutDownVMsAndFinishedNodes(t *testing.T) {
	r := stoppedRun()
	r.Cur = 1
	r.Nodes[0].Status, r.Nodes[0].MaintSet, r.Nodes[0].Moved = "done", false, nil
	r.Nodes[1].Status = "failed"
	r.Nodes[1].Steps[0].Status = "ok"
	r.Nodes[1].Down = []rollDown{{UUID: "u", Name: "pci"}}
	r.Nodes[1].MaintSet = true
	c := rollCondition(r)
	for _, want := range []string{"Updated and checked: a.", "shut down for the update and are not started yet: pci", "b is in maintenance"} {
		if !strings.Contains(c, want) {
			t.Errorf("lacks %q: %s", want, c)
		}
	}
}

func TestRollConditionUntouched(t *testing.T) {
	r := stoppedRun()
	for i := range r.Nodes[0].Steps {
		r.Nodes[0].Steps[i].Status = stepStatusPend
	}
	r.Nodes[0].MaintSet = false
	r.Nodes[0].Moved = nil
	if c := rollCondition(r); !strings.HasPrefix(c, "a was not touched.") {
		t.Fatalf("%s", c)
	}
}

func TestRollReopenContinueResetsOnlyTheFailedStep(t *testing.T) {
	r := stoppedRun()
	r.Error, r.Condition = "x", "y"
	rollReopen(r, false)
	if r.Status != "running" || r.Error != "" || r.Condition != "" || r.Restoring {
		t.Fatalf("%+v", r)
	}
	n := r.Nodes[0]
	if i := rollNextStep(n.Steps); n.Steps[i].ID != stepUpdate || n.Steps[i].Status != stepStatusPend {
		t.Fatalf("next step %s %s", n.Steps[i].ID, n.Steps[i].Status)
	}
	for _, s := range n.Steps {
		if (s.ID == stepMaint || s.ID == stepEvacuate) && s.Status != "ok" {
			t.Errorf("finished step %s was reset", s.ID)
		}
	}
	if n.Status != "running" {
		t.Fatal(n.Status)
	}
}

func TestRollReopenRestoreKeepsOnlyThePuttingBackSteps(t *testing.T) {
	r := stoppedRun()
	rollReopen(r, true)
	if !r.Restoring || r.Status != "running" {
		t.Fatalf("%+v", r)
	}
	var left []string
	for _, s := range r.Nodes[0].Steps {
		if s.Status == stepStatusPend {
			left = append(left, s.ID)
		}
	}
	if strings.Join(left, ",") != stepMaintOff+","+stepReturn {
		// start-vms is skipped here: nobody was shut down
		t.Fatalf("open steps %v", left)
	}
}

func TestRollSummary(t *testing.T) {
	r := &rollRun{Nodes: []*rollNode{{Name: "a", Status: "done"}, {Name: "b", Status: "skipped"}, {Name: "c", Status: "current"}}}
	s := rollSummary(r)
	for _, want := range []string{"Updated: a.", "Already up to date: c.", "Left out because virtual machines cannot move: b."} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q: %s", want, s)
		}
	}
}

func TestRollingAlertRaisedByAFailedRunAndClearedByContinuing(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Now()
	al := Alert{ID: "rolling-update", Kind: "rolling-update", Severity: "critical", Title: "The rolling update stopped after a failure", Link: "/datacenter/nodes"}
	r, _, all := tr.step(alertInput{Rolling: &al}, now)
	if len(r) != 1 || all[0].Kind != "rolling-update" || all[0].Severity != "critical" {
		t.Fatalf("%v", all)
	}
	// While the run is being continued there is no stopped run, so the alert is gone.
	_, c, all := tr.step(alertInput{}, now.Add(time.Minute))
	if len(c) != 1 || len(all) != 0 {
		t.Fatalf("%v %v", c, all)
	}
}

func TestRollingAlertFollowsTheNewestRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st}
	if s.rollingAlert() != nil {
		t.Fatal("no run, no alert")
	}
	save := func(r rollRun) {
		b, _ := json.Marshal(r)
		if _, err := st.CreateRoll(r.Started, r.Status, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	save(rollRun{Started: 1, Status: "ok"})
	if s.rollingAlert() != nil {
		t.Fatal("a finished run raises nothing")
	}
	save(rollRun{Started: 2, Status: "failed", Error: "b: install the updates: boom", Condition: "b is in maintenance."})
	a := s.rollingAlert()
	if a == nil || a.Severity != "critical" || !strings.Contains(a.Message, "boom") || !strings.Contains(a.Message, "b is in maintenance") {
		t.Fatalf("%+v", a)
	}
	save(rollRun{Started: 3, Status: "cancelled", Condition: "a is in maintenance."})
	if a := s.rollingAlert(); a == nil || a.Severity != "warning" {
		t.Fatalf("%+v", a)
	}
	save(rollRun{Started: 4, Status: "failed", Ack: true})
	if s.rollingAlert() != nil {
		t.Fatal("a dismissed run raises nothing")
	}
}

func TestInterruptedMoveNamesFileAndNode(t *testing.T) {
	err := interruptedMoveError("web1", "node-b", []string{"Volume web1.qcow2: already exists on node-b at /var/lib/libvirt/images/web1.qcow2; remove or rename it first"})
	for _, want := range []string{"web1", "/var/lib/libvirt/images/web1.qcow2", "node-b", "still runs here", "Continue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is missing from %q", want, err)
		}
	}
}
