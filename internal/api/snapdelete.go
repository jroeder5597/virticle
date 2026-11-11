package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"virticle/internal/snapsched"
)

// Deleting snapshots. What libvirt does (checked on the lab, libvirt 11.10 on Rocky 10 with SELinux, and 11.3 on
// Debian 13 with AppArmor):
//
//   - A plain delete never touches the snapshots below the deleted one. They stay and their parent becomes the
//     deleted snapshot's parent (a root's children become roots). If the deleted snapshot was the current one,
//     its parent becomes current.
//   - Internal snapshots are independent copies inside the qcow2 file, so any of them can go at any time.
//   - An external snapshot is merged into the disk chain (block commit). libvirt refuses it when the snapshot has
//     several children ("deletion of external disk snapshot with multiple children snapshots not supported"), and
//     when it has a child but is not on the chain leading to the current snapshot ("deletion of non-leaf external
//     snapshot that is not in active chain is not supported"). A leaf can always go, so deleting a branch from its
//     tip downwards works where deleting its root first does not.
//   - On nodes that confine QEMU with AppArmor the merge itself is denied, so external snapshots cannot be deleted.

// snapRule says why one snapshot cannot be deleted in the current shape of the tree, "" if it can.
type snapProblem struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type snapPlan struct {
	// Order is the sequence the snapshots are deleted in: the deepest first, so a branch goes from its tip down.
	Order    []string      `json:"order"`
	Problems []snapProblem `json:"problems"`
}

// hardBlock is the reason a snapshot can never be deleted on this node, whatever else is deleted.
func hardBlock(si snapInfo, apparmor bool) string {
	if si.Kind == "external" && apparmor {
		return "This node confines QEMU with AppArmor, where libvirt cannot merge and delete external snapshots."
	}
	return ""
}

// planSnapshotDelete orders the deletion of names (deepest first) and checks libvirt's rules for each step against
// the tree as it will be at that step: deleting the leaves of a branch first can make its root deletable.
// list must be in topological order (parents first).
func planSnapshotDelete(list []snapInfo, names []string, apparmor bool) snapPlan {
	plan := snapPlan{Order: []string{}, Problems: []snapProblem{}}
	info := map[string]snapInfo{}
	parent := map[string]string{}
	depth := map[string]int{}
	current := ""
	for _, si := range list {
		info[si.Name] = si
		p := si.Parent
		if _, ok := info[p]; !ok {
			p = "" // an orphan (its parent is gone) counts as a root
		}
		parent[si.Name] = p
		if p != "" {
			depth[si.Name] = depth[p] + 1
		}
		if si.IsCurrent {
			current = si.Name
		}
	}
	chosen := map[string]bool{}
	for _, n := range names {
		if _, ok := info[n]; ok && !chosen[n] {
			chosen[n] = true
			plan.Order = append(plan.Order, n)
		}
	}
	idx := map[string]int{}
	for i, si := range list {
		idx[si.Name] = i
	}
	sort.SliceStable(plan.Order, func(i, j int) bool {
		a, b := plan.Order[i], plan.Order[j]
		if depth[a] != depth[b] {
			return depth[a] > depth[b]
		}
		return idx[a] < idx[b]
	})
	children := func(n string) []string {
		var out []string
		for _, si := range list {
			if parent[si.Name] == n && si.Name != n {
				out = append(out, si.Name)
			}
		}
		return out
	}
	removed := map[string]bool{}
	live := func(ns []string) []string {
		var out []string
		for _, n := range ns {
			if !removed[n] {
				out = append(out, n)
			}
		}
		return out
	}
	inActiveChain := func(n string) bool {
		for c := current; c != ""; c = parent[c] {
			if c == n {
				return true
			}
		}
		return false
	}
	for _, n := range plan.Order {
		si := info[n]
		kids := live(children(n))
		reason := hardBlock(si, apparmor)
		if reason == "" && si.Kind == "external" {
			switch {
			case len(kids) > 1:
				reason = fmt.Sprintf("libvirt cannot delete an external snapshot that has several child snapshots (%d here). Delete those branches first, or select them as well.", len(kids))
			case len(kids) == 1 && !inActiveChain(n):
				reason = "libvirt cannot delete an external snapshot that has a child snapshot unless it lies on the path to the current snapshot. Select the snapshots below it as well."
			case parent[n] != "" && info[parent[n]].Kind == "internal":
				reason = "libvirt cannot delete an external snapshot whose parent is an internal snapshot."
			}
		}
		if reason != "" {
			plan.Problems = append(plan.Problems, snapProblem{Name: n, Reason: reason})
			continue
		}
		// Apply the delete to the model: children move up, the current marker moves to the parent.
		for _, k := range kids {
			parent[k] = parent[n]
		}
		if current == n {
			current = parent[n]
		}
		removed[n] = true
	}
	return plan
}

// scheduledSnapshot reports whether the description marks a snapshot the scheduler made.
func scheduledSnapshot(desc string) bool {
	return snapsched.IsScheduled(desc)
}

// annotateSnapshots fills Deletable, Reason and Scheduled of a listing.
func annotateSnapshots(list []snapInfo, apparmor bool) {
	for i := range list {
		list[i].Scheduled = scheduledSnapshot(list[i].Description)
		list[i].Deletable = true
		if r := hardBlock(list[i], apparmor); r != "" {
			list[i].Deletable, list[i].Reason = false, r
		}
	}
}

type deleteSnapshotsRequest struct {
	Names []string `json:"names"`
}

// snapshotSelection reads and checks a request: at least one name, no duplicates, each name valid and existing.
func snapshotSelection(r *http.Request, list []snapInfo) ([]string, error) {
	var q deleteSnapshotsRequest
	if err := readJSON(r, &q); err != nil {
		return nil, err
	}
	if len(q.Names) == 0 {
		return nil, badRequest("select at least one snapshot")
	}
	have := map[string]bool{}
	for _, si := range list {
		have[si.Name] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range q.Names {
		switch {
		case !snapNameRE.MatchString(n):
			return nil, badRequest(snapNameRule)
		case !have[n]:
			return nil, badRequest(fmt.Sprintf("this VM has no snapshot named %s", n))
		case seen[n]:
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// planSnapshotsHandler checks a selection without changing anything.
func (s *Server) planSnapshotsHandler(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	list, err := snapshotList(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	names, err := snapshotSelection(r, list)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, planSnapshotDelete(list, names, nodeIsAppArmor(h)))
}

// deleteSnapshotsHandler deletes the selected snapshots as one task, one after the other, deepest first, and stops at
// the first failure. Nothing outside the selection is touched and libvirt's children option is never used.
func (s *Server) deleteSnapshotsHandler(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	list, err := snapshotList(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	names, err := snapshotSelection(r, list)
	if err != nil {
		fail(w, err)
		return
	}
	plan := planSnapshotDelete(list, names, nodeIsAppArmor(h))
	if len(plan.Problems) > 0 {
		var parts []string
		for _, p := range plan.Problems {
			parts = append(parts, p.Name+": "+p.Reason)
		}
		fail(w, badRequest("these snapshots cannot be deleted as selected: "+strings.Join(parts, " ")))
		return
	}
	target := dom.Name + "@" + strings.Join(plan.Order, ",")
	if len(plan.Order) > 3 {
		target = fmt.Sprintf("%s@%d snapshots", dom.Name, len(plan.Order))
	}
	err = s.taskAsync(r, "snapshot delete", target, h.Name, func(progress func(float64)) error {
		defer refreshPools(h)
		for i, name := range plan.Order {
			v, err := h.DomainSnapshotLookupByName(dom, name, 0)
			if err == nil {
				err = h.DomainSnapshotDelete(v, 0)
			}
			if err != nil {
				return deletionStopped(plan.Order, i, err)
			}
			progress(float64(i+1) / float64(len(plan.Order)))
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "deleted": plan.Order})
}

// deletionStopped words a failure after done snapshots were already deleted.
func deletionStopped(order []string, done int, cause error) error {
	var left []string
	if done+1 < len(order) {
		left = order[done+1:]
	}
	msg := fmt.Sprintf("deleted %s", nameList(order[:done]))
	if done == 0 {
		msg = "nothing was deleted"
	}
	msg += fmt.Sprintf("; %s failed: %v", order[done], cause)
	if len(left) > 0 {
		msg += fmt.Sprintf("; not attempted: %s", strings.Join(left, ", "))
	}
	return fmt.Errorf("%s", msg)
}

func nameList(ns []string) string { return strings.Join(ns, ", ") }
