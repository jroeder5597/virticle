package api

import "testing"

// The start answer must not claim a kind: libvirt may still refuse an incremental backup.
func TestBackupStartAnswerSaysPlanned(t *testing.T) {
	a := backupStartAnswer(7, "live", backupPlan{Kind: kindIncremental, Reason: "x"})
	if _, ok := a["kind"]; ok {
		t.Fatal("the start answer must not have a field kind")
	}
	if a["plannedKind"] != kindIncremental || a["plannedReason"] != "x" || a["task"] != int64(7) {
		t.Fatalf("answer %v", a)
	}
}
