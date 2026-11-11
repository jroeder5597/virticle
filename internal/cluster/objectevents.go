package cluster

import (
	"context"
	"regexp"
	"time"

	"virticle/internal/hostconn"
)

// go-libvirt does not route network and storage pool events, so they are read
// from virsh's event loop on the node (over the existing SSH connection).
var objectEventRE = regexp.MustCompile(`^event '([a-z-]+)' for (network|storage pool) (.+?): (\S+)`)

// objectEvents publishes network and pool lifecycle events until ctx ends.
// The loops are restarted if virsh exits; a node without virsh just gets no
// live updates for these objects.
func (m *Manager) objectEvents(ctx context.Context, node int64, h *hostconn.Host) {
	for _, c := range [][2]string{{"net-event", "network"}, {"pool-event", "pool"}} {
		go func(cmd, typ string) {
			for ctx.Err() == nil {
				h.Stream(ctx, "exec virsh -c qemu:///system "+cmd+" --event lifecycle --loop", func(l string) {
					if g := objectEventRE.FindStringSubmatch(l); g != nil {
						m.Publish(Event{Type: typ, Node: node, Name: g[3], Detail: g[4]})
					}
				})
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
				}
			}
		}(c[0], c[1])
	}
}
