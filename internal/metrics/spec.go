// Package metrics keeps up to seven days of VM and node performance data in
// its own SQLite file (metrics.db), answers range queries for the web UI and
// renders the current values for Prometheus. The file is a cache: it can be
// deleted at any time and only the history is lost.
package metrics

import (
	"math"
	"strings"
)

// Tier is one resolution of the history. Every row holds all metrics of one
// entity at one time step ("wide rows"). Tier 0 is written by the collector;
// the others are rolled up from the tier before and also keep the peak.
type Tier struct {
	Step      int64 // seconds per row
	Retention int64 // seconds kept
}

// Tiers: 15 seconds for 2 hours, 1 minute for 48 hours, 5 minutes for 7 days.
// They are constants on purpose: there are no settings.
var Tiers = [3]Tier{
	{15, 2 * 3600},
	{60, 48 * 3600},
	{300, 7 * 24 * 3600},
}

// How a column is rolled up.
const (
	kGauge = 'g' // average and peak
	kAvg   = 'a' // average only
	kLast  = 'x' // largest value (identifiers such as the node a VM runs on)
)

type col struct {
	name string
	kind byte
}

// vmCols are the columns of a VM row. A row exists only for a time step in
// which the VM was running, so a stopped VM is a gap. Rates are per second.
var vmCols = []col{
	{"node", kLast},
	{"cpu", kGauge},       // percent of the VM's vCPUs
	{"cpu_delay", kGauge}, // percent of vCPU time spent waiting for a host CPU
	{"mem_assigned_kib", kAvg},
	{"mem_used_kib", kGauge}, // used inside the guest, only when the balloon driver reports it
	{"mem_rss_kib", kGauge},  // resident on the host
	{"disk_rd_bps", kGauge},
	{"disk_wr_bps", kGauge},
	{"disk_rd_ops", kGauge},
	{"disk_wr_ops", kGauge},
	{"disk_lat_ms", kGauge},
	{"net_rx_bps", kGauge}, // bits per second
	{"net_tx_bps", kGauge},
	{"net_rx_drop", kGauge},
	{"net_tx_drop", kGauge},
}

var nodeCols = []col{
	{"cpu", kGauge}, // busy percent (not idle, not waiting for disk)
	{"cpu_user", kGauge},
	{"cpu_system", kGauge},
	{"cpu_iowait", kGauge},
	{"cpu_steal", kGauge},
	{"load1", kGauge},
	{"load5", kAvg},
	{"load15", kAvg},
	{"mem_total_kib", kAvg},
	{"mem_used_kib", kGauge},
	{"mem_cached_kib", kAvg},
	{"mem_avail_kib", kAvg},
	{"swap_used_kib", kGauge},
	{"vms_running", kLast},
}

// Device rows carry up to six values; what they mean depends on the kind.
const nDev = 6

// How a series of several devices is combined into "All".
const (
	aggSum = 's'
	aggAvg = 'a'
)

// DevKind describes one kind of per-device row.
type DevKind struct {
	Kind   string
	Entity string // "vm" or "node"
	Group  string
	Series [nDev]string
	Agg    [nDev]byte
}

// Device kinds. Series names are shared with the wide rows, so a chart does
// not care whether a value came from the sum or from one device.
var devKinds = []DevKind{
	{"disk", "vm", "disk", [nDev]string{"disk_rd_bps", "disk_wr_bps", "disk_rd_ops", "disk_wr_ops", "disk_lat_ms"}, [nDev]byte{'s', 's', 's', 's', 'a'}},
	{"vnet", "vm", "network", [nDev]string{"net_rx_bps", "net_tx_bps", "net_rx_drop", "net_tx_drop"}, [nDev]byte{'s', 's', 's', 's'}},
	{"nic", "node", "network", [nDev]string{"net_rx_bps", "net_tx_bps", "net_rx_err", "net_tx_err", "net_rx_drop", "net_tx_drop"}, [nDev]byte{'s', 's', 's', 's', 's', 's'}},
	{"net", "node", "network", [nDev]string{"net_rx_bps", "net_tx_bps", "net_rx_err", "net_tx_err", "net_rx_drop", "net_tx_drop"}, [nDev]byte{'s', 's', 's', 's', 's', 's'}},
	{"bdev", "node", "disk", [nDev]string{"disk_rd_bps", "disk_wr_bps", "disk_rd_ops", "disk_wr_ops", "disk_busy", "disk_lat_ms"}, [nDev]byte{'s', 's', 's', 's', 'a', 'a'}},
	{"pool", "node", "storage", [nDev]string{"pool_capacity_bytes", "pool_used_bytes", "pool_available_bytes"}, [nDev]byte{'s', 's', 's'}},
	{"gpu", "node", "gpu", [nDev]string{"gpu_util", "gpu_mem_used_mib", "gpu_mem_total_mib", "gpu_temp"}, [nDev]byte{'a', 'a', 'a', 'a'}},
}

func kindOf(kind string) *DevKind {
	for i := range devKinds {
		if devKinds[i].Kind == kind {
			return &devKinds[i]
		}
	}
	return nil
}

// groupOf says which group a wide-row series belongs to.
func groupOf(name string) string {
	switch {
	case strings.HasPrefix(name, "cpu"), strings.HasPrefix(name, "load"):
		return "cpu"
	case strings.HasPrefix(name, "mem"), strings.HasPrefix(name, "swap"):
		return "memory"
	case strings.HasPrefix(name, "disk"):
		return "disk"
	case strings.HasPrefix(name, "net"):
		return "network"
	}
	return ""
}

// NaN marks a value that is not known (gap, counter reset, not reported).
var NaN = math.NaN()

func isNaN(v float64) bool { return v != v }
