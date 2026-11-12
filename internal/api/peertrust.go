package api

// Direct node to node file transfer between any two nodes, whether each one is
// reached as its service account or as root (see internal/hostconn/peers.go).
// The controller introduces the nodes; the file data then flows from the
// source node to the target node and never through the controller.
//
// What is set up, for the direction source -> target:
//   - the source's account has its own SSH key (made by "virticle node
//     prepare"); its public half is installed on the target in the login
//     account's authorized_keys, restricted to the node command's peer-recv
//     (that key can deliver one new file inside a storage pool and do nothing
//     else; the forced command has no sudo when the target account is root);
//   - the target's SSH host key is recorded on the source, taken from the
//     controller's own known_hosts. The source checks it strictly.
// Everything is idempotent. Removing a node from the controller undoes it.
// A pair of a root node and a service-account node works in both directions:
// each side's node command decides by its own account.

import (
	"context"
	"fmt"
	"log"
	"strings"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// hasNodeCommand reports whether the controller can run the node command on h.
func hasNodeCommand(h *hostconn.Host) bool {
	return h.Privilege() == hostconn.PrivService || h.Privilege() == hostconn.PrivRoot
}

func (e migEndpoint) port() string {
	if e.Port == "" {
		return "22"
	}
	return e.Port
}

// ensurePeerTrust makes src able to send files to dst and checks that it works.
// dstNode is the stored node record of dst.
func (s *Server) ensurePeerTrust(src, dst *hostconn.Host, dstNode store.Node, ep migEndpoint) error {
	pub, err := src.PeerKey()
	if err != nil {
		return fmt.Errorf("%s has no node key: %w (run \"virticle node prepare\" for it again)", src.Name, err)
	}
	keys, err := hostconn.KnownHostKeys(s.Nodes.KnownHostsFile(), dstNode.Addr)
	if err != nil {
		return fmt.Errorf("the controller has no SSH host key on record for %s: %w", dstNode.Addr, err)
	}
	if err := dst.PeerAllow(pub); err != nil {
		return fmt.Errorf("installing the node key of %s on %s: %w", src.Name, dst.Name, err)
	}
	if err := src.PeerSetHostKey(ep.Host, ep.port(), keys); err != nil {
		return fmt.Errorf("recording the host key of %s on %s: %w", dst.Name, src.Name, err)
	}
	if err := src.PeerPing(ep.Host, ep.port(), ep.User); err != nil {
		return fmt.Errorf("%s cannot reach the node command on %s as %s: %w", src.Name, ep.Host, ep.User, err)
	}
	return nil
}

// dropPeerTrust removes what ensurePeerTrust installed for the node that is
// going away: its key on the other helper nodes, and its host key on them,
// and the other nodes' keys and host keys on the departing node itself.
// Best effort; nodes that are offline keep their entries (the keys only allow
// delivering files into storage pools).
func (s *Server) dropPeerTrust(id int64) {
	h, err := s.Nodes.Host(id)
	if err != nil || !hasNodeCommand(h) {
		return
	}
	pub, err := h.PeerKey()
	if err != nil {
		return
	}
	var gone store.Node
	for _, n := range s.Nodes.Status() {
		if n.ID == id {
			gone = n.Node
		}
	}
	for _, n := range s.Nodes.Status() {
		if n.ID == id {
			continue
		}
		o, err := s.Nodes.Host(n.ID)
		if err != nil || !hasNodeCommand(o) {
			continue
		}
		if err := o.PeerDisallow(pub); err != nil {
			log.Printf("removing the node key of %s from %s: %v", h.Name, o.Name, err)
		}
		if ep, err := endpointFor(gone); err == nil {
			if err := o.PeerUnsetHostKey(ep.Host, ep.port()); err != nil {
				log.Printf("forgetting the host key of %s on %s: %v", h.Name, o.Name, err)
			}
		}
		// The other direction: the departing node may still run and would
		// keep accepting the key of a node it no longer belongs to.
		if opub, err := o.PeerKey(); err == nil {
			if err := h.PeerDisallow(opub); err != nil {
				log.Printf("removing the node key of %s from %s: %v", o.Name, h.Name, err)
			}
		}
		if ep, err := endpointFor(n.Node); err == nil {
			if err := h.PeerUnsetHostKey(ep.Host, ep.port()); err != nil {
				log.Printf("forgetting the host key of %s on %s: %v", o.Name, h.Name, err)
			}
		}
	}
}

// peerMode turns a stat mode such as "644" or "4755" into the three octal
// digits peer-send accepts.
func peerMode(mode string) string {
	if len(mode) > 3 {
		mode = mode[len(mode)-3:]
	}
	if len(mode) != 3 || strings.Trim(mode, "01234567") != "" {
		return "600"
	}
	return mode
}

// copyToNode copies the file src of node from to the same-named path dstPath
// on node to, directly between the nodes with the node key peer transfer (the
// trust is set up by ensurePeerTrust). mode is the octal permission string the
// copy gets (empty: 600). progress, when set, is called with the bytes of this
// file sent so far.
func copyToNode(ctx context.Context, from, to *hostconn.Host, ep migEndpoint, srcPath, dstPath, mode string, progress func(done uint64)) error {
	return from.PeerSend(ctx, ep.Host, ep.port(), ep.User, srcPath, dstPath, peerMode(mode), func(done, total uint64) {
		if progress != nil {
			progress(done)
		}
	})
}
