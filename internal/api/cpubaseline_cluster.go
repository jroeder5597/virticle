package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

const (
	baselineArch    = "x86_64"
	baselineMachine = "q35"
	baselineVirt    = "kvm"
)

type baselineNode struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	HostModel string `json:"hostModel"`
}

type baselineSkipped struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// clusterBaseline is what GET /api/cluster/cpu-baseline returns.
type clusterBaseline struct {
	Model    string            `json:"model,omitempty"`
	Vendor   string            `json:"vendor,omitempty"`
	Features []cpuFeat         `json:"features"`
	Source   string            `json:"source,omitempty"` // libvirt | fallback
	Nodes    []baselineNode    `json:"nodes"`            // included
	Skipped  []baselineSkipped `json:"skipped"`          // left out: offline or no usable CPU data
	Computed time.Time         `json:"computed"`
	Error    string            `json:"error,omitempty"` // set when no baseline could be computed
	Notes    []string          `json:"notes,omitempty"`
}

func (b *clusterBaseline) def() cpuDef {
	return cpuDef{Model: b.Model, Vendor: b.Vendor, Features: b.Features}
}

// same reports whether two results describe the same baseline over the same
// nodes, so a recomputation that changed nothing keeps its timestamp.
func (b *clusterBaseline) same(o *clusterBaseline) bool {
	if o == nil || b.Model != o.Model || b.Error != o.Error || len(b.Nodes) != len(o.Nodes) || len(b.Skipped) != len(o.Skipped) || len(b.Features) != len(o.Features) {
		return false
	}
	for i := range b.Nodes {
		if b.Nodes[i] != o.Nodes[i] {
			return false
		}
	}
	for i := range b.Skipped {
		if b.Skipped[i] != o.Skipped[i] {
			return false
		}
	}
	for i := range b.Features {
		if b.Features[i] != o.Features[i] {
			return false
		}
	}
	return true
}

type baselineState struct {
	once sync.Once
	mu   sync.Mutex
	cur  *clusterBaseline
	run  sync.Mutex // one computation at a time
}

// gatherNodeCPUs asks every online node for its host-model CPU and usable
// model list; nodes that are offline or have no KVM CPU data are returned as
// skipped.
func (s *Server) gatherNodeCPUs() ([]nodeCPU, map[int64]*hostconn.Host, []baselineSkipped) {
	type result struct {
		n    nodeCPU
		h    *hostconn.Host
		skip *baselineSkipped
	}
	sts := s.Nodes.Status()
	res := make([]result, len(sts))
	var wg sync.WaitGroup
	for i, st := range sts {
		name := st.Hostname
		if name == "" {
			name = st.Addr
		}
		h, err := s.Nodes.Host(st.ID)
		if err != nil {
			reason := "offline"
			if st.State == "connecting" {
				reason = "connecting"
			}
			res[i].skip = &baselineSkipped{st.ID, name, reason}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			dc, err := h.ConnectGetDomainCapabilities(nil, libvirt.OptString{baselineArch}, libvirt.OptString{baselineMachine}, libvirt.OptString{baselineVirt}, 0)
			if err != nil {
				res[i].skip = &baselineSkipped{st.ID, name, "no KVM domain capabilities: " + firstLine(err.Error())}
				return
			}
			host, err := parseHostModel(dc)
			if err != nil {
				res[i].skip = &baselineSkipped{st.ID, name, err.Error()}
				return
			}
			res[i].n = nodeCPU{ID: st.ID, Name: name, Host: host, Usable: parseUsableModels(dc)}
			res[i].h = h
		}()
	}
	wg.Wait()
	var nodes []nodeCPU
	hosts := map[int64]*hostconn.Host{}
	var skipped []baselineSkipped
	for _, r := range res {
		if r.skip != nil {
			skipped = append(skipped, *r.skip)
		} else {
			nodes = append(nodes, r.n)
			hosts[r.n.ID] = r.h
		}
	}
	return nodes, hosts, skipped
}

func (s *Server) computeClusterBaseline() *clusterBaseline {
	nodes, hosts, skipped := s.gatherNodeCPUs()
	out := &clusterBaseline{Features: []cpuFeat{}, Nodes: []baselineNode{}, Skipped: skipped, Computed: time.Now().UTC().Truncate(time.Second)}
	if out.Skipped == nil {
		out.Skipped = []baselineSkipped{}
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, baselineNode{n.ID, n.Name, n.Host.Model})
	}
	if len(nodes) == 0 {
		out.Error = "no online node with KVM"
		return out
	}
	probe := realProbe(nodes, hosts, nil)
	res, err := chooseBaseline(baselineArch, nodes, probe)
	out.Notes = res.Notes
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Model, out.Vendor, out.Features, out.Source = res.CPU.Model, res.CPU.Vendor, res.CPU.Features, res.Source
	return out
}

// refreshBaseline recomputes the cached baseline.
func (s *Server) refreshBaseline() *clusterBaseline {
	s.cpuBase.run.Lock()
	defer s.cpuBase.run.Unlock()
	nb := s.computeClusterBaseline()
	s.cpuBase.mu.Lock()
	defer s.cpuBase.mu.Unlock()
	if nb.same(s.cpuBase.cur) {
		return s.cpuBase.cur
	}
	s.cpuBase.cur = nb
	s.Nodes.Publish(cluster.Event{Type: "cpubaseline"})
	return nb
}

// clusterCPUBaseline returns the cached baseline, computing it on first use.
func (s *Server) clusterCPUBaseline() *clusterBaseline {
	s.cpuBase.mu.Lock()
	cur := s.cpuBase.cur
	s.cpuBase.mu.Unlock()
	if cur != nil {
		return cur
	}
	return s.refreshBaseline()
}

// startCPUBaseline keeps the cache current: it recomputes a few seconds after
// the last event that changes the set of online nodes (a node coming online,
// going offline, being added, edited or removed).
func (s *Server) startCPUBaseline() {
	if s.Nodes == nil {
		return
	}
	s.cpuBase.once.Do(func() {
		go func() {
			ch, _ := s.Nodes.Subscribe()
			s.refreshBaseline()
			var timer *time.Timer
			var fire <-chan time.Time
			for {
				select {
				case ev := <-ch:
					if ev.Type == "node" && ev.Detail != "maintenance" {
						if timer == nil {
							timer = time.NewTimer(3 * time.Second)
						} else {
							timer.Reset(3 * time.Second)
						}
						fire = timer.C
					}
				case <-fire:
					fire = nil
					s.refreshBaseline()
				}
			}
		}()
	})
}

func (s *Server) cpuBaseline(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.clusterCPUBaseline())
}

// realProbe asks libvirt on the nodes. extra, when set, is a node that is not
// connected (a candidate under test) and sits last in the node list: it cannot
// be asked, so its answer is derived from its own capabilities.
func realProbe(nodes []nodeCPU, hosts map[int64]*hostconn.Host, extra *nodeCPU) baselineProbe {
	return baselineProbe{
		Baseline: func(inputs []string) (string, error) {
			return hosts[nodes[0].ID].ConnectBaselineHypervisorCPU(nil, libvirt.OptString{baselineArch}, libvirt.OptString{baselineMachine}, libvirt.OptString{baselineVirt}, inputs, uint32(libvirt.ConnectBaselineCPUMigratable))
		},
		Compatible: func(idx int, d cpuDef) (bool, error) {
			if extra != nil && idx == len(nodes)-1 {
				return extra.accepts(d), nil
			}
			r, err := hosts[nodes[idx].ID].ConnectCompareHypervisorCPU(nil, libvirt.OptString{baselineArch}, libvirt.OptString{baselineMachine}, libvirt.OptString{baselineVirt}, compareXML(d), 0)
			if err != nil {
				return false, fmt.Errorf("%s", firstLine(err.Error()))
			}
			return libvirt.CPUCompareResult(r) != libvirt.CPUCompareIncompatible, nil
		},
	}
}
