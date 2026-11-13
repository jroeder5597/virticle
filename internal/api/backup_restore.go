package api

// Restoring a backup as a new VM or as the original, on any node.
//
// The disks are staged in a work directory inside the target pool and only
// moved into place once all of them are there; the domain is defined last.
// A restore as the original replaces an existing VM of that name only when
// asked to, on the same node, with the VM shut off; the old VM is removed after
// the new disks are staged, never before.

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

type restoreReq struct {
	Mode       string `json:"mode"` // new | original
	Name       string `json:"name"` // the new VM's name (mode new)
	TargetNode int64  `json:"targetNode"`
	Pool       string `json:"pool"`
	Replace    bool   `json:"replace"` // original: replace an existing, shut-off VM of the same name on the target node
	Start      bool   `json:"start"`
}

type restorePlan struct {
	s       *Server
	src     *backupRef
	man     *backupManifest
	chain   []chainLink // the backups this one rests on, the full one first; this backup is the last
	domXML  string
	req     restoreReq
	dst     *hostconn.Host
	dstID   int64
	remote  bool
	pd      poolDir
	pool    libvirt.StoragePool
	name    string
	vols    map[string]string // disk target -> volume file name
	oldPath map[string]string // disk target -> path in the backed-up definition
	media   []string          // CD-ROM / floppy images to eject
	nvram   string            // where the UEFI variables go
	old     *libvirt.Domain   // VM replaced by this restore
	ep      migEndpoint       // how the source reaches the target node
	warn    []string
}

var reNvramPath = regexp.MustCompile(`<nvram\b[^>]*>([^<]*)</nvram>`)

func (s *Server) planRestore(src *backupRef, req restoreReq) (*restorePlan, error) {
	man, err := src.load()
	if err != nil {
		return nil, err
	}
	p := &restorePlan{s: s, src: src, man: man, req: req, vols: map[string]string{}, oldPath: map[string]string{}}
	if p.chain, err = src.loadChain(man); err != nil {
		return nil, err
	}
	raw, err := readLimited(src.h, src.dir+"/domain.xml", backupMaxXML)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	p.domXML = string(raw)
	md, err := parseMigDomain(p.domXML)
	if err != nil {
		return nil, badRequest("domain.xml is not a valid domain definition: " + err.Error())
	}
	for _, d := range md.Disks {
		if d.Path != "" {
			p.oldPath[d.Target] = d.Path
			if d.Device == "cdrom" || d.Device == "floppy" {
				p.media = append(p.media, d.Path)
			}
		}
	}
	for _, d := range man.Disks {
		if p.oldPath[d.Target] == "" {
			return nil, badRequest("domain.xml has no disk " + d.Target + " that the manifest lists")
		}
		for _, l := range p.chain {
			ld, ok := l.diskOf(d.Target)
			if !ok {
				return nil, badRequest("the backup " + l.Dir + " that this one builds on has no disk " + d.Target)
			}
			info, err := bkQemuInfo(src.h, l.path(src)+"/"+ld.File) // refuses backing files and the like
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ld.File, err)
			}
			if info.Format != "qcow2" {
				return nil, badRequest(ld.File + " is not a qcow2 image")
			}
		}
	}

	// Target.
	p.dstID = req.TargetNode
	if p.dstID == 0 {
		p.dstID = src.id
	}
	if _, ok := s.nodeStatus(p.dstID); !ok {
		return nil, badRequest("no such target node")
	}
	if p.dst, err = s.Nodes.Host(p.dstID); err != nil {
		return nil, err
	}
	p.remote = p.dstID != src.id
	if p.pool, p.pd, err = destPool(p.dst, req.Pool); err != nil {
		return nil, err
	}
	if !safePathRE.MatchString(p.pd.Dir) {
		return nil, badRequest("unsupported characters in the pool directory " + p.pd.Dir)
	}

	// Identity.
	switch req.Mode {
	case "new":
		if !vmNameRE.MatchString(req.Name) {
			return nil, badRequest("name must start with a letter or digit and contain only letters, digits, dot, dash and underscore")
		}
		p.name = req.Name
		if _, err := p.dst.DomainLookupByName(p.name); err == nil {
			return nil, badRequest("a VM named " + p.name + " already exists on " + p.dst.Name)
		}
	case "original":
		p.name = man.VM
		if err := p.checkOriginal(); err != nil {
			return nil, err
		}
	default:
		return nil, badRequest(`mode must be "new" or "original"`)
	}

	// Volumes.
	var need uint64 = importFreeMargin
	for i, d := range man.Disks {
		v := p.name + ".qcow2"
		if i > 0 {
			v = p.name + "-" + d.Target + ".qcow2"
		}
		if !volNameRE.MatchString(v) {
			return nil, badRequest("volume name " + v + " is not valid")
		}
		p.vols[d.Target] = v
		need += p.chainBytes(d.Target)
		if p.old != nil {
			continue // the replaced VM's files go away first; checked against its disks below
		}
		if _, err := p.dst.StorageVolLookupByName(p.pool, v); err == nil {
			return nil, badRequest("a volume named " + v + " already exists in " + req.Pool + " on " + p.dst.Name)
		}
		if ex, err := p.dst.Exists(p.pd.Dir + "/" + v); err != nil || ex[p.pd.Dir+"/"+v] {
			return nil, badRequest("a file named " + v + " already exists in " + p.pd.Dir + " on " + p.dst.Name)
		}
	}
	if p.old != nil {
		if err := p.checkReplaceFiles(); err != nil {
			return nil, err
		}
	}
	if man.NVRAM != "" {
		if err := p.planNVRAM(md); err != nil {
			return nil, err
		}
	}
	free, err := bkFreeBytes(p.dst, p.pd.Dir)
	if err != nil {
		return nil, err
	}
	if uint64(free) < need {
		return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s: %s available, about %s needed", p.pd.Dir, p.dst.Name, fmtBytes(uint64(free)), fmtBytes(need)))
	}
	if len(p.media) > 0 {
		p.warn = append(p.warn, "install media/CD-ROM images are ejected: "+strings.Join(p.media, ", "))
	}
	if man.TPM {
		p.warn = append(p.warn, "the VM had a TPM; its state is not part of a backup, the restored VM starts with an empty TPM")
	}
	if p.remote {
		dn, _ := s.nodeStatus(p.dstID)
		if p.ep, err = s.checkBackupTransfer(src.h, p.dst, dn.Node); err != nil {
			return nil, err
		}
	}
	if len(p.chain) > 1 {
		// An incremental backup is put together on the node that holds the backups first.
		var flat uint64 = importFreeMargin
		for _, d := range man.Disks {
			flat += p.chainBytes(d.Target)
		}
		if sf, err := bkFreeBytes(src.h, src.pd.Dir); err != nil || uint64(sf) < flat {
			return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s to put the incremental backup together: about %s needed", src.pd.Dir, src.h.Name, fmtBytes(flat)))
		}
	}
	return p, nil
}

// checkOriginal refuses to restore as the original while a domain with the
// same name or UUID exists on any connected node, except for a shut-off
// VM on the target node when replace was asked for.
func (p *restorePlan) checkOriginal() error {
	u, _ := cluster.ParseUUID(p.man.UUID)
	for _, n := range p.s.Nodes.Status() {
		h, err := p.s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		byName, errN := h.DomainLookupByName(p.man.VM)
		byUUID, errU := h.DomainLookupByUUID(u)
		if errN != nil && errU != nil {
			continue
		}
		where := h.Name
		if errN == nil && errU == nil && byName.UUID == byUUID.UUID && n.ID == p.dstID && p.req.Replace {
			st, _, _, _, _, err := h.DomainGetInfo(byName)
			if err != nil {
				return err
			}
			if stateName(st) != "shutoff" {
				return badRequest("the VM " + p.man.VM + " on " + where + " is not shut off; shut it down before replacing it")
			}
			p.old = &byName
			continue
		}
		hint := ""
		switch {
		case n.ID != p.dstID:
			hint = " Delete or migrate it first (the original identity can exist only once), or restore as a new VM."
		case !p.req.Replace:
			hint = " Choose \"replace\" to remove it and restore the backup in its place (it must be shut off), or restore as a new VM."
		default:
			hint = " Name and UUID belong to different VMs there."
		}
		return badRequest("a VM with this name or UUID (" + p.man.VM + ", " + p.man.UUID + ") already exists on " + where + "." + hint)
	}
	return nil
}

// checkReplaceFiles makes sure the new volume names are free except for
// files of the VM that is being replaced.
func (p *restorePlan) checkReplaceFiles() error {
	mine := map[string]bool{}
	if x, err := p.dst.DomainGetXMLDesc(*p.old, libvirt.DomainXMLInactive); err == nil {
		var dx domainXML
		if xml.Unmarshal([]byte(x), &dx) == nil {
			for _, f := range diskPaths(&dx) {
				mine[path.Clean(f)] = true
			}
		}
	}
	for _, v := range p.vols {
		f := p.pd.Dir + "/" + v
		if ex, err := p.dst.Exists(f); (err != nil || ex[f]) && !mine[f] {
			return badRequest("a file " + f + " exists on " + p.dst.Name + " that does not belong to the VM being replaced")
		}
	}
	return nil
}

func (p *restorePlan) planNVRAM(md migDomain) error {
	// The saved file is placed byte for byte, so the definition's format must
	// be the one the manifest recorded (no entry: an old backup, raw).
	if (p.man.NVRAMFormat == "qcow2") != (md.NVRAMFormat == "qcow2") {
		return badRequest("this backup is inconsistent: the manifest and the VM definition disagree about the format of the UEFI variables")
	}
	if p.req.Mode == "original" {
		p.nvram = md.NVRAM
	} else {
		ext := ".fd"
		if md.NVRAMFormat == "qcow2" {
			ext = ".qcow2"
		}
		p.nvram = "/var/lib/libvirt/qemu/nvram/" + p.name + "_VARS" + ext
	}
	if !safePathRE.MatchString(p.nvram) {
		return badRequest("unsupported characters in the NVRAM path " + p.nvram)
	}
	if st, err := p.dst.Stat(path.Dir(p.nvram)); err != nil || !st[0].Exists || st[0].Kind != "dir" {
		return badRequest("the directory " + path.Dir(p.nvram) + " for the UEFI variables does not exist on " + p.dst.Name + " (is OVMF installed?)")
	}
	if p.old == nil {
		if ex, err := p.dst.Exists(p.nvram); err != nil || ex[p.nvram] {
			return badRequest("the UEFI variable file " + p.nvram + " already exists on " + p.dst.Name)
		}
	}
	return nil
}

// definition builds the XML of the restored VM from the backed-up one.
func (p *restorePlan) definition() (string, error) {
	x := stripBackingStore(p.domXML)
	m := map[string]string{}
	for _, d := range p.man.Disks {
		m[p.oldPath[d.Target]] = p.pd.Dir + "/" + p.vols[d.Target]
	}
	for _, f := range p.media {
		m[f] = ""
	}
	if p.req.Mode == "new" {
		out, err := cloneDomainXML(x, cloneEdit{Name: p.name, Disks: m})
		if err != nil {
			return "", err
		}
		if p.nvram != "" {
			// A store that names itself as its template (see withQcow2Nvram) keeps doing so at its new path.
			if n, ok := parseNvramEl(x); ok && n.Template != "" && n.Template == n.Path {
				out = withNvramTemplate(out, p.nvram)
			}
			out = reNvramEl.ReplaceAllString(out, "${1}"+p.nvram+"${2}")
		}
		return out, nil
	}
	return remapDisks(x, m), nil
}

// remapDisks changes the file behind disks in a definition, or empties the
// device when the new path is "".
func remapDisks(x string, m map[string]string) string {
	return reDiskEl.ReplaceAllStringFunc(x, func(d string) string {
		sm := reFileSrc.FindStringSubmatch(d)
		if sm == nil {
			return d
		}
		np, ok := m[xmlUnescape.Replace(sm[2])]
		switch {
		case !ok:
			return d
		case np == "":
			return strings.Replace(d, sm[0], "", 1)
		}
		return strings.Replace(d, sm[0], strings.Replace(sm[0], sm[1]+sm[2]+sm[3], "'"+esc(np)+"'", 1), 1)
	})
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	src, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req restoreReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	p, err := s.planRestore(src, req)
	if err != nil {
		fail(w, err)
		return
	}
	// Building the definition now catches problems before anything is copied.
	if _, err := p.definition(); err != nil {
		fail(w, badRequest("cannot prepare the VM definition: "+err.Error()))
		return
	}
	id, err := s.startBackground(r, "restore", p.name+" ← "+src.name, p.dst.Name, func(progress func(float64)) error {
		_, err := p.run(progress)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"task": id, "name": p.name, "warnings": append([]string{}, p.warn...)})
}

// run performs the restore and returns the UUID of the defined VM.
func (p *restorePlan) run(progress func(float64)) (uuid string, err error) {
	dst, src := p.dst, p.src.h
	progress(0.01)
	wd, err := newWork(dst, p.pd.Dir, "rs")
	if err != nil {
		return "", err
	}
	var moved []string
	nvramPlaced := false
	var defined *libvirt.Domain
	oldGone, kept := false, false
	defer func() {
		if err == nil || kept {
			wd.remove()
			return
		}
		var left []string
		if defined != nil {
			f := libvirt.DomainUndefineNvram | libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata
			if uerr := dst.DomainUndefineFlags(*defined, f); uerr != nil {
				left = append(left, "VM definition "+p.name+": "+uerr.Error())
			}
		}
		for _, f := range moved {
			if rerr := dst.Remove(f); rerr != nil {
				left = append(left, f+" ("+rerr.Error()+")")
			}
		}
		if nvramPlaced {
			if rerr := dst.Remove(p.nvram); rerr != nil {
				left = append(left, p.nvram+" ("+rerr.Error()+")")
			}
		}
		if rerr := wd.remove(); rerr != nil {
			left = append(left, wd.path)
		}
		poolsByPath(dst)
		if oldGone {
			left = append(left, "NOTE: the VM that was to be replaced had already been removed; restore the backup again")
		}
		note := "rolled back"
		if len(left) > 0 {
			note += "; left behind: " + strings.Join(left, ", ")
		}
		err = withNote(err, note)
	}()

	// Stage the disks.
	var total uint64
	for _, d := range p.man.Disks {
		total += p.chainBytes(d.Target) + 1
	}
	var done uint64
	staged := map[string]string{}
	// A restore of an incremental backup puts the chain together on the node
	// that holds the backups, in a work directory of its own.
	var srcWd *bkWork
	if len(p.chain) > 1 && p.remote {
		if srcWd, err = newWork(src, p.src.pd.Dir, "rs"); err != nil {
			return "", err
		}
		defer srcWd.remove()
	}
	for _, d := range p.man.Disks {
		stage := wd.file(p.vols[d.Target])
		var files []string
		for _, l := range p.chain {
			ld, _ := l.diskOf(d.Target)
			f := l.path(p.src) + "/" + ld.File
			if ld.SHA256 != "" {
				sum, serr := src.SHA256(f)
				if serr != nil {
					return "", serr
				}
				if sum != ld.SHA256 {
					return "", fmt.Errorf("%s of the backup %s is damaged: its SHA-256 is %s, the manifest says %s", ld.File, l.Dir, sum, ld.SHA256)
				}
			}
			files = append(files, f)
		}
		base, span := done, p.chainBytes(d.Target)+1
		prog := func(f float64) { progress(0.02 + 0.8*(float64(base)+f*float64(span))/float64(total)) }
		switch {
		case p.remote && len(p.chain) == 1:
			if cerr := copyToNode(context.Background(), src, dst, p.ep, files[0], stage, "600", func(sent uint64) {
				progress(0.02 + 0.8*(float64(base)+float64(sent))/float64(total))
			}); cerr != nil {
				return "", fmt.Errorf("copying %s to %s failed: %w", d.File, dst.Name, cerr)
			}
		case p.remote:
			flat := srcWd.file(p.vols[d.Target])
			if cerr := srcWd.convertChain(files, flat, func(f float64) { prog(f * 0.5) }); cerr != nil {
				return "", fmt.Errorf("putting %s together: %w", d.File, cerr)
			}
			if cerr := copyToNode(context.Background(), src, dst, p.ep, flat, stage, "600", func(sent uint64) {
				prog(0.5 + 0.5*float64(sent)/float64(span))
			}); cerr != nil {
				return "", fmt.Errorf("copying %s to %s failed: %w", d.File, dst.Name, cerr)
			}
			_ = src.Remove(flat)
		default:
			if cerr := wd.convertChain(files, stage, prog); cerr != nil {
				return "", fmt.Errorf("copying %s: %w", d.File, cerr)
			}
		}
		if info, ierr := bkQemuInfo(dst, stage); ierr != nil || info.Format != "qcow2" {
			return "", fmt.Errorf("the copy of %s is not a valid qcow2 image on %s", d.File, dst.Name)
		}
		done += p.chainBytes(d.Target) + 1
		staged[d.Target] = stage
		progress(0.02 + 0.8*float64(done)/float64(total))
	}
	nvramStage := ""
	if p.man.NVRAM != "" {
		nvramStage = wd.file(backupNVRAMFile)
		srcFile := p.src.dir + "/" + p.man.NVRAM
		var cerr error
		if p.remote {
			cerr = copyToNode(context.Background(), src, dst, p.ep, srcFile, nvramStage, "600", nil)
		} else {
			cerr = dst.Copy(srcFile, nvramStage)
		}
		if cerr != nil {
			return "", fmt.Errorf("copying the UEFI variables: %s", impFirstLine(cerr.Error()))
		}
	}
	xmlText, err := p.definition()
	if err != nil {
		return "", err
	}

	// Replace: only now, with everything staged, remove the old VM.
	if p.old != nil {
		oldGone = true
		if err := p.removeOld(); err != nil {
			return "", err
		}
	}
	progress(0.88)

	for _, d := range p.man.Disks {
		f := p.pd.Dir + "/" + p.vols[d.Target]
		moved = append(moved, f)
		if merr := dst.Move(staged[d.Target], f); merr != nil {
			return "", fmt.Errorf("moving %s into the pool: %s", f, impFirstLine(merr.Error()))
		}
	}
	if nvramStage != "" {
		nvramPlaced = true
		if cerr := dst.Copy(nvramStage, p.nvram); cerr != nil {
			return "", fmt.Errorf("placing the UEFI variables at %s: %s", p.nvram, impFirstLine(cerr.Error()))
		}
	}
	dst.StoragePoolRefresh(p.pool, 0)
	dom, derr := dst.DomainDefineXMLFlags(xmlText, libvirt.DomainDefineValidate)
	if derr != nil {
		return "", fmt.Errorf("defining the VM on %s: %w", dst.Name, derr)
	}
	defined = &dom
	uuid = cluster.FormatUUID(dom.UUID)
	progress(0.97)
	if p.req.Start {
		if serr := dst.DomainCreate(dom); serr != nil {
			// The restore itself worked; keep it.
			kept = true
			return uuid, fmt.Errorf("the VM was restored but did not start: %w", serr)
		}
	}
	return uuid, nil
}

// removeOld undefines the VM this restore replaces and deletes its disks.
func (p *restorePlan) removeOld() error {
	dst := p.dst
	x, err := dst.DomainGetXMLDesc(*p.old, libvirt.DomainXMLInactive)
	if err != nil {
		return err
	}
	var dx domainXML
	if err := xml.Unmarshal([]byte(x), &dx); err != nil {
		return err
	}
	f := libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineNvram |
		libvirt.DomainUndefineCheckpointsMetadata | libvirt.DomainUndefineTpm
	if err := dst.DomainUndefineFlags(*p.old, f); err != nil {
		return fmt.Errorf("removing the existing VM: %w", err)
	}
	var left []string
	for _, f := range diskPaths(&dx) {
		vol, err := lookupVolByPath(dst, f)
		if err == nil {
			err = dst.StorageVolDelete(vol, 0)
		}
		if err != nil {
			// Not a pool volume (or already gone): remove the plain file if it is one of ours.
			if rerr := dst.Remove(f); rerr != nil {
				left = append(left, f+" ("+rerr.Error()+")")
			}
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("the existing VM was removed but its disks could not be deleted: %s", strings.Join(left, ", "))
	}
	return nil
}
