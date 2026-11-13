// Package nodeproto holds the two facts the controller and the node command
// (internal/nodecmd) must agree on. It imports nothing, so the controller can
// use it without pulling in the Linux-only node code.
package nodeproto

const (
	// Version is the protocol number `virticle-node version` prints. Raise it
	// whenever a verb changes in a way an older controller or node cannot use.
	Version = "12"
	// Path is where the node command is installed on a node.
	Path = "/usr/local/libexec/virticle-node"
)
