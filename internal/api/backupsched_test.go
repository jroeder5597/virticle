package api

import (
	"strings"
	"testing"
	"time"
)

func TestBackupScheduleRoundTrip(t *testing.T) {
	for _, b := range []backupSchedule{
		{Enabled: true, Mode: "interval", EveryMinutes: 1, TargetNode: 2, Pool: "wm-pool", KeepLast: 2},
		{Enabled: false, Mode: "daily", At: "03:30", Pool: "default", KeepLast: 7},
	} {
		if err := b.validate(); err != nil {
			t.Fatal(err)
		}
		// next to another feature's child, as libvirt hands it back (vp: prefix)
		meta, err := metaSetChild(`<vp:meta xmlns:vp="x"><vp:tags><vp:tag>a</vp:tag></vp:tags></vp:meta>`, backupSchedChild, b.element())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(meta, "<tags>") {
			t.Errorf("other child lost: %s", meta)
		}
		got, err := parseBackupSchedule(meta)
		if err != nil || got == nil || *got != b {
			t.Errorf("round trip: %+v %v want %+v", got, err, b)
		}
		gone, _ := metaSetChild(meta, backupSchedChild, "")
		if g, _ := parseBackupSchedule(gone); g != nil || !strings.Contains(gone, "<tags>") {
			t.Errorf("removal: %s", gone)
		}
	}
	if b, err := parseBackupSchedule(""); b != nil || err != nil {
		t.Error("empty metadata means no schedule")
	}
	if b, err := parseBackupSchedule("<meta><template sealed=\"linux\"/></meta>"); b != nil || err != nil {
		t.Error("other children mean no schedule")
	}
}

func TestBackupScheduleValidate(t *testing.T) {
	ok := backupSchedule{Enabled: true, Mode: "interval", EveryMinutes: 60, Pool: "p", KeepLast: 3}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*backupSchedule){
		"mode":      func(b *backupSchedule) { b.Mode = "weekly" },
		"interval":  func(b *backupSchedule) { b.EveryMinutes = 0 },
		"daily":     func(b *backupSchedule) { b.Mode, b.At = "daily", "25:00" },
		"pool":      func(b *backupSchedule) { b.Pool = "a b" },
		"emptypool": func(b *backupSchedule) { b.Pool = "" },
		"keep0":     func(b *backupSchedule) { b.KeepLast = 0 },
		"keepbig":   func(b *backupSchedule) { b.KeepLast = 1001 },
		"node":      func(b *backupSchedule) { b.TargetNode = -1 },
	} {
		b := ok
		mut(&b)
		if b.validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestBackupScheduleDueAndLast(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	all := []retained{
		{Key: "a", UUID: "u1", Created: t0.Add(-3 * time.Hour)},
		{Key: "b", UUID: "u1", Created: t0.Add(-90 * time.Second)},
		{Key: "c", UUID: "u2", Created: t0.Add(-time.Second)}, // another VM
	}
	last := newestBackup(all, "u1")
	if !last.Equal(t0.Add(-90 * time.Second)) {
		t.Fatalf("last %v", last)
	}
	if !newestBackup(all, "none").IsZero() {
		t.Error("no backups means zero")
	}
	b := backupSchedule{Enabled: true, Mode: "interval", EveryMinutes: 1, Pool: "p", KeepLast: 2}
	if !b.due(t0, last) {
		t.Error("90s after the last backup, a 1-minute schedule is due")
	}
	if b.due(t0, t0.Add(-30*time.Second)) {
		t.Error("30s after: not due")
	}
	if !b.due(t0, time.Time{}) {
		t.Error("never backed up: due")
	}
	b.Enabled = false
	if b.due(t0, time.Time{}) {
		t.Error("disabled")
	}
	d := backupSchedule{Enabled: true, Mode: "daily", At: "03:00", Pool: "p", KeepLast: 2}
	if d.due(time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)) {
		t.Error("already done today")
	}
	// retention: with keepLast 2 the oldest goes
	pr := pruneChains(append(all, retained{Key: "d", UUID: "u1", Created: t0}), "u1", 2, "")
	if len(pr) != 1 || pr[0] != "a" {
		t.Errorf("prune %v", pr)
	}
}

func TestConflictAlert(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Now()
	in := alertInput{Fresh: map[int64]bool{}, Conflicts: []schedConflict{{Node: 4, NodeName: "n4", Other: "abc123", VMs: []string{"v1", "v2"}}}}
	raised, _, all := tr.step(in, now)
	if len(raised) != 1 || all[0].Kind != "scheduler-conflict" || !strings.Contains(all[0].Title, "Another controller is running schedules on node n4") ||
		!strings.Contains(all[0].Message, "abc123") {
		t.Fatalf("%+v", all)
	}
	_, cleared, all := tr.step(alertInput{Fresh: map[int64]bool{4: true}}, now.Add(time.Minute))
	if len(cleared) != 1 || len(all) != 0 {
		t.Errorf("conflict not cleared: %v", all)
	}
}

func TestSetConflictsKeepsUnscannedNodes(t *testing.T) {
	s := &Server{}
	s.setConflicts(map[int64]*schedConflict{1: {Node: 1, Other: "x"}, 2: {Node: 2, Other: "y"}}, map[int64]bool{1: true, 2: true})
	s.setConflicts(map[int64]*schedConflict{}, map[int64]bool{1: true}) // node 2 unreachable this pass
	got := s.currentConflicts()
	if len(got) != 1 || got[0].Node != 2 {
		t.Errorf("%+v", got)
	}
}
