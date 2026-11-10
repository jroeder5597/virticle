package api

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/cluster"
	"virt-proj/internal/hostconn"
	"virt-proj/internal/stats"
)

// Live statistics. Each online node gets one sampler goroutine that wakes
// every stats.Interval and costs three libvirt RPCs: ConnectGetAllDomainStats
// (every domain in one call), NodeGetCPUStats and NodeGetMemoryStats. It runs
// whether or not a browser is watching: three small RPCs per 5 s are cheaper
// than the complexity of start/stop-on-demand, and the charts then have ten
// minutes of history the moment a page opens. The history is a ring buffer in
// memory; nothing is persisted, so a restart of this server starts empty.

const rpcsPerTick = 3

const domainStatMask = uint32(libvirt.DomainStatsState | libvirt.DomainStatsCPUTotal | libvirt.DomainStatsBalloon |
	libvirt.DomainStatsVCPU | libvirt.DomainStatsInterface | libvirt.DomainStatsBlock)

type vmSeries struct {
	name   string
	state  string
	prev   *stats.VMRaw
	ring   stats.Ring[stats.VMPoint]
	lastAt time.Time
	// balloonOn: the guest memory stats period was set for this boot.
	balloonOn bool
}

type nodeSeries struct {
	mu         sync.Mutex
	hostPrev   *stats.HostRaw
	host       stats.Ring[stats.HostPoint]
	vms        map[string]*vmSeries // by uuid
	total      int                  // domains on the node
	running    int
	lastTick   time.Time
	lastErr    string
	cpuN, memN int32 // parameter counts libvirt wants for the host calls
	tookMS     int64
}

type statsHub struct {
	mu      sync.Mutex
	nodes   map[int64]*nodeSeries
	started map[int64]bool
}

var (
	hubOnce sync.Once
	hub     = &statsHub{nodes: map[int64]*nodeSeries{}, started: map[int64]bool{}}
)

func (s *Server) startStatsSampler() {
	hubOnce.Do(func() { go s.superviseSamplers() })
}

// superviseSamplers starts a sampler for every node that is online and has
// none; a sampler ends itself when its node disappears.
func (s *Server) superviseSamplers() {
	for {
		for _, n := range s.Nodes.Status() {
			hub.mu.Lock()
			run := hub.started[n.ID]
			if !run && n.State == "online" {
				hub.started[n.ID] = true
				if hub.nodes[n.ID] == nil {
					hub.nodes[n.ID] = &nodeSeries{vms: map[string]*vmSeries{}}
				}
				go s.sampleNode(n.ID)
			}
			hub.mu.Unlock()
		}
		time.Sleep(stats.Interval)
	}
}

func (s *Server) sampleNode(id int64) {
	defer func() {
		hub.mu.Lock()
		delete(hub.started, id)
		hub.mu.Unlock()
	}()
	hub.mu.Lock()
	ns := hub.nodes[id]
	hub.mu.Unlock()
	for {
		h, err := s.Nodes.Host(id)
		if err != nil {
			if exists(s.Nodes, id) {
				return // offline; the supervisor restarts us when it is back
			}
			hub.mu.Lock()
			delete(hub.nodes, id)
			hub.mu.Unlock()
			return
		}
		start := time.Now()
		ns.sample(h, start)
		ns.mu.Lock()
		ns.tookMS = time.Since(start).Milliseconds()
		ns.mu.Unlock()
		wait := stats.Interval - time.Since(start)
		if wait < time.Second {
			wait = time.Second
		}
		time.Sleep(wait)
	}
}

func exists(m *cluster.Manager, id int64) bool {
	for _, n := range m.Status() {
		if n.ID == id {
			return true
		}
	}
	return false
}

// typedNumber converts a libvirt typed parameter to a number.
func typedNumber(v any) (uint64, bool) {
	switch x := v.(type) {
	case uint64:
		return x, true
	case uint32:
		return uint64(x), true
	case int64:
		if x >= 0 {
			return uint64(x), true
		}
	case int32:
		if x >= 0 {
			return uint64(x), true
		}
	}
	return 0, false
}

// vmRawFromParams reads the counters out of one ConnectGetAllDomainStats
// record: cpu.time, balloon.*, vcpu.current and the per-device
// block.N.rd.bytes / net.N.rx.bytes families summed over devices.
func vmRawFromParams(at time.Time, params []libvirt.TypedParam) (raw stats.VMRaw, state uint64) {
	raw.At = at
	var avail, unused uint64
	var haveAvail, haveUnused bool
	for _, p := range params {
		n, ok := typedNumber(p.Value.I)
		if !ok {
			continue
		}
		switch {
		case p.Field == libvirt.DomainStatsStateState:
			state = n
		case p.Field == libvirt.DomainStatsCPUTime:
			raw.HasCPU, raw.CPUNanos = true, n
		case p.Field == libvirt.DomainStatsVCPUCurrent:
			raw.VCPUs = uint(n)
		case p.Field == libvirt.DomainStatsBalloonCurrent:
			raw.HasBalloon, raw.BalloonKiB = true, n
		case p.Field == libvirt.DomainStatsBalloonAvailable:
			avail, haveAvail = n, true
		case p.Field == libvirt.DomainStatsBalloonUnused:
			unused, haveUnused = n, true
		case strings.HasPrefix(p.Field, "block.") && strings.HasSuffix(p.Field, ".rd.bytes"):
			raw.HasDisk = true
			raw.DiskRead += n
		case strings.HasPrefix(p.Field, "block.") && strings.HasSuffix(p.Field, ".wr.bytes"):
			raw.HasDisk = true
			raw.DiskWrite += n
		case strings.HasPrefix(p.Field, "net.") && strings.HasSuffix(p.Field, ".rx.bytes"):
			raw.HasNet = true
			raw.NetRx += n
		case strings.HasPrefix(p.Field, "net.") && strings.HasSuffix(p.Field, ".tx.bytes"):
			raw.HasNet = true
			raw.NetTx += n
		}
	}
	if haveAvail && haveUnused && avail > 0 {
		raw.HasGuestStats, raw.AvailableKiB, raw.UnusedKiB = true, avail, unused
	}
	return raw, state
}

func (ns *nodeSeries) sample(h *hostconn.Host, at time.Time) {
	recs, err := h.ConnectGetAllDomainStats(nil, domainStatMask, 0)
	// libvirt insists on the exact parameter count (4 each on Linux). Ask
	// once with 0 to learn it, then reuse it so a tick stays at 3 RPCs.
	cpu, cpuErr := ns.nodeCPU(h)
	mem, memErr := ns.nodeMem(h)

	ns.mu.Lock()
	defer ns.mu.Unlock()
	if msg := fmt.Sprint(cpuErr, memErr, err); msg != ns.lastErr {
		// Log a failure once, not every tick.
		ns.lastErr = msg
		if cpuErr != nil || memErr != nil || err != nil {
			log.Printf("stats sampler: cpu: %v; memory: %v; domains: %v", cpuErr, memErr, err)
		}
	}
	ns.lastTick = at

	hr := stats.HostRaw{At: at}
	if cpuErr == nil {
		var total, idle, iowait uint64
		for _, f := range cpu {
			switch f.Field {
			case "idle":
				idle = f.Value
			case "iowait":
				iowait = f.Value
			}
			total += f.Value
		}
		hr.HasCPU, hr.Total, hr.Busy = total > 0, total, total-idle-iowait
	}
	if memErr == nil {
		hr.HasMem = true
		for _, f := range mem {
			switch f.Field {
			case "total":
				hr.MemTotalKiB = f.Value
			case "free":
				hr.MemFreeKiB = f.Value
			case "buffers":
				hr.MemBuffersKiB = f.Value
			case "cached":
				hr.MemCachedKiB = f.Value
			}
		}
	}
	ns.host.Push(stats.NewHostPoint(ns.hostPrev, hr))
	ns.hostPrev = &hr

	if err != nil {
		return // keep the old VM series; the next tick tries again
	}
	seen := map[string]bool{}
	ns.total, ns.running = len(recs), 0
	for _, rec := range recs {
		uuid := cluster.FormatUUID(rec.Dom.UUID)
		raw, state := vmRawFromParams(at, rec.Params)
		v := ns.vms[uuid]
		if v == nil {
			v = &vmSeries{}
			ns.vms[uuid] = v
		}
		v.name, v.state = rec.Dom.Name, stateName(uint8(state))
		seen[uuid] = true
		if v.state == "running" || v.state == "paused" {
			ns.running++
		}
		if v.state != "running" {
			v.prev = nil // counters are not comparable across a stop
			v.balloonOn = false
			continue
		}
		if !v.balloonOn && raw.HasBalloon {
			// Without a stats period the balloon driver reports once at
			// boot and the guest-used figure goes stale. This is a live-only
			// setting (not saved in the definition), applied once per boot.
			h.DomainSetMemoryStatsPeriod(rec.Dom, int32(stats.Interval/time.Second), libvirt.DomainMemLive)
			v.balloonOn = true
		}
		v.ring.Push(stats.NewVMPoint(v.prev, raw))
		v.prev = &raw
		v.lastAt = at
	}
	for uuid, v := range ns.vms {
		// Forget VMs that are gone, and stopped ones once their last
		// sample has aged out of the window.
		if !seen[uuid] || (v.state != "running" && at.Sub(v.lastAt) > stats.Keep*stats.Interval) {
			delete(ns.vms, uuid)
		}
	}
}

// --- API ---

type nodeLatest struct {
	Host    *stats.HostPoint `json:"host"`
	VMs     int              `json:"vms"`
	Running int              `json:"running"`
	// Stale is set when no sample arrived for three intervals.
	Stale bool `json:"stale"`
}

// allStats returns the newest sample of every node and running VM, for the
// list pages.
func (s *Server) allStats(w http.ResponseWriter, r *http.Request) {
	out := struct {
		IntervalSec int                       `json:"intervalSec"`
		Nodes       map[string]nodeLatest     `json:"nodes"`
		VMs         map[string]stats.VMPoint  `json:"vms"` // key "node/uuid"
		Sampler     map[string]map[string]any `json:"sampler"`
	}{int(stats.Interval / time.Second), map[string]nodeLatest{}, map[string]stats.VMPoint{}, map[string]map[string]any{}}
	hub.mu.Lock()
	nodes := make(map[int64]*nodeSeries, len(hub.nodes))
	for id, ns := range hub.nodes {
		nodes[id] = ns
	}
	hub.mu.Unlock()
	for id, ns := range nodes {
		ns.mu.Lock()
		key := strconv.FormatInt(id, 10)
		nl := nodeLatest{VMs: ns.total, Running: ns.running, Stale: time.Since(ns.lastTick) > 3*stats.Interval}
		if p, ok := ns.host.Last(); ok {
			nl.Host = &p
		}
		out.Nodes[key] = nl
		out.Sampler[key] = map[string]any{"rpcsPerTick": rpcsPerTick, "lastTickMs": ns.tookMS}
		for uuid, v := range ns.vms {
			if v.state != "running" {
				continue
			}
			if p, ok := v.ring.Last(); ok {
				out.VMs[key+"/"+uuid] = p
			}
		}
		ns.mu.Unlock()
	}
	writeJSON(w, out)
}

func (s *Server) nodeSeries(r *http.Request) (*nodeSeries, error) {
	id, _, err := s.host(r)
	if err != nil {
		return nil, err
	}
	hub.mu.Lock()
	ns := hub.nodes[id]
	hub.mu.Unlock()
	return ns, nil
}

func (s *Server) nodeStats(w http.ResponseWriter, r *http.Request) {
	ns, err := s.nodeSeries(r)
	if err != nil {
		fail(w, err)
		return
	}
	pts := []stats.HostPoint{}
	if ns != nil {
		ns.mu.Lock()
		pts = ns.host.Slice()
		ns.mu.Unlock()
	}
	writeJSON(w, map[string]any{"intervalSec": int(stats.Interval / time.Second), "points": pts})
}

func (s *Server) vmStats(w http.ResponseWriter, r *http.Request) {
	ns, err := s.nodeSeries(r)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := cluster.ParseUUID(r.PathValue("uuid"))
	if err != nil {
		fail(w, badRequest(fmt.Sprintf("bad uuid: %v", err)))
		return
	}
	pts := []stats.VMPoint{}
	if ns != nil {
		ns.mu.Lock()
		if v := ns.vms[cluster.FormatUUID(u)]; v != nil {
			pts = v.ring.Slice()
		}
		ns.mu.Unlock()
	}
	writeJSON(w, map[string]any{"intervalSec": int(stats.Interval / time.Second), "points": pts})
}

// nodeCPU reads the host CPU counters (kernel, user, idle, iowait), all
// cores together.
func (ns *nodeSeries) nodeCPU(h *hostconn.Host) ([]libvirt.NodeGetCPUStats, error) {
	if ns.cpuN == 0 {
		_, n, err := h.NodeGetCPUStats(-1, 0, 0)
		if err != nil {
			return nil, err
		}
		ns.cpuN = n
	}
	out, _, err := h.NodeGetCPUStats(-1, ns.cpuN, 0)
	if err != nil {
		ns.cpuN = 0
	}
	return out, err
}

// nodeMem reads the host memory counters (total, free, buffers, cached).
func (ns *nodeSeries) nodeMem(h *hostconn.Host) ([]libvirt.NodeGetMemoryStats, error) {
	if ns.memN == 0 {
		_, n, err := h.NodeGetMemoryStats(0, -1, 0)
		if err != nil {
			return nil, err
		}
		ns.memN = n
	}
	out, _, err := h.NodeGetMemoryStats(ns.memN, -1, 0)
	if err != nil {
		ns.memN = 0
	}
	return out, err
}
