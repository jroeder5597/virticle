package api

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeFreezer struct {
	freezeErr error
	thawErrs  []error // one per Thaw call; the last repeats
	events    []string
}

func (f *fakeFreezer) Freeze() error {
	f.events = append(f.events, "freeze")
	return f.freezeErr
}

func (f *fakeFreezer) Thaw() error {
	n := 0
	for _, e := range f.events {
		if e == "thaw" {
			n++
		}
	}
	f.events = append(f.events, "thaw")
	if len(f.thawErrs) == 0 {
		return nil
	}
	if n >= len(f.thawErrs) {
		n = len(f.thawErrs) - 1
	}
	return f.thawErrs[n]
}

func boolp(b bool) *bool { return &b }

func TestWithFrozenGuest(t *testing.T) {
	thawPause = time.Millisecond
	boom := errors.New("boom")
	cases := []struct {
		name       string
		quiesce    *bool
		agent      bool
		freezeErr  error
		beginErr   error
		wantEvents string // freeze/begin/thaw order
		wantFrozen bool
		wantText   string
		wantErr    string // "" none, "freeze" a *freezeFail, "begin" the begin error
	}{
		{"auto, agent, freeze ok", nil, true, nil, nil, "freeze,begin,thaw", true, "guest file systems frozen", ""},
		{"auto, agent, freeze fails", nil, true, boom, nil, "freeze,thaw,begin", false, "not frozen: boom", ""},
		{"auto, agent, freeze never sent", nil, true, &freezeNotTried{boom}, nil, "freeze,begin", false, "not frozen: boom", ""},
		{"auto, no agent", nil, false, nil, nil, "begin", false, "not frozen: no guest agent", ""},
		{"true, agent, freeze ok", boolp(true), true, nil, nil, "freeze,begin,thaw", true, "guest file systems frozen", ""},
		{"true, agent, freeze fails", boolp(true), true, boom, nil, "freeze,thaw", false, "not frozen: boom", "freeze"},
		{"true, agent, freeze never sent", boolp(true), true, &freezeNotTried{boom}, nil, "freeze", false, "not frozen: boom", "freeze"},
		{"true, no agent", boolp(true), false, nil, nil, "", false, "", "freeze"},
		{"false, agent", boolp(false), true, nil, nil, "begin", false, "not frozen: turned off for this backup", ""},
		{"false, no agent", boolp(false), false, nil, nil, "begin", false, "not frozen: turned off for this backup", ""},
		{"auto, freeze ok, begin fails", nil, true, nil, boom, "freeze,begin,thaw", true, "guest file systems frozen", "begin"},
		{"true, freeze ok, begin fails", boolp(true), true, nil, boom, "freeze,begin,thaw", true, "guest file systems frozen", "begin"},
		{"auto, freeze fails, begin fails", nil, true, boom, boom, "freeze,thaw,begin", false, "not frozen: boom", "begin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFreezer{freezeErr: c.freezeErr}
			begin := func() error {
				f.events = append(f.events, "begin")
				return c.beginErr
			}
			out, err := withFrozenGuest(c.quiesce, c.agent, f, begin)
			if got := strings.Join(f.events, ","); got != c.wantEvents {
				t.Errorf("events = %q, want %q", got, c.wantEvents)
			}
			if out.Frozen != c.wantFrozen || out.Text != c.wantText {
				t.Errorf("outcome = %+v, want frozen=%v text=%q", out, c.wantFrozen, c.wantText)
			}
			if out.ThawErr != nil {
				t.Errorf("unexpected ThawErr %v", out.ThawErr)
			}
			var ff *freezeFail
			switch c.wantErr {
			case "":
				if err != nil {
					t.Errorf("err = %v", err)
				}
			case "freeze":
				if !errors.As(err, &ff) {
					t.Errorf("err = %v, want a freeze failure", err)
				} else if strings.Contains(err.Error(), "\n") || err.Error() == "" {
					t.Errorf("unclear message %q", err)
				}
			case "begin":
				if !errors.Is(err, c.beginErr) || errors.As(err, &ff) {
					t.Errorf("err = %v, want the begin error", err)
				}
			}
			// A freeze that went through is always followed by a thaw, and the thaw comes after begin.
			froze := false
			for i, e := range f.events {
				if e == "freeze" && c.freezeErr == nil {
					froze = true
				}
				if e == "begin" && froze {
					rest := strings.Join(f.events[i:], ",")
					if !strings.HasSuffix(rest, "thaw") {
						t.Errorf("no thaw after begin: %q", rest)
					}
				}
			}
		})
	}
}

func TestThawRetriesAndReportsFailure(t *testing.T) {
	thawPause = time.Millisecond
	bad := errors.New("agent busy")
	// Two failures, then success: the guest ends thawed, no problem reported.
	f := &fakeFreezer{thawErrs: []error{bad, bad, nil}}
	out, err := withFrozenGuest(nil, true, f, func() error { f.events = append(f.events, "begin"); return nil })
	if err != nil || out.ThawErr != nil {
		t.Fatalf("err=%v thawErr=%v", err, out.ThawErr)
	}
	if got := strings.Join(f.events, ","); got != "freeze,begin,thaw,thaw,thaw" {
		t.Errorf("events = %q", got)
	}
	// Every try fails: the backup result stands, the outcome says the guest may be frozen.
	f = &fakeFreezer{thawErrs: []error{bad}}
	out, err = withFrozenGuest(nil, true, f, func() error { return nil })
	if err != nil || out.ThawErr == nil {
		t.Fatalf("err=%v thawErr=%v", err, out.ThawErr)
	}
	if n := strings.Count(strings.Join(f.events, ","), "thaw"); n != thawTries {
		t.Errorf("%d thaw tries, want %d", n, thawTries)
	}
}

func TestFinishFreeze(t *testing.T) {
	boom := errors.New("boom")
	var nilRun *bkRun
	if err := nilRun.finishFreeze(boom); err != boom {
		t.Errorf("nil run changed the error: %v", err)
	}
	m := &bkRun{name: "vm1", fz: freezeOutcome{Frozen: true, Text: frozenText}}
	if err := m.finishFreeze(nil); err != doneNote(frozenText) {
		t.Errorf("good run: %v", err)
	}
	if err := m.finishFreeze(boom); err != boom {
		t.Errorf("failed run without thaw problem: %v", err)
	}
	if err := (&bkRun{}).finishFreeze(nil); err != nil {
		t.Errorf("offline run: %v", err)
	}
	m.thawErr = errors.New("no answer")
	if err := m.finishFreeze(nil); err == nil || !strings.Contains(err.Error(), "virsh domfsthaw vm1") {
		t.Errorf("a guest left frozen must be an error that says what to do: %v", err)
	}
	if err := m.finishFreeze(boom); err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "may be frozen") {
		t.Errorf("failed run with thaw problem: %v", err)
	}
}

func TestWithin(t *testing.T) {
	if err := within(time.Second, "late", func() error { return nil }); err != nil {
		t.Errorf("fast call: %v", err)
	}
	boom := errors.New("boom")
	if err := within(time.Second, "late", func() error { return boom }); err != boom {
		t.Errorf("failing call: %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	err := within(20*time.Millisecond, "late", func() error { <-release; return nil })
	if err == nil || err.Error() != "late" || time.Since(start) > time.Second {
		t.Errorf("hung call: %v after %v", err, time.Since(start))
	}
}
