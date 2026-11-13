package metrics

import (
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// PromNode is one node as a scrape sees it. Tracker is nil when the node is
// offline or its data is stale: only virticle_node_up is then written.
type PromNode struct {
	Name    string
	Up      bool
	Tracker *NodeTracker
}

type family struct {
	name, help, typ string
	lines           []string
}

type promOut struct {
	fams  []*family
	index map[string]*family
}

func (o *promOut) add(name, help, typ string, labels []string, v float64) {
	if isNaN(v) || math.IsInf(v, 0) {
		return
	}
	f := o.index[name]
	if f == nil {
		f = &family{name: name, help: help, typ: typ}
		o.index[name] = f
		o.fams = append(o.fams, f)
	}
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i] + `="` + escapeLabel(labels[i+1]) + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
	f.lines = append(f.lines, b.String())
}

// escapeLabel follows the exposition format: backslash, double quote and
// line feed are escaped.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func escapeHelp(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`)
}

func ptr(v uint64) float64 { return float64(v) }

// WriteProm renders the current values in the Prometheus text exposition
// format. Counters are the raw cumulative counters of the node and libvirt,
// converted to base units (seconds, bytes); everything else is a gauge.
func WriteProm(w io.Writer, nodes []PromNode, version, goVersion string) error {
	o := &promOut{index: map[string]*family{}}
	g, c := "gauge", "counter"
	o.add("virticle_build_info", "Version of the controller; the value is always 1.", g, []string{"version", version, "goversion", goVersion}, 1)
	for _, n := range nodes {
		nl := []string{"node", n.Name}
		up := 0.0
		if n.Up {
			up = 1
		}
		o.add("virticle_node_up", "1 when the controller is connected to the node.", g, nl, up)
		t := n.Tracker
		if t == nil || t.LastNode == nil {
			continue
		}
		r := t.LastNode
		o.add("virticle_node_vms_running", "Virtual machines running or paused on the node.", g, nl, float64(t.Running))
		if r.HasCPU {
			modes := []struct {
				m string
				v uint64
			}{{"user", r.CPU[0] + r.CPU[1]}, {"system", r.CPU[2] + r.CPU[5] + r.CPU[6]}, {"idle", r.CPU[3]}, {"iowait", r.CPU[4]}, {"steal", r.CPU[7]}}
			for _, m := range modes {
				o.add("virticle_node_cpu_seconds_total", "CPU time of the node, all cores, by mode.", c, []string{"node", n.Name, "mode", m.m}, float64(m.v)/100)
			}
		}
		if r.HasLoad {
			o.add("virticle_node_load1", "Load average over 1 minute.", g, nl, r.Load[0])
			o.add("virticle_node_load5", "Load average over 5 minutes.", g, nl, r.Load[1])
			o.add("virticle_node_load15", "Load average over 15 minutes.", g, nl, r.Load[2])
		}
		if r.HasMem {
			o.add("virticle_node_memory_total_bytes", "Memory of the node.", g, nl, ptr(r.MemTotal)*1024)
			o.add("virticle_node_memory_available_bytes", "Memory available to programs without swapping.", g, nl, ptr(r.MemAvail)*1024)
			o.add("virticle_node_memory_cached_bytes", "Page cache and buffers.", g, nl, ptr(r.MemCached+r.MemBuffers)*1024)
			if r.SwapTot >= r.SwapFree {
				o.add("virticle_node_swap_used_bytes", "Swap in use.", g, nl, ptr(r.SwapTot-r.SwapFree)*1024)
			}
		}
		for _, d := range r.Nets {
			dl := []string{"node", n.Name, "device", d.Name}
			o.add("virticle_node_network_receive_bytes_total", "Bytes received by a network card or bridge.", c, dl, ptr(d.RxBytes))
			o.add("virticle_node_network_transmit_bytes_total", "Bytes sent by a network card or bridge.", c, dl, ptr(d.TxBytes))
			o.add("virticle_node_network_receive_errors_total", "Receive errors.", c, dl, ptr(d.RxErrs))
			o.add("virticle_node_network_transmit_errors_total", "Transmit errors.", c, dl, ptr(d.TxErrs))
			o.add("virticle_node_network_receive_drops_total", "Received packets dropped.", c, dl, ptr(d.RxDrop))
			o.add("virticle_node_network_transmit_drops_total", "Packets to send that were dropped.", c, dl, ptr(d.TxDrop))
		}
		for _, d := range r.Blocks {
			dl := []string{"node", n.Name, "device", d.Name}
			o.add("virticle_node_disk_read_bytes_total", "Bytes read from a block device.", c, dl, ptr(d.RdSectors)*512)
			o.add("virticle_node_disk_written_bytes_total", "Bytes written to a block device.", c, dl, ptr(d.WrSectors)*512)
			o.add("virticle_node_disk_reads_total", "Read operations completed.", c, dl, ptr(d.RdOps))
			o.add("virticle_node_disk_writes_total", "Write operations completed.", c, dl, ptr(d.WrOps))
			o.add("virticle_node_disk_busy_seconds_total", "Time the device had requests in flight.", c, dl, ptr(d.IOMs)/1000)
		}
		for _, p := range r.Pools {
			pl := []string{"node", n.Name, "pool", p.Name}
			o.add("virticle_node_storage_pool_capacity_bytes", "Size of a storage pool.", g, pl, ptr(p.Capacity))
			o.add("virticle_node_storage_pool_allocation_bytes", "Space used in a storage pool.", g, pl, ptr(p.Allocation))
			o.add("virticle_node_storage_pool_available_bytes", "Space free in a storage pool.", g, pl, ptr(p.Avail))
		}
		for _, gp := range r.GPUs {
			gl := []string{"node", n.Name, "gpu", gp.Index, "name", gp.Name}
			o.add("virticle_node_gpu_utilization_ratio", "Share of time the GPU was busy (0 to 1).", g, gl, gp.Util/100)
			o.add("virticle_node_gpu_memory_used_bytes", "GPU memory in use.", g, gl, gp.MemUsedMiB*1048576)
			o.add("virticle_node_gpu_memory_total_bytes", "GPU memory.", g, gl, gp.MemTotalMiB*1048576)
			o.add("virticle_node_gpu_temperature_celsius", "GPU temperature.", g, gl, gp.TempC)
		}

		uuids := make([]string, 0, len(t.VMs))
		for u := range t.VMs {
			uuids = append(uuids, u)
		}
		sort.Slice(uuids, func(i, j int) bool {
			a, b := t.VMs[uuids[i]], t.VMs[uuids[j]]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.UUID < b.UUID
		})
		for _, u := range uuids {
			v := t.VMs[u]
			vl := []string{"node", n.Name, "vm", v.Name, "uuid", v.UUID}
			state := 0.0
			switch v.State {
			case "running":
				state = 1
			case "paused":
				state = 2
			}
			o.add("virticle_vm_state", "State of a virtual machine: 1 running, 2 paused, 0 anything else.", g, vl, state)
			if v.Raw == nil {
				continue
			}
			x := v.Raw
			o.add("virticle_vm_vcpus", "Virtual CPUs of a running VM.", g, vl, float64(x.VCPUs))
			if x.HasCPU {
				o.add("virticle_vm_cpu_seconds_total", "CPU time used by a VM, all vCPUs.", c, vl, float64(x.CPUNanos)/1e9)
			}
			if x.HasDelay {
				o.add("virticle_vm_cpu_delay_seconds_total", "Time the vCPUs of a VM waited for a host CPU.", c, vl, float64(x.DelayNanos)/1e9)
			}
			if x.HasBalloon {
				o.add("virticle_vm_memory_assigned_bytes", "Memory currently given to a VM.", g, vl, ptr(x.BalloonKiB)*1024)
				if x.HasGuest && x.UnusedKiB <= x.AvailKiB {
					o.add("virticle_vm_memory_used_bytes", "Memory used inside the guest, when the balloon driver reports it.", g, vl, ptr(x.AvailKiB-x.UnusedKiB)*1024)
				}
			}
			if x.HasRSS {
				o.add("virticle_vm_memory_resident_bytes", "Memory of a VM resident on the host.", g, vl, ptr(x.RSSKiB)*1024)
			}
			for _, name := range sortedKeys(x.Disks) {
				d := x.Disks[name]
				dl := append(append([]string{}, vl...), "device", name)
				o.add("virticle_vm_disk_read_bytes_total", "Bytes read from a VM disk.", c, dl, ptr(d.RdBytes))
				o.add("virticle_vm_disk_written_bytes_total", "Bytes written to a VM disk.", c, dl, ptr(d.WrBytes))
				o.add("virticle_vm_disk_reads_total", "Read operations of a VM disk.", c, dl, ptr(d.RdReqs))
				o.add("virticle_vm_disk_writes_total", "Write operations of a VM disk.", c, dl, ptr(d.WrReqs))
				o.add("virticle_vm_disk_read_seconds_total", "Time spent on reads of a VM disk.", c, dl, float64(d.RdNanos)/1e9)
				o.add("virticle_vm_disk_write_seconds_total", "Time spent on writes of a VM disk.", c, dl, float64(d.WrNanos)/1e9)
			}
			for _, name := range sortedKeys(x.Nets) {
				d := x.Nets[name]
				dl := append(append([]string{}, vl...), "device", name)
				o.add("virticle_vm_network_receive_bytes_total", "Bytes received by a VM network card.", c, dl, ptr(d.RxBytes))
				o.add("virticle_vm_network_transmit_bytes_total", "Bytes sent by a VM network card.", c, dl, ptr(d.TxBytes))
				o.add("virticle_vm_network_receive_drops_total", "Received packets dropped.", c, dl, ptr(d.RxDrop))
				o.add("virticle_vm_network_transmit_drops_total", "Packets to send that were dropped.", c, dl, ptr(d.TxDrop))
				o.add("virticle_vm_network_receive_errors_total", "Receive errors.", c, dl, ptr(d.RxErrs))
				o.add("virticle_vm_network_transmit_errors_total", "Transmit errors.", c, dl, ptr(d.TxErrs))
			}
		}
	}
	var b strings.Builder
	for _, f := range o.fams {
		b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n# TYPE " + f.name + " " + f.typ + "\n")
		for _, l := range f.lines {
			b.WriteString(l + "\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}
