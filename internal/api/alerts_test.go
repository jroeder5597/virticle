package api

import (
	"testing"
	"time"

	"virticle/internal/store"
)

func ids(a []Alert) map[string]string {
	m := map[string]string{}
	for _, x := range a {
		m[x.ID] = x.Severity
	}
	return m
}

func TestNodeDownNeedsTwoMinutes(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	// A fake status source: the node flips to offline and stays there.
	status := func(online bool) alertInput {
		return alertInput{Nodes: []alertNode{{ID: 7, Name: "n7", Online: online}}, Fresh: map[int64]bool{}}
	}
	if r, _, all := tr.step(status(true), t0); len(r)+len(all) != 0 {
		t.Fatalf("online node raised %v", all)
	}
	if _, _, all := tr.step(status(false), t0.Add(10*time.Second)); len(all) != 0 {
		t.Fatalf("offline for 0s must not alert: %v", all)
	}
	if _, _, all := tr.step(status(false), t0.Add(2*time.Minute)); len(all) != 0 {
		t.Fatalf("offline for 1m50s must not alert: %v", all)
	}
	raised, _, all := tr.step(status(false), t0.Add(2*time.Minute+30*time.Second))
	if len(raised) != 1 || raised[0].ID != "node-down:7" || raised[0].Severity != "critical" {
		t.Fatalf("raised %v", raised)
	}
	if all[0].Since != t0.Add(2*time.Minute+30*time.Second).Unix() {
		t.Errorf("since should be when the alert was raised")
	}
	// Still down: no repeat.
	if r, c, _ := tr.step(status(false), t0.Add(5*time.Minute)); len(r)+len(c) != 0 {
		t.Fatalf("repeat notification: %v %v", r, c)
	}
	// Back: cleared once.
	_, cleared, all := tr.step(status(true), t0.Add(6*time.Minute))
	if len(cleared) != 1 || len(all) != 0 {
		t.Fatalf("cleared=%v all=%v", cleared, all)
	}
}

func TestNodeDownSuppressedInMaintenance(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	t0 := time.Now()
	in := alertInput{Nodes: []alertNode{{ID: 1, Name: "n1", Online: false, Maintenance: true}}}
	tr.step(in, t0)
	if _, _, all := tr.step(in, t0.Add(10*time.Minute)); len(all) != 0 {
		t.Fatalf("maintenance node alerted: %v", all)
	}
}

func TestPoolThresholds(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Now()
	pool := func(pct uint64) alertInput {
		return alertInput{Fresh: map[int64]bool{1: true}, Pools: []alertPool{{Node: 1, NodeName: "n1", Name: "wk-pool", Capacity: 100, Used: pct}}}
	}
	if _, _, all := tr.step(pool(84), now); len(all) != 0 {
		t.Fatal("84% must not alert")
	}
	r, _, all := tr.step(pool(85), now)
	if len(r) != 1 || all[0].Severity != "warning" {
		t.Fatalf("85%%: %v", all)
	}
	r, _, all = tr.step(pool(95), now)
	if len(r) != 1 || all[0].Severity != "critical" {
		t.Fatalf("95%% must re-raise as critical: %v", all)
	}
	if r, c, _ := tr.step(pool(96), now); len(r)+len(c) != 0 {
		t.Fatal("no change, no notification")
	}
	_, c, all := tr.step(pool(10), now)
	if len(c) != 1 || len(all) != 0 {
		t.Fatalf("cleared=%v", c)
	}
}

func TestAlertsOfUnreadNodeAreKept(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Now()
	in := alertInput{Fresh: map[int64]bool{1: true}, Pools: []alertPool{{Node: 1, NodeName: "n1", Name: "p", Capacity: 100, Used: 99}}}
	tr.step(in, now)
	// The node cannot be read this time: nothing is cleared.
	r, c, all := tr.step(alertInput{Fresh: map[int64]bool{}}, now.Add(time.Minute))
	if len(r)+len(c) != 0 || len(all) != 1 {
		t.Fatalf("alert lost while node was unreadable: %v %v %v", r, c, all)
	}
}

func TestFailedJobClearsOnLaterSuccess(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Now()
	fail := store.Task{ID: 1, Kind: "snapshot create", Target: "vm1", Node: "n1", User: "scheduler", Status: "failed", Error: "no guest agent", Finished: now.Unix()}
	ok := store.Task{ID: 2, Kind: "snapshot create", Target: "vm1", Node: "n1", User: "scheduler", Status: "ok"}
	other := store.Task{ID: 3, Kind: "snapshot create", Target: "vm2", Node: "n1", User: "scheduler", Status: "ok"}
	r, _, all := tr.step(alertInput{Jobs: []store.Task{fail, other}}, now)
	if len(r) != 1 || all[0].Kind != "failed-job" {
		t.Fatalf("%v", all)
	}
	_, c, all := tr.step(alertInput{Jobs: []store.Task{fail, ok, other}}, now)
	if len(c) != 1 || len(all) != 0 {
		t.Fatalf("a later success must clear: %v %v", c, all)
	}
}

func TestCertExpiryAndVMState(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	_, _, all := tr.step(alertInput{CertExpiry: now.Add(40 * 24 * time.Hour)}, now)
	if len(all) != 0 {
		t.Fatalf("40 days left must not alert: %v", all)
	}
	_, _, all = tr.step(alertInput{CertExpiry: now.Add(10 * 24 * time.Hour), VMs: []alertVM{{Node: 1, NodeName: "n1", UUID: "u", Name: "v", Why: "has crashed"}}}, now)
	m := ids(all)
	if m["tls-expiry"] != "warning" || m["vm:1:u"] != "critical" {
		t.Fatalf("%v", m)
	}
	_, _, all = tr.step(alertInput{CertExpiry: now.Add(-time.Hour)}, now)
	if ids(all)["tls-expiry"] != "critical" {
		t.Fatalf("expired: %v", all)
	}
}

func TestBadVMState(t *testing.T) {
	if badVMState(6, 0) == "" {
		t.Error("crashed")
	}
	if badVMState(3, 5) == "" {
		t.Error("paused with I/O error")
	}
	if badVMState(3, 1) != "" || badVMState(1, 0) != "" || badVMState(5, 0) != "" {
		t.Error("ordinary pause / running / shut off must not alert")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:9000/hook", "https://example.org/x"} {
		if err := ValidateWebhookURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ftp://x", "file:///etc/passwd", "example.org", "http://"} {
		if ValidateWebhookURL(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
