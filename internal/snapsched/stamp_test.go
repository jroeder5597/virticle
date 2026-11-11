package snapsched

import (
	"reflect"
	"testing"
	"time"
)

func TestStampAndForeign(t *testing.T) {
	if Controller(Marker) != "" || Controller("user text") != "" || Controller(Stamp("")) != "" {
		t.Error("bare marker has no controller")
	}
	if got := Controller(Stamp("ab12cd34")); got != "ab12cd34" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{Marker + " [controller ]", Marker + " [controller a b]", Marker + " [controller x]y", Marker + " [controller x"} {
		if Controller(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
	mk := func(name, desc, at string) Snap { return Snap{name, desc, ts(at)} }
	snaps := []Snap{
		mk("auto-20260101-100000", Marker, "2026-01-01 10:00"),         // old format, nobody
		mk("auto-20260101-110000", Stamp("aaa"), "2026-01-01 11:00"),   // self
		mk("auto-20260101-114000", Stamp("bbb"), "2026-01-01 11:40"),   // other
		mk("auto-20260101-115000", Stamp("bbb"), "2026-01-01 11:50"),   // other, newest
		mk("other-20260101-115500", Stamp("ccc"), "2026-01-01 11:55"),  // other prefix
		mk("auto-20260101-115900", "user text", "2026-01-01 11:59"),    // not scheduled
		mk("auto-20260101-120000-x", Stamp("ddd"), "2026-01-01 12:00"), // name does not match
	}
	if got := Foreign(snaps, "auto", "aaa", ts("2026-01-01 11:00")); got != "bbb" {
		t.Errorf("got %q", got)
	}
	if got := Foreign(snaps, "auto", "bbb", ts("2026-01-01 11:00")); got != "aaa" {
		t.Errorf("got %q", got)
	}
	if got := Foreign(snaps, "auto", "aaa", ts("2026-01-01 11:55")); got != "" {
		t.Errorf("old foreign snapshots must not count: %q", got)
	}
	// pruning treats stamped and unstamped scheduled snapshots alike
	got := Prune(snaps, "auto", 2)
	if !reflect.DeepEqual(got, []string{"auto-20260101-100000", "auto-20260101-110000"}) {
		t.Errorf("prune: %v", got)
	}
	if !Ours(snaps[0], "auto") || !Ours(snaps[1], "auto") || Ours(snaps[5], "auto") {
		t.Error("Ours")
	}
	if w := (Schedule{Mode: "interval", EveryMinutes: 1}).ForeignWindow(); w != 10*time.Minute {
		t.Error(w)
	}
}

// Snapshots described by the version before the rename are still ours.
func TestLegacyMarker(t *testing.T) {
	const old = "Scheduled snapshot (virt-proj)"
	if Controller(old+" [controller 1a2b3c4d]") != "1a2b3c4d" || Controller(old) != "" {
		t.Error("legacy stamp")
	}
	if !Ours(Snap{"auto-20260101-000000", old, time.Time{}}, "auto") || !Ours(Snap{"auto-20260101-000000", old + " [controller x]", time.Time{}}, "auto") {
		t.Error("legacy scheduled snapshot not recognised")
	}
	if Stamp("x") != "Scheduled snapshot (virticle) [controller x]" {
		t.Errorf("new stamp %q", Stamp("x"))
	}
}
