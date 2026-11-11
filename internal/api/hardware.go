package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/domxml"
	"virt-proj/internal/hostconn"
)

// Hardware editing. Every change goes through libvirt's device-level calls or
// setters; the one exception is the boot order, which libvirt can only change
// by redefining the domain, so that edit is a targeted splice of the inactive
// XML (see package domxml). Nothing regenerates a definition from a model.

const (
	affLive   = uint32(libvirt.DomainAffectLive)
	affConfig = uint32(libvirt.DomainAffectConfig)
)

// hwResult tells the UI how a change was applied. "live+config" means the
// running VM and its saved definition both changed; "config" means only the
// saved definition did (always the case for a VM that is off), and then
// NeedsRestart says whether the running VM still lacks the change.
type hwResult struct {
	Applied      string `json:"applied"`
	NeedsRestart bool   `json:"needsRestart"`
	Note         string `json:"note,omitempty"`
	Target       string `json:"target,omitempty"`
	MAC          string `json:"mac,omitempty"`
}

type hwCtx struct {
	h       *hostconn.Host
	dom     libvirt.Domain
	running bool // any state except shut off
	// why collects libvirt's reasons for falling back from a live change to a
	// config-only one, so the UI can show them.
	why *string
}

func (s *Server) hwContext(r *http.Request) (hwCtx, error) {
	h, dom, err := s.domain(r)
	if err != nil {
		return hwCtx{}, err
	}
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		return hwCtx{}, err
	}
	return hwCtx{h: h, dom: dom, running: stateName(state) != "shutoff", why: new(string)}, nil
}

// docs returns the persistent definition and, for a running VM, the live one.
// For a VM that is off both are the same document.
func (c hwCtx) docs() (live, cfg *domxml.Doc, err error) {
	raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive)
	if err != nil {
		return nil, nil, err
	}
	if cfg, err = domxml.Parse(raw); err != nil {
		return nil, nil, err
	}
	if !c.running {
		return cfg, cfg, nil
	}
	if raw, err = c.h.DomainGetXMLDesc(c.dom, 0); err != nil {
		return nil, nil, err
	}
	live, err = domxml.Parse(raw)
	return live, cfg, err
}

func (c hwCtx) result(applied string) hwResult {
	r := hwResult{Applied: applied, NeedsRestart: applied == "config" && c.running}
	if r.NeedsRestart && *c.why != "" {
		r.Note = "libvirt refused the live change: " + *c.why
	}
	return r
}

// addNote appends a sentence; notes are shown as running text, so each one is capitalised and ends with a full stop.
func (r *hwResult) addNote(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if c := s[0]; c >= 'a' && c <= 'z' {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	r.Note = strings.TrimSpace(r.Note + " " + s)
}

// attach hot-plugs a new device and records it in the saved definition; if
// libvirt refuses the hot-plug (the bus does not support it) it is saved for
// the next start instead.
func (c hwCtx) attach(devXML string) (string, error) {
	if c.running {
		liveErr := c.h.DomainAttachDeviceFlags(c.dom, devXML, affLive|affConfig)
		if liveErr == nil {
			return "live+config", nil
		}
		if err := c.h.DomainAttachDeviceFlags(c.dom, devXML, affConfig); err != nil {
			return "", liveErr
		}
		*c.why = liveErr.Error()
		return "config", nil
	}
	return "config", c.h.DomainAttachDeviceFlags(c.dom, devXML, affConfig)
}

// update changes an existing device. edit receives a document and the device
// in it and returns the new device XML. A running VM is updated from its live
// description (which carries everything libvirt compares), falling back to the
// saved one when the live change is refused.
func (c hwCtx) update(kind string, match func(*domxml.Node) bool, edit func(*domxml.Doc, *domxml.Node) (string, error)) (string, error) {
	live, cfg, err := c.docs()
	if err != nil {
		return "", err
	}
	find := func(d *domxml.Doc) *domxml.Node {
		for _, n := range d.Devices(kind) {
			if match(n) {
				return n
			}
		}
		return nil
	}
	ln, cn := find(live), find(cfg)
	if ln == nil && cn == nil {
		return "", badRequest("no such device")
	}
	if c.running && ln != nil {
		// The live and the saved definition can differ (a boot order saved
		// but not yet started with, say), so each is updated from its own
		// description rather than pushing one into both.
		devXML, err := edit(live, ln)
		if err != nil {
			return "", err
		}
		modify := func(f uint32) libvirt.DomainDeviceModifyFlags { return libvirt.DomainDeviceModifyFlags(f) }
		liveErr := c.h.DomainUpdateDeviceFlags(c.dom, devXML, modify(affLive))
		if liveErr == nil && cn == nil {
			return "live", nil
		}
		if cn == nil {
			return "", liveErr
		}
		cfgXML, err := edit(cfg, cn)
		if err != nil {
			return "", err
		}
		if err := c.h.DomainUpdateDeviceFlags(c.dom, cfgXML, modify(affConfig)); err != nil {
			if liveErr == nil {
				return "", fmt.Errorf("applied to the running VM, but saving it to the definition failed: %w", err)
			}
			return "", liveErr
		}
		if liveErr == nil {
			return "live+config", nil
		}
		*c.why = liveErr.Error()
		return "config", nil
	}
	if cn == nil {
		return "", badRequest("no such device in the saved definition")
	}
	devXML, err := edit(cfg, cn)
	if err != nil {
		return "", err
	}
	return "config", c.h.DomainUpdateDeviceFlags(c.dom, devXML, libvirt.DomainDeviceModifyFlags(affConfig))
}

// detach removes a device. pending is true when the request was accepted but
// the guest has not yet released the device (hot-unplug needs its cooperation).
func (c hwCtx) detach(kind string, match func(*domxml.Node) bool) (applied string, pending bool, err error) {
	live, cfg, err := c.docs()
	if err != nil {
		return "", false, err
	}
	find := func(d *domxml.Doc) *domxml.Node {
		for _, n := range d.Devices(kind) {
			if match(n) {
				return n
			}
		}
		return nil
	}
	ln, cn := find(live), find(cfg)
	if ln == nil && cn == nil {
		return "", false, badRequest("no such device")
	}
	if c.running && ln != nil {
		// Live and saved descriptions differ (aliases, addresses), and libvirt
		// matches a device against every definition it is applied to, so each
		// is detached with its own description.
		liveErr := c.h.DomainDetachDeviceFlags(c.dom, live.Raw(ln), affLive)
		if cn != nil {
			if err := c.h.DomainDetachDeviceFlags(c.dom, cfg.Raw(cn), affConfig); err != nil {
				if liveErr == nil {
					return "", false, fmt.Errorf("detached from the running VM, but removing it from the definition failed: %w", err)
				}
				return "", false, liveErr
			}
		}
		if liveErr != nil {
			if cn == nil {
				return "", false, liveErr
			}
			*c.why = liveErr.Error()
			return "config", false, nil
		}
		applied = "live+config"
		if cn == nil {
			applied = "live"
		}
		// The unplug finishes when the guest acknowledges it.
		for i := 0; i < 12; i++ {
			time.Sleep(500 * time.Millisecond)
			l, _, err := c.docs()
			if err != nil {
				break
			}
			gone := true
			for _, n := range l.Devices(kind) {
				if match(n) {
					gone = false
				}
			}
			if gone {
				return applied, false, nil
			}
		}
		return applied, true, nil
	}
	if cn == nil {
		return "", false, badRequest("no such device in the saved definition")
	}
	return "config", false, c.h.DomainDetachDeviceFlags(c.dom, cfg.Raw(cn), affConfig)
}

// --- CPU and memory ---

func (s *Server) hwCPU(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		VCPUs    int `json:"vcpus"`
		MaxVCPUs int `json:"maxVcpus"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	const limit = 512
	if req.VCPUs < 1 || req.VCPUs > limit || req.MaxVCPUs < 0 || req.MaxVCPUs > limit {
		fail(w, badRequest("vcpus must be between 1 and 512"))
		return
	}
	var res hwResult
	err = s.task(r, "set vCPUs", c.dom.Name, c.h.Name, func(func(float64)) error {
		vcpus := func(f libvirt.DomainVCPUFlags) (int, error) {
			n, err := c.h.DomainGetVcpusFlags(c.dom, uint32(f))
			return int(n), err
		}
		cfgMax, err := vcpus(libvirt.DomainVCPUConfig | libvirt.DomainVCPUMaximum)
		if err != nil {
			return err
		}
		cfgCur, err := vcpus(libvirt.DomainVCPUConfig)
		if err != nil {
			return err
		}
		newMax := cfgMax
		if req.MaxVCPUs > 0 {
			newMax = req.MaxVCPUs
		}
		if req.VCPUs > newMax {
			return badRequest(fmt.Sprintf("%d vCPUs exceed the maximum of %d; raise the maximum too (a new maximum applies after a restart)", req.VCPUs, newMax))
		}
		setMax := func() error {
			if newMax == cfgMax {
				return nil
			}
			return c.h.DomainSetVcpusFlags(c.dom, uint32(newMax), uint32(libvirt.DomainVCPUConfig|libvirt.DomainVCPUMaximum))
		}
		setCur := func() error {
			return c.h.DomainSetVcpusFlags(c.dom, uint32(req.VCPUs), uint32(libvirt.DomainVCPUConfig))
		}
		// The maximum can neither drop below the current count nor the
		// current count rise above the maximum, so order the two writes.
		first, second := setMax, setCur
		if newMax < cfgCur {
			first, second = setCur, setMax
		}
		if err := first(); err != nil {
			return err
		}
		if err := second(); err != nil {
			return err
		}
		res = c.result("config")
		if !c.running {
			return nil
		}
		liveMax, err := vcpus(libvirt.DomainVCPULive | libvirt.DomainVCPUMaximum)
		if err != nil {
			return err
		}
		liveCur, err := vcpus(libvirt.DomainVCPULive)
		if err != nil {
			return err
		}
		res.NeedsRestart = newMax != liveMax
		switch {
		case req.VCPUs > liveMax:
			res.NeedsRestart = true
			res.Note = fmt.Sprintf("the running VM allows at most %d vCPUs; the change is saved and applies after a restart.", liveMax)
		case req.VCPUs == liveCur:
			res.Applied = "live+config"
		default:
			if err := c.h.DomainSetVcpusFlags(c.dom, uint32(req.VCPUs), uint32(libvirt.DomainVCPULive)); err != nil {
				res.NeedsRestart = true
				res.Note = "saved, but the running VM refused the live change: " + err.Error()
				return nil
			}
			res.Applied = "live+config"
		}
		if newMax != liveMax {
			res.Note = strings.TrimSpace(res.Note + " The new maximum applies after a restart.")
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// kibOf converts the text and unit attribute of a memory element to KiB.
func kibOf(d *domxml.Doc, n *domxml.Node) uint64 {
	if n == nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(d.Text(n)), 10, 64)
	mult := map[string]float64{"": 1, "k": 1, "kib": 1, "kb": 1000.0 / 1024, "m": 1024, "mib": 1024, "mb": 1e6 / 1024,
		"g": 1 << 20, "gib": 1 << 20, "gb": 1e9 / 1024, "t": 1 << 30, "tib": 1 << 30, "tb": 1e12 / 1024, "b": 1.0 / 1024, "bytes": 1.0 / 1024}
	m, ok := mult[strings.ToLower(n.Attrs["unit"])]
	if !ok {
		m = 1
	}
	return uint64(float64(v) * m)
}

func (s *Server) hwMemory(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		MemoryMiB    int `json:"memoryMiB"`
		MaxMemoryMiB int `json:"maxMemoryMiB"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	const limit = 16 << 20 // 16 TiB
	if req.MemoryMiB < 64 || req.MemoryMiB > limit || req.MaxMemoryMiB < 0 || req.MaxMemoryMiB > limit {
		fail(w, badRequest("memory must be at least 64 MiB"))
		return
	}
	var res hwResult
	err = s.task(r, "set memory", c.dom.Name, c.h.Name, func(func(float64)) error {
		_, cfg, err := c.docs()
		if err != nil {
			return err
		}
		cfgMax := kibOf(cfg, cfg.Root.Child("memory"))
		cfgCur := cfgMax
		if n := cfg.Root.Child("currentMemory"); n != nil {
			cfgCur = kibOf(cfg, n)
		}
		cur, newMax := uint64(req.MemoryMiB)<<10, cfgMax
		if req.MaxMemoryMiB > 0 {
			newMax = uint64(req.MaxMemoryMiB) << 10
		}
		if cur > newMax {
			return badRequest(fmt.Sprintf("%d MiB exceeds the maximum of %d MiB; raise the maximum too (a new maximum applies after a restart)", req.MemoryMiB, newMax>>10))
		}
		setMax := func() error {
			if newMax == cfgMax {
				return nil
			}
			return c.h.DomainSetMemoryFlags(c.dom, newMax, uint32(libvirt.DomainMemConfig|libvirt.DomainMemMaximum))
		}
		setCur := func() error { return c.h.DomainSetMemoryFlags(c.dom, cur, uint32(libvirt.DomainMemConfig)) }
		first, second := setMax, setCur
		if newMax < cfgCur {
			first, second = setCur, setMax
		}
		if err := first(); err != nil {
			return err
		}
		if err := second(); err != nil {
			return err
		}
		res = c.result("config")
		if !c.running {
			return nil
		}
		_, liveMax, _, _, _, err := c.h.DomainGetInfo(c.dom)
		if err != nil {
			return err
		}
		res.NeedsRestart = newMax != liveMax
		switch {
		case cur > liveMax:
			res.NeedsRestart = true
			res.Note = fmt.Sprintf("the running VM allows at most %d MiB; the change is saved and applies after a restart.", liveMax>>10)
		default:
			if err := c.h.DomainSetMemoryFlags(c.dom, cur, uint32(libvirt.DomainMemLive)); err != nil {
				res.NeedsRestart = true
				res.Note = "saved, but the running VM refused the live change: " + err.Error()
				return nil
			}
			res.Applied = "live+config"
		}
		if newMax != liveMax {
			res.Note = strings.TrimSpace(res.Note + " The new maximum applies after a restart.")
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// --- disks ---

var cacheModes = map[string]bool{"": true, "default": true, "none": true, "writeback": true, "writethrough": true, "directsync": true, "unsafe": true}

type newVolumeReq struct {
	Name    string `json:"name"`
	SizeGiB int    `json:"sizeGiB"`
	Format  string `json:"format"`
}

type diskAddReq struct {
	Pool      string        `json:"pool"`
	Volume    string        `json:"volume"`
	NewVolume *newVolumeReq `json:"newVolume"`
	Bus       string        `json:"bus"`
	ReadOnly  bool          `json:"readonly"`
	Cache     string        `json:"cache"`
}

func usedTargets(docs ...*domxml.Doc) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d.DiskTargets()...)
	}
	return out
}

func diskSourcePath(n *domxml.Node) string {
	if src := n.Child("source"); src != nil {
		if f := src.Attrs["file"]; f != "" {
			return f
		}
		return src.Attrs["dev"]
	}
	return ""
}

func (s *Server) hwDiskAdd(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req diskAddReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	prefix := map[string]string{"virtio": "vd", "sata": "sd", "scsi": "sd"}[req.Bus]
	switch {
	case prefix == "":
		err = badRequest("bus must be virtio, sata or scsi")
	case req.Pool == "":
		err = badRequest("a storage pool is required")
	case (req.NewVolume == nil) == (req.Volume == ""):
		err = badRequest("give either an existing volume or a newVolume")
	case !cacheModes[req.Cache]:
		err = badRequest("unknown cache mode " + req.Cache)
	case req.NewVolume != nil && (!volNameRE.MatchString(req.NewVolume.Name) || req.NewVolume.SizeGiB < 1 || req.NewVolume.SizeGiB > 65536 || (req.NewVolume.Format != "qcow2" && req.NewVolume.Format != "raw")):
		err = badRequest("a new volume needs a plain name, a size of 1 GiB or more and format qcow2 or raw")
	}
	if err != nil {
		fail(w, err)
		return
	}
	pool, err := c.h.StoragePoolLookupByName(req.Pool)
	if err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, "add disk", c.dom.Name, c.h.Name, func(func(float64)) error {
		live, cfg, err := c.docs()
		if err != nil {
			return err
		}
		var vol libvirt.StorageVol
		created := false
		if req.NewVolume != nil {
			nv := req.NewVolume
			if vol, err = c.h.StorageVolCreateXML(pool, volumeXML(nv.Name, nv.Format, uint64(nv.SizeGiB)<<30), 0); err != nil {
				return err
			}
			created = true
		} else if vol, err = c.h.StorageVolLookupByName(pool, req.Volume); err != nil {
			return err
		}
		rollback := func(cause error) error {
			if !created {
				return cause
			}
			if derr := c.h.StorageVolDelete(vol, 0); derr != nil {
				return fmt.Errorf("%w (the new volume %s/%s was created and could not be removed again: %v)", cause, pool.Name, vol.Name, derr)
			}
			return cause
		}
		raw, err := c.h.StorageVolGetXMLDesc(vol, 0)
		if err != nil {
			return rollback(err)
		}
		var vx struct {
			Type   string `xml:"type,attr"`
			Target struct {
				Path   string `xml:"path"`
				Format struct {
					Type string `xml:"type,attr"`
				} `xml:"format"`
			} `xml:"target"`
		}
		if err := xml.Unmarshal([]byte(raw), &vx); err != nil {
			return rollback(err)
		}
		if vx.Type != "file" && vx.Type != "block" {
			return rollback(badRequest("volumes of type " + vx.Type + " cannot be attached here; use the XML tab"))
		}
		for _, d := range []*domxml.Doc{live, cfg} {
			for _, k := range d.Devices("disk") {
				if diskSourcePath(k) == vx.Target.Path {
					return rollback(badRequest("this volume is already attached to the VM as " + k.Child("target").Attrs["dev"]))
				}
			}
		}
		format := vx.Target.Format.Type
		if format == "" || format == "none" || format == "dir" {
			format = "raw"
		}
		target := domxml.NextTarget(prefix, usedTargets(live, cfg))
		var b strings.Builder
		diskType, srcAttr := "file", "file"
		if vx.Type == "block" {
			diskType, srcAttr = "block", "dev"
		}
		fmt.Fprintf(&b, "<disk type='%s' device='disk'><driver name='qemu' type='%s'", diskType, domxml.Esc(format))
		if req.Cache != "" && req.Cache != "default" {
			fmt.Fprintf(&b, " cache='%s'", req.Cache)
		}
		fmt.Fprintf(&b, " discard='unmap'/><source %s='%s'/><target dev='%s' bus='%s'/>", srcAttr, domxml.Esc(vx.Target.Path), target, req.Bus)
		if req.ReadOnly {
			b.WriteString("<readonly/>")
		}
		b.WriteString("</disk>")
		applied, err := c.attach(b.String())
		if err != nil {
			return rollback(err)
		}
		res = c.result(applied)
		res.Target = target
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func diskMatch(target string) func(*domxml.Node) bool {
	return func(n *domxml.Node) bool {
		t := n.Child("target")
		return t != nil && t.Attrs["dev"] == target
	}
}

func (s *Server) hwDiskDelete(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	target := r.PathValue("target")
	wantDelete := r.URL.Query().Get("delete") == "1"
	var res hwResult
	err = s.task(r, "detach disk", c.dom.Name+"/"+target, c.h.Name, func(func(float64)) error {
		live, cfg, err := c.docs()
		if err != nil {
			return err
		}
		// Remember what the disk was before it disappears.
		var src, device string
		var readOnly bool
		for _, d := range []*domxml.Doc{live, cfg} {
			for _, k := range d.Devices("disk") {
				if diskMatch(target)(k) && src == "" {
					src, device, readOnly = diskSourcePath(k), k.Attrs["device"], k.Child("readonly") != nil
				}
			}
		}
		applied, pending, err := c.detach("disk", diskMatch(target))
		if err != nil {
			return err
		}
		res = c.result(applied)
		if pending {
			res.Applied, res.NeedsRestart = "config", true
			res.addNote("the guest has not released the disk yet; it disappears once the guest lets go of it, or at the next restart")
		}
		if wantDelete {
			switch {
			case pending:
				res.addNote("The volume was not deleted because it may still be in use.")
			case device != "disk" || readOnly || src == "":
				res.addNote("The volume was not deleted: CD-ROMs and read-only media are shared and never deleted.")
			default:
				vol, err := c.h.StorageVolLookupByPath(src)
				if err != nil {
					res.addNote(src + " is not in a storage pool and was left in place.")
				} else if deps, _ := linkedClones(c.h, []string{src}); len(deps) > 0 {
					res.addNote("The volume was not deleted because other volumes are built on it: " + strings.Join(deps, ", ") + ".")
				} else if err := c.h.StorageVolDelete(vol, 0); err != nil {
					return fmt.Errorf("the disk was detached but its volume %s could not be deleted: %w", src, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) hwDiskResize(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	target := r.PathValue("target")
	var req struct {
		SizeGiB int `json:"sizeGiB"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.SizeGiB < 1 || req.SizeGiB > 65536 {
		fail(w, badRequest("sizeGiB must be between 1 and 65536"))
		return
	}
	size := uint64(req.SizeGiB) << 30
	var res hwResult
	err = s.task(r, "resize disk", c.dom.Name+"/"+target, c.h.Name, func(func(float64)) error {
		live, _, err := c.docs()
		if err != nil {
			return err
		}
		var src string
		for _, k := range live.Devices("disk") {
			if diskMatch(target)(k) {
				if k.Attrs["device"] != "disk" {
					return badRequest(target + " is not a disk")
				}
				src = diskSourcePath(k)
			}
		}
		if src == "" {
			return badRequest("no disk " + target + " with a backing file")
		}
		_, capacity, _, err := c.h.DomainGetBlockInfo(c.dom, src, 0)
		if err != nil {
			return err
		}
		if size <= capacity {
			return badRequest(fmt.Sprintf("disks can only grow: %s is already %d GiB", target, capacity>>30))
		}
		if c.running {
			if err := c.h.DomainBlockResize(c.dom, target, size, libvirt.DomainBlockResizeBytes); err != nil {
				return err
			}
			res = hwResult{Applied: "live+config", Note: "the disk was grown under the running guest; grow its partition and filesystem inside the guest"}
			return nil
		}
		vol, err := c.h.StorageVolLookupByPath(src)
		if err != nil {
			return fmt.Errorf("%s is not in a storage pool, so it cannot be resized while the VM is off: %w", src, err)
		}
		if err := c.h.StorageVolResize(vol, size, 0); err != nil {
			return err
		}
		res = hwResult{Applied: "config", Note: "grow the partition and filesystem inside the guest after it starts"}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// --- CD-ROM ---

// hwCDROMAdd adds an empty SATA drive. libvirt cannot hot-plug SATA, so on a
// running VM the drive is saved and appears after a restart (an empty USB
// CD-ROM is not allowed either).

func (s *Server) hwCDROMAdd(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, "add CD-ROM", c.dom.Name, c.h.Name, func(func(float64)) error {
		live, cfg, err := c.docs()
		if err != nil {
			return err
		}
		for _, k := range cfg.Devices("disk") {
			if k.Attrs["device"] == "cdrom" {
				return badRequest("the VM already has a CD-ROM drive: " + k.Child("target").Attrs["dev"])
			}
		}
		target := domxml.NextTarget("sd", usedTargets(live, cfg))
		applied, err := c.attach(fmt.Sprintf("<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><target dev='%s' bus='sata'/><readonly/></disk>", target))
		if err != nil {
			return err
		}
		res = c.result(applied)
		res.Target = target
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// hwCDROMMedia inserts or swaps the medium of a CD-ROM drive; an empty body
// or null ejects it.
func (s *Server) hwCDROMMedia(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	target := r.PathValue("target")
	var req struct {
		Pool   string `json:"pool"`
		Volume string `json:"volume"`
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if t := strings.TrimSpace(string(body)); t != "" && t != "null" {
		if err := json.Unmarshal(body, &req); err != nil {
			fail(w, badRequest("invalid request body: "+err.Error()))
			return
		}
	}
	if (req.Pool == "") != (req.Volume == "") {
		fail(w, badRequest("give both pool and volume, or neither to eject"))
		return
	}
	source := ""
	if req.Volume != "" {
		pool, err := c.h.StoragePoolLookupByName(req.Pool)
		if err != nil {
			fail(w, err)
			return
		}
		vol, err := c.h.StorageVolLookupByName(pool, req.Volume)
		if err != nil {
			fail(w, err)
			return
		}
		path, err := c.h.StorageVolGetPath(vol)
		if err != nil {
			fail(w, err)
			return
		}
		source = "<source file='" + domxml.Esc(path) + "'/>"
	}
	kind := "eject CD-ROM"
	if source != "" {
		kind = "insert CD-ROM media"
	}
	var res hwResult
	err = s.task(r, kind, c.dom.Name+"/"+target, c.h.Name, func(func(float64)) error {
		match := func(n *domxml.Node) bool { return diskMatch(target)(n) && n.Attrs["device"] == "cdrom" }
		applied, err := c.update("disk", match, func(d *domxml.Doc, n *domxml.Node) (string, error) {
			return d.DiskWithSource(n, source)
		})
		if err != nil {
			return err
		}
		res = c.result(applied)
		res.Target = target
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// --- network interfaces ---

var (
	macRE    = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	bridgeRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)
	nicModel = map[string]bool{"virtio": true, "e1000": true, "e1000e": true, "rtl8139": true}
)

func nicMatch(mac string) func(*domxml.Node) bool {
	return func(n *domxml.Node) bool {
		m := n.Child("mac")
		return m != nil && strings.EqualFold(m.Attrs["address"], mac)
	}
}

func macs(docs ...*domxml.Doc) map[string]bool {
	out := map[string]bool{}
	for _, d := range docs {
		for _, n := range d.Devices("interface") {
			if m := n.Child("mac"); m != nil {
				out[strings.ToLower(m.Attrs["address"])] = true
			}
		}
	}
	return out
}

func (c hwCtx) checkAttachment(network, bridge string) error {
	if (network == "") == (bridge == "") {
		return badRequest("give either a network or a bridge")
	}
	if network != "" {
		_, err := c.h.NetworkLookupByName(network)
		return err
	}
	if !bridgeRE.MatchString(bridge) {
		return badRequest("invalid bridge name")
	}
	nics, err := hostNICList(c.h)
	if err != nil {
		return err
	}
	return checkBridge(nics, bridge)
}

func (s *Server) hwNICAdd(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Network string `json:"network"`
		Bridge  string `json:"bridge"`
		Model   string `json:"model"`
		MAC     string `json:"mac"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Model == "" {
		req.Model = "virtio"
	}
	switch {
	case !nicModel[req.Model]:
		err = badRequest("model must be virtio, e1000, e1000e or rtl8139")
	case req.MAC != "" && !macRE.MatchString(req.MAC):
		err = badRequest("invalid MAC address")
	default:
		err = c.checkAttachment(req.Network, req.Bridge)
	}
	if err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, "add network interface", c.dom.Name, c.h.Name, func(func(float64)) error {
		live, cfg, err := c.docs()
		if err != nil {
			return err
		}
		before := macs(live, cfg)
		if req.MAC != "" && before[strings.ToLower(req.MAC)] {
			return badRequest("the VM already has an interface with MAC " + req.MAC)
		}
		var b strings.Builder
		if req.Network != "" {
			b.WriteString("<interface type='network'>")
		} else {
			b.WriteString("<interface type='bridge'>")
		}
		if req.MAC != "" {
			fmt.Fprintf(&b, "<mac address='%s'/>", strings.ToLower(req.MAC))
		}
		if req.Network != "" {
			fmt.Fprintf(&b, "<source network='%s'/>", domxml.Esc(req.Network))
		} else {
			fmt.Fprintf(&b, "<source bridge='%s'/>", domxml.Esc(req.Bridge))
		}
		fmt.Fprintf(&b, "<model type='%s'/></interface>", req.Model)
		applied, err := c.attach(b.String())
		if err != nil {
			return err
		}
		res = c.result(applied)
		if nl, nc, err := c.docs(); err == nil {
			for m := range macs(nl, nc) {
				if !before[m] {
					res.MAC = m
				}
			}
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) hwNICDelete(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	mac := r.PathValue("mac")
	if !macRE.MatchString(mac) {
		fail(w, badRequest("invalid MAC address"))
		return
	}
	var res hwResult
	err = s.task(r, "remove network interface", c.dom.Name+"/"+mac, c.h.Name, func(func(float64)) error {
		applied, pending, err := c.detach("interface", nicMatch(mac))
		if err != nil {
			return err
		}
		res = c.result(applied)
		if pending {
			res.Applied, res.NeedsRestart = "config", true
			res.addNote("the guest has not released the interface yet; it disappears once the guest lets go of it, or at the next restart")
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) hwNICUpdate(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	mac := r.PathValue("mac")
	var req struct {
		Network   string `json:"network"`
		Bridge    string `json:"bridge"`
		LinkState string `json:"linkState"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	switch {
	case !macRE.MatchString(mac):
		err = badRequest("invalid MAC address")
	case req.LinkState != "" && req.LinkState != "up" && req.LinkState != "down":
		err = badRequest("linkState must be up or down")
	case req.Network == "" && req.Bridge == "" && req.LinkState == "":
		err = badRequest("nothing to change")
	case req.Network != "" || req.Bridge != "":
		err = c.checkAttachment(req.Network, req.Bridge)
	}
	if err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, "change network interface", c.dom.Name+"/"+mac, c.h.Name, func(func(float64)) error {
		applied, err := c.update("interface", nicMatch(mac), func(d *domxml.Doc, n *domxml.Node) (string, error) {
			return d.SetNICSource(n, req.Network, req.Bridge, req.LinkState)
		})
		if err != nil {
			return err
		}
		res = c.result(applied)
		res.MAC = strings.ToLower(mac)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// --- boot order ---

// hwBoot sets the boot order as per-device <boot order> elements. libvirt has
// no call for this, so the saved definition is edited in place and redefined.
func (s *Server) hwBoot(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Order []string `json:"order"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if len(req.Order) == 0 {
		fail(w, badRequest("the boot order needs at least one device"))
		return
	}
	var res hwResult
	err = s.task(r, "set boot order", c.dom.Name, c.h.Name, func(func(float64)) error {
		raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive)
		if err != nil {
			return err
		}
		edited, err := domxml.SetBootOrder(raw, req.Order)
		if err != nil {
			return badRequest(err.Error())
		}
		if _, err := c.h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate); err != nil {
			return err
		}
		res = c.result("config")
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}
