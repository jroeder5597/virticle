package api

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/metrics"
	"virticle/internal/stats"
)

// Statistics collector. Each online node gets one sampler goroutine that
// wakes every stats.Interval and costs one libvirt call (ConnectGetAllDomainStats,
// every domain at once) and one small command on the node (NodeReadCmd: CPU,
// load, memory, network cards, block devices, GPUs; storage pools every
// fourth round). It runs whether or not a browser is watching. The samples
// feed three consumers: a ring buffer in memory for the live charts, the
// metrics file for the history (internal/metrics), and the latest raw counters
// for the Prometheus endpoint, so one collector serves all of them.

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
	mu       sync.Mutex
	hostPrev *stats.HostRaw
	host     stats.Ring[stats.HostPoint]
	vms      map[string]*vmSeries // by uuid
	total    int                  // domains on the node
	running  int
	lastTick time.Time
	lastErr  string
	trk      *metrics.NodeTracker
	tick     int
	pools    []metrics.Pool
	tookMS   int64
	cpus     uint16
}

type statsHub struct {
	db      *metrics.DB // the history file; nil in tests without a data directory
	mu      sync.Mutex
	nodes   map[int64]*nodeSeries
	started map[int64]bool
}

var (
	hubOnce sync.Once
	hub     = &statsHub{nodes: map[int64]*nodeSeries{}, started: map[int64]bool{}}
)

func (s *Server) startStatsSampler() {
	hubOnce.Do(func() {
		if s.DataDir != "" {
			if db, err := metrics.Open(filepath.Join(s.DataDir, "metrics.db")); err != nil {
				log.Printf("metrics: history is off: %v", err)
			} else {
				hub.db = db
				go db.Run(make(chan struct{}))
			}
		}
		go s.superviseSamplers()
	})
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
					hub.nodes[n.ID] = &nodeSeries{vms: map[string]*vmSeries{}, trk: metrics.NewNodeTracker(n.ID, nodeLabel(n))}
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
		if ns.cpus == 0 { // fixed for the life of the connection; read once
			if _, _, c, _, _, _, _, _, err := h.NodeGetInfo(); err == nil {
				ns.mu.Lock()
				ns.cpus = uint16(c)
				ns.mu.Unlock()
			}
		}
		batch := ns.sample(h, start)
		if hub.db != nil {
			if err := hub.db.Insert(batch); err != nil {
				log.Printf("metrics: writing: %v", err)
			}
		}
		ns.mu.Lock()
		ns.tookMS = time.Since(start).Milliseconds()
		ns.mu.Unlock()
		// Wake on the next 15-second boundary, not 15 seconds after this
		// start: a period that is a few milliseconds long drifts across the
		// boundaries of the history's time slots and would leave a slot empty
		// every few minutes.
		wait := time.Until(start.Truncate(stats.Interval).Add(stats.Interval))
		if wait < time.Second {
			wait += stats.Interval
		}
		time.Sleep(wait + 50*time.Millisecond)
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

// readPools reads the size figures of the active storage pools.
func readPools(h *hostconn.Host) []metrics.Pool {
	pools, _, err := h.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil
	}
	out := []metrics.Pool{}
	for _, p := range pools {
		if _, capacity, alloc, avail, err := h.StoragePoolGetInfo(p); err == nil {
			out = append(out, metrics.Pool{Name: p.Name, Capacity: capacity, Allocation: alloc, Avail: avail})
		}
	}
	return out
}

// sample takes one round of statistics and returns what the history should
// store for it.
func (ns *nodeSeries) sample(h *hostconn.Host, at time.Time) metrics.Batch {
	recs, err := h.ConnectGetAllDomainStats(nil, domainStatMask, 0)
	out, cmdErr := h.Run("sh -c " + hostconn.Quote(metrics.NodeReadCmd))
	var nr *metrics.NodeRaw
	if cmdErr == nil {
		if r := metrics.ParseNodeRead(at, out); r.HasCPU || r.HasMem {
			nr = &r
		} else {
			cmdErr = fmt.Errorf("the node gave no CPU or memory figures")
		}
	}
	// Storage pools change slowly: ask once a minute and repeat the figure.
	ns.mu.Lock()
	if ns.tick%4 == 0 {
		ns.mu.Unlock()
		pools := readPools(h)
		ns.mu.Lock()
		ns.pools = pools
	}
	ns.tick++
	if nr != nil {
		nr.Pools = ns.pools
	}
	defer ns.mu.Unlock()
	if msg := fmt.Sprint(cmdErr, err); msg != ns.lastErr {
		// Log a failure once, not every tick.
		ns.lastErr = msg
		if cmdErr != nil || err != nil {
			log.Printf("stats sampler: node read: %v; domains: %v", cmdErr, err)
		}
	}
	ns.lastTick = at

	hr := stats.HostRaw{At: at}
	if nr != nil {
		if nr.HasCPU {
			var total uint64
			for _, j := range nr.CPU {
				total += j
			}
			// Jiffies of 10 ms, as nanoseconds; busy excludes idle and iowait.
			hr.HasCPU, hr.Total, hr.Busy = total > 0, total*1e7, (total-nr.CPU[3]-nr.CPU[4])*1e7
		}
		if nr.HasMem {
			hr.HasMem = true
			hr.MemTotalKiB, hr.MemFreeKiB, hr.MemBuffersKiB, hr.MemCachedKiB = nr.MemTotal, nr.MemFree, nr.MemBuffers, nr.MemCached
		}
	}
	ns.host.Push(stats.NewHostPoint(ns.hostPrev, hr))
	ns.hostPrev = &hr

	if err != nil {
		// Keep the old VM series; the next tick tries again. The history
		// gets no VM rows for this round, which shows as a gap.
		return ns.trk.Collect(at, nil, false, nr)
	}
	var doms []metrics.Dom
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
		doms = append(doms, metrics.Dom{UUID: uuid, Name: v.name, State: v.state, Raw: metrics.ParseDomain(at, rec.Params)})
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
	return ns.trk.Collect(at, doms, true, nr)
}

// --- API ---

type nodeLatest struct {
	Host    *stats.HostPoint `json:"host"`
	VMs     int              `json:"vms"`
	Running int              `json:"running"`
	// Stale is set when no sample arrived for three intervals.
	Stale bool `json:"stale"`
	// CPUs is the number of host CPUs, 0 until the sampler has asked.
	CPUs uint16 `json:"cpus"`
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
		nl := nodeLatest{VMs: ns.total, Running: ns.running, Stale: time.Since(ns.lastTick) > 3*stats.Interval, CPUs: ns.cpus}
		if p, ok := ns.host.Last(); ok {
			nl.Host = &p
		}
		out.Nodes[key] = nl
		out.Sampler[key] = map[string]any{"lastTickMs": ns.tookMS}
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
