package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testPoint(seq, gen int, ck string) *replPoint {
	return &replPoint{Format: replFormat, Seq: seq, Gen: gen, Time: "2026-11-13T10:00:00Z", Kind: kindIncremental, Checkpoint: ck,
		Disks: []replDisk{{Target: "vda", File: replOverlayName(seq, "vda"), Virt: 10 << 30, Source: "/p/vm.qcow2", Vol: "vm.qcow2"}}}
}

func TestDecideReplica(t *testing.T) {
	layout := []diskLayout{{Target: "vda", Source: "/p/vm.qcow2", Virt: 10 << 30}}
	good := replInput{Backupable: true, AllQcow2: true, Supported: true, Layout: layout,
		Checkpoints: map[string]bool{"virticle-a": true}, Latest: testPoint(3, 1, "virticle-a"), Intact: true}
	with := func(f func(*replInput)) replInput {
		in := good
		in.Checkpoints = map[string]bool{"virticle-a": true}
		f(&in)
		return in
	}
	tests := []struct {
		name string
		in   replInput
		kind string
		why  string
		ck   bool
	}{
		{"incremental", good, kindIncremental, "", true},
		{"first", with(func(in *replInput) { in.Latest = nil }), kindFull, whyReplFirst, true},
		{"shut off cannot be tracked", with(func(in *replInput) { in.Backupable = false }), kindFull, whyReplShutOff, false},
		{"raw disk", with(func(in *replInput) { in.AllQcow2 = false }), kindFull, whyReplFormat, false},
		{"old libvirt", with(func(in *replInput) { in.Supported = false }), kindFull, whyReplOldVirt, false},
		{"damaged replica", with(func(in *replInput) { in.Intact = false }), kindFull, whyReplDamaged, true},
		{"checkpoint gone", with(func(in *replInput) { in.Checkpoints = map[string]bool{} }), kindFull, whyReplNoBase, true},
		{"point without checkpoint", with(func(in *replInput) { in.Latest = testPoint(3, 1, "") }), kindFull, whyReplNoBase, true},
		{"disk resized", with(func(in *replInput) { in.Layout = []diskLayout{{Target: "vda", Source: "/p/vm.qcow2", Virt: 20 << 30}} }), kindFull, whyReplLayout, true},
		{"disk added", with(func(in *replInput) {
			in.Layout = append(append([]diskLayout{}, layout...), diskLayout{Target: "vdb", Source: "/p/b.qcow2", Virt: 1 << 30})
		}), kindFull, whyReplLayout, true},
		{"disk moved", with(func(in *replInput) { in.Layout = []diskLayout{{Target: "vda", Source: "/q/vm.qcow2", Virt: 10 << 30}} }), kindFull, whyReplLayout, true},
	}
	for _, tc := range tests {
		got := decideReplica(tc.in)
		if got.Kind != tc.kind || got.Reason != tc.why || got.Checkpoint != tc.ck {
			t.Errorf("%s: got %+v, want kind %s reason %q checkpoint %v", tc.name, got, tc.kind, tc.why, tc.ck)
		}
		if got.Kind == kindIncremental && got.ParentCheckpoint != "virticle-a" {
			t.Errorf("%s: parent checkpoint %q", tc.name, got.ParentCheckpoint)
		}
	}
}

func TestPointManifestRoundTrip(t *testing.T) {
	p := testPoint(4, 2, "virticle-x")
	b, _ := json.Marshal(p)
	q, err := parseReplPoint(b)
	if err != nil || !reflect.DeepEqual(p, q) {
		t.Fatalf("round trip: %v %+v", err, q)
	}
	for name, mut := range map[string]func(*replPoint){
		"format":   func(p *replPoint) { p.Format = 9 },
		"seq":      func(p *replPoint) { p.Seq = 0 },
		"no disks": func(p *replPoint) { p.Disks = nil },
		"time":     func(p *replPoint) { p.Time = "yesterday" },
		"path":     func(p *replPoint) { p.Disks[0].File = "../p1-vda.qcow2" },
		"vol":      func(p *replPoint) { p.Disks[0].Vol = "a/b" },
	} {
		p := testPoint(1, 1, "")
		mut(p)
		b, _ := json.Marshal(p)
		if _, err := parseReplPoint(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseReplPoint([]byte("{")); err == nil {
		t.Error("broken json accepted")
	}
}

func TestNextSeqAndGen(t *testing.T) {
	names := []string{"b1-vda.qcow2", "b2-vda.qcow2", "p1.json", "p1-vda.qcow2", "p4-vda.qcow2", "in7-vda.qcow2", "n3.nvram", "p2.json"}
	if got := nextSeq(names); got != 8 {
		t.Errorf("nextSeq = %d, want 8 (above the interrupted run's in7)", got)
	}
	if got := nextGen(names); got != 3 {
		t.Errorf("nextGen = %d", got)
	}
	if nextSeq(nil) != 1 || nextGen(nil) != 1 {
		t.Error("empty directory starts at 1")
	}
}

func TestScanReplNames(t *testing.T) {
	s := scanReplNames([]string{"p10.json", "p2.json", "b1-vda.qcow2", "in3-vda.qcow2", "px.json"})
	if !reflect.DeepEqual(s.Points, []int{2, 10}) {
		t.Errorf("points %v", s.Points)
	}
	if len(s.Maybe) != 3 {
		t.Errorf("other %v", s.Maybe)
	}
}

func TestLivePoints(t *testing.T) {
	pts := []*replPoint{testPoint(1, 1, ""), testPoint(2, 1, ""), testPoint(3, 1, ""), testPoint(4, 2, ""), testPoint(5, 2, "")}
	kept, dropped := livePoints(pts, replKeep)
	if len(kept) != 2 || kept[0].Seq != 5 || kept[1].Seq != 4 {
		t.Errorf("kept %v", seqs(kept))
	}
	if !reflect.DeepEqual(seqs(dropOrder(dropped)), []int{1, 2, 3}) {
		t.Errorf("dropped, oldest first: %v", seqs(dropOrder(dropped)))
	}
	// the newest generation has one point: older generations are dropped, never mixed in
	kept, dropped = livePoints([]*replPoint{testPoint(1, 1, ""), testPoint(2, 1, ""), testPoint(3, 2, "")}, replKeep)
	if !reflect.DeepEqual(seqs(kept), []int{3}) || len(dropped) != 2 {
		t.Errorf("new generation: kept %v dropped %v", seqs(kept), seqs(dropped))
	}
	if k, d := livePoints(nil, 2); k != nil || d != nil {
		t.Error("no points")
	}
}

func seqs(p []*replPoint) []int {
	var out []int
	for _, x := range p {
		out = append(out, x.Seq)
	}
	return out
}

func TestKeepSetAndStrays(t *testing.T) {
	kept := []*replPoint{testPoint(5, 1, ""), testPoint(4, 1, "")}
	chains := map[string][]string{
		"p5-vda.qcow2": {"p4-vda.qcow2", "b1-vda.qcow2"},
		"p4-vda.qcow2": {"b1-vda.qcow2"},
	}
	keep := keepSet(kept, chains)
	names := []string{"b1-vda.qcow2", "p3-vda.qcow2", "p3.json", "p4-vda.qcow2", "p4.json", "p5-vda.qcow2", "p5.json", "in6-vda.qcow2", "p6-vda.qcow2", "b0-vda.qcow2"}
	got := strays(names, keep)
	want := []string{"b0-vda.qcow2", "p3-vda.qcow2", "p3.json", "in6-vda.qcow2", "p6-vda.qcow2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("strays %v, want %v", got, want)
	}
	// a half merged chain: p3 is an ancestor of the oldest kept overlay and stays until the merge ends
	chains["p4-vda.qcow2"] = []string{"p3-vda.qcow2", "b1-vda.qcow2"}
	keep = keepSet(kept, chains)
	for _, n := range strays(names, keep) {
		if n == "p3-vda.qcow2" {
			t.Error("an overlay on a kept chain would be deleted")
		}
	}
}

func TestPlanMerge(t *testing.T) {
	chain := []string{"b1-vda.qcow2", "p1-vda.qcow2", "p2-vda.qcow2", "p3-vda.qcow2"}
	st := planMerge("vda", chain, 2)
	if st == nil || st.Old != "p1-vda.qcow2" || st.Base != "b1-vda.qcow2" || st.Next != "p2-vda.qcow2" {
		t.Fatalf("step %+v", st)
	}
	// after the merge the chain is shorter; nothing more to do
	if planMerge("vda", []string{"b1-vda.qcow2", "p2-vda.qcow2", "p3-vda.qcow2"}, 2) != nil {
		t.Error("a chain at the limit is not merged")
	}
	if planMerge("vda", []string{"b1-vda.qcow2"}, 2) != nil || planMerge("vda", nil, 2) != nil {
		t.Error("nothing to merge")
	}
}

func TestStaleReplCheckpoints(t *testing.T) {
	all := []ckInfo{
		{Name: "virticle-1", Target: "replica:7", Created: 10},
		{Name: "virticle-2", Target: "n1:default", Created: 20},
		{Name: "virticle-3", Target: "replica:7", Created: 30},
		{Name: "virticle-4", Target: "replica:8", Created: 40},
		{Name: "virticle-5", Target: "replica:7", Created: 50},
	}
	got := staleReplCheckpoints(all, "replica:7", "virticle-5")
	if !reflect.DeepEqual(got, []string{"virticle-3", "virticle-1"}) {
		t.Errorf("stale replication checkpoints (newest first, only this target): %v", got)
	}
}

// The backups' own cleanup must neither count nor delete replication checkpoints, whatever else the VM holds.
func TestBackupCleanupLeavesReplicationAlone(t *testing.T) {
	all := []ckInfo{
		{Name: "virticle-r", Target: "replica:7", Created: 1}, // the oldest of all
		{Name: "virticle-a1", Target: "n1:a", Created: 10},
		{Name: "virticle-b1", Target: "n1:b", Created: 20},
		{Name: "virticle-c1", Target: "n1:c", Created: 30},
		{Name: "virticle-d1", Target: "n1:d", Created: 40},
		{Name: "virticle-e1", Target: "n1:e", Created: 50},
	}
	got := staleCheckpoints(all, "n1:e", "virticle-e1")
	for _, n := range got {
		if n == "virticle-r" {
			t.Fatalf("a backup deleted the replication checkpoint: %v", got)
		}
	}
	// four backup checkpoints are kept: the cap counts only backups, so a is dropped, not the replica's
	if !reflect.DeepEqual(got, []string{"virticle-a1"}) {
		t.Errorf("stale = %v", got)
	}
	// and the other way round: a run of replication deletes no backup checkpoint
	for _, n := range staleReplCheckpoints(all, "replica:7", "virticle-new") {
		if !strings.HasPrefix(n, "virticle-r") {
			t.Errorf("replication deleted %s", n)
		}
	}
}

func TestReplIntervalAndAge(t *testing.T) {
	for _, n := range []int{5, 15, 60, 1440, 10080} {
		if replIntervalOK(n) != nil {
			t.Errorf("%d refused", n)
		}
	}
	for _, n := range []int{-1, 0, 4, 10081} {
		if replIntervalOK(n) == nil {
			t.Errorf("%d accepted", n)
		}
	}
	if ageText(30*time.Second) != "a minute" || ageText(20*time.Minute) != "20 minutes" || ageText(5*time.Hour) != "5 hours" || ageText(72*time.Hour) != "3 days" {
		t.Error("age words")
	}
}

func TestReplVolNames(t *testing.T) {
	if replVolName("web", "vda", 0) != "web.qcow2" || replVolName("web", "vdb", 1) != "web-vdb.qcow2" {
		t.Error("volume names follow the restore's")
	}
}
