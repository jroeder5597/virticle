package cluster

import (
	"context"
	"errors"
	"strings"
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
// one is noticed through Disconnected() instead. One error is not an answer:
// "client socket is closed" comes from the node's libvirt proxy daemon, whose
// own connection to one of the driver daemons broke (it happened when the
// storage daemon had a call stuck on an NFS mount whose server was gone). The
// controller's socket to the proxy stays open and every call to that driver
// keeps failing, so Disconnected() never fires; only a new connection helps.
func watch(ctx context.Context, ping func() error, interval, timeout time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		done := make(chan error, 1)
		go func() { done <- ping() }()
		timer := time.NewTimer(timeout)
		select {
		case err := <-done:
			timer.Stop()
			if err != nil && strings.Contains(err.Error(), "client socket is closed") {
				return errWedged
			}
		case <-timer.C:
			return errWedged
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}
