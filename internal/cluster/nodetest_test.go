package cluster

import (
	"errors"
	"testing"

	"virticle/internal/store"
)

func TestDialStage(t *testing.T) {
	for msg, want := range map[string]string{
		"jump host 1.2.3.4: ssh: handshake failed: x":                          "jump",
		"ssh h: ssh: handshake failed: knownhosts: key mismatch":               "hostkey",
		"ssh h: ssh: handshake failed: ssh: unable to authenticate, attempted": "ssh",
		"libvirt on h: boom": "libvirt",
		"ssh h: dial tcp 10.0.0.1:22: connect: connection refused": "reach",
		"timed out connecting": "reach",
	} {
		if got := dialStage(errors.New(msg)); got != want {
			t.Errorf("%q: stage %s, want %s", msg, got, want)
		}
	}
}

func steps(st *stepper) map[string]string {
	m := map[string]string{}
	for _, s := range st.steps {
		m[s.ID] = s.Status
	}
	return m
}

func TestDialFailedMarksLaterStepsNotRun(t *testing.T) {
	cfg := store.Node{Addr: "h", User: "bob"}
	st := &stepper{plan: testPlan(cfg)}
	st.add("address", StepOK, "")
	st.dialFailed("ssh", errors.New("ssh h: ssh: handshake failed: ssh: unable to authenticate"), true)
	got := steps(st)
	want := map[string]string{"address": "ok", "reach": "ok", "hostkey": "ok", "ssh": "error", "libvirt": "skipped", "privilege": "skipped", "migration": "skipped"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("step %s = %q, want %q", k, got[k], v)
		}
	}
	if len(st.steps) != len(want) || !st.failed() {
		t.Errorf("steps: %+v", st.steps)
	}

	st = &stepper{plan: testPlan(cfg)}
	st.add("address", StepOK, "")
	st.dialFailed("reach", errors.New("ssh h: dial tcp: refused"), false)
	if got := steps(st); got["reach"] != "error" || got["hostkey"] != "skipped" || got["ssh"] != "skipped" {
		t.Errorf("reach failure: %v", got)
	}

	st = &stepper{plan: testPlan(cfg)}
	st.add("address", StepOK, "")
	st.dialFailed("hostkey", errors.New("knownhosts: key mismatch"), true)
	if got := steps(st); got["reach"] != "ok" || got["hostkey"] != "error" || got["ssh"] != "skipped" {
		t.Errorf("hostkey failure: %v", got)
	}
}

func TestJumpFailure(t *testing.T) {
	cfg := store.Node{Addr: "h", User: "bob", Jump: "u@j"}
	st := &stepper{plan: testPlan(cfg)}
	st.add("address", StepOK, "")
	st.dialFailed("jump", errors.New("jump host j: dial tcp: refused"), false)
	got := steps(st)
	if got["jump"] != "error" || got["reach"] != "skipped" || len(st.steps) != 8 {
		t.Errorf("jump failure: %v (%d)", got, len(st.steps))
	}
}
