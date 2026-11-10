package stats

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Unix(1000, 0)

func near(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil, want %v", name, want)
	}
	if math.Abs(*got-want) > 0.01 {
		t.Errorf("%s: got %v, want %v", name, *got, want)
	}
}

func TestFirstSampleHasNoRates(t *testing.T) {
	p := NewVMPoint(nil, VMRaw{At: t0, VCPUs: 2, HasCPU: true, CPUNanos: 5e9, HasDisk: true, DiskRead: 9, HasBalloon: true, BalloonKiB: 1024})
	if p.CPU != nil || p.DiskRead != nil || p.NetRx != nil {
		t.Errorf("rates on first sample: %+v", p)
	}
	if p.MemUsedKiB == nil || *p.MemUsedKiB != 1024 || p.MemGuest {
		t.Errorf("memory without guest stats should be the balloon size: %+v", p)
	}
}

func TestRates(t *testing.T) {
	prev := VMRaw{At: t0, VCPUs: 2, HasCPU: true, CPUNanos: 1e9, HasDisk: true, DiskRead: 1000, DiskWrite: 0, HasNet: true, NetRx: 10, NetTx: 20}
	cur := VMRaw{At: t0.Add(5 * time.Second), VCPUs: 2, HasCPU: true, CPUNanos: 1e9 + 5e9, HasDisk: true, DiskRead: 1000 + 5000, DiskWrite: 500, HasNet: true, NetRx: 60, NetTx: 20}
	p := NewVMPoint(&prev, cur)
	// 5 CPU-seconds in 5 s on 2 vCPUs is 50 %.
	near(t, "cpu", p.CPU, 50)
	near(t, "diskRead", p.DiskRead, 1000)
	near(t, "diskWrite", p.DiskWrite, 100)
	near(t, "netRx", p.NetRx, 10)
	near(t, "netTx", p.NetTx, 0)
}

func TestCounterResetGivesNil(t *testing.T) {
	prev := VMRaw{At: t0, VCPUs: 1, HasCPU: true, CPUNanos: 9e9, HasDisk: true, DiskRead: 100, DiskWrite: 100}
	cur := VMRaw{At: t0.Add(5 * time.Second), VCPUs: 1, HasCPU: true, CPUNanos: 1e9, HasDisk: true, DiskRead: 50, DiskWrite: 200}
	p := NewVMPoint(&prev, cur)
	if p.CPU != nil || p.DiskRead != nil {
		t.Errorf("a reset counter must not produce a rate: %+v", p)
	}
	near(t, "diskWrite", p.DiskWrite, 20) // the other counter is still fine
}

func TestMissingFields(t *testing.T) {
	prev := VMRaw{At: t0, VCPUs: 1, HasCPU: true, CPUNanos: 1}
	cur := VMRaw{At: t0.Add(5 * time.Second), VCPUs: 1, HasCPU: true, CPUNanos: 1 + 1e9}
	p := NewVMPoint(&prev, cur)
	near(t, "cpu", p.CPU, 20)
	if p.DiskRead != nil || p.NetRx != nil || p.MemUsedKiB != nil {
		t.Errorf("fields the hypervisor did not report must stay nil: %+v", p)
	}
	// vcpus unknown: no CPU percentage rather than a division by zero.
	cur.VCPUs = 0
	if NewVMPoint(&prev, cur).CPU != nil {
		t.Error("cpu without a vcpu count")
	}
}

func TestGapAndOverrun(t *testing.T) {
	prev := VMRaw{At: t0, VCPUs: 1, HasCPU: true, CPUNanos: 0}
	late := VMRaw{At: t0.Add(MaxGap + time.Second), VCPUs: 1, HasCPU: true, CPUNanos: 1e9}
	if NewVMPoint(&prev, late).CPU != nil {
		t.Error("rate across a long gap")
	}
	same := VMRaw{At: t0, VCPUs: 1, HasCPU: true, CPUNanos: 1e9}
	if NewVMPoint(&prev, same).CPU != nil {
		t.Error("rate with zero elapsed time")
	}
	// Accounting jitter can report slightly more than 100 %.
	over := VMRaw{At: t0.Add(5 * time.Second), VCPUs: 1, HasCPU: true, CPUNanos: 5.4e9}
	near(t, "clamped", NewVMPoint(&prev, over).CPU, 100)
}

func TestGuestMemory(t *testing.T) {
	cur := VMRaw{At: t0, HasBalloon: true, BalloonKiB: 524288, HasGuestStats: true, AvailableKiB: 500000, UnusedKiB: 300000}
	p := NewVMPoint(nil, cur)
	if !p.MemGuest || *p.MemUsedKiB != 200000 || *p.MemTotalKiB != 500000 {
		t.Errorf("guest memory: used %d total %d guest %v", *p.MemUsedKiB, *p.MemTotalKiB, p.MemGuest)
	}
	cur.UnusedKiB = 600000 // nonsense from the guest: fall back to the balloon
	p = NewVMPoint(nil, cur)
	if p.MemGuest || *p.MemUsedKiB != 524288 {
		t.Errorf("implausible guest stats should be ignored: %+v", p)
	}
}

func TestHostPoint(t *testing.T) {
	prev := HostRaw{At: t0, HasCPU: true, Busy: 1000, Total: 4000}
	cur := HostRaw{At: t0.Add(5 * time.Second), HasCPU: true, Busy: 1500, Total: 5000,
		HasMem: true, MemTotalKiB: 1000, MemFreeKiB: 100, MemBuffersKiB: 50, MemCachedKiB: 250}
	p := NewHostPoint(&prev, cur)
	near(t, "cpu", p.CPU, 50)
	if *p.MemUsedKiB != 600 || *p.MemTotalKiB != 1000 {
		t.Errorf("memory %d/%d", *p.MemUsedKiB, *p.MemTotalKiB)
	}
	if NewHostPoint(nil, cur).CPU != nil {
		t.Error("first host sample has no cpu rate")
	}
	cur.Total = 3000 // reset
	if NewHostPoint(&prev, cur).CPU != nil {
		t.Error("host counter reset")
	}
}

func TestRing(t *testing.T) {
	var r Ring[int]
	if _, ok := r.Last(); ok {
		t.Error("empty ring has no last")
	}
	for i := 0; i < Keep+10; i++ {
		r.Push(i)
	}
	s := r.Slice()
	if len(s) != Keep || s[0] != 10 || s[Keep-1] != Keep+9 {
		t.Errorf("ring order: len %d first %d last %d", len(s), s[0], s[len(s)-1])
	}
	if v, _ := r.Last(); v != Keep+9 {
		t.Errorf("last %d", v)
	}
}
