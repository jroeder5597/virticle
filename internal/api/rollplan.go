package api

import (
	"fmt"
	"sort"
	"strings"

	"virticle/internal/nodeproto"
)

// The planning side of rolling node updates. Everything here is pure: it gets what was read from the nodes and
// returns the plan, the steps of a node and the sentence that says what state a stopped run left. rolling.go reads
// the nodes and runs the plan.
//
// Rules the plan applies:
//   - Nodes are updated one at a time, in the order of their ids, the machine of the controller last.
//   - The VMs of the node that are running are moved to other nodes that are online and not in maintenance, by the
//     evacuation planner. A target that was updated earlier in the run is tried first. Every target keeps a reserve of
//     memory free, so a move never fills a node up.
//   - A VM that cannot move is listed with the reasons. The one choice of a plan is what to do with those: "shutdown"
//     (stop them for the update, start them again afterwards) or "skip" (leave the node they run on out of the run).
//   - The run refuses to start when it cannot be done safely (a node is offline, a node to update is in maintenance
//     or has no node command, a node's updates cannot be read, another job is running).

const (
	rollChoiceShutdown = "shutdown"
	rollChoiceSkip     = "skip"

	rollMinReserveKiB = 512 << 10 // every target keeps at least this much memory free
)

// rollReserveKiB is the memory a target keeps free after the moves: 5 percent of its memory, at least 512 MiB.
func rollReserveKiB(totalKiB uint64) uint64 {
	r := totalKiB / 20
	if r < rollMinReserveKiB {
		r = rollMinReserveKiB
	}
	return r
}

// rollNodeInfo is what is known about one node when a plan is made.
type rollNodeInfo struct {
	ID          int64
	Name        string
	Online      bool
	Maintenance bool
	Local       bool // the machine the controller runs on
	NoNodeCmd   bool // the node command is not installed or does not answer
	Family      string
	FreeKiB     uint64
	TotalKiB    uint64
	Pending     []string // names of packages with an update
	RebootFlag  bool     // the node already says it needs a restart
	UpdateErr   string   // the updates could not be read
	VMs         []evacVM // the VMs that are not shut off
}

type rollPlanIn struct {
	Nodes  []rollNodeInfo
	Select map[int64]bool // empty: every node
	Choice string
	Busy   string // non-empty: why no run can start now
	// Done lists nodes that were updated earlier in this run (live planning during a run); they are tried first as
	// targets. In a plan made before the run, the nodes earlier in the order count as done.
	Done map[int64]bool
	// Check answers "may this VM go to that node?" (the migration pre-flight); none means yes.
	Check func(vm evacVM, from int64, to evacTarget) []string
}

type rollMove struct {
	VM     evacVM `json:"vm"`
	To     int64  `json:"to"`
	ToName string `json:"toName"`
}

type rollStuck struct {
	VM      evacVM   `json:"vm"`
	Reasons []string `json:"reasons"`
}

type rollNodePlan struct {
	Node        int64       `json:"node"`
	Name        string      `json:"name"`
	Local       bool        `json:"local,omitempty"`
	Updates     int         `json:"updates"`
	UpdateNames []string    `json:"updateNames,omitempty"`
	Reboot      bool        `json:"reboot"`
	RebootWhy   string      `json:"rebootWhy,omitempty"`
	Action      string      `json:"action"` // update | skip | current
	SkipWhy     string      `json:"skipWhy,omitempty"`
	Moves       []rollMove  `json:"moves"`
	Stuck       []rollStuck `json:"stuck"`
	Note        string      `json:"note,omitempty"`
}

type rollPlan struct {
	Nodes       []rollNodePlan `json:"nodes"`
	Refusals    []string       `json:"refusals"`
	NeedsChoice bool           `json:"needsChoice"`
	Choice      string         `json:"choice,omitempty"`
}

// rollOrder returns the indexes of the nodes to update in the order they are updated.
func rollOrder(nodes []rollNodeInfo, sel map[int64]bool) []int {
	var idx []int
	for i, n := range nodes {
		if len(sel) == 0 || sel[n.ID] {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		na, nb := nodes[idx[a]], nodes[idx[b]]
		if na.Local != nb.Local {
			return nb.Local // the controller's machine last
		}
		return na.ID < nb.ID
	})
	return idx
}

// rollRebootExpected says whether the update of a node is expected to need a restart, and why.
func rollRebootExpected(n rollNodeInfo) (bool, string) {
	var why []string
	if n.RebootFlag {
		why = append(why, "the node already needs a restart")
	}
	why = append(why, nodeproto.RebootReasons(n.Pending)...)
	return len(why) > 0, strings.Join(why, ", ")
}

// planRolling makes the plan for the datacenter.
func planRolling(in rollPlanIn) rollPlan {
	p := rollPlan{Nodes: []rollNodePlan{}, Refusals: []string{}, Choice: in.Choice}
	order := rollOrder(in.Nodes, in.Select)
	if in.Busy != "" {
		p.Refusals = append(p.Refusals, in.Busy)
	}
	if len(order) == 0 {
		p.Refusals = append(p.Refusals, "There is no node to update.")
	}
	selected := map[int64]bool{}
	for _, i := range order {
		selected[in.Nodes[i].ID] = true
	}
	for _, n := range in.Nodes {
		switch {
		case !n.Online:
			p.Refusals = append(p.Refusals, fmt.Sprintf("%s is offline. No node is updated while another one is down; bring %s back first.", n.Name, n.Name))
		case selected[n.ID] && n.Maintenance:
			p.Refusals = append(p.Refusals, fmt.Sprintf("%s is in maintenance. End maintenance on it first: a node that was put in maintenance by hand is not touched by an update.", n.Name))
		case selected[n.ID] && n.NoNodeCmd:
			p.Refusals = append(p.Refusals, fmt.Sprintf("%s has no working node command, so it cannot be updated from here. Run \"virticle node prepare\" for it.", n.Name))
		case selected[n.ID] && n.UpdateErr != "":
			p.Refusals = append(p.Refusals, fmt.Sprintf("%s: the updates cannot be read: %s", n.Name, n.UpdateErr))
		case selected[n.ID] && n.Family != "dnf" && n.Family != "apt":
			p.Refusals = append(p.Refusals, fmt.Sprintf("%s runs a system that is not based on dnf or apt; it cannot be updated from here.", n.Name))
		}
	}

	done := map[int64]bool{}
	for k := range in.Done {
		done[k] = true
	}
	for _, i := range order {
		n := in.Nodes[i]
		np := rollNodePlan{Node: n.ID, Name: n.Name, Local: n.Local, Updates: len(n.Pending), UpdateNames: n.Pending,
			Action: "update", Moves: []rollMove{}, Stuck: []rollStuck{}}
		np.Reboot, np.RebootWhy = rollRebootExpected(n)
		if np.Updates == 0 && !n.RebootFlag {
			np.Action = "current"
			np.Reboot = false
			p.Nodes = append(p.Nodes, np)
			continue
		}
		var targets []evacTarget
		for _, o := range in.Nodes {
			if o.ID == n.ID {
				continue
			}
			free := uint64(0)
			if r := rollReserveKiB(o.TotalKiB); o.FreeKiB > r {
				free = o.FreeKiB - r
			}
			t := evacTarget{ID: o.ID, Name: o.Name, FreeKiB: free, Online: o.Online, Maintenance: o.Maintenance}
			if done[o.ID] {
				t.Rank = 1
			}
			targets = append(targets, t)
		}
		check := func(vm evacVM, t evacTarget) []string {
			if in.Check == nil {
				return nil
			}
			return in.Check(vm, n.ID, t)
		}
		plan, blocked := planEvacuation(n.VMs, targets, check)
		for _, a := range plan {
			np.Moves = append(np.Moves, rollMove{VM: a.VM, To: a.Target, ToName: a.TargetName})
		}
		for _, b := range blocked {
			np.Stuck = append(np.Stuck, rollStuck{VM: b.VM, Reasons: b.Reasons})
		}
		if len(np.Stuck) > 0 {
			p.NeedsChoice = true
			if in.Choice == rollChoiceSkip {
				np.Action = "skip"
				names := make([]string, len(np.Stuck))
				for j, s := range np.Stuck {
					names[j] = s.VM.Name
				}
				np.SkipWhy = "not updated, because " + strings.Join(names, ", ") + " cannot move"
			}
		}
		if np.Action == "update" {
			done[n.ID] = true
			if n.Local {
				if np.Reboot {
					np.Note = "This is the machine the controller runs on, so it is updated last. While it restarts this web interface is not available, for a few minutes. The update carries on by itself when the controller is back; open the Nodes page again to follow it."
				} else {
					np.Note = "This is the machine the controller runs on, so it is updated last. It does not need a restart."
				}
			}
		}
		p.Nodes = append(p.Nodes, np)
	}
	return p
}

// --- steps of a node ---

// Step ids, in the order they run.
const (
	stepMaint      = "maintenance"
	stepEvacuate   = "evacuate"
	stepStop       = "stop-vms"
	stepUpdate     = "update"
	stepReboot     = "reboot"
	stepWait       = "wait"
	stepVerify     = "verify"
	stepMaintOff   = "maintenance-off"
	stepStart      = "start-vms"
	stepReturn     = "return"
	stepStatusPend = "pending"
)

var rollStepTitles = map[string]string{
	stepMaint:    "Put the node in maintenance",
	stepEvacuate: "Move its running virtual machines to other nodes",
	stepStop:     "Shut down the virtual machines that cannot move",
	stepUpdate:   "Install the updates",
	stepReboot:   "Restart the node",
	stepWait:     "Wait for the node to come back",
	stepVerify:   "Check that the node is healthy",
	stepMaintOff: "End maintenance",
	stepStart:    "Start the virtual machines that were shut down",
	stepReturn:   "Move the virtual machines back",
}

type rollStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // pending | running | ok | skipped | failed
	Detail string `json:"detail,omitempty"`
}

// rollStepsFor lists the steps of one node. Steps that do not apply to the node are marked skipped with the reason,
// so the list always has the same shape.
func rollStepsFor(np rollNodePlan, choice string) []rollStep {
	var out []rollStep
	for _, id := range []string{stepMaint, stepEvacuate, stepStop, stepUpdate, stepReboot, stepWait, stepVerify, stepMaintOff, stepStart, stepReturn} {
		s := rollStep{ID: id, Title: rollStepTitles[id], Status: stepStatusPend}
		switch id {
		case stepEvacuate:
			if len(np.Moves) == 0 {
				s.Status, s.Detail = "skipped", "no virtual machine is running on it"
				if len(np.Stuck) > 0 {
					s.Detail = "none of its virtual machines can move"
				}
			}
		case stepStop, stepStart:
			if len(np.Stuck) == 0 || choice != rollChoiceShutdown {
				s.Status, s.Detail = "skipped", "every virtual machine can move"
			}
		case stepReturn:
			if len(np.Moves) == 0 {
				s.Status, s.Detail = "skipped", "nothing was moved"
			}
		}
		out = append(out, s)
	}
	return out
}

// rollNextStep returns the index of the first step that still has to run, or -1.
func rollNextStep(steps []rollStep) int {
	for i, s := range steps {
		if s.Status == stepStatusPend || s.Status == "running" || s.Status == "failed" {
			return i
		}
	}
	return -1
}

// --- the condition a stopped run leaves ---

// rollCondition says, in plain words, what state a run that stopped (failed, cancelled, or still waiting for a
// decision) left the datacenter in, and what can be done about it.
func rollCondition(r *rollRun) string {
	var lines []string
	cur := r.Cur
	var untouched []string
	for i, n := range r.Nodes {
		if i > cur && n.Status == stepStatusPend {
			untouched = append(untouched, n.Name)
		}
	}
	if cur >= 0 && cur < len(r.Nodes) {
		n := r.Nodes[cur]
		if n.Status != "done" && n.Status != "skipped" && n.Status != "current" {
			started := false
			for _, s := range n.Steps {
				if s.Status != stepStatusPend && s.Status != "skipped" {
					started = true
				}
			}
			switch {
			case !started:
				lines = append(lines, fmt.Sprintf("%s was not touched.", n.Name))
			default:
				stepOK := func(id string) bool {
					for _, s := range n.Steps {
						if s.ID == id {
							return s.Status == "ok"
						}
					}
					return false
				}
				switch {
				case stepOK(stepUpdate):
					lines = append(lines, fmt.Sprintf("The updates are installed on %s.", n.Name))
				case stepOK(stepMaint):
					lines = append(lines, fmt.Sprintf("The updates are not installed on %s.", n.Name))
				}
				if n.RebootSent && !stepOK(stepWait) {
					lines = append(lines, fmt.Sprintf("%s was restarted and has not been confirmed healthy yet.", n.Name))
				}
				if n.MaintSet && !stepOK(stepMaintOff) {
					lines = append(lines, fmt.Sprintf("%s is in maintenance.", n.Name))
				}
				var away, down []string
				for _, m := range n.Moved {
					if m.Status != "back" && m.Status != "stay" {
						away = append(away, fmt.Sprintf("%s (on %s)", m.Name, m.ToName))
					}
				}
				for _, d := range n.Down {
					if !d.Started {
						down = append(down, d.Name)
					}
				}
				if len(away) > 0 {
					lines = append(lines, fmt.Sprintf("These virtual machines belong to %s but run elsewhere: %s.", n.Name, strings.Join(away, ", ")))
				}
				if len(down) > 0 {
					lines = append(lines, fmt.Sprintf("These virtual machines were shut down for the update and are not started yet: %s.", strings.Join(down, ", ")))
				}
			}
		}
	}
	var finished []string
	for _, n := range r.Nodes {
		if n.Status == "done" {
			finished = append(finished, n.Name)
		}
	}
	if len(finished) > 0 {
		lines = append(lines, fmt.Sprintf("Updated and checked: %s.", strings.Join(finished, ", ")))
	}
	if len(untouched) > 0 {
		lines = append(lines, fmt.Sprintf("Not touched: %s.", strings.Join(untouched, ", ")))
	}
	if len(lines) == 0 {
		return "Nothing was changed."
	}
	return strings.Join(lines, " ")
}

// rollReopen turns a stopped run (failed or cancelled) into one that runs again. Continuing re-runs the step that
// failed and the steps after it. Restoring ends the run for the node it stopped on: the steps that were not done are
// dropped, and what is left is ending maintenance, starting the VMs that were shut down and moving the VMs back.
func rollReopen(run *rollRun, restore bool) {
	run.Status, run.Cancel, run.Ack, run.Error, run.Finished, run.Condition = "running", false, false, "", 0, ""
	run.Restoring = restore
	if run.Cur < 0 || run.Cur >= len(run.Nodes) {
		return
	}
	n := run.Nodes[run.Cur]
	for i := range n.Steps {
		st := &n.Steps[i]
		if restore {
			switch st.ID {
			case stepMaintOff, stepStart, stepReturn:
				if st.Status == "failed" || st.Status == "running" {
					st.Status = stepStatusPend
				}
			default:
				if st.Status != "ok" && st.Status != "skipped" {
					st.Status, st.Detail = "skipped", "not done: the run was put back"
				}
			}
			continue
		}
		if st.Status == "failed" {
			st.Status = stepStatusPend
		}
	}
	if n.Status != "done" {
		n.Status = "running"
	}
}
