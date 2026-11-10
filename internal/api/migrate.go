package api

// Migration of a VM between two nodes without shared storage.
//
// Design choices worth knowing:
//   - Live moves use libvirt's peer-to-peer migration: the source libvirtd
//     connects to the destination itself (qemu+ssh), so memory and the NBD
//     disk mirror flow node to node, never through the controller.
//   - Files that libvirt does not mirror (backing chains, snapshot memory
//     states, read-only disks) are copied with scp run on the source node via
//     Host.Run. The nodes must trust each other over SSH for the libvirt
//     migration anyway, so this adds no prerequisite, and bulk data stays off
//     the controller (which in the lab sits behind a jump host).
//   - Offline moves do not use libvirt's offline migrate flag: it only
//     defines the XML and still needs the copy, refuses domains with snapshot
//     metadata and does not carry NVRAM. Copy + define + undefine by hand is
//     the same amount of code and each step can be rolled back.

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/cluster"
	"virt-proj/internal/hostconn"
)

type migReq struct {
	Target          int64  `json:"target"`
	Live            *bool  `json:"live"`
	KeepSourceDisks bool   `json:"keepSourceDisks"`
	MaxDowntimeMs   uint64 `json:"maxDowntimeMs"`
	BandwidthMiB    uint64 `json:"bandwidthMiB"`
	// Encrypt asks for TLS on the memory and disk streams. nil means "yes
	// when both nodes are set up for it" (see migtls.go).
	Encrypt *bool `json:"encrypt"`
}

type migCheck struct {
	Check    string `json:"check"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"` // error | warning | info
	Message  string `json:"message"`
}

type checkList []migCheck

func (c *checkList) pass(name, msg string) {
	*c = append(*c, migCheck{name, true, "info", msg})
}
func (c *checkList) bad(name, msg string) {
	*c = append(*c, migCheck{name, false, "error", msg})
}
func (c *checkList) warn(name, msg string) {
	*c = append(*c, migCheck{name, false, "warning", msg})
}
func (c checkList) blocked() bool {
	for _, k := range c {
		if !k.OK && k.Severity == "error" {
			return true
		}
	}
	return false
}

func fmtBytes(n uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

type planDisk struct {
	migDisk
	Chain []imgInfo // active image first
}

type migSnap struct {
	Name    string
	XML     string
	Current bool
	Files   snapFiles
}

// migPlan is everything learned about the VM on the source node.
type migPlan struct {
	Dom        migDomain
	Name, UUID string
	Mode       string // live | suspended | offline
	LiveXML    string
	PersistXML string
	Disks      []planDisk // file-backed disk devices
	Media      []string   // CD-ROM / floppy images
	Files      fileSet
	NVRAM      *migFile // copied in offline mode only
	Snaps      []migSnap
	MemKiB     uint64
	TLS        bool   // memory and NBD disk streams are encrypted (migtls.go)
	TLSName    string // host name QEMU verifies the target's certificate against
}

func (p *migPlan) active() bool { return p.Mode != "offline" }

// mirrored reports whether libvirt mirrors this disk's active image while the
// VM runs, as opposed to the disk being copied by us.
func (p *migPlan) mirrored(d planDisk) bool { return p.active() && !d.ReadOnly }

func (p *migPlan) mirrorFlag() libvirt.DomainMigrateFlags {
	mirrors := false
	for _, d := range p.Disks {
		mirrors = mirrors || p.mirrored(d)
	}
	if !mirrors {
		return 0
	}
	// With a backing chain (snapshots, or an overlay on a base image) the
	// chain is copied by us and only the active overlay is mirrored. libvirt
	// cannot flatten a chain while migrating: the target would be told to
	// open the backing files.
	if p.hasChain() {
		return libvirt.MigrateNonSharedInc
	}
	return libvirt.MigrateNonSharedDisk
}

func (p *migPlan) hasChain() bool {
	if len(p.Snaps) > 0 {
		return true
	}
	for _, d := range p.Disks {
		if !d.ReadOnly && len(d.Chain) > 1 {
			return true
		}
	}
	return false
}

// copyList lists the files this server copies node to node.
func (p *migPlan) copyList() []*migFile {
	var out []*migFile
	for _, f := range p.Files.list {
		if !f.Active && !f.Skip {
			out = append(out, f)
		}
	}
	if p.NVRAM != nil {
		out = append(out, p.NVRAM)
	}
	return out
}

func modeFor(state uint8, live *bool) string {
	switch {
	case stateName(state) == "shutoff":
		return "offline"
	case live != nil && !*live:
		return "suspended"
	}
	return "live"
}

// buildPlan inspects the VM and its snapshots on the source node. Problems
// that make migration impossible are added to c; the plan is nil only when
// the analysis could not be completed.
func buildPlan(src *hostconn.Host, dom libvirt.Domain, mode string, c *checkList) *migPlan {
	p := &migPlan{Mode: mode, Name: dom.Name, UUID: cluster.FormatUUID(dom.UUID)}
	if pers, err := src.DomainIsPersistent(dom); err == nil && pers == 0 {
		c.bad("Persistent definition", "this VM is transient (not defined, only running); define it first")
		return nil
	}
	var err error
	_, p.MemKiB, _, _, _, err = src.DomainGetInfo(dom)
	if err != nil {
		c.bad("Source VM", err.Error())
		return nil
	}
	if p.PersistXML, err = src.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive); err != nil {
		c.bad("Source VM", err.Error())
		return nil
	}
	p.LiveXML = p.PersistXML
	if p.active() {
		if p.LiveXML, err = src.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure); err != nil {
			c.bad("Source VM", err.Error())
			return nil
		}
	}
	if p.Dom, err = parseMigDomain(p.LiveXML); err != nil {
		c.bad("Source VM", "cannot read the domain XML: "+err.Error())
		return nil
	}

	// Tools used on the source node.
	if out, err := src.Run(`for t in qemu-img scp ssh; do command -v "$t" >/dev/null 2>&1 || echo "$t"; done`); err != nil || out != "" {
		c.bad("Tools on source", "missing on the source node: "+strings.Join(strings.Fields(out), ", ")+" (qemu-img, scp and ssh are required)")
		return nil
	}

	snaps, _, err := src.DomainListAllSnapshots(dom, 1, uint32(libvirt.DomainSnapshotListTopological))
	if err != nil {
		c.bad("Snapshots", err.Error())
		return nil
	}
	current := ""
	if cur, err := src.DomainSnapshotCurrent(dom, 0); err == nil {
		current = cur.Name
	}
	for _, sn := range snaps {
		x, err := src.DomainSnapshotGetXMLDesc(sn, uint32(libvirt.DomainSnapshotXMLSecure))
		if err != nil {
			c.bad("Snapshots", err.Error())
			return nil
		}
		f, err := parseSnapshotFiles(x)
		if err != nil {
			c.bad("Snapshots", "cannot read snapshot "+sn.Name+": "+err.Error())
			return nil
		}
		p.Snaps = append(p.Snaps, migSnap{Name: sn.Name, XML: x, Current: sn.Name == current, Files: f})
	}

	failed := false
	for _, d := range p.Dom.Disks {
		if d.Device == "cdrom" || d.Device == "floppy" {
			if d.Path != "" {
				p.Media = append(p.Media, d.Path)
			}
			continue
		}
		label := "Disk " + d.Target
		switch {
		case d.Type != "file":
			c.bad(label, "is a "+d.Type+" disk; only file-based disks in a storage pool can be moved between nodes")
			failed = true
		case d.Shareable:
			c.bad(label, "is shareable; a shared disk needs shared storage, not a copy")
			failed = true
		case d.Device != "disk":
			c.bad(label, "has device type "+d.Device+" which cannot be moved")
			failed = true
		default:
			out, err := src.Run("qemu-img info -U --backing-chain --output=json -- " + shq(d.Path))
			if err != nil {
				c.bad(label, "qemu-img could not inspect "+d.Path+": "+out)
				failed = true
				continue
			}
			chain, err := parseChain(out)
			if err != nil {
				c.bad(label, err.Error())
				failed = true
				continue
			}
			p.Disks = append(p.Disks, planDisk{d, chain})
		}
	}
	if failed {
		return nil
	}

	for _, d := range p.Disks {
		top := d.Chain[0].Path
		if d.ReadOnly {
			for _, img := range d.Chain {
				p.Files.add(img.Path, false, false)
			}
			continue
		}
		p.Files.add(top, true, p.mirrored(d))
		for _, img := range d.Chain[1:] {
			p.Files.add(img.Path, false, false)
		}
	}
	critical := map[string]bool{} // files the running or defined VM itself needs
	for _, f := range p.Files.list {
		critical[f.Path] = true
	}
	for _, sn := range p.Snaps {
		p.Files.add(sn.Files.Memory, true, false)
		for _, f := range sn.Files.Disks {
			p.Files.add(f, true, false)
		}
		for _, f := range sn.Files.Domain {
			p.Files.add(f, true, false)
		}
	}
	if !p.active() && p.Dom.NVRAM != "" {
		p.NVRAM = &migFile{Path: p.Dom.NVRAM}
	}

	// Sizes as allocated on the source; a file that no longer exists is an
	// error for files the VM owns.
	all := append([]*migFile{}, p.Files.list...)
	if p.NVRAM != nil {
		all = append(all, p.NVRAM)
	}
	for _, f := range all {
		if !safePathRE.MatchString(f.Path) {
			c.bad("File names", "unsupported characters in path "+f.Path+" (allowed: letters, digits and . _ + = , @ / -)")
			failed = true
		}
	}
	if failed {
		return nil
	}
	var script strings.Builder
	script.WriteString("for f in")
	for _, f := range all {
		script.WriteString(" " + shq(f.Path))
	}
	script.WriteString(`; do stat -L -c '%b %B' -- "$f" 2>/dev/null || echo MISSING; done`)
	out, err := src.Run(script.String())
	if err != nil {
		c.bad("Source files", "cannot inspect files on the source node: "+out)
		return nil
	}
	lines := strings.Split(out, "\n")
	if len(lines) != len(all) {
		c.bad("Source files", "unexpected output while inspecting files: "+out)
		return nil
	}
	for i, f := range all {
		var blocks, bsize uint64
		if _, err := fmt.Sscanf(lines[i], "%d %d", &blocks, &bsize); err != nil {
			if critical[f.Path] || f == p.NVRAM {
				c.bad("Source files", f.Path+" does not exist on the source node")
				failed = true
			} else {
				// Reverting to an older external snapshot deletes the overlays
				// of its descendants; those snapshots are then unusable anyway.
				c.warn("Source files", f.Path+" is referenced by a snapshot but no longer exists; it is skipped")
				p.Files.remove(f.Path)
			}
			continue
		}
		f.Size = blocks * bsize
	}
	if failed {
		return nil
	}
	return p
}

// dstPool is a storage pool of the target, found by its directory.
type dstPool struct {
	pool   libvirt.StoragePool
	active bool
	avail  uint64
}

func poolsByPath(h *hostconn.Host) (map[string]dstPool, error) {
	pools, _, err := h.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]dstPool{}
	for _, pl := range pools {
		raw, err := h.StoragePoolGetXMLDesc(pl, 0)
		if err != nil {
			continue
		}
		var x struct {
			Target struct {
				Path string `xml:"path"`
			} `xml:"target"`
		}
		if xml.Unmarshal([]byte(raw), &x) != nil || x.Target.Path == "" {
			continue
		}
		act, _ := h.StoragePoolIsActive(pl)
		dp := dstPool{pool: pl, active: act != 0}
		if dp.active {
			h.StoragePoolRefresh(pl, 0)
			if _, _, _, avail, err := h.StoragePoolGetInfo(pl); err == nil {
				dp.avail = avail
			}
		}
		out[path.Clean(x.Target.Path)] = dp
	}
	return out, nil
}

func (s *Server) nodeStatus(id int64) (cluster.NodeStatus, bool) {
	for _, n := range s.Nodes.Status() {
		if n.ID == id {
			return n, true
		}
	}
	return cluster.NodeStatus{}, false
}

// preflight runs every check against the target and returns the plan when
// the VM could be analysed. Errors in the list block the migration.
func (s *Server) preflight(srcID int64, src *hostconn.Host, dom libvirt.Domain, req migReq) (*migPlan, checkList) {
	var c checkList
	state, _, err := src.DomainGetState(dom, 0)
	if err != nil {
		c.bad("Source VM", err.Error())
		return nil, c
	}
	mode := modeFor(uint8(state), req.Live)

	dstNode, ok := s.nodeStatus(req.Target)
	switch {
	case !ok:
		c.bad("Target node", "no such node")
		return nil, c
	case req.Target == srcID:
		c.bad("Target node", "the target is the node the VM already runs on")
		return nil, c
	}
	dst, err := s.Nodes.Host(req.Target)
	if err != nil {
		c.bad("Target node", "not connected: "+err.Error())
		return nil, c
	}
	c.pass("Target node", dst.Name+" is connected")
	if dstNode.Maintenance {
		c.warn("Maintenance", dst.Name+" is marked as in maintenance; node evacuation never picks it, so this move is only sensible if you know why it is flagged")
	}

	plan := buildPlan(src, dom, mode, &c)
	if plan == nil {
		return nil, c
	}
	switch plan.Mode {
	case "live":
		c.pass("Migration type", "live migration: the VM keeps running")
	case "suspended":
		c.pass("Migration type", "non-live migration: the VM is paused while its memory is copied, then resumed on the target")
	default:
		c.pass("Migration type", "offline migration: the VM is shut off, its disks are copied and its definition moved")
	}

	// Name and UUID.
	if _, err := dst.DomainLookupByName(plan.Name); err == nil {
		c.bad("Name on target", "a VM named "+plan.Name+" already exists on "+dst.Name)
	} else if d, err := dst.DomainLookupByUUID(dom.UUID); err == nil {
		c.bad("UUID on target", "a VM with the same UUID ("+plan.UUID+") exists on "+dst.Name+" as "+d.Name)
	} else {
		c.pass("Name and UUID", "no VM with this name or UUID on "+dst.Name)
	}

	// Devices tied to the host.
	if plan.Dom.Hostdevs > 0 {
		c.bad("Passthrough devices", fmt.Sprintf("%d host device(s) (PCI/USB/mediated) are passed through; remove them before migrating", plan.Dom.Hostdevs))
	}
	if plan.Dom.TPM {
		if plan.active() {
			c.pass("TPM", "the VM has a TPM; libvirt transfers its state during the migration")
		} else {
			c.bad("TPM", "offline migration does not carry TPM state; migrate while the VM is running or remove the TPM")
		}
	}
	if plan.active() && plan.Dom.NVRAM != "" {
		c.pass("UEFI variables", "the VM uses UEFI; libvirt transfers the NVRAM contents during the migration")
	}

	// Machine type and QEMU binary: both are part of the definition and must
	// exist on the target (they differ between distributions).
	if plan.Dom.Machine != "" {
		opt := func(v string) libvirt.OptString {
			if v == "" {
				return nil
			}
			return libvirt.OptString{v}
		}
		if _, err := dst.ConnectGetDomainCapabilities(opt(plan.Dom.Emulator), opt(plan.Dom.Arch), opt(plan.Dom.Machine), nil, 0); err != nil {
			c.bad("Machine type", fmt.Sprintf("%s cannot run machine type %s with %s (%s). Nodes need the same distribution family and a QEMU at least as new as the source's", dst.Name, plan.Dom.Machine, plan.Dom.Emulator, firstLine(err.Error())))
		}
	}

	// CPU.
	switch {
	case plan.Dom.CPUXML == "":
		c.pass("CPU", "no CPU model is pinned")
	case plan.Dom.CPUMode == "host-passthrough":
		c.warn("CPU", "host-passthrough exposes the exact host CPU; the target must have an identical CPU or the guest may crash after the move")
	case plan.Dom.CPUMode == "host-model" && !plan.active():
		c.pass("CPU", "host-model is resolved against the target CPU when the VM starts there")
	default:
		arch := libvirt.OptString{}
		if plan.Dom.Arch != "" {
			arch = libvirt.OptString{plan.Dom.Arch}
		}
		res, err := dst.ConnectCompareHypervisorCPU(nil, arch, nil, libvirt.OptString{"kvm"}, plan.Dom.CPUXML, 0)
		switch {
		case err != nil:
			c.warn("CPU", "libvirt could not compare the CPU with the target: "+err.Error())
		case libvirt.CPUCompareResult(res) == libvirt.CPUCompareIncompatible:
			c.bad("CPU", "the target's CPU cannot provide the CPU model and features this VM uses")
		default:
			c.pass("CPU", "the target CPU is compatible with the VM's CPU definition")
		}
	}

	s.checkTuning(dst, plan, &c)

	// Memory.
	if free, err := dst.NodeGetFreeMemory(); err == nil {
		if free < plan.MemKiB*1024 {
			c.warn("Memory", fmt.Sprintf("the VM has %s of memory, the target reports %s free", fmtBytes(plan.MemKiB*1024), fmtBytes(free)))
		} else {
			c.pass("Memory", "the target has enough free memory ("+fmtBytes(free)+" free)")
		}
	}

	// Networks.
	for _, i := range plan.Dom.Ifs {
		switch {
		case i.Type == "network":
			n, err := dst.NetworkLookupByName(i.Network)
			if err != nil {
				c.bad("Network "+i.Network, "does not exist on "+dst.Name+" ("+err.Error()+")")
			} else if act, _ := dst.NetworkIsActive(n); act == 0 {
				c.bad("Network "+i.Network, "exists on "+dst.Name+" but is not active")
			} else {
				c.pass("Network "+i.Network, "exists and is active on "+dst.Name)
			}
		case i.Type == "bridge" && i.Bridge != "":
			if !safeIfRE.MatchString(i.Bridge) {
				c.bad("Bridge "+i.Bridge, "unsupported interface name")
			} else if out, err := dst.Run("test -d /sys/class/net/" + shq(i.Bridge) + "/bridge"); err != nil {
				c.bad("Bridge "+i.Bridge, "does not exist on "+dst.Name+" "+out)
			} else {
				c.pass("Bridge "+i.Bridge, "exists on "+dst.Name)
			}
		case i.Type == "direct" && i.Dev != "":
			if !safeIfRE.MatchString(i.Dev) {
				c.bad("Interface "+i.Dev, "unsupported interface name")
			} else if _, err := dst.Run("test -e /sys/class/net/" + shq(i.Dev)); err != nil {
				c.bad("Interface "+i.Dev, "macvtap parent device does not exist on "+dst.Name)
			} else {
				c.pass("Interface "+i.Dev, "exists on "+dst.Name)
			}
		}
	}

	// Install media must be reachable at the same path on the target.
	for _, m := range plan.Media {
		if !safePathRE.MatchString(m) {
			c.bad("Media "+m, "unsupported characters in path")
		} else if _, err := dst.Run("test -e " + shq(m)); err != nil {
			c.bad("Media "+m, "the image does not exist on "+dst.Name+"; copy it to the same path or eject it from the VM first")
		} else {
			c.pass("Media "+m, "exists on "+dst.Name)
		}
	}

	s.checkTargetFiles(dst, plan, &c)

	// Node to node SSH.
	s.checkSSHTrust(src, dst, dstNode, &c)

	// Encryption of the migration streams.
	s.checkEncryption(srcID, src, req.Target, dst, plan, req, &c)

	// Source cleanup would delete images that linked clones are built on.
	if !req.KeepSourceDisks {
		var owned, all []string
		for _, f := range plan.Files.list {
			all = append(all, f.Path)
			if f.Owned {
				owned = append(owned, f.Path)
			}
		}
		if deps, err := dependentVolumes(src, owned, all); err != nil {
			c.bad("Linked clones", err.Error())
		} else if len(deps) > 0 {
			c.bad("Linked clones", "linked clones on "+src.Name+" depend on this VM's disks ("+strings.Join(deps, ", ")+"). Migrate with \"keep source disks\", or delete the clones first")
		}
	}

	if len(plan.Snaps) > 0 {
		names := make([]string, len(plan.Snaps))
		for i, sn := range plan.Snaps {
			names[i] = sn.Name
		}
		var internal []string
		for _, sn := range plan.Snaps {
			if sn.Files.Internal {
				internal = append(internal, sn.Name)
			}
		}
		if len(internal) > 0 && plan.active() {
			c.bad("Internal snapshots", fmt.Sprintf("snapshot(s) %s are stored inside the disk image, which a migration of a running VM does not copy. Shut the VM down and migrate it offline (the whole image is copied), or delete these snapshots first", strings.Join(internal, ", ")))
		}
		c.pass("Snapshots", fmt.Sprintf("%d snapshot(s) (%s): the snapshot-preserving path is used; snapshot files are copied and the snapshots recreated on the target", len(names), strings.Join(names, ", ")))
	} else {
		for _, d := range plan.Disks {
			if !d.ReadOnly && len(d.Chain) > 1 {
				c.pass("Disk "+d.Target, "is an overlay on "+d.Chain[1].Path+"; the backing chain is copied and kept as it is (shared base images stay on the source)")
			}
		}
	}
	return plan, c
}

var safeIfRE = hostRE

// checkTargetFiles verifies pools, free space and name clashes for every file
// the VM needs on the target.
func (s *Server) checkTargetFiles(dst *hostconn.Host, plan *migPlan, c *checkList) {
	files := append([]*migFile{}, plan.Files.list...)
	if plan.NVRAM != nil {
		files = append(files, plan.NVRAM)
	}
	if len(files) == 0 {
		return
	}
	var script strings.Builder
	script.WriteString("for f in")
	for _, f := range files {
		script.WriteString(" " + shq(f.Path))
	}
	script.WriteString(`; do [ -e "$f" ] && echo "$f"; done; true`)
	out, err := dst.Run(script.String())
	if err != nil {
		c.bad("Files on target", "cannot inspect the target: "+out)
		return
	}
	exists := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		exists[strings.TrimSpace(l)] = true
	}
	clash := false
	for _, f := range files {
		if !exists[f.Path] {
			continue
		}
		if f.Owned || f.Active || f == plan.NVRAM {
			c.bad("Volume "+path.Base(f.Path), "already exists on "+dst.Name+" at "+f.Path+"; remove or rename it first")
			clash = true
		} else {
			f.Skip = true
			c.warn("Volume "+path.Base(f.Path), "already exists on "+dst.Name+"; it is a base or read-only image and is assumed to be identical, so it is not copied")
		}
	}
	if !clash {
		c.pass("Volumes on target", "no name clashes")
	}

	pools, err := poolsByPath(dst)
	if err != nil {
		c.bad("Storage on target", err.Error())
		return
	}
	need := map[string]uint64{}
	var dirs []string
	for _, f := range plan.Files.list {
		if f.Skip {
			continue
		}
		d := path.Dir(f.Path)
		if _, ok := need[d]; !ok {
			dirs = append(dirs, d)
		}
		need[d] += f.Size
	}
	for _, d := range dirs {
		pl, ok := pools[d]
		switch {
		case !ok:
			c.bad("Storage "+d, "no storage pool on "+dst.Name+" has the directory "+d+"; create a pool for it first")
		case !pl.active:
			c.bad("Storage "+d, "the pool "+pl.pool.Name+" on "+dst.Name+" is not active")
		case pl.avail < need[d]:
			c.bad("Storage "+d, fmt.Sprintf("pool %s has %s free, %s are needed", pl.pool.Name, fmtBytes(pl.avail), fmtBytes(need[d])))
		default:
			c.pass("Storage "+d, fmt.Sprintf("pool %s has %s free, about %s are needed", pl.pool.Name, fmtBytes(pl.avail), fmtBytes(need[d])))
		}
	}
	if plan.NVRAM != nil && !exists[path.Dir(plan.NVRAM.Path)] {
		if _, err := dst.Run("test -d " + shq(path.Dir(plan.NVRAM.Path))); err != nil {
			c.bad("NVRAM", "the directory "+path.Dir(plan.NVRAM.Path)+" does not exist on "+dst.Name)
		}
	}
}

// sshTarget is how the source reaches the target: the target's host name as
// libvirt reports it (it has to resolve from the source) and the SSH user
// configured for the node.
func sshTarget(dst *hostconn.Host, n cluster.NodeStatus) (user, host string, err error) {
	user, host = n.User, dst.Name
	if user == "" {
		user = "root"
	}
	if !userRE.MatchString(user) || !hostRE.MatchString(host) {
		return "", "", fmt.Errorf("unsupported characters in user %q or host name %q", user, host)
	}
	return user, host, nil
}

func (s *Server) checkSSHTrust(src, dst *hostconn.Host, n cluster.NodeStatus, c *checkList) {
	user, host, err := sshTarget(dst, n)
	if err != nil {
		c.bad("SSH between nodes", err.Error())
		return
	}
	out, err := src.Run("ssh -o BatchMode=yes -o ConnectTimeout=10 -o LogLevel=ERROR " + shq(user+"@"+host) + " hostname")
	if err != nil {
		c.bad("SSH between nodes", fmt.Sprintf(
			"%s cannot log in to %s@%s without a password (%s). Live migration and file copies run over this connection. "+
				"Fix: as root on %s create a key (ssh-keygen -t ed25519), install it with ssh-copy-id %s@%s, and run ssh %s@%s true once to accept the host key. "+
				"The name %s must also resolve from %s.",
			src.Name, user, host, firstLine(out), src.Name, user, host, user, host, host, src.Name))
		return
	}
	// Compare the short names: one side may report an FQDN.
	short := func(n string) string { h, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(n)), "."); return h }
	if short(out) != short(host) {
		c.bad("SSH between nodes", fmt.Sprintf("%s reaches a machine named %q when connecting to %s, expected %s", src.Name, firstLine(out), host, host))
		return
	}
	c.pass("SSH between nodes", fmt.Sprintf("%s can log in to %s@%s without a password", src.Name, user, host))
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// --- HTTP ---

type migPreview struct {
	OK         bool       `json:"ok"`
	Mode       string     `json:"mode,omitempty"`
	Checks     checkList  `json:"checks"`
	CopyBytes  uint64     `json:"copyBytes"`
	MirrorSize uint64     `json:"mirrorBytes"`
	Encrypt    bool       `json:"encrypt"` // the streams will be TLS-encrypted
	Files      []fileInfo `json:"files,omitempty"`
}

type fileInfo struct {
	Path   string `json:"path"`
	Size   uint64 `json:"size"`
	Action string `json:"action"` // copy | mirror | skip
}

func (s *Server) preview(srcID int64, src *hostconn.Host, dom libvirt.Domain, req migReq) (*migPlan, migPreview) {
	plan, checks := s.preflight(srcID, src, dom, req)
	out := migPreview{OK: !checks.blocked(), Checks: checks}
	if plan != nil {
		out.Mode = plan.Mode
		out.Encrypt = plan.TLS
		files := append([]*migFile{}, plan.Files.list...)
		if plan.NVRAM != nil {
			files = append(files, plan.NVRAM)
		}
		for _, f := range files {
			a := "copy"
			switch {
			case f.Skip:
				a = "skip"
			case f.Active:
				a = "mirror"
				out.MirrorSize += f.Size
			default:
				out.CopyBytes += f.Size
			}
			out.Files = append(out.Files, fileInfo{f.Path, f.Size, a})
		}
	}
	return plan, out
}

func (s *Server) migrateCheck(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req migReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	srcID, _, _ := s.host(r)
	_, out := s.preview(srcID, h, dom, req)
	writeJSON(w, out)
}

// migRuns tracks migrations in flight, keyed by VM UUID, so that cancelling
// and double starts can be handled.
var migRuns = struct {
	sync.Mutex
	m map[string]*migRun
}{m: map[string]*migRun{}}

func (s *Server) migrate(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req migReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	srcID, _, _ := s.host(r)
	l, pre, err := s.launchMigration(r, srcID, h, dom, req)
	if errors.Is(err, errPreflight) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		pre.OK = false
		json.NewEncoder(w).Encode(map[string]any{"error": "pre-flight checks failed", "checks": pre.Checks})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"task": l.Task, "mode": l.Mode})
}

var errPreflight = errors.New("pre-flight checks failed")

// migLaunch is a migration that has been started in the background.
type migLaunch struct {
	Task int64
	Mode string
	Done <-chan error // receives the task's result once
}

// launchMigration runs the pre-flight checks and starts the migration as a
// background task. It is shared by the HTTP handler and node evacuation.
// When the checks fail it returns errPreflight together with the preview.
func (s *Server) launchMigration(r *http.Request, srcID int64, h *hostconn.Host, dom libvirt.Domain, req migReq) (*migLaunch, migPreview, error) {
	plan, pre := s.preview(srcID, h, dom, req)
	if !pre.OK || plan == nil {
		return nil, pre, errPreflight
	}
	dst, err := s.Nodes.Host(req.Target)
	if err != nil {
		return nil, pre, err
	}
	dstNode, _ := s.nodeStatus(req.Target)
	user, host, err := sshTarget(dst, dstNode)
	if err != nil {
		return nil, pre, badRequest(err.Error())
	}
	m := &migRun{s: s, src: h, dst: dst, dom: dom, plan: plan, req: req, uri: "qemu+ssh://" + user + "@" + host + "/system", user: user, host: host}
	migRuns.Lock()
	if _, busy := migRuns.m[plan.UUID]; busy {
		migRuns.Unlock()
		return nil, pre, badRequest("a migration of this VM is already in progress")
	}
	migRuns.m[plan.UUID] = m
	migRuns.Unlock()

	id, done, err := s.startBackgroundWait(r, "migrate", plan.Name+" → "+dst.Name, h.Name, func(progress func(float64)) error {
		defer func() {
			migRuns.Lock()
			delete(migRuns.m, plan.UUID)
			migRuns.Unlock()
		}()
		m.progress = progress
		return m.run()
	})
	if err != nil {
		migRuns.Lock()
		delete(migRuns.m, plan.UUID)
		migRuns.Unlock()
		return nil, pre, err
	}
	return &migLaunch{Task: id, Mode: plan.Mode, Done: done}, pre, nil
}

// startBackground is like s.task but returns as soon as the task is
// recorded, so a request does not have to stay open for a long migration.
func (s *Server) startBackground(r *http.Request, kind, target, node string, fn func(progress func(float64)) error) (int64, error) {
	id, _, err := s.startBackgroundWait(r, kind, target, node, fn)
	return id, err
}

// startBackgroundWait is startBackground plus a channel that receives the
// task's result when it has finished.
func (s *Server) startBackgroundWait(r *http.Request, kind, target, node string, fn func(progress func(float64)) error) (int64, <-chan error, error) {
	t, err := s.Store.StartTask(kind, target, node, userOf(r))
	if err != nil {
		return 0, nil, err
	}
	done := make(chan error, 1)
	s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
	go func() {
		last := 0.0
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("internal error: %v", p)
					log.Printf("%s task panicked: %v", kind, p)
				}
			}()
			err = fn(func(p float64) {
				if p-last >= 0.01 {
					last = p
					s.Store.TaskProgress(t.ID, p)
					t.Progress = p
					s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
				}
			})
		}()
		s.Store.FinishTask(&t, err)
		s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
		done <- err
	}()
	return t.ID, done, nil
}

func (s *Server) migrateCancel(w http.ResponseWriter, r *http.Request) {
	_, dom, err := s.domain(r)
	uuid := r.PathValue("uuid")
	if err == nil {
		uuid = cluster.FormatUUID(dom.UUID)
	}
	migRuns.Lock()
	m := migRuns.m[uuid]
	migRuns.Unlock()
	if m == nil {
		fail(w, badRequest("no migration of this VM is in progress"))
		return
	}
	m.cancelled.Store(true)
	if m.inJob.Load() {
		if err := m.src.DomainAbortJob(m.dom); err != nil {
			// The poll loop retries; report only that cancelling was requested.
			log.Printf("abort job: %v", err)
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// --- the migration itself ---

type migRun struct {
	s        *Server
	src, dst *hostconn.Host
	dom      libvirt.Domain
	plan     *migPlan
	req      migReq
	uri      string
	user     string
	host     string
	progress func(float64)

	cancelled atomic.Bool
	inJob     atomic.Bool
	created   []string // paths created on the target, for rollback
}

var errCancelled = fmt.Errorf("migration cancelled")

func (m *migRun) checkCancel() error {
	if m.cancelled.Load() {
		return errCancelled
	}
	return nil
}

// spread maps a fraction of a phase into the overall 0..1 progress.
func (m *migRun) spread(lo, hi, f float64) {
	if f > 1 {
		f = 1
	}
	m.progress(lo + (hi-lo)*f)
}

func (m *migRun) run() error {
	m.progress(0.01)
	var err error
	if m.plan.Mode == "offline" {
		err = m.runOffline()
	} else {
		err = m.runOnline()
	}
	if err != nil && m.cancelled.Load() && !strings.Contains(err.Error(), "cancelled") {
		err = fmt.Errorf("migration cancelled: %w", err)
	}
	return err
}

// copyFiles copies everything libvirt will not mirror to the same path on
// the target.
func (m *migRun) copyFiles(lo, hi float64) error {
	list := m.plan.copyList()
	var total, done uint64
	for _, f := range list {
		total += f.Size
	}
	if total == 0 {
		total = 1
	}
	for _, f := range list {
		if err := m.checkCancel(); err != nil {
			return err
		}
		m.created = append(m.created, f.Path) // recorded first: a half-written file must be removed too
		cmd := "scp -q -p -o BatchMode=yes -- " + shq(f.Path) + " " + shq(m.user+"@"+m.host+":"+f.Path)
		if out, err := m.src.Run(cmd); err != nil {
			return fmt.Errorf("copying %s to %s failed: %w: %s", f.Path, m.dst.Name, err, out)
		}
		done += f.Size
		m.spread(lo, hi, float64(done)/float64(total))
	}
	m.refreshDst()
	return nil
}

func (m *migRun) refreshDst() {
	poolsByPath(m.dst) // refreshes every active pool as a side effect
}

// createActive pre-creates the empty destination image of every disk that
// libvirt will mirror, with the right size, format and backing file.
func (m *migRun) createActive() error {
	pools, err := poolsByPath(m.dst)
	if err != nil {
		return err
	}
	for _, d := range m.plan.Disks {
		if !m.plan.mirrored(d) {
			continue
		}
		top := d.Chain[0]
		pl, ok := pools[path.Dir(top.Path)]
		if !ok || !pl.active {
			return fmt.Errorf("no active storage pool for %s on %s", path.Dir(top.Path), m.dst.Name)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<volume><name>%s</name><capacity unit='bytes'>%d</capacity>", esc(path.Base(top.Path)), top.VirtualSize)
		if top.Format == "raw" {
			b.WriteString("<allocation unit='bytes'>0</allocation>")
		}
		fmt.Fprintf(&b, "<target><format type='%s'/></target>", esc(top.Format))
		if len(d.Chain) > 1 {
			fmt.Fprintf(&b, "<backingStore><path>%s</path><format type='%s'/></backingStore>", esc(d.Chain[1].Path), esc(d.Chain[1].Format))
		}
		b.WriteString("</volume>")
		m.created = append(m.created, top.Path)
		if _, err := m.dst.StorageVolCreateXML(pl.pool, b.String(), 0); err != nil {
			return fmt.Errorf("creating %s on %s: %w", top.Path, m.dst.Name, err)
		}
	}
	return nil
}

// rollbackDst removes what this run created on the target and describes
// anything it could not remove.
func (m *migRun) rollbackDst() string {
	var left []string
	for i := len(m.created) - 1; i >= 0; i-- {
		p := m.created[i]
		if vol, err := m.dst.StorageVolLookupByPath(p); err == nil {
			if err := m.dst.StorageVolDelete(vol, 0); err == nil {
				continue
			}
		}
		if out, err := m.dst.Run("rm -f -- " + shq(p)); err != nil {
			left = append(left, p+" ("+out+")")
		}
	}
	m.created = nil
	m.refreshDst()
	if len(left) > 0 {
		return "files left behind on " + m.dst.Name + ": " + strings.Join(left, ", ")
	}
	return ""
}

func withNote(err error, notes ...string) error {
	var keep []string
	for _, n := range notes {
		if n != "" {
			keep = append(keep, n)
		}
	}
	if len(keep) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s)", err, strings.Join(keep, "; "))
}

// deleteSnapshotMetadata removes the snapshot records, children first, and
// returns the ones removed so far if it fails.
func deleteSnapshotMetadata(h *hostconn.Host, dom libvirt.Domain, snaps []migSnap) ([]migSnap, error) {
	var gone []migSnap
	for i := len(snaps) - 1; i >= 0; i-- {
		sn, err := h.DomainSnapshotLookupByName(dom, snaps[i].Name, 0)
		if err == nil {
			err = h.DomainSnapshotDelete(sn, libvirt.DomainSnapshotDeleteMetadataOnly)
		}
		if err != nil {
			return gone, fmt.Errorf("removing metadata of snapshot %s: %w", snaps[i].Name, err)
		}
		gone = append([]migSnap{snaps[i]}, gone...) // keep parents first
	}
	return gone, nil
}

// redefineSnapshots recreates snapshot records, parents before children,
// marking the one that was current.
func redefineSnapshots(h *hostconn.Host, dom libvirt.Domain, snaps []migSnap) error {
	for _, sn := range snaps {
		flags := libvirt.DomainSnapshotCreateRedefine
		if sn.Current {
			flags |= libvirt.DomainSnapshotCreateCurrent
		}
		if _, err := h.DomainSnapshotCreateXML(dom, sn.XML, uint32(flags)); err != nil {
			return fmt.Errorf("redefining snapshot %s on %s: %w", sn.Name, h.Name, err)
		}
	}
	return nil
}

func (m *migRun) runOnline() error {
	p := m.plan
	if err := m.checkCancel(); err != nil {
		return err
	}
	autostart, _ := m.src.DomainGetAutostart(m.dom)

	if err := m.copyFiles(0.02, 0.25); err != nil {
		return withNote(err, m.rollbackDst())
	}
	if err := m.createActive(); err != nil {
		return withNote(err, m.rollbackDst())
	}
	if err := m.checkCancel(); err != nil {
		return withNote(err, m.rollbackDst())
	}

	// libvirt refuses to migrate a domain that has snapshot metadata. Take
	// the records off the source for the duration; the files stay.
	var removed []migSnap
	if len(p.Snaps) > 0 {
		var err error
		if removed, err = deleteSnapshotMetadata(m.src, m.dom, p.Snaps); err != nil {
			if rerr := redefineSnapshots(m.src, m.dom, removed); rerr != nil {
				err = fmt.Errorf("%w (restoring snapshots on the source failed: %v)", err, rerr)
			}
			return withNote(err, m.rollbackDst())
		}
	}

	flags := libvirt.MigratePeer2peer | libvirt.MigratePersistDest | libvirt.MigrateUndefineSource | p.mirrorFlag()
	if p.Mode == "live" {
		flags |= libvirt.MigrateLive | libvirt.MigrateAutoConverge
	}
	if p.TLS {
		flags |= libvirt.MigrateTLS
	}
	err := m.perform(flags, 0.25, 0.95)
	if err != nil {
		// A lost connection can hide a migration that actually completed.
		if d, derr := m.dst.DomainLookupByUUID(m.dom.UUID); derr == nil {
			if active, _ := m.dst.DomainIsActive(d); active != 0 {
				log.Printf("migrate %s: error %v but the VM is active on %s", p.Name, err, m.dst.Name)
				return fmt.Errorf("libvirt reported an error (%v) but the VM is running on %s; nothing was cleaned up, check both nodes", err, m.dst.Name)
			}
		}
		var notes []string
		if len(removed) > 0 {
			if rerr := redefineSnapshots(m.src, m.dom, removed); rerr != nil {
				notes = append(notes, "restoring snapshot metadata on the source failed: "+rerr.Error())
				for _, sn := range removed {
					log.Printf("migrate %s: snapshot %s XML (restore manually with virsh snapshot-create --redefine):\n%s", p.Name, sn.Name, sn.XML)
				}
			}
		}
		notes = append(notes, m.rollbackDst())
		return withNote(err, notes...)
	}

	moved, err := m.dst.DomainLookupByUUID(m.dom.UUID)
	if err != nil {
		return fmt.Errorf("migration finished but the VM was not found on %s: %w", m.dst.Name, err)
	}
	if autostart != 0 {
		if err := m.dst.DomainSetAutostart(moved, 1); err != nil {
			log.Printf("migrate %s: could not restore autostart on %s: %v", p.Name, m.dst.Name, err)
		}
	}
	if len(removed) > 0 {
		if err := redefineSnapshots(m.dst, moved, removed); err != nil {
			for _, sn := range removed {
				log.Printf("migrate %s: snapshot %s XML:\n%s", p.Name, sn.Name, sn.XML)
			}
			return fmt.Errorf("the VM moved to %s but its snapshots could not be recreated there: %w; the snapshot files were kept on the source", m.dst.Name, err)
		}
	}
	m.progress(0.97)
	return m.cleanSource()
}

// perform runs the peer-to-peer migration call and, while it is in flight,
// turns libvirt's job statistics into task progress.
func (m *migRun) perform(flags libvirt.DomainMigrateFlags, lo, hi float64) error {
	if err := m.checkCancel(); err != nil {
		return err
	}
	var params []libvirt.TypedParam
	if m.req.BandwidthMiB > 0 {
		params = append(params, libvirt.TypedParam{Field: libvirt.MigrateParamBandwidth, Value: *libvirt.NewTypedParamValueUllong(m.req.BandwidthMiB)})
	}
	if m.plan.TLS {
		params = append(params, libvirt.TypedParam{Field: libvirt.MigrateParamTLSDestination, Value: *libvirt.NewTypedParamValueString(m.plan.TLSName)})
	}
	m.inJob.Store(true)
	defer m.inJob.Store(false)
	done := make(chan error, 1)
	go func() {
		_, err := m.src.DomainMigratePerform3Params(m.dom, libvirt.OptString{m.uri}, params, nil, flags)
		done <- err
	}()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	downtimeSet, aborted := m.req.MaxDowntimeMs == 0, false
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			if m.cancelled.Load() && !aborted {
				aborted = m.src.DomainAbortJob(m.dom) == nil
			}
			jt, ps, err := m.src.DomainGetJobStats(m.dom, 0)
			if err != nil || jt == 0 {
				continue
			}
			if !downtimeSet {
				if err := m.src.DomainMigrateSetMaxDowntime(m.dom, m.req.MaxDowntimeMs, 0); err != nil {
					log.Printf("migrate %s: setting max downtime: %v", m.plan.Name, err)
				}
				downtimeSet = true
			}
			st := map[string]float64{}
			for _, tp := range ps {
				st[tp.Field] = toFloat(tp.Value.I)
			}
			if total := st["data_total"]; total > 0 {
				f := st["data_processed"] / total
				if f > 0.99 {
					f = 0.99
				}
				m.spread(lo, hi, f)
			}
		}
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case uint64:
		return float64(n)
	case int64:
		return float64(n)
	case uint32:
		return float64(n)
	case int32:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

// runOffline moves a shut-off VM: copy every file, define it on the target,
// recreate snapshots there, and only then take it off the source.
func (m *migRun) runOffline() error {
	p := m.plan
	autostart, _ := m.src.DomainGetAutostart(m.dom)
	if err := m.copyFiles(0.02, 0.80); err != nil {
		return withNote(err, m.rollbackDst())
	}
	if err := m.checkCancel(); err != nil {
		return withNote(err, m.rollbackDst())
	}
	moved, err := m.dst.DomainDefineXMLFlags(p.PersistXML, 0)
	if err != nil {
		return withNote(fmt.Errorf("defining the VM on %s: %w", m.dst.Name, err), m.rollbackDst())
	}
	undoDst := func(err error) error {
		uf := libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineNvram | libvirt.DomainUndefineManagedSave
		var notes []string
		if uerr := m.dst.DomainUndefineFlags(moved, uf); uerr != nil {
			notes = append(notes, "could not undefine the VM on "+m.dst.Name+": "+uerr.Error())
		}
		return withNote(err, append(notes, m.rollbackDst())...)
	}
	m.progress(0.85)
	if err := redefineSnapshots(m.dst, moved, p.Snaps); err != nil {
		return undoDst(err)
	}
	uf := libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineCheckpointsMetadata
	if p.NVRAM != nil {
		if m.req.KeepSourceDisks {
			uf |= libvirt.DomainUndefineKeepNvram
		} else {
			uf |= libvirt.DomainUndefineNvram
		}
	}
	if err := m.src.DomainUndefineFlags(m.dom, uf); err != nil {
		return undoDst(fmt.Errorf("removing the VM from %s: %w", m.src.Name, err))
	}
	if autostart != 0 {
		if err := m.dst.DomainSetAutostart(moved, 1); err != nil {
			log.Printf("migrate %s: could not restore autostart on %s: %v", p.Name, m.dst.Name, err)
		}
	}
	m.progress(0.95)
	return m.cleanSource()
}

// cleanSource deletes the source files that belonged to this VM. Read-only
// media and shared base images are never touched, and neither is a file that
// another domain on the source still refers to.
func (m *migRun) cleanSource() error {
	if m.req.KeepSourceDisks {
		return nil
	}
	// A live migration recreates the UEFI variable store on the target, but
	// undefining the source leaves its file behind.
	if nv := path.Clean(m.plan.Dom.NVRAM); m.plan.active() && strings.HasPrefix(nv, "/var/lib/libvirt/qemu/nvram/") && safePathRE.MatchString(nv) {
		if out, err := m.src.Run("rm -f -- " + shq(nv)); err != nil {
			log.Printf("migrate %s: could not remove %s on %s: %v %s", m.plan.Name, nv, m.src.Name, err, out)
		}
	}
	var others []string
	if doms, _, err := m.src.ConnectListAllDomains(1, 0); err == nil {
		for _, d := range doms {
			if d.Name == m.plan.Name {
				continue
			}
			if x, err := m.src.DomainGetXMLDesc(d, libvirt.DomainXMLInactive); err == nil {
				others = append(others, x)
			}
		}
	}
	var left []string
	refreshed := false
	for _, f := range m.plan.Files.list {
		if !f.Owned {
			continue
		}
		used := false
		for _, x := range others {
			if strings.Contains(x, "'"+f.Path+"'") || strings.Contains(x, `"`+f.Path+`"`) {
				used = true
			}
		}
		if used {
			left = append(left, f.Path+" (in use by another VM)")
			continue
		}
		vol, err := m.src.StorageVolLookupByPath(f.Path)
		if err != nil && !refreshed {
			refreshed = true
			if pools, _, perr := m.src.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive); perr == nil {
				for _, pl := range pools {
					m.src.StoragePoolRefresh(pl, 0)
				}
			}
			vol, err = m.src.StorageVolLookupByPath(f.Path)
		}
		if err != nil {
			left = append(left, f.Path+" (not in a storage pool)")
			continue
		}
		if err := m.src.StorageVolDelete(vol, 0); err != nil {
			left = append(left, f.Path+" ("+err.Error()+")")
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("the VM was moved to %s, but these source files were left in place: %s", m.dst.Name, strings.Join(left, ", "))
	}
	return nil
}
