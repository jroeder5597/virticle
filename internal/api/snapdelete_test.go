package api

import (
	"reflect"
	"strings"
	"testing"
)

func problemNames(p snapPlan) []string {
	out := []string{}
	for _, x := range p.Problems {
		out = append(out, x.Name)
	}
	return out
}

// The tree of the lab experiment on libvirt 11.10: e1 (external) has two children, the internal b1 (current) and
// the external e2, which has the external child e3.
func branched() []snapInfo {
	return []snapInfo{
		{Name: "e1", Kind: "external"},
		{Name: "b1", Kind: "internal", Parent: "e1", IsCurrent: true},
		{Name: "e2", Kind: "external", Parent: "e1"},
		{Name: "e3", Kind: "external", Parent: "e2"},
	}
}

func TestPlanSnapshotDeleteExternalRules(t *testing.T) {
	l := branched()
	if p := planSnapshotDelete(l, []string{"e1"}, false); !reflect.DeepEqual(problemNames(p), []string{"e1"}) || !strings.Contains(p.Problems[0].Reason, "several child") {
		t.Errorf("branch point alone: %+v", p)
	}
	if p := planSnapshotDelete(l, []string{"e2"}, false); !reflect.DeepEqual(problemNames(p), []string{"e2"}) || !strings.Contains(p.Problems[0].Reason, "path to the current") {
		t.Errorf("non-leaf off the active chain: %+v", p)
	}
	// A leaf goes anywhere; the branch from its tip down works.
	if p := planSnapshotDelete(l, []string{"e3"}, false); len(p.Problems) != 0 {
		t.Errorf("leaf: %+v", p)
	}
	p := planSnapshotDelete(l, []string{"e2", "e3"}, false)
	if len(p.Problems) != 0 || !reflect.DeepEqual(p.Order, []string{"e3", "e2"}) {
		t.Errorf("branch tip first: %+v", p)
	}
	// Everything: e3, e2, then e1 which by then has only the current b1 below it.
	p = planSnapshotDelete(l, []string{"e1", "e2", "e3", "b1"}, false)
	if len(p.Problems) != 0 || p.Order[len(p.Order)-1] != "e1" {
		t.Errorf("all: %+v", p)
	}
	// e1 and e2 without e3: e2 still has a child off the active chain.
	if p := planSnapshotDelete(l, []string{"e1", "e2"}, false); len(p.Problems) == 0 {
		t.Errorf("e2 with e3 kept must be refused: %+v", p)
	}
}

func TestPlanSnapshotDeleteChains(t *testing.T) {
	// A chain on the current path: the oldest external snapshot (one child) can go, and so can the current one.
	l := []snapInfo{{Name: "e1", Kind: "external"}, {Name: "e2", Kind: "external", Parent: "e1"}, {Name: "e3", Kind: "external", Parent: "e2", IsCurrent: true}}
	for _, n := range []string{"e1", "e2", "e3"} {
		if p := planSnapshotDelete(l, []string{n}, false); len(p.Problems) != 0 {
			t.Errorf("%s: %+v", n, p)
		}
	}
	// Internal snapshots are always deletable, in any order and any selection.
	in := []snapInfo{{Name: "a", Kind: "internal"}, {Name: "b", Kind: "internal", Parent: "a"}, {Name: "c", Kind: "internal", Parent: "b"}, {Name: "d", Kind: "internal", Parent: "a", IsCurrent: true}}
	p := planSnapshotDelete(in, []string{"a", "b", "c"}, false)
	if len(p.Problems) != 0 || !reflect.DeepEqual(p.Order, []string{"c", "b", "a"}) {
		t.Errorf("internal: %+v", p)
	}
	// AppArmor nodes: no external snapshot at all, internal ones unaffected.
	if p := planSnapshotDelete(append(in, snapInfo{Name: "x", Kind: "external"}), []string{"x", "a"}, true); !reflect.DeepEqual(problemNames(p), []string{"x"}) {
		t.Errorf("apparmor: %+v", p)
	}
}

func TestPlanSnapshotDeleteOrphanAndUnknown(t *testing.T) {
	// The parent of o is not in the list: it counts as a root. Unknown and duplicate names are ignored.
	l := []snapInfo{{Name: "o", Kind: "internal", Parent: "gone"}, {Name: "p", Kind: "internal", Parent: "o"}}
	p := planSnapshotDelete(l, []string{"o", "p", "p", "nope"}, false)
	if !reflect.DeepEqual(p.Order, []string{"p", "o"}) || len(p.Problems) != 0 {
		t.Errorf("%+v", p)
	}
}

func TestAnnotateSnapshots(t *testing.T) {
	l := []snapInfo{{Name: "a", Kind: "external", Description: "Scheduled snapshot (virt-proj) [controller ab12]"}, {Name: "b", Kind: "internal"}}
	annotateSnapshots(l, true)
	if l[0].Deletable || l[0].Reason == "" || !l[0].Scheduled || !l[1].Deletable || l[1].Scheduled {
		t.Errorf("%+v", l)
	}
}

type errText string

func (e errText) Error() string { return string(e) }

func TestDeletionStopped(t *testing.T) {
	if got := deletionStopped([]string{"c", "b", "a"}, 1, errText("boom")).Error(); got != "deleted c; b failed: boom; not attempted: a" {
		t.Error(got)
	}
	if got := deletionStopped([]string{"c", "b"}, 0, errText("x")).Error(); got != "nothing was deleted; c failed: x; not attempted: b" {
		t.Error(got)
	}
}
