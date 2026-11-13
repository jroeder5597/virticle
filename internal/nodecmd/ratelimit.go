package nodecmd

import (
	"context"
	"io"
	"time"
)

// rateReader passes the bytes of r on at no more than rate bytes per second (a token bucket). A rate of 0
// is no limit: NewRateReader then returns r itself. The bucket holds a tenth of a second of data (at least
// 32 KiB), so the transfer starts at once and the average stays at the rate. Reading ends with ctx.Err()
// when ctx is cancelled while the reader waits for tokens.
type rateReader struct {
	r      io.Reader
	ctx    context.Context
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
}

// NewRateReader limits r to rate bytes per second; 0 means unlimited.
func NewRateReader(ctx context.Context, r io.Reader, rate uint64) io.Reader {
	if rate == 0 {
		return r
	}
	burst := float64(rate) / 10
	if burst < 32<<10 {
		burst = 32 << 10
	}
	return &rateReader{r: r, ctx: ctx, rate: float64(rate), burst: burst, tokens: burst, last: time.Now(), now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (l *rateReader) Read(p []byte) (int, error) {
	if err := l.ctx.Err(); err != nil {
		return 0, err
	}
	if float64(len(p)) > l.burst {
		p = p[:int(l.burst)]
	}
	n, err := l.r.Read(p)
	if n > 0 {
		if werr := l.take(float64(n)); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// take spends n tokens, waiting until the bucket has enough (it may go negative: the wait pays the debt).
func (l *rateReader) take(n float64) error {
	now := l.now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	l.tokens -= n
	if l.tokens >= 0 {
		return nil
	}
	wait := time.Duration(-l.tokens / l.rate * float64(time.Second))
	if err := l.sleep(l.ctx, wait); err != nil {
		return err
	}
	l.tokens, l.last = 0, l.now()
	return nil
}
