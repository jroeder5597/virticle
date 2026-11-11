package api

import (
	"fmt"
	"strings"

	"virticle/internal/cluster"
)

// accepts is the stand-in for ConnectCompareHypervisorCPU on a node that is
// not connected: the model must be its host model or usable in custom mode,
// and every feature the CPU requires must be one the node's host model has.
func (n nodeCPU) accepts(d cpuDef) bool {
	if _, ok := n.Usable[d.Model]; !ok && d.Model != n.Host.Model {
		return false
	}
	for _, f := range d.Features {
		if f.Policy == "require" && !n.Host.has(f.Name) {
			return false
		}
	}
	return true
}

// describeBaselineChange names the old and new baseline for people: the model
// names when they differ, else the model and what the new one lacks.
func describeBaselineChange(oldB, newB cpuDef) (from, to string) {
	if oldB.Model != newB.Model {
		return oldB.Model, newB.Model
	}
	var lost []string
	for _, f := range oldB.enabled() {
		if !newB.has(f) {
			lost = append(lost, f)
		}
	}
	if len(lost) == 0 {
		return oldB.Model, newB.Model
	}
	shown := lost
	if len(shown) > 3 {
		shown = shown[:3]
	}
	to = newB.Model + " without " + strings.Join(shown, ", ")
	if len(lost) > len(shown) {
		to += fmt.Sprintf(" and %d more", len(lost)-len(shown))
	}
	return oldB.Model, to
}

// cpuBaselineStep adds a "Cluster CPU baseline" step to a connection test of
// node self and returns the warning text when the node would lower the
// baseline: the baseline of the other online nodes is compared with the one
// that includes the tested node's CPU.
func (s *Server) cpuBaselineStep(self int64, res *cluster.TestResult) []string {
	if res.DomCaps == "" {
		return nil
	}
	step := cluster.TestStep{ID: "cpu-baseline", Title: "Cluster CPU baseline", Status: cluster.StepOK}
	defer func() { res.Steps = append(res.Steps, step) }()
	host, err := parseHostModel(res.DomCaps)
	if err != nil {
		step.Status, step.Detail = cluster.StepInfo, "not used for the baseline: "+err.Error()
		return nil
	}
	cand := nodeCPU{ID: self, Name: "this node", Host: host, Usable: parseUsableModels(res.DomCaps)}
	all, hosts, _ := s.gatherNodeCPUs()
	var others []nodeCPU
	for _, n := range all {
		if n.ID != self {
			others = append(others, n)
		}
	}
	if len(others) == 0 {
		step.Detail = "no other node is online; this node sets the baseline (" + host.Model + ")"
		return nil
	}
	without, err := chooseBaseline(baselineArch, others, realProbe(others, hosts, nil))
	if err != nil {
		step.Status, step.Detail = cluster.StepInfo, "the other nodes have no common baseline: "+err.Error()
		return nil
	}
	withNodes := append(append([]nodeCPU(nil), others...), cand)
	with, err := chooseBaseline(baselineArch, withNodes, realProbe(withNodes, hosts, &cand))
	if err != nil {
		step.Status, step.Detail = cluster.StepError, "no CPU model is usable on this node and the others: "+err.Error()
		return []string{"This node shares no CPU model with the other nodes, so VMs with the cluster baseline cannot migrate to or from it."}
	}
	rel := cpuRelation(with.CPU.Model, with.CPU.enabled(), without.CPU)
	if rel == relSame {
		step.Detail = "same baseline with this node: " + with.CPU.Model
		return nil
	}
	from, to := describeBaselineChange(without.CPU, with.CPU)
	verb := "Changes"
	if rel == relOlder {
		verb = "Lowers"
	}
	used := 0
	for _, v := range s.collectVMs() {
		if v.CPU != nil && cpuRelation(v.CPU.Model, v.CPU.features, with.CPU) == relNewer {
			used++
		}
	}
	msg := fmt.Sprintf("%s the cluster CPU baseline from %s to %s; %d %s use the higher one", verb, from, to, used, plural(used, "VM", "VMs"))
	step.Status, step.Detail = cluster.StepWarning, msg
	return []string{msg}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
