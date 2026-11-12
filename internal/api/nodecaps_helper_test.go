package api

import (
	"strings"
	"testing"

	"virticle/internal/prepare"
)

func TestCapabilitiesByAccessMode(t *testing.T) {
	if len(nodeCapabilities("helper")) != 6 {
		t.Error("expected six node command features")
	}
	for _, p := range []string{"root", "helper", ""} {
		for k, v := range nodeCapabilities(p) {
			if !v.OK {
				t.Errorf("%q %s should be allowed", p, k)
			}
		}
	}
	for k, v := range nodeCapabilities("none") {
		if v.OK || !strings.Contains(v.Reason, "virticle node prepare") {
			t.Errorf("none %s should be off with the fix named: %+v", k, v)
		}
	}
}

func TestPrepareProgress(t *testing.T) {
	rows := []prepare.Row{
		{ID: "arch", Status: prepare.OK},
		{ID: "do-packages", Status: prepare.OK},
		{ID: "do-services", Status: prepare.Skipped},
		{ID: "do-key", Status: prepare.Skipped},
	}
	if got := progressOf(rows); got <= 0 || got >= 1 {
		t.Errorf("progress %v", got)
	}
	if progressOf(nil) != 0 {
		t.Error("no rows, no progress")
	}
}
