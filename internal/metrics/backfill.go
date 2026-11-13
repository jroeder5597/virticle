package metrics

import (
	"math"
	"sort"
	"time"
)

// Backfill writes synthetic history for development: it lets the 1-minute and
// 5-minute tiers and the 7-day view be looked at without waiting days. It
// goes through the same insert and rollup code as the collector, with the
// clock moved forward an hour at a time. It is reachable only from a hidden
// developer command, never from the API.
type Backfill struct {
	VMs   map[string]string // uuid -> name
	Node  int64
	NodeN string
	Days  int
	Until time.Time // stop here; real data starts after it
}

// EarliestRow is the oldest time with data for an entity, or the zero time.
func (d *DB) EarliestRow(ent string) time.Time {
	id, ok := d.lookup(ent)
	if !ok {
		return time.Time{}
	}
	var t int64
	for tier := len(Tiers) - 1; tier >= 0; tier-- {
		table := tableFor("vm", tier)
		if len(ent) > 5 && ent[:5] == "node:" {
			table = tableFor("node", tier)
		}
		if err := d.db.QueryRow("SELECT coalesce(min(t),0) FROM "+table+" WHERE ent=?", id).Scan(&t); err == nil && t > 0 {
			return time.Unix(t, 0)
		}
	}
	return time.Time{}
}

func wave(sec int64, period, phase float64) float64 {
	return 0.5 + 0.5*math.Sin(2*math.Pi*(float64(sec)/period)+phase)
}

// Run generates the data. Gaps: every VM is off for an hour a day and moves
// to node+1 on day 3; the node is unreadable for 20 minutes on day 5.
func (b Backfill) Run(d *DB) error {
	d.db.Exec(`DELETE FROM meta`) // roll up from the start, not from where the controller left off
	saved := d.now
	defer func() { d.now = saved }()
	uuids := make([]string, 0, len(b.VMs))
	for u := range b.VMs {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	end := b.Until.Truncate(15 * time.Second)
	start := end.Add(-time.Duration(b.Days) * 24 * time.Hour)
	// Replace an earlier backfill: everything before the end of this one.
	for t := range Tiers {
		for _, e := range []string{"vm", "node", "dev"} {
			d.db.Exec("DELETE FROM "+tableFor(e, t)+" WHERE t < ?", end.Unix())
		}
	}
	var pending []Batch
	var clock time.Time
	d.now = func() time.Time { return clock }
	flush := func(at time.Time) error {
		clock = at
		if err := d.InsertMany(pending); err != nil {
			return err
		}
		pending = pending[:0]
		return d.Maintain()
	}
	n := 0
	for ts := start; ts.Before(end); ts = ts.Add(15 * time.Second) {
		sec := ts.Unix()
		day := float64(sec-start.Unix()) / 86400
		daily := wave(sec, 86400, 0)
		var rows []Row
		for i, uuid := range uuids {
			name := b.VMs[uuid]
			i++
			phase := float64(i)
			// off for one hour a day (at a different hour per VM)
			if (sec/3600+int64(i)*5)%24 == 3 {
				continue
			}
			node := b.Node
			if day > 3 && day < 3.6 {
				node = b.Node + 1
			}
			v := blankRow(len(vmCols))
			spike := 0.0
			if (sec/900)%37 == int64(i) { // a 15-minute burst now and then
				spike = 55
			}
			v[vmIdx["node"]] = float64(node)
			v[vmIdx["cpu"]] = pct(8 + 30*daily*wave(sec, 1800, phase) + spike + 4*wave(sec, 47, phase))
			v[vmIdx["cpu_delay"]] = pct(v[vmIdx["cpu"]] / 15)
			v[vmIdx["mem_assigned_kib"]] = 524288
			v[vmIdx["mem_used_kib"]] = 180000 + 150000*daily
			v[vmIdx["mem_rss_kib"]] = 330000 + 120000*daily
			rd, wr := 2e6*wave(sec, 3600, phase)+spike*1e5, 6e6*daily*wave(sec, 900, phase)+spike*2e5
			v[vmIdx["disk_rd_bps"]], v[vmIdx["disk_wr_bps"]] = rd, wr
			v[vmIdx["disk_rd_ops"]], v[vmIdx["disk_wr_ops"]] = rd/65536, wr/32768
			v[vmIdx["disk_lat_ms"]] = 1.5 + 6*wave(sec, 5400, phase)
			v[vmIdx["net_rx_bps"]], v[vmIdx["net_tx_bps"]] = 3e6*daily+spike*1e5, 8e5*daily*wave(sec, 600, phase)
			v[vmIdx["net_rx_drop"]], v[vmIdx["net_tx_drop"]] = 0, 0
			if spike > 0 {
				v[vmIdx["net_rx_drop"]] = 0.3
			}
			dv := []DevRow{
				{Kind: "disk", Name: "vda", Vals: [nDev]float64{rd, wr, rd / 65536, wr / 32768, v[vmIdx["disk_lat_ms"]], NaN}},
				{Kind: "vnet", Name: "vnet0", Vals: [nDev]float64{v[vmIdx["net_rx_bps"]], v[vmIdx["net_tx_bps"]], v[vmIdx["net_rx_drop"]], 0, NaN, NaN}},
			}
			rows = append(rows, Row{Ent: "vm:" + uuid, Label: name, Vals: v, Devs: dv})
		}
		// the node is unreadable for 20 minutes on day 5
		if !(day > 5 && day < 5+20.0/1440) {
			v := blankRow(len(nodeCols))
			cpu := 10 + 35*daily + 3*wave(sec, 61, 0)
			v[nodeIdx["cpu"]] = pct(cpu)
			v[nodeIdx["cpu_user"]] = pct(cpu * 0.7)
			v[nodeIdx["cpu_system"]] = pct(cpu * 0.25)
			v[nodeIdx["cpu_iowait"]] = pct(2 + 3*wave(sec, 3000, 1))
			v[nodeIdx["cpu_steal"]] = pct(0.5)
			v[nodeIdx["load1"]] = cpu / 25
			v[nodeIdx["load5"]] = cpu / 28
			v[nodeIdx["load15"]] = cpu / 30
			v[nodeIdx["mem_total_kib"]] = 3 << 20
			v[nodeIdx["mem_used_kib"]] = 1.2e6 + 5e5*daily
			v[nodeIdx["mem_cached_kib"]] = 6e5
			v[nodeIdx["mem_avail_kib"]] = 3<<20 - v[nodeIdx["mem_used_kib"]]
			v[nodeIdx["swap_used_kib"]] = 0
			v[nodeIdx["vms_running"]] = float64(len(b.VMs))
			dv := []DevRow{
				{Kind: "nic", Name: "eth0", Vals: [nDev]float64{4e6 * daily, 1.5e6 * daily, 0, 0, 0, 0}},
				{Kind: "net", Name: "virbr0", Vals: [nDev]float64{1e6 * daily, 3e6 * daily, 0, 0, 0, 0}},
				{Kind: "bdev", Name: "vda", Vals: [nDev]float64{3e6 * daily, 8e6 * daily, 40 * daily, 90 * daily, 2 + 25*daily, 2 + 4*daily}},
				{Kind: "pool", Name: "default", Vals: [nDev]float64{20e9, 2.5e9 + 1e8*day, 17.5e9 - 1e8*day, NaN, NaN, NaN}},
			}
			rows = append(rows, Row{Ent: "node:" + itoa(b.Node), Label: b.NodeN, Vals: v, Devs: dv})
		}
		pending = append(pending, Batch{T: sec / 15 * 15, Rows: rows})
		n++
		if n%240 == 0 {
			if err := flush(ts.Add(15 * time.Second)); err != nil {
				return err
			}
		}
	}
	return flush(end)
}
