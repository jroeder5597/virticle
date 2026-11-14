package hostconn

import (
	"fmt"
	"strconv"
	"strings"
)

// The questions the controller asks a node about a VM on another node, for the checks before an unplanned
// failover (see internal/api/replica_checks.go).

// PeerAsk asks the node command of user@host:port, from this node, one question about a VM: "vm-state" (the
// answer is "running", "shutoff", "absent" and so on) or "took-over" (the answer is "took-over" or
// "not-took-over": whether that node started the VM from its copy after the generation gen). It is what the
// controller cannot ask itself when it has lost the node.
func (h *Host) PeerAsk(host, port, user, kind, uuid string, gen int) (string, error) {
	args := []string{host, port, user, kind, uuid}
	if kind == "took-over" {
		args = append(args, strconv.Itoa(gen))
	}
	out, err := h.RunNode("peer-ask", args...)
	if err != nil {
		return "", nodeErr(out, err)
	}
	line := strings.TrimSpace(out)
	switch {
	case kind == "vm-state" && strings.HasPrefix(line, "@@vm "):
		return strings.TrimPrefix(line, "@@vm "), nil
	case kind == "took-over" && strings.HasPrefix(line, "@@took-over"):
		return "took-over", nil
	case kind == "took-over" && line == "@@not-took-over":
		return "not-took-over", nil
	}
	return "", fmt.Errorf("unexpected answer: %q", line)
}

// ProbeReply is one address that answered a probe, with the hardware address that answered.
type ProbeReply struct{ IP, MAC string }

// VMProbe looks from this node for the addresses ips on the bridge br. tool says how ("arping", "ip-neigh", or
// "none" when neither exists); problem is "nobridge" when this node has no such bridge and "noaddr" when the bridge
// has no IPv4 address to ask from.
func (h *Host) VMProbe(br string, ips []string) (replies []ProbeReply, tool, problem string, err error) {
	out, err := h.RunNode("vm-probe", append([]string{br}, ips...)...)
	if err != nil {
		return nil, "", "", nodeErr(out, err)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 1 && f[0] == "@@nobridge":
			problem = "nobridge"
		case len(f) == 1 && f[0] == "@@noaddr":
			problem = "noaddr"
		case len(f) == 2 && f[0] == "@@tool":
			tool = f[1]
		case len(f) == 3 && f[0] == "@@reply":
			replies = append(replies, ProbeReply{IP: f[1], MAC: f[2]})
		}
	}
	return replies, tool, problem, nil
}
