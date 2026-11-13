package nodecmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRateReaderRate(t *testing.T) {
	const rate = 2_000_000 // bytes per second
	const size = 1_200_000
	r := NewRateReader(context.Background(), bytes.NewReader(make([]byte, size)), rate)
	start := time.Now()
	n, err := io.Copy(io.Discard, r)
	took := time.Since(start)
	if err != nil || n != size {
		t.Fatalf("copied %d bytes, err %v", n, err)
	}
	// The bucket lets 0.1 s of data through at once: (size - rate/10) / rate seconds remain.
	want := time.Duration(float64(size-rate/10) / rate * float64(time.Second))
	if took < want*7/10 || took > want*14/10 {
		t.Errorf("took %v, expected about %v", took, want)
	}
}

func TestRateReaderFakeClock(t *testing.T) {
	// A fake clock: the sleeps add up to what the rate demands, exactly.
	var slept time.Duration
	clock := time.Unix(1000, 0)
	l := NewRateReader(context.Background(), bytes.NewReader(make([]byte, 1_000_000)), 1_000_000).(*rateReader)
	l.now = func() time.Time { return clock }
	l.last = clock
	l.sleep = func(_ context.Context, d time.Duration) error { slept += d; clock = clock.Add(d); return nil }
	if _, err := io.Copy(io.Discard, l); err != nil {
		t.Fatal(err)
	}
	// 1 MB at 1 MB/s, minus the first burst of 0.1 s.
	if slept < 890*time.Millisecond || slept > 910*time.Millisecond {
		t.Errorf("slept %v, want 0.9 s", slept)
	}
}

func TestRateReaderZeroIsUnlimited(t *testing.T) {
	src := bytes.NewReader(make([]byte, 10))
	if got := NewRateReader(context.Background(), src, 0); got != io.Reader(src) {
		t.Fatalf("rate 0 must return the reader itself, got %T", got)
	}
	r := NewRateReader(context.Background(), bytes.NewReader(make([]byte, 50<<20)), 0)
	start := time.Now()
	io.Copy(io.Discard, r)
	if time.Since(start) > 2*time.Second {
		t.Error("an unlimited reader was slow")
	}
}

func TestRateReaderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRateReader(ctx, bytes.NewReader(make([]byte, 10<<20)), 100_000) // would take 100 s
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := io.Copy(io.Discard, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("cancel took %v", time.Since(start))
	}
}
