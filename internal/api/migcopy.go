package api

// Node to node connections: what the source node needs in order to reach the
// target, and the pre-flight check for it.
//
// Live migration is coordinated by the controller (migclient.go) and needs no
// SSH between the nodes. Files that must be copied (a migration with disks on
// local storage, snapshot files, a backup restored on another node) travel
// from node to node with the node key peer transfer (peertrust.go), for every
// pair of nodes, whether each is reached as its service account or as root.

import (
	"fmt"
	"strings"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// reachCmd tests a TCP connection from the node to host:port; the shell is
// bash because /dev/tcp is a bash feature. Exit 0: connected, 1: refused or
// unreachable at once, 124: no answer (typically a firewall dropping it).
func reachCmd(host, port string) string {
	return "timeout 6 bash -c " + shq("exec 3<>/dev/tcp/"+host+"/"+port)
}

// checkNodeTransfer is the "SSH between nodes" check for a pair of nodes (see
// peertrust.go and migclient.go).
//   - Live migration is coordinated by the controller: no SSH between the
//     nodes is needed for it.
//   - Files that must be copied travel from node to node with the node key
//     peer transfer. The trust for that is set up here, idempotently, and
//     tested with a real connection.
func (s *Server) checkNodeTransfer(src, dst *hostconn.Host, n store.Node, plan *migPlan, c *checkList) {
	ep, err := endpointFor(n)
	if err != nil {
		c.bad("SSH between nodes", err.Error())
		return
	}
	if len(plan.copyList()) == 0 {
		c.pass("SSH between nodes", "no file has to be copied between the nodes, and the controller coordinates the migration itself: no SSH between "+src.Name+" and "+dst.Name+" is needed")
	} else {
		port := ep.port()
		if _, err := src.Run(reachCmd(ep.Host, port)); err != nil {
			c.bad("SSH between nodes", fmt.Sprintf("%s cannot open a TCP connection to %s port %s. Nodes reach each other by the IP address or name configured in the node's settings (migration address); check that it is right and that no firewall blocks the SSH port.", src.Name, ep.Host, port))
			return
		}
		if err := s.ensurePeerTrust(src, dst, n, ep); err != nil {
			c.bad("SSH between nodes", err.Error())
			return
		}
		c.pass("SSH between nodes", fmt.Sprintf("%s sends files straight to %s as %s with its own node key (restricted to delivering files into storage pools), host key checked; the file data does not pass through the controller", src.Name, ep.String(), ep.User))
	}

	if _, err := src.Run(reachCmd(ep.Host, "49152")); err != nil && strings.Contains(err.Error(), "124") {
		c.warn("Migration ports", fmt.Sprintf("%s does not answer on %s:49152. The migration data connection uses TCP ports 49152-49215 on the target; open them in the target's firewall, or a live migration will hang.", src.Name, ep.Host))
	} else {
		c.pass("Migration ports", fmt.Sprintf("%s can reach %s on the migration port range (tested 49152)", src.Name, ep.Host))
	}
}
