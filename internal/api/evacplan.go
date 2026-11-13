package api

import (
	"fmt"
	"sort"
	"strings"
)

// The evacuation planner is a pure function: it is given the VMs to move, the
// candidate targets with their free memory, and a callback that answers "may
// this VM go to that node?" (the migration pre-flight in production). It
// knows nothing about libvirt, so the policy can be tested exhaustively.

type evacVM struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	State     string `json:"state"`
	MemoryKiB uint64 `json:"memoryKiB"`
}

// needsMemory: only a VM that runs takes memory from the target at once. A
// shut-off VM is just moved; it needs the memory when it is started later.
func (v evacVM) needsMemory() uint64 {
	if v.State == "shutoff" {
		return 0
	}
	return v.MemoryKiB
}

type evacTarget struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	FreeKiB     uint64 `json:"freeKiB"`
	Online      bool   `json:"online"`
	Maintenance bool   `json:"maintenance"`
	// Rank orders the targets before memory does: a higher rank is tried first. Zero for all in a plain evacuation.
	Rank int `json:"-"`
}

func (t evacTarget) usable() bool { return t.Online && !t.Maintenance }

type evacAssignment struct {
	VM         evacVM `json:"vm"`
	Target     int64  `json:"target"`
	TargetName string `json:"targetName"`
}

type evacBlocked struct {
	VM      evacVM   `json:"vm"`
	Reasons []string `json:"reasons"`
}

// evacCheck returns the reasons a VM cannot go to a target; none means it can.
type evacCheck func(vm evacVM, t evacTarget) []string

// planEvacuation assigns every VM to a target. Largest VMs go first (a big VM
// is the hardest to place), each to the usable target with the most memory
// left that also passes check. Memory promised to earlier VMs of the same plan
// is subtracted, so two VMs are never both counted against the same free
// memory. check is called only for targets that have enough memory left.
func planEvacuation(vms []evacVM, targets []evacTarget, check evacCheck) ([]evacAssignment, []evacBlocked) {
	order := append([]evacVM(nil), vms...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].needsMemory() > order[j].needsMemory() })

	left := map[int64]uint64{}
	var cands []evacTarget
	for _, t := range targets {
		if t.usable() {
			cands = append(cands, t)
			left[t.ID] = t.FreeKiB
		}
	}

	var plan []evacAssignment
	var blocked []evacBlocked
	for _, vm := range order {
		if len(cands) == 0 {
			blocked = append(blocked, evacBlocked{vm, []string{"no target node is available (every other node is offline, in maintenance or not selected)"}})
			continue
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Rank != cands[j].Rank {
				return cands[i].Rank > cands[j].Rank
			}
			return left[cands[i].ID] > left[cands[j].ID]
		})
		var reasons []string
		placed := false
		for _, t := range cands {
			need := vm.needsMemory()
			if left[t.ID] < need {
				reasons = append(reasons, fmt.Sprintf("%s: not enough free memory (%s needed, %s left after the VMs already planned)",
					t.Name, fmtBytes(need*1024), fmtBytes(left[t.ID]*1024)))
				continue
			}
			if bad := check(vm, t); len(bad) > 0 {
				for _, b := range bad {
					reasons = append(reasons, t.Name+": "+b)
				}
				continue
			}
			left[t.ID] -= need
			plan = append(plan, evacAssignment{VM: vm, Target: t.ID, TargetName: t.Name})
			placed = true
			break
		}
		if !placed {
			blocked = append(blocked, evacBlocked{vm, reasons})
		}
	}
	return plan, blocked
}

// blockingReasons turns pre-flight results into "Check: message" lines for
// the checks that block a migration.
func blockingReasons(c checkList) []string {
	var out []string
	for _, k := range c {
		if !k.OK && k.Severity == "error" {
			out = append(out, strings.TrimSpace(k.Check+": "+k.Message))
		}
	}
	return out
}
