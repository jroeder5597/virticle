package api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/domxml"
	"virticle/internal/hostconn"
)

// Moving a VM's disk to another storage pool on the same node.
//
// Running VM: libvirt block copy into a volume that is created in the target
// pool first, then a pivot; the guest never stops. Shut-off VM: the volume is
// copied with the storage API and only that disk's <source> is changed in the
// saved definition (a targeted splice, see package domxml).
//
// Both paths copy the guest-visible data only. Internal snapshots live inside
// the qcow2 file and are not carried over by either path, so a disk that
// snapshots refer to is refused (the plan says why).

type moveReq struct {
	Pool      string `json:"pool"`
	Name      string `json:"name"`   // new volume name; empty keeps the old one
	Format    string `json:"format"` // qcow2 | raw; empty keeps the old one
	DeleteOld bool   `json:"deleteOld"`
	// Flatten acknowledges that a disk with a backing chain (a linked clone)
	// is copied as one standalone image.
	Flatten bool `json:"flatten"`
}

var (
	moveNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	moveFormats  = map[string]bool{"qcow2": true, "raw": true}
	errMoveAbort = errors.New("disk move cancelled")
)

// moveDisk is what the move needs to know about one <disk> element.
type moveDisk struct {
	Target    string
	Device    string // disk | cdrom | floppy | lun
	Type      string // file | block | network | volume | ...
	Format    string // driver type
	File      string // source file (type file)
	ReadOnly  bool
	Shareable bool
	Backing   bool // the definition carries a non-empty <backingStore>
}

func moveDiskOf(n *domxml.Node) moveDisk {
	d := moveDisk{Device: n.Attrs["device"], Type: n.Attrs["type"], ReadOnly: n.Child("readonly") != nil, Shareable: n.Child("shareable") != nil}
	if t := n.Child("target"); t != nil {
		d.Target = t.Attrs["dev"]
	}
	if dr := n.Child("driver"); dr != nil {
		d.Format = dr.Attrs["type"]
	}
	if src := n.Child("source"); src != nil {
		d.File = src.Attrs["file"]
	}
	if b := n.Child("backingStore"); b != nil && len(b.Children) > 0 {
		d.Backing = true
	}
	return d
}

func findMoveDisk(d *domxml.Doc, target string) (*domxml.Node, moveDisk, bool) {
	for _, n := range d.Devices("disk") {
		if m := moveDiskOf(n); m.Target == target {
			return n, m, true
		}
	}
	return nil, moveDisk{}, false
}

// diskMoveXML points the disk with the given target at newPath: only the
// source's file attribute (and the driver type when the format changes) are
// touched. A populated <backingStore> is dropped, because the new volume is a
// standalone image.
func diskMoveXML(src, target, newPath, newFormat string) (string, error) {
	doc, err := domxml.Parse(src)
	if err != nil {
		return "", err
	}
	n, _, ok := findMoveDisk(doc, target)
	if !ok {
		return "", fmt.Errorf("no disk %s in the definition", target)
	}
	s := n.Child("source")
	if s == nil {
		return "", fmt.Errorf("disk %s has no source", target)
	}
	edits := []domxml.Splice{doc.SetAttr(s, "file", newPath)}
	if newFormat != "" {
		if dr := n.Child("driver"); dr != nil {
			edits = append(edits, doc.SetAttr(dr, "type", newFormat))
		} else {
			edits = append(edits, doc.AppendChild(n, "<driver name='qemu' type='"+domxml.Esc(newFormat)+"'/>"))
		}
	}
	// An empty <backingStore/> only says "no backing file" and stays.
	if b := n.Child("backingStore"); b != nil && len(b.Children) > 0 {
		edits = append(edits, doc.Remove(b))
	}
	return domxml.Apply(src, edits)
}

func blockCopyDestXML(file, format string) string {
	return fmt.Sprintf("<disk type='file'><driver name='qemu' type='%s'/><source file='%s'/></disk>", domxml.Esc(format), domxml.Esc(file))
}

// moveSpace says how much room the new volume may need. need is what must be
// free; worst, when larger, is what a fully allocated copy would take.
func moveSpace(dstFormat string, capacity, alloc uint64, chain bool) (need, worst uint64) {
	if chain {
		// The top layer's allocation says nothing about the flattened size.
		return capacity, capacity
	}
	need = alloc + alloc/20 + 1<<20
	if dstFormat == "raw" {
		return need, capacity
	}
	return need, need
}

// poolSupported says whether volumes of a pool type can be moved to. Only
// file based pools are tested; the others would need a different disk source
// (block device, network) than the one this feature writes.
func poolSupported(t string) (bool, string) {
	switch t {
	case "dir", "fs", "netfs":
		return true, ""
	case "logical", "disk", "iscsi", "iscsi-direct", "scsi", "mpath", "rbd", "sheepdog", "gluster", "zfs", "vstorage":
		return false, "pool type " + t + " is not supported (block or network volumes need a different disk source; only dir, fs and netfs pools are tested)"
	}
	return false, "pool type " + t + " is untested and refused"
}

// snapRefs sorts the snapshots that touch disk target into internal and
// external ones. A snapshot without a per-disk list is counted as internal,
// the conservative choice.
func snapRefs(snaps []snapXML, target string) (internal, external, other []string) {
	for _, s := range snaps {
		if len(s.Disks) == 0 {
			internal = append(internal, s.Name)
			continue
		}
		hit := false
		for _, d := range s.Disks {
			if d.Name != target {
				continue
			}
			hit = true
			switch d.Snapshot {
			case "external":
				external = append(external, s.Name)
			case "no":
				other = append(other, s.Name)
			default:
				internal = append(internal, s.Name)
			}
		}
		if !hit {
			other = append(other, s.Name)
		}
	}
	return
}

// volDesc is the part of a volume's XML the move uses.
type volDesc struct {
	Name       string
	Path       string
	Format     string
	Capacity   uint64
	Allocation uint64
	Backing    string
}

func parseVolDesc(raw string) (volDesc, error) {
	var x struct {
		Name       string `xml:"name"`
		Capacity   uint64 `xml:"capacity"`
		Allocation uint64 `xml:"allocation"`
		Target     struct {
			Path   string `xml:"path"`
			Format struct {
				Type string `xml:"type,attr"`
			} `xml:"format"`
		} `xml:"target"`
		Backing struct {
			Path string `xml:"path"`
		} `xml:"backingStore"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return volDesc{}, err
	}
	return volDesc{x.Name, x.Target.Path, x.Target.Format.Type, x.Capacity, x.Allocation, strings.TrimSpace(x.Backing.Path)}, nil
}

func volDescOf(h *hostconn.Host, v libvirt.StorageVol) (volDesc, error) {
	raw, err := h.StorageVolGetXMLDesc(v, 0)
	if err != nil {
		return volDesc{}, err
	}
	return parseVolDesc(raw)
}

// moveFacts is everything the move needs, collected by the plan.
type moveFacts struct {
	Running          bool
	Target           string
	SrcPath          string
	SrcFormat        string
	DstFormat        string
	DstName          string
	Capacity, Alloc  uint64
	Chain            bool
	SrcVol           libvirt.StorageVol
	SrcPool, DstPool libvirt.StoragePool
	DstTarget        string // the target pool's directory
}

type movePlanOut struct {
	OK         bool      `json:"ok"`
	Mode       string    `json:"mode,omitempty"` // live | offline
	Checks     checkList `json:"checks"`
	Source     string    `json:"source,omitempty"`
	Format     string    `json:"format,omitempty"`
	Capacity   uint64    `json:"capacity"`
	Allocation uint64    `json:"allocation"`
	Backing    bool      `json:"backing"`
	NewPath    string    `json:"newPath,omitempty"`
}

var moveRuns = struct {
	sync.Mutex
	m map[string]*moveRun
}{m: map[string]*moveRun{}}

func moveKey(dom libvirt.Domain, target string) string {
	return cluster.FormatUUID(dom.UUID) + "/" + target
}

// movePlan runs the pre-flight checks. facts is nil when the disk itself
// cannot be moved; otherwise the caller must still look at checks.blocked().
func (s *Server) movePlan(h *hostconn.Host, dom libvirt.Domain, target string, req moveReq) (*moveFacts, checkList) {
	var c checkList
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		c.bad("VM", err.Error())
		return nil, c
	}
	st := stateName(state)
	switch st {
	case "running":
		c.pass("VM", "running: the disk is copied while the guest keeps running")
	case "shutoff":
		c.pass("VM", "shut off: the volume is copied and the definition updated")
	default:
		c.bad("VM", "the VM is "+st+"; a disk can only be moved while it is running or shut off")
		return nil, c
	}
	hc := hwCtx{h: h, dom: dom, running: st == "running", why: new(string)}
	live, cfg, err := hc.docs()
	if err != nil {
		c.bad("VM", err.Error())
		return nil, c
	}
	_, cd, ok := findMoveDisk(cfg, target)
	if !ok {
		c.bad("Disk", "the VM has no disk "+target)
		return nil, c
	}
	if hc.running {
		_, ld, ok := findMoveDisk(live, target)
		if !ok {
			c.bad("Disk", target+" is in the saved definition but not attached to the running VM; restart the VM first")
			return nil, c
		}
		if ld.File != cd.File {
			c.bad("Disk", "the running VM and its saved definition use different files for "+target+"; restart the VM first")
			return nil, c
		}
	}
	switch {
	case cd.Device == "cdrom" || cd.Device == "floppy":
		c.bad("Disk", target+" is a CD-ROM or floppy drive")
		return nil, c
	case cd.Device == "lun":
		c.bad("Disk", target+" is a passthrough LUN")
		return nil, c
	case cd.Type != "file" || cd.File == "":
		c.bad("Disk", target+" is not a plain image file (type "+cd.Type+"); passthrough devices and network or pool-defined disks are not offered")
		return nil, c
	case cd.Shareable:
		c.bad("Disk", target+" is shareable; other VMs may use it")
		return nil, c
	case cd.ReadOnly && hc.running:
		c.bad("Disk", target+" is read-only; a running VM cannot copy it live. Shut the VM down first")
		return nil, c
	}

	f := &moveFacts{Running: hc.running, Target: target, SrcPath: cd.File, SrcFormat: cd.Format}
	if f.SrcFormat == "" {
		f.SrcFormat = "raw"
	}
	vol, err := h.StorageVolLookupByPath(cd.File)
	if err != nil {
		c.bad("Disk", cd.File+" is not a volume of a storage pool on this node, so it cannot be moved from here")
		return nil, c
	}
	f.SrcVol = vol
	if f.SrcPool, err = h.StoragePoolLookupByVolume(vol); err != nil {
		c.bad("Disk", err.Error())
		return nil, c
	}
	vd, err := volDescOf(h, vol)
	if err != nil {
		c.bad("Disk", err.Error())
		return nil, c
	}
	f.Capacity, f.Alloc = vd.Capacity, vd.Allocation
	f.Chain = vd.Backing != "" || cd.Backing
	if !moveFormats[f.SrcFormat] {
		c.bad("Format", "the disk format "+f.SrcFormat+" is not supported (qcow2 and raw are)")
		return nil, c
	}
	c.pass("Disk", fmt.Sprintf("%s: %s, %s, %s allocated", cd.File, f.SrcFormat, fmtBytes(f.Capacity), fmtBytes(f.Alloc)))

	// Target format and name.
	f.DstFormat = req.Format
	if f.DstFormat == "" {
		f.DstFormat = f.SrcFormat
	}
	if !moveFormats[f.DstFormat] {
		c.bad("Format", "the format must be qcow2 or raw")
		return f, c
	}
	if f.DstFormat != f.SrcFormat {
		c.warn("Format", "the data is converted from "+f.SrcFormat+" to "+f.DstFormat)
	}
	f.DstName = req.Name
	if f.DstName == "" {
		f.DstName = vol.Name
	}
	if !moveNameRE.MatchString(f.DstName) {
		c.bad("Volume name", "the name may contain letters, digits, dot, dash, plus and underscore, and must start with a letter or digit")
		return f, c
	}

	// Where to.
	if req.Pool == "" {
		c.bad("Target pool", "choose a target pool")
		return f, c
	}
	dp, err := h.StoragePoolLookupByName(req.Pool)
	if err != nil {
		c.bad("Target pool", "no pool "+req.Pool+" on this node")
		return f, c
	}
	f.DstPool = dp
	pstate, _, _, avail, err := h.StoragePoolGetInfo(dp)
	if err != nil {
		c.bad("Target pool", err.Error())
		return f, c
	}
	if pstate != 2 { // VIR_STORAGE_POOL_RUNNING
		c.bad("Target pool", "pool "+req.Pool+" is not active")
		return f, c
	}
	rawPool, err := h.StoragePoolGetXMLDesc(dp, 0)
	if err != nil {
		c.bad("Target pool", err.Error())
		return f, c
	}
	var px struct {
		Type   string `xml:"type,attr"`
		Target struct {
			Path string `xml:"path"`
		} `xml:"target"`
	}
	if err := xml.Unmarshal([]byte(rawPool), &px); err != nil {
		c.bad("Target pool", err.Error())
		return f, c
	}
	f.DstTarget = px.Target.Path
	if ok, why := poolSupported(px.Type); !ok {
		c.bad("Target pool", why)
		return f, c
	}
	c.pass("Target pool", fmt.Sprintf("%s (%s) is active, %s free", req.Pool, px.Type, fmtBytes(avail)))

	h.StoragePoolRefresh(dp, 0) // see volumes created outside libvirt
	if _, err := h.StorageVolLookupByName(dp, f.DstName); err == nil {
		c.bad("Volume name", "pool "+req.Pool+" already has a volume named "+f.DstName)
		return f, c
	}
	if dp.Name == f.SrcPool.Name && f.DstName == vol.Name {
		c.bad("Target pool", "that is the pool the disk is already in; pick another pool or a new name")
		return f, c
	}
	if strings.HasSuffix(f.DstName, ".qcow2") && f.DstFormat == "raw" || strings.HasSuffix(f.DstName, ".raw") && f.DstFormat == "qcow2" {
		c.warn("Volume name", "the name suggests a different format than "+f.DstFormat)
	}

	need, worst := moveSpace(f.DstFormat, f.Capacity, f.Alloc, f.Chain)
	switch {
	case avail < need:
		c.bad("Free space", fmt.Sprintf("the new volume needs about %s, the pool has %s free", fmtBytes(need), fmtBytes(avail)))
	case avail < worst:
		c.warn("Free space", fmt.Sprintf("%s free; the copy needs at least %s and up to %s if it fills up", fmtBytes(avail), fmtBytes(need), fmtBytes(worst)))
	default:
		c.pass("Free space", fmt.Sprintf("about %s needed, %s free", fmtBytes(need), fmtBytes(avail)))
	}

	// Things that depend on this exact volume.
	if deps, err := dependentVolumes(h, []string{cd.File}, nil); err != nil {
		c.bad("Dependents", "could not check for linked clones: "+err.Error())
	} else if len(deps) > 0 {
		c.bad("Dependents", "volumes are built on this disk and would break (delete or flatten them first): "+strings.Join(deps, ", "))
	} else {
		c.pass("Dependents", "no linked clones use this disk")
	}

	// Snapshots.
	snaps, err := moveSnapshots(h, dom)
	if err != nil {
		c.bad("Snapshots", "could not list snapshots: "+err.Error())
	} else {
		in, ex, oth := snapRefs(snaps, target)
		switch {
		case len(ex) > 0:
			c.bad("Snapshots", "external snapshots ("+strings.Join(ex, ", ")+") are chained to this disk; moving it would silently break or flatten that chain. Delete or merge the snapshots first")
		case len(in) > 0:
			why := "a live block copy does not carry them over"
			if !f.Running {
				why = "the volume copy does not carry them over"
			}
			c.bad("Snapshots", "internal snapshots ("+strings.Join(in, ", ")+") are stored inside this disk image and "+why+", and their saved definitions point at the old file. Delete the snapshots first")
		case len(oth) > 0:
			c.warn("Snapshots", "snapshots exist ("+strings.Join(oth, ", ")+") but do not include this disk; reverting one restores the old disk path, so the old volume is kept unless you tick delete")
		default:
			c.pass("Snapshots", "the VM has no snapshots")
		}
	}

	// Backing chain.
	switch {
	case f.Chain && !req.Flatten:
		c.bad("Backing chain", "this disk is a linked clone (it is built on "+orDash(vd.Backing)+"). Moving copies it as one standalone image that no longer shares the base; tick the box to accept that")
	case f.Chain:
		c.warn("Backing chain", "the disk is copied as one standalone image; the base volume stays where it is and is not touched")
	default:
		c.pass("Backing chain", "the disk has no backing file")
	}

	// A block job or a migration already running on the VM.
	if _, busy := moveRuns.m[moveKey(dom, target)]; busy {
		c.bad("Busy", "a move of this disk is already in progress")
	}
	if migRuns.m[cluster.FormatUUID(dom.UUID)] != nil {
		c.bad("Busy", "the VM is being migrated")
	}
	if f.Running {
		if found, _, _, _, _, err := h.DomainGetBlockJobInfo(dom, target, 0); err == nil && found != 0 {
			c.bad("Busy", "a block job is already running on "+target)
		}
	}
	if req.DeleteOld {
		c.pass("Old volume", "will be deleted after a successful move")
	} else {
		c.pass("Old volume", "stays in pool "+f.SrcPool.Name+" and is no longer used by this VM")
	}
	return f, c
}

func orDash(s string) string {
	if s == "" {
		return "a base image"
	}
	return s
}

func moveSnapshots(h *hostconn.Host, dom libvirt.Domain) ([]snapXML, error) {
	snaps, _, err := h.DomainListAllSnapshots(dom, 1, 0)
	if err != nil {
		return nil, err
	}
	var out []snapXML
	for _, sn := range snaps {
		raw, err := h.DomainSnapshotGetXMLDesc(sn, 0)
		if err != nil {
			return nil, err
		}
		var x snapXML
		if err := xml.Unmarshal([]byte(raw), &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func (s *Server) diskMovePlan(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	req := moveReq{Pool: q.Get("pool"), Name: q.Get("name"), Format: q.Get("format"), DeleteOld: q.Get("deleteOld") == "1", Flatten: q.Get("flatten") == "1"}
	moveRuns.Lock()
	migRuns.Lock()
	f, checks := s.movePlan(h, dom, r.PathValue("target"), req)
	migRuns.Unlock()
	moveRuns.Unlock()
	out := movePlanOut{Checks: checks, OK: !checks.blocked()}
	if f != nil {
		out.Mode = "offline"
		if f.Running {
			out.Mode = "live"
		}
		out.Source, out.Format, out.Capacity, out.Allocation, out.Backing = f.SrcPath, f.SrcFormat, f.Capacity, f.Alloc, f.Chain
		if f.DstTarget != "" && f.DstName != "" {
			out.NewPath = path.Join(f.DstTarget, f.DstName)
		}
	}
	writeJSON(w, out)
}

func (s *Server) diskMove(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req moveReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	target := r.PathValue("target")
	key := moveKey(dom, target)
	// The plan checks "busy" and the registration below must not interleave
	// with another start.
	moveRuns.Lock()
	migRuns.Lock()
	f, checks := s.movePlan(h, dom, target, req)
	migRuns.Unlock()
	if f == nil || checks.blocked() {
		moveRuns.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"error": "pre-flight checks failed", "checks": checks})
		return
	}
	m := &moveRun{s: s, h: h, dom: dom, f: f, req: req}
	moveRuns.m[key] = m
	moveRuns.Unlock()

	id, err := s.startBackground(r, "move disk", dom.Name+"/"+target+" → "+req.Pool, h.Name, func(progress func(float64)) error {
		defer func() {
			moveRuns.Lock()
			delete(moveRuns.m, key)
			moveRuns.Unlock()
		}()
		m.progress = progress
		return m.run()
	})
	if err != nil {
		moveRuns.Lock()
		delete(moveRuns.m, key)
		moveRuns.Unlock()
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"task": id, "mode": map[bool]string{true: "live", false: "offline"}[f.Running]})
}

func (s *Server) diskMoveCancel(w http.ResponseWriter, r *http.Request) {
	_, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	moveRuns.Lock()
	m := moveRuns.m[moveKey(dom, r.PathValue("target"))]
	moveRuns.Unlock()
	if m == nil {
		fail(w, badRequest("no move of this disk is in progress"))
		return
	}
	m.cancelled.Store(true)
	writeJSON(w, map[string]bool{"ok": true})
}

// --- the move itself ---

type moveRun struct {
	s        *Server
	h        *hostconn.Host
	dom      libvirt.Domain
	f        *moveFacts
	req      moveReq
	progress func(float64)

	cancelled atomic.Bool
	created   *libvirt.StorageVol // the new volume, until the move has succeeded
}

func (m *moveRun) run() error {
	m.progress(0.01)
	var err error
	if m.f.Running {
		err = m.runLive()
	} else {
		err = m.runOffline()
	}
	if err == nil {
		refreshPools(m.h)
	}
	return err
}

// createTarget creates the empty destination volume and returns its path.
func (m *moveRun) createTarget() (string, error) {
	v, err := m.h.StorageVolCreateXML(m.f.DstPool, volumeXML(m.f.DstName, m.f.DstFormat, m.f.Capacity), 0)
	if err != nil {
		return "", fmt.Errorf("creating %s in pool %s: %w", m.f.DstName, m.req.Pool, err)
	}
	m.created = &v
	vd, err := volDescOf(m.h, v)
	if err != nil {
		return "", err
	}
	return vd.Path, nil
}

// dropTarget removes the new volume again; the result is a note for the
// error message when that fails.
func (m *moveRun) dropTarget() string {
	if m.created == nil {
		return ""
	}
	err := m.h.StorageVolDelete(*m.created, 0)
	if err != nil {
		return fmt.Sprintf(" The new volume %s/%s could not be removed (%v) and was left behind.", m.req.Pool, m.created.Name, err)
	}
	m.created = nil
	return ""
}

func (m *moveRun) finishFail(err error, notes ...string) error {
	if m.cancelled.Load() && !errors.Is(err, errMoveAbort) {
		err = fmt.Errorf("%w: %v", errMoveAbort, err)
	}
	return withNote(err, notes...)
}

func (m *moveRun) deleteOld() error {
	if !m.req.DeleteOld {
		return nil
	}
	if err := m.h.StorageVolDelete(m.f.SrcVol, 0); err != nil {
		return fmt.Errorf("the disk was moved, but the old volume %s could not be deleted: %w", m.f.SrcPath, err)
	}
	return nil
}

func (m *moveRun) runLive() error {
	h, dom, f := m.h, m.dom, m.f
	newPath, err := m.createTarget()
	if err != nil {
		return m.finishFail(err, m.dropTarget())
	}
	m.progress(0.02)
	// TRANSIENT_JOB: libvirt 11 refuses a block copy on a persistent domain
	// without it ("domain is not transient"). REUSE_EXT: the destination
	// already exists with the right size and format.
	flags := libvirt.DomainBlockCopyReuseExt | libvirt.DomainBlockCopyTransientJob
	if err := h.DomainBlockCopy(dom, f.Target, blockCopyDestXML(newPath, f.DstFormat), nil, flags); err != nil {
		return m.finishFail(err, m.dropTarget())
	}
	abort := func(err error) error {
		note := ""
		if aerr := h.DomainBlockJobAbort(dom, f.Target, 0); aerr != nil {
			// The job may simply be gone (the VM stopped); only a live one is a problem.
			if found, _, _, _, _, ierr := h.DomainGetBlockJobInfo(dom, f.Target, 0); ierr != nil || found != 0 {
				note = fmt.Sprintf(" The block copy could not be aborted (%v); check with virsh blockjob %s %s and leave the new volume alone until it has ended.", aerr, dom.Name, f.Target)
				return m.finishFail(err, note)
			}
		}
		return m.finishFail(err, m.dropTarget())
	}
	for ready := false; !ready; {
		if m.cancelled.Load() {
			return abort(errMoveAbort)
		}
		found, typ, _, cur, end, err := h.DomainGetBlockJobInfo(dom, f.Target, 0)
		switch {
		case err != nil:
			return abort(err)
		case found == 0:
			return abort(errors.New("the block copy ended unexpectedly (did the VM stop?); the VM keeps its old disk"))
		case typ != 2:
			return abort(errors.New("another kind of block job replaced the copy"))
		}
		if end > 0 {
			frac := float64(cur) / float64(end)
			m.progress(0.02 + 0.95*frac)
			ready = cur >= end
		}
		if !ready {
			time.Sleep(time.Second)
		}
	}
	if m.cancelled.Load() {
		return abort(errMoveAbort)
	}
	// Mirrored and in sync: switch the guest over to the copy.
	if err := h.DomainBlockJobAbort(dom, f.Target, libvirt.DomainBlockJobAbortPivot); err != nil {
		return abort(fmt.Errorf("switching to the new disk: %w", err))
	}
	m.progress(0.98)
	m.created = nil // the guest uses it now
	var notes []string
	if err := m.ensureSaved(newPath); err != nil {
		// The saved definition may still name the old file: keep it, or the VM would not start again.
		notes = append(notes, err.Error(), "The old volume was kept.")
	} else if err := m.deleteOld(); err != nil {
		notes = append(notes, err.Error())
	}
	if len(notes) > 0 {
		return errors.New(strings.Join(notes, " "))
	}
	return nil
}

// ensureSaved makes sure the saved definition names the new file. libvirt
// updates it when the pivot completes; this repairs the case where it did not.
func (m *moveRun) ensureSaved(newPath string) error {
	raw, err := m.h.DomainGetXMLDesc(m.dom, libvirt.DomainXMLInactive)
	if err != nil {
		return fmt.Errorf("the disk was moved, but the saved definition could not be read back: %w", err)
	}
	doc, err := domxml.Parse(raw)
	if err != nil {
		return err
	}
	if _, d, ok := findMoveDisk(doc, m.f.Target); ok && d.File == newPath {
		return nil
	}
	edited, err := diskMoveXML(raw, m.f.Target, newPath, m.f.DstFormat)
	if err != nil {
		return fmt.Errorf("the disk was moved, but the saved definition still names the old file and could not be corrected: %w", err)
	}
	if _, err := m.h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate); err != nil {
		return fmt.Errorf("the disk was moved, but the saved definition still names the old file and could not be corrected: %w", err)
	}
	return nil
}

func (m *moveRun) runOffline() error {
	h, dom, f := m.h, m.dom, m.f
	// Progress: the size of the file that is being written.
	stop := make(chan struct{})
	defer close(stop)
	if p := path.Join(f.DstTarget, f.DstName); safePathRE.MatchString(p) && f.Alloc > 0 {
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
				}
				out, err := h.Run("stat -c '%b %B' " + shq(p))
				var blocks, size float64
				if err == nil {
					if _, e := fmt.Sscanf(out, "%f %f", &blocks, &size); e == nil {
						frac := blocks * size / float64(f.Alloc)
						if frac > 1 {
							frac = 1
						}
						m.progress(0.02 + 0.9*frac)
					}
				}
			}
		}()
	}
	// The storage API cannot interrupt a volume copy, so a cancel request is
	// honoured when the copy returns, before the definition is touched.
	nv, err := h.StorageVolCreateXMLFrom(f.DstPool, volumeXML(f.DstName, f.DstFormat, f.Capacity), f.SrcVol, 0)
	if err != nil {
		// libvirt may have left a partial file behind.
		h.StoragePoolRefresh(f.DstPool, 0)
		if v, lerr := h.StorageVolLookupByName(f.DstPool, f.DstName); lerr == nil {
			m.created = &v
		}
		return m.finishFail(fmt.Errorf("copying the volume: %w", err), m.dropTarget())
	}
	m.created = &nv
	m.progress(0.93)
	if m.cancelled.Load() {
		return m.finishFail(errMoveAbort, m.dropTarget())
	}
	vd, err := volDescOf(h, nv)
	if err != nil {
		return m.finishFail(err, m.dropTarget())
	}
	if state, _, _, _, _, err := h.DomainGetInfo(dom); err != nil || stateName(state) != "shutoff" {
		return m.finishFail(errors.New("the VM was started while its disk was being copied; nothing was changed"), m.dropTarget())
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return m.finishFail(err, m.dropTarget())
	}
	if doc, perr := domxml.Parse(raw); perr != nil {
		return m.finishFail(perr, m.dropTarget())
	} else if _, d, ok := findMoveDisk(doc, f.Target); !ok || d.File != f.SrcPath {
		return m.finishFail(errors.New("the disk definition changed while it was being copied; nothing was changed"), m.dropTarget())
	}
	edited, err := diskMoveXML(raw, f.Target, vd.Path, f.DstFormat)
	if err != nil {
		return m.finishFail(err, m.dropTarget())
	}
	if _, err := h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate); err != nil {
		return m.finishFail(fmt.Errorf("updating the VM definition: %w", err), m.dropTarget())
	}
	m.created = nil
	m.progress(0.97)
	return m.deleteOld()
}
