package cluster

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchHealthyAndSlowPingsAreNotWedged(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		res <- watch(ctx, func() error {
			calls.Add(1)
			time.Sleep(20 * time.Millisecond) // slow but within the timeout
			if calls.Load() == 2 {
				return errors.New("libvirt error is still an answer")
			}
			return nil
		}, 10*time.Millisecond, 500*time.Millisecond)
	}()
	time.Sleep(250 * time.Millisecond)
	cancel()
	if err := <-res; err != nil {
		t.Fatalf("got %v, want nil", err)
	}
	if calls.Load() < 3 {
		t.Fatalf("only %d pings", calls.Load())
	}
}

func TestWatchDetectsWedge(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	err := watch(context.Background(), func() error { <-block; return nil }, 10*time.Millisecond, 50*time.Millisecond)
	if !errors.Is(err, errWedged) {
		t.Fatalf("got %v, want errWedged", err)
	}
	if d := time.Since(start); d < 55*time.Millisecond || d > time.Second {
		t.Fatalf("detected after %v", d)
	}
}

func TestWatchWedgeAfterHealthyPings(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var n atomic.Int32
	err := watch(context.Background(), func() error {
		if n.Add(1) > 3 {
			<-block
		}
		return nil
	}, 5*time.Millisecond, 40*time.Millisecond)
	if !errors.Is(err, errWedged) || n.Load() != 4 {
		t.Fatalf("err=%v pings=%d", err, n.Load())
	}
}

func TestWatchStopsWithoutLeakingGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		res := make(chan error, 1)
		go func() { res <- watch(ctx, func() error { return nil }, time.Millisecond, time.Second) }()
		time.Sleep(10 * time.Millisecond)
		cancel()
		<-res
	}
	// also cancel while a ping is in flight
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	res := make(chan error, 1)
	go func() {
		res <- watch(ctx, func() error { <-release; return nil }, time.Millisecond, time.Minute)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

// A proxy daemon that lost its connection to one driver daemon answers every call to that driver with
// "client socket is closed"; the connection must be replaced, an ordinary error must not.
func TestWatchClosedDriverSocketIsWedged(t *testing.T) {
	err := watch(context.Background(), func() error { return errors.New("internal error: client socket is closed") }, 5*time.Millisecond, time.Second)
	if !errors.Is(err, errWedged) {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := watch(ctx, func() error { return errors.New("some other error") }, 5*time.Millisecond, time.Second); err != nil {
		t.Fatalf("an ordinary error is an answer: %v", err)
	}
}
