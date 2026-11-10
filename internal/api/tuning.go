package api

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/domxml"
	"virt-proj/internal/hostconn"
)

// Advanced VM tuning: CPU pinning, CPU model and topology, huge pages and
// NUMA memory policy. All of it is configuration: it is written to the saved
// definition and takes effect at the next start, whether the VM is running
// or not. Everything is validated against the real topology of the node the
// VM is defined on.

const tuningMigrationWarning = "Pinning to host CPUs, huge pages and strict NUMA placement tie a VM to the layout of this node. " +
	"A migration to a node with a different CPU or memory layout is blocked or runs degraded; the migration pre-flight checks this."

type tuningView struct {
	State          string        `json:"state"`
	MaxVCPUs       int           `json:"maxVcpus"`
	MemoryKiB      uint64        `json:"memoryKiB"`
	Saved          domxml.Tuning `json:"saved"`
	PendingRestart bool          `json:"pendingRestart"`
	Host           tuningHost    `json:"host"`
	Warning        string        `json:"warning"`
	Warnings       []string      `json:"warnings"`
}

type tuningHost struct {
	CPUs             []int      `json:"cpus"`
	Cells            []hostCell `json:"cells"`
	CPUModels        []string   `json:"cpuModels"`
	HugepageSizesKiB []uint64   `json:"hugepageSizesKiB"`
}

func domainTypeArch(raw string) (virt, arch string) {
	d, err := domxml.Parse(raw)
	if err != nil {
		return "", ""
	}
	virt = d.Root.Attrs["type"]
	if os := d.Root.Child("os"); os != nil {
		if t := os.Child("type"); t != nil {
			arch = t.Attrs["arch"]
		}
	}
	return
}

func cpuModelsFor(h *hostconn.Host, raw string) []string {
	virt, arch := domainTypeArch(raw)
	var a, v libvirt.OptString
	if arch != "" {
		a = libvirt.OptString{arch}
	}
	if virt != "" {
		v = libvirt.OptString{virt}
	}
	dc, err := h.ConnectGetDomainCapabilities(libvirt.OptString{}, a, libvirt.OptString{}, v, 0)
	if err != nil {
		return nil
	}
	return parseCPUModels(dc)
}

func (s *Server) vmTuning(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive)
	if err != nil {
		fail(w, err)
		return
	}
	saved, err := domxml.ReadTuning(raw)
	if err != nil {
		fail(w, err)
		return
	}
	state, maxMem, _, _, _, err := c.h.DomainGetInfo(c.dom)
	if err != nil {
		fail(w, err)
		return
	}
	v := tuningView{State: stateName(state), MemoryKiB: maxMem, Saved: saved, Warning: tuningMigrationWarning, Warnings: []string{}}
	if n, err := c.h.DomainGetVcpusFlags(c.dom, uint32(libvirt.DomainVCPUConfig|libvirt.DomainVCPUMaximum)); err == nil {
		v.MaxVCPUs = int(n)
	}
	if c.running {
		if live, err := c.h.DomainGetXMLDesc(c.dom, 0); err == nil {
			if lt, err := domxml.ReadTuning(live); err == nil {
				v.PendingRestart = !sameTuning(lt, saved)
			}
		}
	}
	topo, err := nodeTopology(c.h)
	if err != nil {
		v.Warnings = append(v.Warnings, "The node's topology could not be read: "+err.Error())
	}
	v.Host = tuningHost{CPUs: topo.cpus(), Cells: topo.Cells, HugepageSizesKiB: topo.hugepageSizes(), CPUModels: cpuModelsFor(c.h, raw)}
	if v.Host.Cells == nil {
		v.Host.Cells = []hostCell{}
	}
	if saved.Hugepages {
		v.Warnings = append(v.Warnings, hugepageShortage(saved.HugepageKiB, maxMem, topo)...)
	}
	writeJSON(w, v)
}

// sameTuning compares the running definition (a) with the saved one (b).
// libvirt shows a host-model CPU of a running VM as a resolved custom model,
// and the model name and vCPU count are not tuning, so those are not compared.
func sameTuning(a, b domxml.Tuning) bool {
	a.VCPUs, b.VCPUs = 0, 0
	a.CPUModel, b.CPUModel = "", ""
	if b.CPUMode == "host-model" && a.CPUMode == "custom" {
		a.CPUMode = "host-model"
	}
	return reflect.DeepEqual(a, b)
}

// hugepageShortage warns when the node has fewer free pages of the size than
// the VM's memory needs. size 0 (the host default) is not checked.
func hugepageShortage(sizeKiB, memKiB uint64, t hostTopo) []string {
	if sizeKiB == 0 || len(t.Cells) == 0 {
		return nil
	}
	need := (memKiB + sizeKiB - 1) / sizeKiB
	free := t.freePages(sizeKiB)
	if free >= need {
		return nil
	}
	return []string{fmt.Sprintf("The VM needs %d pages of %s but the node has only %d free. The VM will not start until more are reserved on the node (for example sysctl vm.nr_hugepages=%d).",
		need, fmtBytes(sizeKiB*1024), free, need+free)}
}

// tuningEdit applies a change to the saved definition by editing its XML
// and redefining the domain. edit returns the new XML and an optional note.
func (s *Server) tuningEdit(w http.ResponseWriter, r *http.Request, kind string, edit func(c hwCtx, raw string, topo hostTopo) (string, string, error)) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, kind, c.dom.Name, c.h.Name, func(func(float64)) error {
		raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive|libvirt.DomainXMLSecure)
		if err != nil {
			return err
		}
		topo, err := nodeTopology(c.h)
		if err != nil {
			return err
		}
		edited, note, err := edit(c, raw, topo)
		if err != nil {
			return err
		}
		if edited != raw {
			if _, err := c.h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate); err != nil {
				return err
			}
		}
		res = c.result("config")
		res.addNote(note)
		res.addNote("Takes effect at the next start of the VM.")
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) tuningPinning(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		VCPUs    []domxml.Pin `json:"vcpus"`
		Emulator *string      `json:"emulator"`
		Clear    bool         `json:"clear"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if !req.Clear && len(req.VCPUs) == 0 && req.Emulator == nil {
		fail(w, badRequest("nothing to change"))
		return
	}
	topo, err := nodeTopology(c.h)
	if err != nil {
		fail(w, err)
		return
	}
	hostCPUs := topo.cpus()
	valid := map[int]bool{}
	maxID := 0
	for _, id := range hostCPUs {
		valid[id] = true
		if id > maxID {
			maxID = id
		}
	}
	nHost := maxID + 1
	if _, _, cpus, _, _, _, _, _, err := c.h.NodeGetInfo(); err == nil && int(cpus) > nHost {
		nHost = int(cpus)
	}
	bitmap := func(set string) ([]byte, string, error) {
		ids, err := domxml.ParseCPUSet(set)
		if err != nil {
			return nil, "", badRequest(err.Error())
		}
		m := make([]byte, (nHost+7)/8)
		for _, id := range ids {
			if !valid[id] {
				return nil, "", badRequest(fmt.Sprintf("this node has no CPU %d (it has %s)", id, domxml.FormatCPUSet(hostCPUs)))
			}
			m[id/8] |= 1 << (id % 8)
		}
		return m, domxml.FormatCPUSet(ids), nil
	}
	maxV := 0
	if n, err := c.h.DomainGetVcpusFlags(c.dom, uint32(libvirt.DomainVCPUConfig|libvirt.DomainVCPUMaximum)); err == nil {
		maxV = int(n)
	}
	seen := map[int]bool{}
	type job struct {
		vcpu int
		mask []byte
	}
	var jobs []job
	for _, p := range req.VCPUs {
		if p.VCPU < 0 || (maxV > 0 && p.VCPU >= maxV) {
			fail(w, badRequest(fmt.Sprintf("vCPU %d does not exist (the VM has %d)", p.VCPU, maxV)))
			return
		}
		if seen[p.VCPU] {
			fail(w, badRequest(fmt.Sprintf("vCPU %d is listed twice", p.VCPU)))
			return
		}
		seen[p.VCPU] = true
		m, _, err := bitmap(p.CPUSet)
		if err != nil {
			fail(w, err)
			return
		}
		jobs = append(jobs, job{p.VCPU, m})
	}
	var emu []byte
	if req.Emulator != nil && strings.TrimSpace(*req.Emulator) != "" {
		if emu, _, err = bitmap(*req.Emulator); err != nil {
			fail(w, err)
			return
		}
	}
	var res hwResult
	err = s.task(r, "set CPU pinning", c.dom.Name, c.h.Name, func(func(float64)) error {
		if req.Clear {
			raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive|libvirt.DomainXMLSecure)
			if err != nil {
				return err
			}
			cleared, err := domxml.ClearPins(raw)
			if err != nil {
				return err
			}
			if cleared != raw {
				if _, err := c.h.DomainDefineXMLFlags(cleared, libvirt.DomainDefineValidate); err != nil {
					return err
				}
			}
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].vcpu < jobs[j].vcpu })
		for _, j := range jobs {
			if err := c.h.DomainPinVcpuFlags(c.dom, uint32(j.vcpu), j.mask, affConfig); err != nil {
				return fmt.Errorf("pinning vCPU %d: %w", j.vcpu, err)
			}
		}
		if emu != nil {
			if err := c.h.DomainPinEmulator(c.dom, emu, libvirt.DomainAffectConfig); err != nil {
				return fmt.Errorf("pinning the emulator threads: %w", err)
			}
		}
		res = c.result("config")
		res.addNote("Takes effect at the next start of the VM.")
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) tuningCPU(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode       string `json:"mode"`
		Model      string `json:"model"`
		Sockets    int    `json:"sockets"`
		Cores      int    `json:"cores"`
		Threads    int    `json:"threads"`
		NoTopology bool   `json:"noTopology"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	s.tuningEdit(w, r, "set CPU model", func(c hwCtx, raw string, _ hostTopo) (string, string, error) {
		spec := domxml.CPUSpec{Mode: req.Mode, Model: req.Model, ClearTopology: req.NoTopology}
		if spec.Mode != "custom" {
			spec.Model = ""
		}
		if spec.Mode == "custom" {
			models := cpuModelsFor(c.h, raw)
			if len(models) > 0 && !contains(models, spec.Model) {
				return "", "", badRequest(fmt.Sprintf("this node's hypervisor has no CPU model %q", spec.Model))
			}
		}
		if !req.NoTopology && (req.Sockets != 0 || req.Cores != 0 || req.Threads != 0) {
			tp := &domxml.CPUTopology{Sockets: req.Sockets, Cores: req.Cores, Threads: req.Threads}
			cur, err := domxml.ReadTuning(raw)
			if err != nil {
				return "", "", err
			}
			if cur.Topology != nil { // dies and clusters are not edited here but count
				tp.Dies, tp.Clusters = cur.Topology.Dies, cur.Topology.Clusters
			}
			maxV := cur.VCPUs
			if n, err := c.h.DomainGetVcpusFlags(c.dom, uint32(libvirt.DomainVCPUConfig|libvirt.DomainVCPUMaximum)); err == nil {
				maxV = int(n)
			}
			if tp.Sockets < 1 || tp.Cores < 1 || tp.Threads < 1 {
				return "", "", badRequest("sockets, cores and threads must each be at least 1")
			}
			if tp.Total() != maxV {
				return "", "", badRequest(fmt.Sprintf("%d sockets × %d cores × %d threads = %d vCPUs, but the VM is configured for a maximum of %d; change the vCPU count first or pick a matching topology",
					tp.Sockets, tp.Cores, tp.Threads, tp.Total(), maxV))
			}
			spec.Topology = tp
		}
		out, err := domxml.SetCPU(raw, spec)
		if err != nil {
			return "", "", badRequest(err.Error())
		}
		note := ""
		if spec.Mode == "host-passthrough" {
			note = "host-passthrough exposes the exact CPU of this node; live migration then needs an identical CPU on the target."
		}
		return out, note, nil
	})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (s *Server) tuningHugepages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PageSizeKiB uint64 `json:"pageSizeKiB"` // 0: ordinary pages
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	s.tuningEdit(w, r, "set huge pages", func(c hwCtx, raw string, topo hostTopo) (string, string, error) {
		note := ""
		if req.PageSizeKiB != 0 {
			sizes := topo.hugepageSizes()
			ok := false
			for _, sz := range sizes {
				ok = ok || sz == req.PageSizeKiB
			}
			if !ok {
				return "", "", badRequest(fmt.Sprintf("this node offers huge pages of %v KiB only", sizes))
			}
			_, maxMem, _, _, _, err := c.h.DomainGetInfo(c.dom)
			if err != nil {
				return "", "", err
			}
			if maxMem%req.PageSizeKiB != 0 {
				return "", "", badRequest(fmt.Sprintf("the VM's memory (%s) is not a multiple of the page size (%s)", fmtBytes(maxMem*1024), fmtBytes(req.PageSizeKiB*1024)))
			}
			note = strings.Join(hugepageShortage(req.PageSizeKiB, maxMem, topo), " ")
		}
		out, err := domxml.SetHugepages(raw, req.PageSizeKiB)
		return out, note, err
	})
}

func (s *Server) tuningNUMA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode    string `json:"mode"` // "" removes the policy
		Nodeset string `json:"nodeset"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	s.tuningEdit(w, r, "set NUMA policy", func(c hwCtx, raw string, topo hostTopo) (string, string, error) {
		switch req.Mode {
		case "":
			out, err := domxml.SetNumatune(raw, "", "")
			return out, "", err
		case "strict", "preferred", "interleave":
		default:
			return "", "", badRequest("mode must be strict, preferred or interleave")
		}
		nodes, err := parseNodeset(req.Nodeset, topo.cellIDs())
		if err != nil {
			return "", "", badRequest(err.Error())
		}
		if req.Mode == "preferred" && len(nodes) != 1 {
			return "", "", badRequest("preferred takes exactly one NUMA node")
		}
		note := ""
		if len(topo.Cells) == 1 {
			note = "This node has a single NUMA node, so the policy changes nothing here; it still limits where the VM can be migrated."
		}
		out, err := domxml.SetNumatune(raw, req.Mode, req.Nodeset)
		return out, note, err
	})
}

// checkTuning is the migration pre-flight for VMs tied to host topology.
func (s *Server) checkTuning(dst *hostconn.Host, plan *migPlan, c *checkList) {
	t, err := domxml.ReadTuning(plan.LiveXML)
	if err != nil || !t.Any() {
		return
	}
	topo, err := nodeTopology(dst)
	if err != nil {
		topo = hostTopo{}
	}
	*c = append(*c, tuningChecks(t, topo, plan.MemKiB, dst.Name, err)...)
}

// tuningChecks decides what a target's topology means for a VM that is
// pinned, uses huge pages or a NUMA policy. topoErr is set when the topology
// of the target could not be read; the hard checks are then skipped.
func tuningChecks(t domxml.Tuning, topo hostTopo, memKiB uint64, dst string, topoErr error) checkList {
	var c checkList
	if !t.Any() {
		return c
	}
	var what []string
	if len(t.Pins) > 0 || t.EmulatorPin != "" || t.VCPUCPUSet != "" {
		what = append(what, "CPU pinning")
	}
	if t.Hugepages {
		what = append(what, "huge pages")
	}
	if t.NumaMode != "" {
		what = append(what, "NUMA policy ("+t.NumaMode+")")
	}
	c.warn("Host topology", "this VM uses "+strings.Join(what, ", ")+", which tie it to the CPU and memory layout of its node; on a target with a different layout it may run slower or not start.")
	if topoErr != nil {
		c.warn("Host topology", "the topology of "+dst+" could not be read: "+topoErr.Error())
		return c
	}
	if pinned := t.Pinned(); len(pinned) > 0 {
		have := map[int]bool{}
		for _, id := range topo.cpus() {
			have[id] = true
		}
		var missing []int
		for _, id := range pinned {
			if !have[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			c.bad("CPU pinning", fmt.Sprintf("the VM is pinned to host CPU(s) %s, which %s does not have (it has %s)", domxml.FormatCPUSet(missing), dst, domxml.FormatCPUSet(topo.cpus())))
		}
	}
	if t.Hugepages && t.HugepageKiB != 0 {
		ok := false
		for _, sz := range topo.hugepageSizes() {
			ok = ok || sz == t.HugepageKiB
		}
		if !ok {
			c.bad("Huge pages", fmt.Sprintf("the VM uses pages of %s, which %s does not offer", fmtBytes(t.HugepageKiB*1024), dst))
		} else {
			for _, msg := range hugepageShortage(t.HugepageKiB, memKiB, topo) {
				c.warn("Huge pages", dst+": "+msg)
			}
		}
	}
	if t.NumaMode != "" && t.NumaNodeset != "" {
		if _, err := parseNodeset(t.NumaNodeset, topo.cellIDs()); err != nil {
			if t.NumaMode == "strict" {
				c.bad("NUMA policy", dst+": "+err.Error())
			} else {
				c.warn("NUMA policy", dst+": "+err.Error())
			}
		}
	}
	return c
}
