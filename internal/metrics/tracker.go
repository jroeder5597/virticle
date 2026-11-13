package metrics

import "time"

// Row is one entity's values at one time step.
type Row struct {
	Ent   string // "vm:<uuid>" or "node:<id>"
	Label string // name shown in overviews
	Vals  []float64
	Devs  []DevRow
}

// Batch is what one node produced in one collection cycle.
type Batch struct {
	T    int64 // unix seconds, aligned to the tier-0 step
	Rows []Row
}

// Dom is one domain as listed by libvirt.
type Dom struct {
	UUID, Name string
	State      string // "running", "paused", "shut off", ...
	Raw        DomRaw // meaningful when running
}

// VMView is what a scrape needs to know about a VM.
type VMView struct {
	UUID, Name, State string
	Raw               *DomRaw // latest counters while running
}

// NodeTracker holds the previous counters of one node and its VMs, turns the
// next ones into rates, and keeps the latest raw values for Prometheus. The
// caller serialises access.
type NodeTracker struct {
	ID   int64
	Name string

	prevNode *NodeRaw
	prevVM   map[string]*DomRaw

	// Latest values, for /metrics.
	LastNode *NodeRaw
	Running  int
	VMs      map[string]*VMView
}

func NewNodeTracker(id int64, name string) *NodeTracker {
	return &NodeTracker{ID: id, Name: name, prevVM: map[string]*DomRaw{}, VMs: map[string]*VMView{}}
}

// Collect processes one cycle. nr may be nil when the node read failed: the
// node row is then left out (a gap) and the previous counters are dropped.
// doms is nil when the domain call failed; VM rows are then skipped too.
func (t *NodeTracker) Collect(at time.Time, doms []Dom, domsOK bool, nr *NodeRaw) Batch {
	step := Tiers[0].Step
	b := Batch{T: at.Unix() / step * step}

	if domsOK {
		seen := map[string]bool{}
		t.Running = 0
		for i := range doms {
			d := &doms[i]
			seen[d.UUID] = true
			view := t.VMs[d.UUID]
			if view == nil {
				view = &VMView{UUID: d.UUID}
				t.VMs[d.UUID] = view
			}
			view.Name, view.State = d.Name, d.State
			if d.State == "running" || d.State == "paused" {
				t.Running++
			}
			if d.State != "running" {
				view.Raw = nil
				delete(t.prevVM, d.UUID) // counters are not comparable across a stop
				continue
			}
			raw := d.Raw
			view.Raw = &raw
			vals, devs := VMSample(t.prevVM[d.UUID], raw, t.ID)
			t.prevVM[d.UUID] = &raw
			b.Rows = append(b.Rows, Row{Ent: "vm:" + d.UUID, Label: d.Name, Vals: vals, Devs: devs})
		}
		for u := range t.VMs {
			if !seen[u] {
				delete(t.VMs, u)
				delete(t.prevVM, u)
			}
		}
	}

	if nr == nil {
		t.prevNode, t.LastNode = nil, nil
		return b
	}
	vals, devs := NodeSample(t.prevNode, *nr, t.Running)
	cp := *nr
	t.prevNode, t.LastNode = &cp, &cp
	b.Rows = append(b.Rows, Row{Ent: "node:" + itoa(t.ID), Label: t.Name, Vals: vals, Devs: devs})
	return b
}

// Snapshot copies what a scrape needs, so it can be rendered after the
// caller's lock is released. The raw counters are never changed after they
// are stored, so sharing them is safe.
func (t *NodeTracker) Snapshot() *NodeTracker {
	c := &NodeTracker{ID: t.ID, Name: t.Name, LastNode: t.LastNode, Running: t.Running, VMs: make(map[string]*VMView, len(t.VMs))}
	for u, v := range t.VMs {
		cp := *v
		c.VMs[u] = &cp
	}
	return c
}
