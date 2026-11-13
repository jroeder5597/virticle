package metrics

import (
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"
)

// MaxGap is the longest time between two samples that still yields rates;
// after a longer pause the counters are treated as a fresh start.
const MaxGap = 4 * 15 * time.Second

// --- raw counters ---

// DiskRaw and NetRaw are the cumulative counters of one device.
type DiskRaw struct {
	RdBytes, WrBytes, RdReqs, WrReqs, RdNanos, WrNanos uint64
}
type NetRaw struct {
	RxBytes, RxPkts, RxErrs, RxDrop, TxBytes, TxPkts, TxErrs, TxDrop uint64
}

// DomRaw is one VM's counters at one instant, as libvirt reports them.
type DomRaw struct {
	At         time.Time
	VCPUs      uint
	HasCPU     bool
	CPUNanos   uint64
	HasDelay   bool
	DelayNanos uint64 // time the vCPUs waited for a host CPU, summed

	HasBalloon                      bool
	BalloonKiB, AvailKiB, UnusedKiB uint64
	HasGuest                        bool
	RSSKiB                          uint64
	HasRSS                          bool
	Disks                           map[string]DiskRaw
	Nets                            map[string]NetRaw
}

func number(v any) (uint64, bool) {
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

// ParseDomain reads one ConnectGetAllDomainStats record: cpu.time, balloon.*,
// vcpu.N.delay and the per-device block.N.* and net.N.* families.
func ParseDomain(at time.Time, params []libvirt.TypedParam) DomRaw {
	r := DomRaw{At: at, Disks: map[string]DiskRaw{}, Nets: map[string]NetRaw{}}
	blkName, netName := map[string]string{}, map[string]string{}
	blk, nets := map[string]*DiskRaw{}, map[string]*NetRaw{}
	for _, p := range params {
		if s, ok := p.Value.I.(string); ok {
			if f := strings.Split(p.Field, "."); len(f) == 3 && f[2] == "name" {
				switch f[0] {
				case "block":
					blkName[f[1]] = s
				case "net":
					netName[f[1]] = s
				}
			}
			continue
		}
		n, ok := number(p.Value.I)
		if !ok {
			continue
		}
		switch p.Field {
		case "cpu.time":
			r.HasCPU, r.CPUNanos = true, n
			continue
		case "vcpu.current":
			r.VCPUs = uint(n)
			continue
		case "balloon.current":
			r.HasBalloon, r.BalloonKiB = true, n
			continue
		case "balloon.available":
			r.AvailKiB = n
			continue
		case "balloon.unused":
			r.UnusedKiB, r.HasGuest = n, true
			continue
		case "balloon.rss":
			r.HasRSS, r.RSSKiB = true, n
			continue
		}
		f := strings.Split(p.Field, ".")
		switch {
		case len(f) == 3 && f[0] == "vcpu" && f[2] == "delay":
			r.HasDelay = true
			r.DelayNanos += n
		case len(f) >= 3 && f[0] == "block":
			d := blk[f[1]]
			if d == nil {
				d = &DiskRaw{}
				blk[f[1]] = d
			}
			switch strings.Join(f[2:], ".") {
			case "rd.bytes":
				d.RdBytes = n
			case "wr.bytes":
				d.WrBytes = n
			case "rd.reqs":
				d.RdReqs = n
			case "wr.reqs":
				d.WrReqs = n
			case "rd.times":
				d.RdNanos = n
			case "wr.times":
				d.WrNanos = n
			}
		case len(f) >= 3 && f[0] == "net":
			d := nets[f[1]]
			if d == nil {
				d = &NetRaw{}
				nets[f[1]] = d
			}
			switch strings.Join(f[2:], ".") {
			case "rx.bytes":
				d.RxBytes = n
			case "rx.pkts":
				d.RxPkts = n
			case "rx.errs":
				d.RxErrs = n
			case "rx.drop":
				d.RxDrop = n
			case "tx.bytes":
				d.TxBytes = n
			case "tx.pkts":
				d.TxPkts = n
			case "tx.errs":
				d.TxErrs = n
			case "tx.drop":
				d.TxDrop = n
			}
		}
	}
	for i, d := range blk {
		if name := blkName[i]; name != "" {
			r.Disks[name] = *d
		}
	}
	for i, d := range nets {
		name := netName[i]
		if name == "" {
			name = "net" + i
		}
		r.Nets[name] = *d
	}
	if r.AvailKiB == 0 {
		r.HasGuest = false
	}
	return r
}

// --- rates ---

// rate is the per-second increase of a counter, NaN when it went backwards.
func rate(prev, cur uint64, dt float64) float64 {
	if cur < prev || dt <= 0 {
		return NaN
	}
	return float64(cur-prev) / dt
}

func delta(prev, cur uint64) (uint64, bool) {
	if cur < prev {
		return 0, false
	}
	return cur - prev, true
}

func pct(v float64) float64 {
	if isNaN(v) {
		return v
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// latency is the mean time per operation in milliseconds, NaN when nothing
// was done in the step (there is nothing to average) or a counter reset.
func latency(dNanos uint64, ok bool, dOps uint64, okOps bool) float64 {
	if !ok || !okOps || dOps == 0 {
		return NaN
	}
	return float64(dNanos) / float64(dOps) / 1e6
}

func blankRow(n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = NaN
	}
	return v
}

func colIndex(cols []col) map[string]int {
	m := map[string]int{}
	for i, c := range cols {
		m[c.name] = i
	}
	return m
}

var vmIdx = colIndex(vmCols)
var nodeIdx = colIndex(nodeCols)

// DevRow is one device's values at one instant.
type DevRow struct {
	Kind, Name string
	Vals       [nDev]float64
}

func newDev(kind, name string) DevRow {
	d := DevRow{Kind: kind, Name: name}
	for i := range d.Vals {
		d.Vals[i] = NaN
	}
	return d
}

// VMSample computes the wide row and device rows of a VM. prev is nil for
// the first sample after a start, a migration or a long pause: the rates are
// then unknown (NaN) while the gauges are still filled.
func VMSample(prev *DomRaw, cur DomRaw, node int64) ([]float64, []DevRow) {
	v := blankRow(len(vmCols))
	v[vmIdx["node"]] = float64(node)
	if cur.HasBalloon {
		v[vmIdx["mem_assigned_kib"]] = float64(cur.BalloonKiB)
		if cur.HasGuest && cur.UnusedKiB <= cur.AvailKiB {
			v[vmIdx["mem_used_kib"]] = float64(cur.AvailKiB - cur.UnusedKiB)
		}
	}
	if cur.HasRSS {
		v[vmIdx["mem_rss_kib"]] = float64(cur.RSSKiB)
	}
	var devs []DevRow
	if prev == nil {
		return v, devs
	}
	dt := cur.At.Sub(prev.At).Seconds()
	if dt <= 0 || cur.At.Sub(prev.At) > MaxGap {
		return v, devs
	}
	if cur.HasCPU && prev.HasCPU && cur.VCPUs > 0 {
		v[vmIdx["cpu"]] = pct(rate(prev.CPUNanos, cur.CPUNanos, dt) / 1e9 / float64(cur.VCPUs) * 100)
	}
	if cur.HasDelay && prev.HasDelay && cur.VCPUs > 0 {
		v[vmIdx["cpu_delay"]] = pct(rate(prev.DelayNanos, cur.DelayNanos, dt) / 1e9 / float64(cur.VCPUs) * 100)
	}

	// Disks: per device, then summed. One device that went backwards makes
	// the sum unknown for the step (a partial sum would look like a dip).
	var sRd, sWr, sRdOps, sWrOps, sNanos, sOps float64
	seenDisk, diskOK := false, true
	for name, c := range cur.Disks {
		p, ok := prev.Disks[name]
		if !ok {
			continue
		}
		dRd, o1 := delta(p.RdBytes, c.RdBytes)
		dWr, o2 := delta(p.WrBytes, c.WrBytes)
		dRq, o3 := delta(p.RdReqs, c.RdReqs)
		dWq, o4 := delta(p.WrReqs, c.WrReqs)
		dRt, o5 := delta(p.RdNanos, c.RdNanos)
		dWt, o6 := delta(p.WrNanos, c.WrNanos)
		d := newDev("disk", name)
		if o1 && o2 && o3 && o4 && o5 && o6 {
			d.Vals[0], d.Vals[1] = float64(dRd)/dt, float64(dWr)/dt
			d.Vals[2], d.Vals[3] = float64(dRq)/dt, float64(dWq)/dt
			d.Vals[4] = latency(dRt+dWt, true, dRq+dWq, true)
			sRd += d.Vals[0]
			sWr += d.Vals[1]
			sRdOps += d.Vals[2]
			sWrOps += d.Vals[3]
			sNanos += float64(dRt + dWt)
			sOps += float64(dRq + dWq)
			seenDisk = true
		} else {
			diskOK = false
		}
		devs = append(devs, d)
	}
	if seenDisk && diskOK {
		v[vmIdx["disk_rd_bps"]], v[vmIdx["disk_wr_bps"]] = sRd, sWr
		v[vmIdx["disk_rd_ops"]], v[vmIdx["disk_wr_ops"]] = sRdOps, sWrOps
		if sOps > 0 {
			v[vmIdx["disk_lat_ms"]] = sNanos / sOps / 1e6
		}
	}

	var sRx, sTx, sRxD, sTxD float64
	seenNet, netOK := false, true
	for name, c := range cur.Nets {
		p, ok := prev.Nets[name]
		if !ok {
			continue
		}
		d := newDev("vnet", name)
		rx, o1 := delta(p.RxBytes, c.RxBytes)
		tx, o2 := delta(p.TxBytes, c.TxBytes)
		rd, o3 := delta(p.RxDrop, c.RxDrop)
		td, o4 := delta(p.TxDrop, c.TxDrop)
		if o1 && o2 && o3 && o4 {
			d.Vals[0], d.Vals[1] = float64(rx)*8/dt, float64(tx)*8/dt
			d.Vals[2], d.Vals[3] = float64(rd)/dt, float64(td)/dt
			sRx += d.Vals[0]
			sTx += d.Vals[1]
			sRxD += d.Vals[2]
			sTxD += d.Vals[3]
			seenNet = true
		} else {
			netOK = false
		}
		devs = append(devs, d)
	}
	if seenNet && netOK {
		v[vmIdx["net_rx_bps"]], v[vmIdx["net_tx_bps"]] = sRx, sTx
		v[vmIdx["net_rx_drop"]], v[vmIdx["net_tx_drop"]] = sRxD, sTxD
	}
	return v, devs
}

// --- the node ---

// NodeRaw is what one run of NodeReadCmd found, plus the pool figures.
type NodeRaw struct {
	At time.Time

	HasCPU bool
	CPU    [8]uint64 // user nice system idle iowait irq softirq steal, in jiffies

	HasLoad bool
	Load    [3]float64

	HasMem                                                                bool
	MemTotal, MemFree, MemAvail, MemBuffers, MemCached, SwapTot, SwapFree uint64 // KiB

	Nets   []NodeNet
	Blocks []NodeBlock
	GPUs   []GPU
	Pools  []Pool
}

type NodeNet struct {
	Name                                             string
	Physical                                         bool
	RxBytes, TxBytes, RxErrs, TxErrs, RxDrop, TxDrop uint64
}

// NodeBlock holds the counters of /sys/block/NAME/stat.
type NodeBlock struct {
	Name                                                 string
	RdOps, RdSectors, RdMs, WrOps, WrSectors, WrMs, IOMs uint64
}

type GPU struct {
	Index, Name                          string
	Util, MemUsedMiB, MemTotalMiB, TempC float64 // NaN when the card does not say
}

// Pool is a storage pool's size figures in bytes.
type Pool struct {
	Name                        string
	Capacity, Allocation, Avail uint64
}

// NodeReadCmd is the one command run on a node per interval. It uses shell
// built-ins only (no process is started except nvidia-smi, when it exists).
const NodeReadCmd = `LC_ALL=C; export LC_ALL
echo '#stat'; read -r l < /proc/stat; echo "$l"
echo '#load'; read -r l < /proc/loadavg; echo "$l"
echo '#mem'; while read -r k v u; do case $k in MemTotal:|MemFree:|MemAvailable:|Buffers:|Cached:|SwapTotal:|SwapFree:) echo "$k $v";; esac; done < /proc/meminfo
echo '#net'
for d in /sys/class/net/*; do n=${d##*/}; case $n in lo|vnet*|tap*|veth*|docker*|virbr*-nic) continue;; esac
 [ -r $d/statistics/rx_bytes ] || continue
 k=net; [ -e $d/device ] && k=nic
 s="$k $n"; for f in rx_bytes tx_bytes rx_errors tx_errors rx_dropped tx_dropped; do read -r v < $d/statistics/$f; s="$s $v"; done; echo "$s"
done
echo '#blk'
for d in /sys/block/*; do n=${d##*/}; case $n in loop*|ram*|zram*|nbd*|sr*|fd*|dm-*|md*) continue;; esac; read -r l < $d/stat && echo "$n $l"; done
echo '#gpu'
if command -v nvidia-smi >/dev/null 2>&1; then timeout 5 nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits 2>/dev/null; fi
exit 0`

func u(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

func fl(s string) float64 {
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return NaN // "[N/A]" and the like
	}
	return f
}

// ParseNodeRead turns the output of NodeReadCmd into counters. Sections that
// are missing or garbled stay empty; the caller decides what that means.
func ParseNodeRead(at time.Time, out string) NodeRaw {
	r := NodeRaw{At: at}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			section = line
			continue
		}
		f := strings.Fields(line)
		switch section {
		case "#stat":
			if len(f) >= 9 && f[0] == "cpu" {
				for i := 0; i < 8; i++ {
					r.CPU[i] = u(f[i+1])
				}
				r.HasCPU = true
			}
		case "#load":
			if len(f) >= 3 {
				r.HasLoad = true
				for i := 0; i < 3; i++ {
					r.Load[i] = fl(f[i])
				}
			}
		case "#mem":
			if len(f) == 2 {
				r.HasMem = true
				n := u(f[1])
				switch f[0] {
				case "MemTotal:":
					r.MemTotal = n
				case "MemFree:":
					r.MemFree = n
				case "MemAvailable:":
					r.MemAvail = n
				case "Buffers:":
					r.MemBuffers = n
				case "Cached:":
					r.MemCached = n
				case "SwapTotal:":
					r.SwapTot = n
				case "SwapFree:":
					r.SwapFree = n
				}
			}
		case "#net":
			if len(f) == 8 {
				r.Nets = append(r.Nets, NodeNet{Name: f[1], Physical: f[0] == "nic",
					RxBytes: u(f[2]), TxBytes: u(f[3]), RxErrs: u(f[4]), TxErrs: u(f[5]), RxDrop: u(f[6]), TxDrop: u(f[7])})
			}
		case "#blk":
			if len(f) >= 11 { // name + at least ten counters
				b := NodeBlock{Name: f[0], RdOps: u(f[1]), RdSectors: u(f[3]), RdMs: u(f[4]), WrOps: u(f[5]), WrSectors: u(f[7]), WrMs: u(f[8])}
				if len(f) >= 11 {
					b.IOMs = u(f[10])
				}
				r.Blocks = append(r.Blocks, b)
			}
		case "#gpu":
			p := strings.Split(line, ",")
			if len(p) == 6 {
				r.GPUs = append(r.GPUs, GPU{Index: strings.TrimSpace(p[0]), Name: strings.TrimSpace(p[1]),
					Util: fl(p[2]), MemUsedMiB: fl(p[3]), MemTotalMiB: fl(p[4]), TempC: fl(p[5])})
			}
		}
	}
	return r
}

// NodeSample computes the wide row and device rows of a node. Pools and GPUs
// are gauges and need no previous sample.
func NodeSample(prev *NodeRaw, cur NodeRaw, running int) ([]float64, []DevRow) {
	v := blankRow(len(nodeCols))
	v[nodeIdx["vms_running"]] = float64(running)
	if cur.HasLoad {
		v[nodeIdx["load1"]], v[nodeIdx["load5"]], v[nodeIdx["load15"]] = cur.Load[0], cur.Load[1], cur.Load[2]
	}
	if cur.HasMem && cur.MemTotal > 0 {
		avail := cur.MemAvail
		if avail == 0 {
			avail = cur.MemFree + cur.MemBuffers + cur.MemCached
		}
		if avail > cur.MemTotal {
			avail = cur.MemTotal
		}
		v[nodeIdx["mem_total_kib"]] = float64(cur.MemTotal)
		v[nodeIdx["mem_used_kib"]] = float64(cur.MemTotal - avail)
		v[nodeIdx["mem_cached_kib"]] = float64(cur.MemCached + cur.MemBuffers)
		v[nodeIdx["mem_avail_kib"]] = float64(avail)
		if cur.SwapTot >= cur.SwapFree {
			v[nodeIdx["swap_used_kib"]] = float64(cur.SwapTot - cur.SwapFree)
		}
	}
	var devs []DevRow
	for _, p := range cur.Pools {
		d := newDev("pool", p.Name)
		d.Vals[0], d.Vals[1], d.Vals[2] = float64(p.Capacity), float64(p.Allocation), float64(p.Avail)
		devs = append(devs, d)
	}
	for _, g := range cur.GPUs {
		d := newDev("gpu", g.Index+" "+g.Name)
		d.Vals[0], d.Vals[1], d.Vals[2], d.Vals[3] = g.Util, g.MemUsedMiB, g.MemTotalMiB, g.TempC
		devs = append(devs, d)
	}
	if prev == nil {
		return v, devs
	}
	dt := cur.At.Sub(prev.At).Seconds()
	if dt <= 0 || cur.At.Sub(prev.At) > MaxGap {
		return v, devs
	}
	if cur.HasCPU && prev.HasCPU {
		var tot uint64
		var d [8]uint64
		ok := true
		for i := range d {
			var o bool
			d[i], o = delta(prev.CPU[i], cur.CPU[i])
			ok = ok && o
			tot += d[i]
		}
		if ok && tot > 0 {
			share := func(n uint64) float64 { return float64(n) / float64(tot) * 100 }
			v[nodeIdx["cpu"]] = pct(100 - share(d[3]) - share(d[4]))
			v[nodeIdx["cpu_user"]] = pct(share(d[0] + d[1]))
			v[nodeIdx["cpu_system"]] = pct(share(d[2] + d[5] + d[6]))
			v[nodeIdx["cpu_iowait"]] = pct(share(d[4]))
			v[nodeIdx["cpu_steal"]] = pct(share(d[7]))
		}
	}
	pn := map[string]NodeNet{}
	for _, n := range prev.Nets {
		pn[n.Name] = n
	}
	for _, c := range cur.Nets {
		p, ok := pn[c.Name]
		if !ok {
			continue
		}
		kind := "net"
		if c.Physical {
			kind = "nic"
		}
		d := newDev(kind, c.Name)
		rx, o1 := delta(p.RxBytes, c.RxBytes)
		tx, o2 := delta(p.TxBytes, c.TxBytes)
		if o1 && o2 {
			d.Vals[0], d.Vals[1] = float64(rx)*8/dt, float64(tx)*8/dt
		}
		d.Vals[2], d.Vals[3] = rate(p.RxErrs, c.RxErrs, dt), rate(p.TxErrs, c.TxErrs, dt)
		d.Vals[4], d.Vals[5] = rate(p.RxDrop, c.RxDrop, dt), rate(p.TxDrop, c.TxDrop, dt)
		devs = append(devs, d)
	}
	pb := map[string]NodeBlock{}
	for _, b := range prev.Blocks {
		pb[b.Name] = b
	}
	for _, c := range cur.Blocks {
		p, ok := pb[c.Name]
		if !ok {
			continue
		}
		d := newDev("bdev", c.Name)
		rs, o1 := delta(p.RdSectors, c.RdSectors)
		ws, o2 := delta(p.WrSectors, c.WrSectors)
		ro, o3 := delta(p.RdOps, c.RdOps)
		wo, o4 := delta(p.WrOps, c.WrOps)
		rm, o5 := delta(p.RdMs, c.RdMs)
		wm, o6 := delta(p.WrMs, c.WrMs)
		io, o7 := delta(p.IOMs, c.IOMs)
		if o1 && o2 && o3 && o4 {
			d.Vals[0], d.Vals[1] = float64(rs)*512/dt, float64(ws)*512/dt
			d.Vals[2], d.Vals[3] = float64(ro)/dt, float64(wo)/dt
		}
		if o7 {
			d.Vals[4] = pct(float64(io) / (dt * 1000) * 100)
		}
		if o3 && o4 && o5 && o6 && ro+wo > 0 {
			d.Vals[5] = float64(rm+wm) / float64(ro+wo)
		}
		devs = append(devs, d)
	}
	return v, devs
}
