package cluster

import (
	"context"
	"errors"
	"time"
)

// Watchdog timings. A trivial libvirt call must answer within watchTimeout;
// a node that is merely slow (migration, heavy IO) still does, a connection
// whose client-side state is wedged (see third_party/go-libvirt/README.md)
// never does.
const (
	watchInterval = 30 * time.Second
	watchTimeout  = 15 * time.Second
)

// errWedged means the libvirt connection stopped answering while the
// transport looked alive.
var errWedged = errors.New("libvirt connection unresponsive")

// watch calls ping every interval until ctx ends. ping is a blocking
// libvirt round trip with no way to cancel it, so it runs in its own
// goroutine; if it has not returned after timeout, watch returns errWedged
// and abandons that goroutine (it ends when the connection is torn down, and
// only one is ever abandoned per connection because watch stops). An error
// returned by ping counts as an answer: the connection is alive, and a dead
// one is noticed through Disconnected() instead.
func watch(ctx context.Context, ping func() error, interval, timeout time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			ping()
		}()
		timer := time.NewTimer(timeout)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			return errWedged
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}
