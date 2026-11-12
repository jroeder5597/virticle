package api

import (
	"strings"
	"testing"

	"virticle/internal/prepare"
)

func TestCapabilitiesUnderTheHelper(t *testing.T) {
	c := nodeCapabilities("helper")
	for _, k := range []string{"installTLS", "migration", "backups"} {
		if !c[k].OK {
			t.Errorf("%s goes through the helper: %+v", k, c[k])
		}
	}
	for _, k := range []string{"writePools", "diskMove", "clone"} {
		if c[k].OK || !strings.Contains(c[k].Reason, "broad sudo") {
			t.Errorf("%s under the helper: %+v", k, c[k])
		}
	}
	for _, p := range []string{"root", "sudo", ""} {
		for k, v := range nodeCapabilities(p) {
			if !v.OK {
				t.Errorf("%q %s should be allowed", p, k)
			}
		}
	}
	for k, v := range nodeCapabilities("none") {
		if v.OK {
			t.Errorf("none %s should be off", k)
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
