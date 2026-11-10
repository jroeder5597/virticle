package api

import (
	"errors"
	"strings"
	"testing"

	"virt-proj/internal/domxml"
)

func TestTuningChecksForMigration(t *testing.T) {
	two, _ := parseHostTopology(twoCellCaps) // CPUs 0-3, nodes 0-1, 512 2M pages per node
	one, _ := parseHostTopology(fixture(t, "capabilities_n1.xml"))

	if c := tuningChecks(domxml.Tuning{}, one, 1, "n2", nil); len(c) != 0 {
		t.Errorf("an untuned VM needs no topology checks: %v", c)
	}
	pinned := domxml.Tuning{Pins: []domxml.Pin{{VCPU: 0, CPUSet: "2-3"}}, EmulatorPin: "0"}
	c := tuningChecks(pinned, two, 1, "n2", nil)
	if c.blocked() || len(c) != 1 || c[0].Severity != "warning" {
		t.Errorf("target has the CPUs: only the warning expected, got %v", c)
	}
	c = tuningChecks(pinned, one, 1, "n2", nil)
	if !c.blocked() || !strings.Contains(c[len(c)-1].Message, "2-3") {
		t.Errorf("target lacks CPUs 2-3: want an error naming them, got %v", c)
	}

	hp := domxml.Tuning{Hugepages: true, HugepageKiB: 2048}
	if c := tuningChecks(hp, two, 96*1024, "n2", nil); c.blocked() {
		t.Errorf("2M pages exist on the target: %v", c)
	}
	// 96 MiB needs 48 pages; the one-cell lab node has none free: a warning, not an error.
	c = tuningChecks(hp, one, 96*1024, "n2", nil)
	if c.blocked() || len(c) != 2 {
		t.Errorf("shortage of pages should warn: %v", c)
	}
	if c := tuningChecks(domxml.Tuning{Hugepages: true, HugepageKiB: 16384}, two, 1024, "n2", nil); !c.blocked() {
		t.Errorf("a page size the target does not offer must block: %v", c)
	}

	strict := domxml.Tuning{NumaMode: "strict", NumaNodeset: "1"}
	if c := tuningChecks(strict, one, 1, "n2", nil); !c.blocked() {
		t.Errorf("strict policy on a missing node must block: %v", c)
	}
	pref := domxml.Tuning{NumaMode: "preferred", NumaNodeset: "1"}
	if c := tuningChecks(pref, one, 1, "n2", nil); c.blocked() {
		t.Errorf("preferred policy only warns: %v", c)
	}
	if c := tuningChecks(pinned, hostTopo{}, 1, "n2", errors.New("boom")); c.blocked() {
		t.Errorf("unreadable topology must not block: %v", c)
	}
}
