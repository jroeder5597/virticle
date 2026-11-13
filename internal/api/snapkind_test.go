package api

import (
	"strings"
	"testing"
)

// One row per case of the table in snapkind.go. want is the kind, or "" for a refusal whose reason contains why.
func TestChooseSnapshotKind(t *testing.T) {
	rows := []struct {
		name string
		in   snapInputs
		want string
		why  string
	}{
		// No snapshots yet.
		{"running, state saved: external", snapInputs{Active: true, WithState: true}, kindExternal, ""},
		{"running, disk only: external", snapInputs{Active: true}, kindExternal, ""},
		{"running, raw UEFI store, state saved: external", snapInputs{Active: true, WithState: true, RawNVRAM: true}, kindExternal, ""},
		{"shut off: internal", snapInputs{}, kindInternal, ""},
		{"shut off, raw UEFI store: internal", snapInputs{RawNVRAM: true}, kindInternal, ""},
		{"shut off with the state asked for", snapInputs{WithState: true}, "", "no running state"},
		// AppArmor nodes: internal only.
		{"AppArmor, running, state saved: internal", snapInputs{Active: true, WithState: true, AppArmor: true}, kindInternal, ""},
		{"AppArmor, running, disk only: refused", snapInputs{Active: true, AppArmor: true}, "", "running state"},
		{"AppArmor, running, raw UEFI store, state saved: refused", snapInputs{Active: true, WithState: true, AppArmor: true, RawNVRAM: true}, "", "UEFI"},
		{"AppArmor, shut off: internal", snapInputs{AppArmor: true}, kindInternal, ""},
		// The VM already has internal snapshots.
		{"has internal, running, state saved: internal", snapInputs{Active: true, WithState: true, HasInt: true}, kindInternal, ""},
		{"has internal, running, disk only: refused", snapInputs{Active: true, HasInt: true}, "", "running state saved"},
		{"has internal, running, raw UEFI store, state saved: refused", snapInputs{Active: true, WithState: true, HasInt: true, RawNVRAM: true}, "", "Shut the VM down"},
		{"has internal, shut off: internal", snapInputs{HasInt: true}, kindInternal, ""},
		// The VM already has external snapshots.
		{"has external, running, state saved: external", snapInputs{Active: true, WithState: true, HasExt: true}, kindExternal, ""},
		{"has external, running, disk only: external", snapInputs{Active: true, HasExt: true}, kindExternal, ""},
		{"has external, shut off: external", snapInputs{HasExt: true}, kindExternal, ""},
		{"has external, AppArmor, running: refused", snapInputs{Active: true, WithState: true, HasExt: true, AppArmor: true}, "", "cannot delete"},
		{"has external, AppArmor, shut off: refused", snapInputs{HasExt: true, AppArmor: true}, "", "cannot delete"},
		// Both kinds.
		{"mixed tree", snapInputs{Active: true, WithState: true, HasInt: true, HasExt: true}, "", "both kinds"},
	}
	for _, r := range rows {
		got := chooseSnapshotKind(r.in)
		if r.want != "" {
			if !got.Allowed || got.Kind != r.want || got.Reason != "" {
				t.Errorf("%s: %+v", r.name, got)
			}
			continue
		}
		if got.Allowed || !strings.Contains(got.Reason, r.why) {
			t.Errorf("%s: %+v, want a refusal mentioning %q", r.name, got, r.why)
		}
	}
}

func TestSnapshotOptions(t *testing.T) {
	// Free choice on a running VM without snapshots.
	o := snapshotOptions(snapInputs{Active: true})
	if !o.WithState.Allowed || !o.DiskOnly.Allowed || o.Reason != "" {
		t.Errorf("%+v", o)
	}
	// Earlier internal snapshots force the running state.
	o = snapshotOptions(snapInputs{Active: true, HasInt: true})
	if !o.WithState.Allowed || o.DiskOnly.Allowed || o.DiskOnly.Reason == "" || o.Reason != "" {
		t.Errorf("%+v", o)
	}
	// Nothing at all on a running VM with a raw store and internal snapshots, but shutting down works.
	o = snapshotOptions(snapInputs{Active: true, HasInt: true, RawNVRAM: true})
	if o.WithState.Allowed || o.DiskOnly.Allowed || !strings.Contains(o.Reason, "Shutting the VM down") {
		t.Errorf("%+v", o)
	}
	// Shut off: the checkbox does not exist, the disk-only choice decides.
	o = snapshotOptions(snapInputs{})
	if o.WithState.Allowed || !o.DiskOnly.Allowed || o.Reason != "" {
		t.Errorf("%+v", o)
	}
	o = snapshotOptions(snapInputs{HasExt: true, AppArmor: true})
	if o.Reason == "" {
		t.Errorf("%+v", o)
	}
}

func TestMixedTreeNote(t *testing.T) {
	if n := mixedTreeNote([]snapInfo{{Name: "a", Kind: "internal"}, {Name: "b", Kind: "internal", Parent: "a"}}); n != "" {
		t.Error(n)
	}
	// External child of an internal parent: neither can be deleted.
	n := mixedTreeNote([]snapInfo{{Name: "i1", Kind: "internal"}, {Name: "e1", Kind: "external", Parent: "i1"}})
	if !strings.Contains(n, "i1, e1") || !strings.Contains(n, "Reverting to i1") {
		t.Error(n)
	}
	// Internal child of an external parent is fine when deleted from the tip down.
	if n := mixedTreeNote([]snapInfo{{Name: "e1", Kind: "external"}, {Name: "i1", Kind: "internal", Parent: "e1"}}); n != "" {
		t.Error(n)
	}
}

func TestPlanInternalWithExternalBelow(t *testing.T) {
	l := []snapInfo{{Name: "i1", Kind: "internal"}, {Name: "e1", Kind: "external", Parent: "i1", IsCurrent: true}}
	p := planSnapshotDelete(l, []string{"i1"}, false)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0].Reason, "lies below") {
		t.Errorf("%+v", p)
	}
}
