package api

import (
	"testing"
)

func metaOf(t *testing.T, kids map[string]string) string {
	t.Helper()
	m, err := setMetaChildren("", kids)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The table covers the situations of a failover with the old node back: which VM is powered off, which is left.
func TestDecideOldCopy(t *testing.T) {
	taken := failedOverMeta{FromNode: 1, FromName: "n1", At: 2000, Gen: 1}
	repl := replConfig{Enabled: true, TargetNode: 2, TargetName: "n2", Pool: "p", EveryMinutes: 15}.element()
	cases := []struct {
		name string
		fo   failedOverMeta
		old  oldVMFacts
		want oldCopyAction
	}{
		{"old node back, VM still running with its replication: power off", taken,
			oldVMFacts{Meta: metaOf(t, map[string]string{replChild: repl}), Running: true}, oldPowerOff},
		{"old node back, VM shut off: only marked", taken,
			oldVMFacts{Meta: metaOf(t, map[string]string{replChild: repl}), Running: false}, oldMark},
		{"old VM has no metadata at all (replication was removed there): still the old copy", taken,
			oldVMFacts{Meta: "", Running: true}, oldPowerOff},
		{"already marked: nothing, even when running (the guard and the next check deal with it)", taken,
			oldVMFacts{Meta: metaOf(t, map[string]string{staleChild: staleMeta{ToNode: 2, At: 5}.element()}), Running: true}, oldNothing},
		{"it is the replica of the running VM now: never touched", taken,
			oldVMFacts{Meta: metaOf(t, map[string]string{replicaChild: replicaMeta{Dir: "/d", SourceNode: 2}.element()}), Running: false}, oldNothing},
		// Failback: the old VM took over later than this record (higher generation): it is the live one.
		{"the other VM took over later: this record is the out-of-date one", taken,
			oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: failedOverMeta{FromNode: 2, Gen: 2, At: 3000}.element()}), Running: true}, oldNothing},
		{"the other VM took over earlier: it is the old copy", failedOverMeta{FromNode: 1, At: 5000, Gen: 3},
			oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: failedOverMeta{FromNode: 2, Gen: 2, At: 9000}.element()}), Running: true}, oldPowerOff},
		// Clocks: the generation decides, never the time. The record with the larger At has the smaller generation.
		{"a clock that runs ahead does not win over the generation", failedOverMeta{FromNode: 1, At: 100, Gen: 4},
			oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: failedOverMeta{FromNode: 2, Gen: 3, At: 99999999}.element()}), Running: true}, oldPowerOff},
		{"a clock that runs behind does not lose against the generation", failedOverMeta{FromNode: 1, At: 99999999, Gen: 2},
			oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: failedOverMeta{FromNode: 2, Gen: 3, At: 100}.element()}), Running: true}, oldNothing},
		{"equal generations cannot be told apart: nothing is powered off", failedOverMeta{FromNode: 1, At: 100, Gen: 2},
			oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: failedOverMeta{FromNode: 2, Gen: 2, At: 50}.element()}), Running: true}, oldNothing},
		{"records from before generations existed (both 0): nothing", failedOverMeta{FromNode: 1, At: 100},
			oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: failedOverMeta{FromNode: 2, At: 50}.element()}), Running: true}, oldNothing},
		{"damaged metadata of the old VM counts as no metadata, not as a reason to skip", taken,
			oldVMFacts{Meta: "<broken", Running: true}, oldPowerOff},
	}
	for _, c := range cases {
		if got := decideOldCopy(c.fo, c.old); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// Whichever of the two VMs is looked at first, at most one of them is ever the one to power off.
func TestDecideOldCopyNeverBothWays(t *testing.T) {
	for _, g := range [][2]int{{0, 0}, {1, 2}, {2, 1}, {3, 3}, {5, 0}, {0, 5}} {
		a := failedOverMeta{FromNode: 2, Gen: g[0], At: 10}
		b := failedOverMeta{FromNode: 1, Gen: g[1], At: 20}
		offA := decideOldCopy(a, oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: b.element()}), Running: true}) == oldPowerOff
		offB := decideOldCopy(b, oldVMFacts{Meta: metaOf(t, map[string]string{failedOverChd: a.element()}), Running: true}) == oldPowerOff
		if offA && offB {
			t.Errorf("generations %v: both VMs would be powered off", g)
		}
	}
}

func TestGenerationRoundTrip(t *testing.T) {
	fo := failedOverMeta{FromNode: 1, FromName: "n1", At: 7, Planned: true, Gen: 4}
	if g := parseFailedOver(metaOf(t, map[string]string{failedOverChd: fo.element()})); g == nil || *g != fo {
		t.Errorf("failed-over: %+v", g)
	}
	rm := replicaMeta{Dir: "/d", SourceNode: 1, SrcGen: 3}
	if g := parseReplicaMeta(metaOf(t, map[string]string{replicaChild: rm.element()})); g == nil || g.SrcGen != 3 {
		t.Errorf("replica: %+v", g)
	}
}
