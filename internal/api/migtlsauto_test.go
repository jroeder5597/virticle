package api

import (
	"errors"
	"testing"
	"time"

	"virticle/internal/migpki"
)

// fakeEnsurer stands in for the nodes: it counts installs and can fail or be opted out.
type fakeEnsurer struct {
	calls int
	err   error
	off   map[int64]bool
}

func (f *fakeEnsurer) Ensure(id int64) error  { f.calls++; return f.err }
func (f *fakeEnsurer) OptedOut(id int64) bool { return f.off[id] }

func newTestAuto(f *fakeEnsurer, clock *time.Time) (*migTLSAuto, *[]int64) {
	var changed []int64
	a := newMigTLSAuto(f, func(id int64) { changed = append(changed, id) })
	a.now = func() time.Time { return *clock }
	return a, &changed
}

func TestMigAutoRateLimit(t *testing.T) {
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeEnsurer{}
	a, _ := newTestAuto(f, &clock)
	if !a.Try(1) || f.calls != 1 {
		t.Fatal("the first trigger must run")
	}
	// A reconnect a minute later does not run it again, even though it worked.
	clock = clock.Add(time.Minute)
	if a.Try(1) || f.calls != 1 {
		t.Fatal("a second trigger within the interval must not run")
	}
	// Another node has its own interval.
	if !a.Try(2) || f.calls != 2 {
		t.Fatal("another node must run")
	}
	clock = clock.Add(migAutoRetry)
	if !a.Try(1) || f.calls != 3 {
		t.Fatal("after the interval it runs again")
	}
}

func TestMigAutoFailureIsRecordedAndRetriedLater(t *testing.T) {
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeEnsurer{err: errors.New("disk full")}
	a, changed := newTestAuto(f, &clock)
	a.Try(1)
	if a.Error(1) != "disk full" || len(*changed) != 1 {
		t.Fatalf("error %q, changed %v", a.Error(1), *changed)
	}
	clock = clock.Add(time.Minute)
	if a.Try(1) || f.calls != 1 {
		t.Fatal("a failure must not be retried at once")
	}
	clock = clock.Add(migAutoRetry)
	f.err = nil
	a.Try(1)
	if a.Error(1) != "" || f.calls != 2 || len(*changed) != 2 {
		t.Fatalf("error %q, calls %d, changed %v", a.Error(1), f.calls, *changed)
	}
}

func TestMigAutoSkipsAndReset(t *testing.T) {
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeEnsurer{err: errMigAutoSkip}
	a, changed := newTestAuto(f, &clock)
	if a.Try(1) || a.Error(1) != "" || len(*changed) != 0 {
		t.Fatal("a node that cannot take part is not an attempt and shows nothing")
	}
	// Not tried, so it does not use up the interval: once the node is prepared, the next trigger runs.
	f.err = nil
	if !a.Try(1) {
		t.Fatal("must run as soon as the node can")
	}
	clock = clock.Add(time.Minute)
	a.Reset(1)
	if !a.Try(1) {
		t.Fatal("Reset lets the next trigger run at once")
	}
}

func TestMigAutoRemovedByHandStaysRemoved(t *testing.T) {
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeEnsurer{off: map[int64]bool{1: true}}
	a, _ := newTestAuto(f, &clock)
	for i := 0; i < 3; i++ {
		clock = clock.Add(2 * migAutoRetry)
		if a.Try(1) {
			t.Fatal("a node removed by hand must not be set up again")
		}
	}
	if f.calls != 0 {
		t.Fatalf("%d installs on an opted-out node", f.calls)
	}
	// Fix clears the flag; the node is looked after again.
	f.off[1] = false
	if !a.Try(1) || f.calls != 1 {
		t.Fatal("after Fix the node is looked after again")
	}
}

func TestAddressOnlyMismatch(t *testing.T) {
	cases := []struct {
		st   migpki.Status
		want bool
	}{
		{migpki.Status{State: migpki.StateMismatch, Problems: []string{"the certificate does not cover 10.0.0.2"}}, true},
		{migpki.Status{State: migpki.StateMismatch, Problems: []string{"the certificate does not cover x", "ca-cert.pem differs"}}, false},
		{migpki.Status{State: migpki.StateMismatch}, false},
		{migpki.Status{State: migpki.StateOK, Problems: []string{"does not cover"}}, false},
	}
	for i, c := range cases {
		if got := addressOnlyMismatch(c.st); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}
