package api

import (
	"testing"
	"time"
)

func TestLimiterBackoff(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < 4; i++ {
		l.fail("k", 5)
	}
	if l.locked("k") != 0 {
		t.Fatal("locked before the threshold")
	}
	l.fail("k", 5)
	if got := l.locked("k"); got != limBaseLock {
		t.Fatalf("first lock = %v, want %v", got, limBaseLock)
	}
	// Failures while locked double the lock.
	l.fail("k", 5)
	if got := l.locked("k"); got != 2*limBaseLock {
		t.Fatalf("second lock = %v", got)
	}
	// The lock runs out.
	now = now.Add(2*limBaseLock + time.Second)
	if l.locked("k") != 0 {
		t.Fatal("still locked after the lock time")
	}
	// The next failure is longer still (the count was not forgotten).
	l.fail("k", 5)
	if got := l.locked("k"); got != 4*limBaseLock {
		t.Fatalf("third lock = %v", got)
	}
	// The cap holds.
	for i := 0; i < 30; i++ {
		l.fail("k", 5)
	}
	if got := l.locked("k"); got != limMaxLock {
		t.Fatalf("capped lock = %v, want %v", got, limMaxLock)
	}
	// Silence for the window forgets everything.
	now = now.Add(limMaxLock + limWindow + time.Second)
	l.fail("k", 5)
	if l.locked("k") != 0 {
		t.Fatal("old failures were not forgotten")
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 3; i++ {
		l.fail("a", 3)
	}
	if l.locked("a") == 0 || l.locked("b") != 0 {
		t.Fatal("keys are not independent")
	}
	if l.locked("b", "a") == 0 {
		t.Fatal("locked() must report the longest lock among its keys")
	}
	l.reset("a")
	if l.locked("a") != 0 {
		t.Fatal("reset did not clear the lock")
	}
}
