package metrics

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"
)

var t0 = time.Unix(1_800_000_000, 0)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if isNaN(got) || math.Abs(got-want) > 0.01 {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}

func dom(at time.Time, cpuNs, rd, rdReqs, rdNs, rx uint64) DomRaw {
	return DomRaw{At: at, VCPUs: 2, HasCPU: true, CPUNanos: cpuNs, HasBalloon: true, BalloonKiB: 1 << 20, HasGuest: true, AvailKiB: 1 << 20, UnusedKiB: 1 << 19,
		Disks: map[string]DiskRaw{"vda": {RdBytes: rd, RdReqs: rdReqs, RdNanos: rdNs}},
		Nets:  map[string]NetRaw{"vnet0": {RxBytes: rx}}}
}

func TestVMRates(t *testing.T) {
	prev := dom(t0, 1e9, 1000, 10, 0, 100)
	cur := dom(t0.Add(15*time.Second), 1e9+15e9, 1000+15000, 40, 30*1e6, 100+1500)
	v, devs := VMSample(&prev, cur, 7)
	near(t, "cpu", v[vmIdx["cpu"]], 50) // 15 CPU-seconds in 15 s on 2 vCPUs
	near(t, "disk_rd_bps", v[vmIdx["disk_rd_bps"]], 1000)
	near(t, "disk_rd_ops", v[vmIdx["disk_rd_ops"]], 2)
	near(t, "disk_lat_ms", v[vmIdx["disk_lat_ms"]], 1) // 30 ms over 30 operations
	near(t, "net_rx_bps", v[vmIdx["net_rx_bps"]], 800)
	near(t, "mem_used", v[vmIdx["mem_used_kib"]], 1<<19)
	near(t, "node", v[vmIdx["node"]], 7)
	if len(devs) != 2 {
		t.Fatalf("devices: %+v", devs)
	}
}

func TestVMFirstSampleAndGapHaveNoRates(t *testing.T) {
	cur := dom(t0, 5e9, 9, 9, 9, 9)
	v, _ := VMSample(nil, cur, 1)
	if !isNaN(v[vmIdx["cpu"]]) || !isNaN(v[vmIdx["disk_rd_bps"]]) {
		t.Errorf("rates on the first sample: %v", v)
	}
	if isNaN(v[vmIdx["mem_assigned_kib"]]) {
		t.Errorf("gauges should still be filled")
	}
	prev := dom(t0, 1e9, 0, 0, 0, 0)
	late := dom(t0.Add(5*time.Minute), 9e9, 0, 0, 0, 0)
	v, _ = VMSample(&prev, late, 1)
	if !isNaN(v[vmIdx["cpu"]]) {
		t.Errorf("a long pause must not produce a rate: %v", v[vmIdx["cpu"]])
	}
}

func TestVMCounterResetIsAGapNotASpike(t *testing.T) {
	prev := dom(t0, 100e9, 1_000_000, 100, 0, 5000)
	cur := dom(t0.Add(15*time.Second), 1e9, 10, 1, 0, 5) // VM restarted: all counters smaller
	v, devs := VMSample(&prev, cur, 1)
	for _, n := range []string{"cpu", "disk_rd_bps", "disk_rd_ops", "net_rx_bps"} {
		if !isNaN(v[vmIdx[n]]) {
			t.Errorf("%s after a reset should be unknown, got %v", n, v[vmIdx[n]])
		}
	}
	for _, d := range devs {
		if !isNaN(d.Vals[0]) {
			t.Errorf("device %s after reset: %v", d.Name, d.Vals)
		}
	}
}

func TestVMTwoDisksOneResetMakesSumUnknown(t *testing.T) {
	prev := dom(t0, 0, 0, 0, 0, 0)
	prev.Disks = map[string]DiskRaw{"vda": {RdBytes: 1000}, "vdb": {RdBytes: 1000}}
	cur := dom(t0.Add(15*time.Second), 0, 0, 0, 0, 0)
	cur.Disks = map[string]DiskRaw{"vda": {RdBytes: 16000}, "vdb": {RdBytes: 5}}
	v, devs := VMSample(&prev, cur, 1)
	if !isNaN(v[vmIdx["disk_rd_bps"]]) {
		t.Errorf("a partial sum would look like a dip: %v", v[vmIdx["disk_rd_bps"]])
	}
	good := 0
	for _, d := range devs {
		if d.Kind == "disk" && !isNaN(d.Vals[0]) {
			good++
			near(t, "vda", d.Vals[0], 1000)
		}
	}
	if good != 1 {
		t.Errorf("the intact disk keeps its rate: %+v", devs)
	}
}

func TestParseDomain(t *testing.T) {
	str := func(s string) libvirt.TypedParamValue { return libvirt.TypedParamValue{I: s} }
	num := func(n uint64) libvirt.TypedParamValue { return libvirt.TypedParamValue{I: n} }
	r := ParseDomain(t0, []libvirt.TypedParam{
		{Field: "cpu.time", Value: num(5)}, {Field: "vcpu.current", Value: num(2)},
		{Field: "vcpu.0.delay", Value: num(10)}, {Field: "vcpu.1.delay", Value: num(20)},
		{Field: "balloon.current", Value: num(1024)}, {Field: "balloon.available", Value: num(1000)}, {Field: "balloon.unused", Value: num(400)}, {Field: "balloon.rss", Value: num(900)},
		{Field: "block.0.name", Value: str("vda")}, {Field: "block.0.rd.bytes", Value: num(7)}, {Field: "block.0.wr.times", Value: num(9)},
		{Field: "block.1.name", Value: str("sda")}, {Field: "block.1.rd.reqs", Value: num(3)},
		{Field: "net.0.name", Value: str("vnet3")}, {Field: "net.0.tx.bytes", Value: num(44)}, {Field: "net.0.rx.drop", Value: num(2)},
	})
	if r.CPUNanos != 5 || r.VCPUs != 2 || r.DelayNanos != 30 || !r.HasRSS || r.RSSKiB != 900 || !r.HasGuest || r.UnusedKiB != 400 {
		t.Errorf("%+v", r)
	}
	if r.Disks["vda"].RdBytes != 7 || r.Disks["vda"].WrNanos != 9 || r.Disks["sda"].RdReqs != 3 || r.Nets["vnet3"].TxBytes != 44 || r.Nets["vnet3"].RxDrop != 2 {
		t.Errorf("devices: %+v %+v", r.Disks, r.Nets)
	}
}

const nodeOut = `#stat
cpu  1000 0 500 8000 300 0 0 200 0 0
#load
0.52 0.40 0.30 1/300 999
#mem
MemTotal: 8000000
MemFree: 1000000
MemAvailable: 5000000
Buffers: 100000
Cached: 2000000
SwapTotal: 1000000
SwapFree: 900000
#net
nic eth0 1000 2000 1 2 3 4
net br0 500 600 0 0 0 0
#blk
sda 100 0 2048 50 200 0 4096 150 0 400 0 0
#gpu
0, NVIDIA T4, 37, 1200, 15360, [N/A]
`

func TestParseAndNodeSample(t *testing.T) {
	a := ParseNodeRead(t0, nodeOut)
	if !a.HasCPU || !a.HasMem || a.CPU[3] != 8000 || len(a.Nets) != 2 || !a.Nets[0].Physical || a.Nets[1].Physical || len(a.Blocks) != 1 || len(a.GPUs) != 1 {
		t.Fatalf("%+v", a)
	}
	if !isNaN(a.GPUs[0].TempC) || a.GPUs[0].Util != 37 {
		t.Errorf("gpu: %+v", a.GPUs[0])
	}
	b := a
	b.At = t0.Add(15 * time.Second)
	b.CPU = [8]uint64{1000 + 100, 0, 500 + 50, 8000 + 800, 300 + 40, 0, 0, 200 + 10}
	b.Nets = []NodeNet{{Name: "eth0", Physical: true, RxBytes: 1000 + 1500, TxBytes: 2000, RxErrs: 1, TxErrs: 2, RxDrop: 3, TxDrop: 4}, {Name: "br0", RxBytes: 500, TxBytes: 600}}
	b.Blocks = []NodeBlock{{Name: "sda", RdOps: 100 + 15, RdSectors: 2048 + 30, RdMs: 50 + 30, WrOps: 200 + 15, WrSectors: 4096, WrMs: 150 + 30, IOMs: 400 + 1500}}
	v, devs := NodeSample(&a, b, 3)
	// 1000 jiffies in the step: user 100, system 50, idle 800, iowait 40, steal 10
	total := 1000.0
	near(t, "cpu", v[nodeIdx["cpu"]], 100-800/total*100-40/total*100)
	near(t, "cpu_user", v[nodeIdx["cpu_user"]], 10)
	near(t, "cpu_iowait", v[nodeIdx["cpu_iowait"]], 4)
	near(t, "mem_used", v[nodeIdx["mem_used_kib"]], 3_000_000)
	near(t, "swap", v[nodeIdx["swap_used_kib"]], 100_000)
	near(t, "load", v[nodeIdx["load1"]], 0.52)
	near(t, "running", v[nodeIdx["vms_running"]], 3)
	var nic, bdev, gpu *DevRow
	for i := range devs {
		switch devs[i].Kind {
		case "nic":
			nic = &devs[i]
		case "bdev":
			bdev = &devs[i]
		case "gpu":
			gpu = &devs[i]
		}
	}
	if nic == nil || bdev == nil || gpu == nil {
		t.Fatalf("devices: %+v", devs)
	}
	near(t, "nic rx", nic.Vals[0], 1500*8/15.0)
	near(t, "bdev rd", bdev.Vals[0], 30*512/15.0)
	near(t, "bdev busy", bdev.Vals[4], 1500/15000.0*100)
	near(t, "bdev lat", bdev.Vals[5], 60.0/30)
	near(t, "gpu util", gpu.Vals[0], 37)
}

// --- storage ---

func newDB(t *testing.T, start time.Time) (*DB, *time.Time) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := start
	d.SetClock(func() time.Time { return now })
	return d, &now
}

func vmRow(uuid string, node int64, cpu float64, devs ...DevRow) Row {
	v := blankRow(len(vmCols))
	v[vmIdx["node"]] = float64(node)
	v[vmIdx["cpu"]] = cpu
	v[vmIdx["net_rx_bps"]] = cpu * 10
	return Row{Ent: "vm:" + uuid, Label: "vm-" + uuid, Vals: v, Devs: devs}
}

func TestRollupKeepsAverageAndPeak(t *testing.T) {
	d, now := newDB(t, time.Unix(1_800_000_000, 0).Truncate(time.Hour).Add(time.Hour))
	base := now.Add(-30 * time.Minute).Unix()
	base = base / 300 * 300
	// One 5-minute bucket of 20 samples at 15 s: 19 at 10 percent, one spike of 90.
	for i := 0; i < 20; i++ {
		c := 10.0
		if i == 7 {
			c = 90
		}
		if err := d.Insert(Batch{T: base + int64(i)*15, Rows: []Row{vmRow("a", 1, c)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Maintain(); err != nil {
		t.Fatal(err)
	}
	var avg, peak float64
	if err := d.db.QueryRow(`SELECT cpu, cpu_max FROM vm_2 WHERE t=?`, base).Scan(&avg, &peak); err != nil {
		t.Fatal(err)
	}
	near(t, "5 min average", avg, (19*10+90)/20.0)
	near(t, "5 min peak", peak, 90)
	if err := d.db.QueryRow(`SELECT cpu, cpu_max FROM vm_1 WHERE t=?`, base+60).Scan(&avg, &peak); err != nil {
		t.Fatal(err)
	}
	near(t, "1 min average (samples 4..7)", avg, (10+10+10+90)/4.0)
	near(t, "1 min peak", peak, 90)
}

func TestQueryShapeGapsAndEvents(t *testing.T) {
	d, now := newDB(t, time.Unix(1_800_000_000, 0))
	base := now.Unix()/15*15 - 1200
	// 20 minutes: running on node 1 for 5, a gap of 5, running on node 2 for 10.
	for i := 0; i < 80; i++ {
		ts := base + int64(i)*15
		var rows []Row
		switch {
		case i < 20:
			rows = []Row{vmRow("a", 1, 20)}
		case i < 40:
		default:
			rows = []Row{vmRow("a", 2, 30, DevRow{Kind: "disk", Name: "vda", Vals: [nDev]float64{1000, 2000, 1, 2, 3, NaN}})}
		}
		if i%10 == 0 {
			// Another VM and a node, to check that they do not leak into the answer.
			rows = append(rows, vmRow("b", 1, 99))
		}
		if err := d.Insert(Batch{T: ts, Rows: rows}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := d.Query(Request{Kind: "vm", ID: "a", From: time.Unix(base, 0), To: time.Unix(base+1199, 0), Groups: []string{"cpu", "disk"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Step != 15 || res.Peak || len(res.T) != 80 {
		t.Fatalf("step %d peak %v points %d", res.Step, res.Peak, len(res.T))
	}
	cpu := res.Series["cpu"]
	if len(cpu) != len(res.T) || cpu[0] != 20 || !isNaN(cpu[25]) || cpu[50] != 30 {
		t.Errorf("cpu: %v", cpu)
	}
	if _, ok := res.Series["net_rx_bps"]; ok {
		t.Errorf("a group that was not asked for is in the answer")
	}
	var kinds []string
	for _, e := range res.Events {
		kinds = append(kinds, fmt.Sprintf("%s@%d", e.Kind, (e.T-base)/15))
	}
	if strings.Join(kinds, " ") != "stopped@19 started@40" {
		// the node moved at the same time as the gap: a stop and a start, not a migration
		t.Errorf("events: %v", kinds)
	}
	if len(res.Devices["disk"]) != 1 || res.Devices["disk"][0].ID != "disk:vda" {
		t.Errorf("devices: %+v", res.Devices)
	}
	one, err := d.Query(Request{Kind: "vm", ID: "a", From: time.Unix(base, 0), To: time.Unix(base+1199, 0), Groups: []string{"disk"}, Device: "disk:vda"})
	if err != nil {
		t.Fatal(err)
	}
	if v := one.Series["disk_wr_bps"]; v[50] != 2000 || !isNaN(v[0]) {
		t.Errorf("device series: %v", v)
	}
	// JSON: gaps are null.
	b, _ := res.Series["cpu"].MarshalJSON()
	if !strings.Contains(string(b), "null") || strings.Contains(string(b), "NaN") {
		t.Errorf("json: %s", b[:80])
	}
	// CSV
	var buf bytes.Buffer
	if err := res.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 81 || !strings.HasPrefix(lines[0], "time,cpu") {
		t.Errorf("csv: %d lines, header %q", len(lines), lines[0])
	}
}

func TestMigrationWithoutGapIsMigrated(t *testing.T) {
	d, now := newDB(t, time.Unix(1_800_000_000, 0))
	base := now.Unix()/15*15 - 600
	for i := 0; i < 40; i++ {
		node := int64(1)
		if i >= 20 {
			node = 2
		}
		if err := d.Insert(Batch{T: base + int64(i)*15, Rows: []Row{vmRow("a", node, 5)}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := d.Query(Request{Kind: "vm", ID: "a", From: time.Unix(base, 0), To: time.Unix(base+599, 0), Groups: []string{"cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Kind != "migrated" || res.Events[0].Node != 2 {
		t.Errorf("events: %+v", res.Events)
	}
}

func TestNodeQuerySumsDevicesAndListsGroups(t *testing.T) {
	d, now := newDB(t, time.Unix(1_800_000_000, 0))
	base := now.Unix()/15*15 - 300
	for i := 0; i < 20; i++ {
		v := blankRow(len(nodeCols))
		v[nodeIdx["cpu"]] = 40
		v[nodeIdx["load1"]] = 1.5
		row := Row{Ent: "node:3", Label: "n3", Vals: v, Devs: []DevRow{
			{Kind: "nic", Name: "eth0", Vals: [nDev]float64{100, 200, 0, 0, 0, 0}},
			{Kind: "nic", Name: "eth1", Vals: [nDev]float64{10, 20, 0, 0, 0, 0}},
			{Kind: "net", Name: "br0", Vals: [nDev]float64{110, 220, 0, 0, 0, 0}},
			{Kind: "pool", Name: "default", Vals: [nDev]float64{1000, 400, 600, NaN, NaN, NaN}},
		}}
		if err := d.Insert(Batch{T: base + int64(i)*15, Rows: []Row{row}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := d.Query(Request{Kind: "node", ID: "3", From: time.Unix(base, 0), To: time.Unix(base+299, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Series["net_rx_bps"][5] != 110 {
		t.Errorf("All (sum) counts physical cards only, not the bridge: %v", res.Series["net_rx_bps"][5])
	}
	if res.Series["cpu"][5] != 40 || res.Series["load1"][5] != 1.5 {
		t.Errorf("wide row: %v", res.Series["cpu"])
	}
	if res.Series["pool_used_bytes/default"][3] != 400 {
		t.Errorf("pools are one series each: %v", res.Series)
	}
	if len(res.Devices["network"]) != 3 || len(res.Devices["storage"]) != 1 || len(res.Devices["gpu"]) != 0 {
		t.Errorf("devices: %+v", res.Devices)
	}
	br, _ := d.Query(Request{Kind: "node", ID: "3", From: time.Unix(base, 0), To: time.Unix(base+299, 0), Groups: []string{"network"}, Device: "net:br0"})
	if br.Series["net_tx_bps"][2] != 220 {
		t.Errorf("single device: %v", br.Series)
	}
	if _, err := d.Query(Request{Kind: "vm", ID: "x", From: time.Unix(base, 0), To: time.Unix(base+1, 0), Groups: []string{"bogus"}}); err == nil {
		t.Error("unknown group accepted")
	}
}

func TestChooseStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		back, span time.Duration
		step       int64
		tier       int
	}{
		{time.Hour, time.Hour, 15, 0},
		{2 * time.Hour, 2 * time.Hour, 15, 0},
		{6 * time.Hour, 6 * time.Hour, 60, 1},
		{24 * time.Hour, 24 * time.Hour, 180, 1},
		{7 * 24 * time.Hour, 7 * 24 * time.Hour, 1200, 2},
		{50 * time.Hour, 10 * time.Minute, 300, 2}, // an old short range: only the 5-minute tier still has it
		{5 * time.Hour, 10 * time.Minute, 60, 1},
	}
	for _, c := range cases {
		from := now.Add(-c.back)
		tier, step := ChooseStep(now, from, from.Add(c.span))
		if tier != c.tier || step != c.step {
			t.Errorf("back %v span %v: tier %d step %d, want %d %d", c.back, c.span, tier, step, c.tier, c.step)
		}
		if n := c.span.Seconds() / float64(step); n > MaxPoints+1 {
			t.Errorf("%v points", n)
		}
	}
}

func TestRank(t *testing.T) {
	d, now := newDB(t, time.Unix(1_800_000_000, 0))
	base := now.Unix()/15*15 - 600
	for i := 0; i < 40; i++ {
		rows := []Row{vmRow("a", 1, 10), vmRow("b", 1, 50), vmRow("c", 2, 30)}
		if i == 3 {
			rows[0] = vmRow("a", 1, 100)
		}
		if err := d.Insert(Batch{T: base + int64(i)*15, Rows: rows}); err != nil {
			t.Fatal(err)
		}
	}
	o, err := d.Rank(time.Unix(base, 0), time.Unix(base+600, 0), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Cpu) != 2 || o.Cpu[0].UUID != "b" || o.Cpu[1].UUID != "c" || o.Cpu[0].Name != "vm-b" || o.Cpu[1].Node != 2 {
		t.Errorf("cpu ranking: %+v", o.Cpu)
	}
	near(t, "peak", o.Cpu[0].Max, 50)
	o, _ = d.Rank(time.Unix(base, 0), time.Unix(base+600, 0), 1, 5)
	if len(o.Cpu) != 2 || o.Cpu[0].UUID != "b" || o.Cpu[1].UUID != "a" {
		t.Errorf("node filter: %+v", o.Cpu)
	}
	if o.Cpu[1].Max != 100 || len(o.Memory) != 0 {
		t.Errorf("a's spike should show as its peak: %+v / memory %+v", o.Cpu[1], o.Memory)
	}
	if len(o.Network) != 2 || o.Network[0].UUID != "b" {
		t.Errorf("network: %+v", o.Network)
	}
}

// Eight simulated days of writes with maintenance every half minute: the
// file must stop growing once the seven days are full, and the oldest data
// must be gone.
func TestEightDaysSizeStaysFlat(t *testing.T) {
	start := time.Unix(1_800_000_000, 0).Truncate(time.Hour)
	d, now := newDB(t, start)
	size := func() int64 {
		d.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		fi, err := os.Stat(d.path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	var sizes []int64
	var pending []Batch
	flush := func() {
		if err := d.InsertMany(pending); err != nil {
			t.Fatal(err)
		}
		pending = pending[:0]
	}
	end := start.Add(8 * 24 * time.Hour)
	for ts := start; ts.Before(end); ts = ts.Add(15 * time.Second) {
		*now = ts
		var rows []Row
		for i := 0; i < 3; i++ {
			rows = append(rows, vmRow(fmt.Sprint("vm", i), 1, float64(ts.Unix()%100),
				DevRow{Kind: "disk", Name: "vda", Vals: [nDev]float64{1, 2, 3, 4, 5, NaN}},
				DevRow{Kind: "vnet", Name: "vnet0", Vals: [nDev]float64{1, 2, 3, 4, NaN, NaN}}))
		}
		nv := blankRow(len(nodeCols))
		nv[nodeIdx["cpu"]] = 10
		rows = append(rows, Row{Ent: "node:1", Label: "n1", Vals: nv, Devs: []DevRow{{Kind: "nic", Name: "eth0", Vals: [nDev]float64{1, 2, 3, 4, 5, 6}}}})
		pending = append(pending, Batch{T: ts.Unix() / 15 * 15, Rows: rows})
		if len(pending) == 240 { // an hour per transaction keeps the test fast
			flush()
			if err := d.Maintain(); err != nil {
				t.Fatal(err)
			}
			if ts.Sub(start) >= 7*24*time.Hour && ts.Sub(start)%(24*time.Hour) < time.Hour {
				sizes = append(sizes, size())
			}
		}
	}
	flush()
	d.Maintain()
	if len(sizes) < 1 {
		t.Fatalf("only %d size samples", len(sizes))
	}
	first, last := sizes[0], size()
	t.Logf("file size after 7 days %d bytes, after 8 days %d bytes", first, last)
	if float64(last) > float64(first)*1.1 {
		t.Errorf("the file keeps growing: %d -> %d", first, last)
	}
	var n int
	d.db.QueryRow(`SELECT count(*) FROM vm_2 WHERE t < ?`, end.Unix()-Tiers[2].Retention-900).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows older than 7 days remain", n)
	}
	d.db.QueryRow(`SELECT count(*) FROM vm_0 WHERE t < ?`, end.Unix()-Tiers[0].Retention-900).Scan(&n) // pruning runs every ten minutes
	if n != 0 {
		t.Errorf("%d rows older than 2 hours remain at 15 s", n)
	}
	res, err := d.Query(Request{Kind: "vm", ID: "vm1", From: end.Add(-7 * 24 * time.Hour), To: end, Groups: []string{"cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Step != 1200 || len(res.T) > MaxPoints {
		t.Errorf("7 day answer: step %d, %d points", res.Step, len(res.T))
	}
	filled := 0
	for _, v := range res.Series["cpu"] {
		if !isNaN(v) {
			filled++
		}
	}
	if filled < len(res.T)*9/10 {
		t.Errorf("only %d of %d points have data", filled, len(res.T))
	}
}

func TestOpenReplacesUnusableFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "metrics.db")
	if err := os.WriteFile(p, []byte("this is not a database, just text that is long enough to look like one......................................."), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Insert(Batch{T: 15, Rows: []Row{vmRow("a", 1, 1)}}); err != nil {
		t.Fatal(err)
	}
}
