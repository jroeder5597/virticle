// Package stats turns libvirt's cumulative counters into rates and keeps a
// short in-memory history. It knows nothing about libvirt itself: callers
// pass in the raw counters, which keeps the delta math testable.
package stats

import "time"

// Interval is the sampling period; Keep is how many samples are retained
// (10 minutes).
const (
	Interval = 5 * time.Second
	Keep     = 120
	// MaxGap is the longest time between two samples that still yields
	// rates; after a longer pause (connection lost, node busy) the counters
	// are treated as a fresh start.
	MaxGap = 4 * Interval
)

// VMRaw is one VM's counters at one instant. Fields the hypervisor did not
// report stay zero with their Has flag false.
type VMRaw struct {
	At time.Time

	VCPUs    uint
	HasCPU   bool
	CPUNanos uint64 // cumulative CPU time over all vCPUs

	BalloonKiB    uint64 // memory currently given to the guest
	AvailableKiB  uint64 // guest-visible RAM as the balloon driver sees it
	UnusedKiB     uint64 // free inside the guest
	HasBalloon    bool
	HasGuestStats bool // AvailableKiB and UnusedKiB are meaningful

	HasDisk             bool
	DiskRead, DiskWrite uint64 // cumulative bytes over all disks

	HasNet bool
	NetRx  uint64 // cumulative bytes over all interfaces
	NetTx  uint64
}

// VMPoint is one chart sample. A nil value means "not known for this
// sample" (first sample, counter reset, field not reported).
type VMPoint struct {
	T int64 `json:"t"` // unix milliseconds

	CPU *float64 `json:"cpu"` // percent of the VM's vCPUs, 0..100

	// MemUsedKiB is what the guest uses when the balloon driver reports it,
	// else the memory allocated to the guest; MemGuest says which.
	MemUsedKiB  *uint64 `json:"memUsedKiB"`
	MemTotalKiB *uint64 `json:"memTotalKiB"`
	MemGuest    bool    `json:"memGuest"`

	DiskRead  *float64 `json:"diskRead"` // bytes per second
	DiskWrite *float64 `json:"diskWrite"`
	NetRx     *float64 `json:"netRx"`
	NetTx     *float64 `json:"netTx"`
}

// rate is the per-second increase of a cumulative counter, or nil when it
// cannot be known: a counter that went backwards (device removed, guest
// restarted) or no time elapsed.
func rate(prev, cur uint64, dt time.Duration) *float64 {
	if cur < prev || dt <= 0 {
		return nil
	}
	v := float64(cur-prev) / dt.Seconds()
	return &v
}

func usable(prev *VMRaw, cur VMRaw) (time.Duration, bool) {
	if prev == nil {
		return 0, false
	}
	dt := cur.At.Sub(prev.At)
	return dt, dt > 0 && dt <= MaxGap
}

// NewVMPoint computes the sample for cur given the previous raw counters
// (nil for the first sample of a VM).
func NewVMPoint(prev *VMRaw, cur VMRaw) VMPoint {
	p := VMPoint{T: cur.At.UnixMilli()}
	if cur.HasBalloon {
		total := cur.BalloonKiB
		used := total
		if cur.HasGuestStats && cur.AvailableKiB > 0 && cur.UnusedKiB <= cur.AvailableKiB {
			// "available" is the RAM the guest OS sees, a little less than
			// the balloon; using it as the total keeps used/total consistent.
			total = cur.AvailableKiB
			used = cur.AvailableKiB - cur.UnusedKiB
			p.MemGuest = true
		}
		p.MemUsedKiB, p.MemTotalKiB = &used, &total
	}
	dt, ok := usable(prev, cur)
	if !ok {
		return p
	}
	if cur.HasCPU && prev.HasCPU && cur.VCPUs > 0 {
		if r := rate(prev.CPUNanos, cur.CPUNanos, dt); r != nil {
			p.CPU = clamp(*r/1e9/float64(cur.VCPUs)*100, 100)
		}
	}
	if cur.HasDisk && prev.HasDisk {
		p.DiskRead = rate(prev.DiskRead, cur.DiskRead, dt)
		p.DiskWrite = rate(prev.DiskWrite, cur.DiskWrite, dt)
	}
	if cur.HasNet && prev.HasNet {
		p.NetRx = rate(prev.NetRx, cur.NetRx, dt)
		p.NetTx = rate(prev.NetTx, cur.NetTx, dt)
	}
	return p
}

func clamp(v, max float64) *float64 {
	if v < 0 {
		v = 0
	}
	if v > max {
		v = max
	}
	return &v
}

// HostRaw is the host's counters at one instant.
type HostRaw struct {
	At time.Time

	HasCPU bool
	// Busy and Total are cumulative CPU nanoseconds over all cores: Total is
	// the sum of every state, Busy excludes idle and iowait.
	Busy, Total uint64

	HasMem                      bool
	MemTotalKiB, MemFreeKiB     uint64
	MemBuffersKiB, MemCachedKiB uint64
}

// HostPoint is one host chart sample.
type HostPoint struct {
	T           int64    `json:"t"`
	CPU         *float64 `json:"cpu"` // percent of all cores
	MemUsedKiB  *uint64  `json:"memUsedKiB"`
	MemTotalKiB *uint64  `json:"memTotalKiB"`
}

// NewHostPoint computes the host sample. Used memory excludes buffers and
// page cache, like `free`'s "used" column.
func NewHostPoint(prev *HostRaw, cur HostRaw) HostPoint {
	p := HostPoint{T: cur.At.UnixMilli()}
	if cur.HasMem && cur.MemTotalKiB > 0 {
		reclaim := cur.MemFreeKiB + cur.MemBuffersKiB + cur.MemCachedKiB
		used := uint64(0)
		if cur.MemTotalKiB > reclaim {
			used = cur.MemTotalKiB - reclaim
		}
		total := cur.MemTotalKiB
		p.MemUsedKiB, p.MemTotalKiB = &used, &total
	}
	if prev != nil && cur.HasCPU && prev.HasCPU {
		dt := cur.At.Sub(prev.At)
		if dt > 0 && dt <= MaxGap && cur.Total > prev.Total && cur.Busy >= prev.Busy {
			p.CPU = clamp(float64(cur.Busy-prev.Busy)/float64(cur.Total-prev.Total)*100, 100)
		}
	}
	return p
}

// Ring keeps the newest Keep values.
type Ring[T any] struct {
	buf   []T
	start int
}

// Push appends v, dropping the oldest value when full.
func (r *Ring[T]) Push(v T) {
	if len(r.buf) < Keep {
		r.buf = append(r.buf, v)
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % Keep
}

// Slice returns the values oldest first, as a copy.
func (r *Ring[T]) Slice() []T {
	out := make([]T, 0, len(r.buf))
	out = append(out, r.buf[r.start:]...)
	out = append(out, r.buf[:r.start]...)
	return out
}

// Last returns the newest value.
func (r *Ring[T]) Last() (T, bool) {
	var zero T
	if len(r.buf) == 0 {
		return zero, false
	}
	return r.buf[(r.start+len(r.buf)-1)%len(r.buf)], true
}
