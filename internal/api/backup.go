package api

// Backing up a VM to a directory-backed pool on this or another node.
//
//   - A running VM is backed up with libvirt's backup API (push mode): QEMU
//     copies the disks while the guest keeps running. Each backup leaves a
//     libvirt checkpoint (a QEMU dirty bitmap per disk), and the next backup
//     copies only what changed since it, an incremental backup. Whenever that
//     cannot be done safely the backup is a full one (backup_chain.go decides).
//     The job is polled for progress and can be aborted; an aborted or failed
//     backup leaves no checkpoint behind.
//   - A shut-off VM is backed up with qemu-img convert, which also flattens
//     backing chains. It is always a full backup and makes no checkpoint.
//   - Files are first written to a hidden work directory inside a pool on the
//     node that has the disks; for another node they are copied with scp (as
//     migration does) into a work directory in the target pool. The manifest
//     is written last and the directory renamed into place, so a backup either
//     exists completely or not at all, and cancelling removes everything.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

type backupReq struct {
	TargetNode int64  `json:"targetNode"` // 0 = the node the VM is on
	Pool       string `json:"pool"`
	KeepLast   int    `json:"keepLast"` // 0 = keep everything
	// SHA256 records checksums; absent means yes.
	SHA256 *bool `json:"sha256"`
	// Quiesce freezes guest file systems through the guest agent (running VM).
	// Absent means automatic: freeze when the agent is connected and fall back
	// to no freeze if that fails. true insists, false never freezes.
	Quiesce *bool `json:"quiesce"`
}

// bkDisk is a writable disk that goes into the backup.
type bkDisk struct {
	migDisk
	File   string // file name inside the backup
	Actual uint64 // bytes allocated by its whole chain
	Size   uint64 // bytes of the backup file, once written
	Virt   uint64
	SHA    string
	// Base is the image at the bottom of the disk's backing chain: the file that stays
	// when snapshots come and go. Whether a backup can build on an earlier one is judged
	// by it, not by the top image (see diskLayout).
	Base string
}

type bkRun struct {
	s        *Server
	src, dst *hostconn.Host
	dom      libvirt.Domain
	req      backupReq
	name     string // VM name
	uuid     string
	live     bool
	domXML   string // inactive definition
	md       migDomain
	disks    []bkDisk
	skip     []string // targets of disks left out of the job
	srcDir   string   // pool directory the disks' work happens in on src
	dstDir   string   // target pool directory
	dstPool  string
	ep       migEndpoint // how the source reaches the target node
	remote   bool
	plan     backupPlan    // full or incremental, and on what (see decideBackup)
	target   string        // targetKey of the destination
	ckName   string        // the checkpoint this run created ("" = none yet)
	fz       freezeOutcome // what the last backup-begin did to the guest's file systems
	thawErr  error         // set when a thaw failed on every try: the guest may be frozen
	changed  uint64        // bytes libvirt expects an incremental backup to copy
	progress func(float64)
	ctx      context.Context // cancelled by cancel(); ends a running node to node transfer
	stopCtx  context.CancelFunc

	mu        sync.Mutex
	srcWork   *bkWork
	cancelled atomic.Bool
	inJob     atomic.Bool
}

var bkRuns = struct {
	sync.Mutex
	m map[string]*bkRun
}{m: map[string]*bkRun{}}

var errBackupCancelled = errors.New("backup cancelled")

// chainActual is the number of bytes a `qemu-img info --backing-chain` JSON
// listing says the images occupy.
func chainActual(out string) uint64 {
	var raw []struct {
		Actual uint64 `json:"actual-size"`
	}
	var sum uint64
	if json.Unmarshal([]byte(out), &raw) == nil {
		for _, r := range raw {
			sum += r.Actual
		}
	}
	return sum
}

// planBackup inspects the VM and the target and builds the run. Everything
// that can be refused is refused here, before a task exists.
func (s *Server) planBackup(srcID int64, src *hostconn.Host, dom libvirt.Domain, req backupReq) (*bkRun, error) {
	sweepWork() // before the free space is checked
	if pers, err := src.DomainIsPersistent(dom); err != nil || pers == 0 {
		return nil, badRequest("this VM is transient (only running, not defined); define it first")
	}
	state, _, err := src.DomainGetState(dom, 0)
	if err != nil {
		return nil, err
	}
	ctx, stopCtx := context.WithCancel(context.Background())
	m := &bkRun{ctx: ctx, stopCtx: stopCtx, s: s, src: src, dom: dom, req: req, name: dom.Name, uuid: cluster.FormatUUID(dom.UUID), live: stateName(uint8(state)) != "shutoff"}
	if m.domXML, err = src.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive); err != nil {
		return nil, err
	}
	if len(m.domXML) > backupMaxXML {
		return nil, badRequest("the domain definition is unreasonably large")
	}
	if m.md, err = parseMigDomain(m.domXML); err != nil {
		return nil, badRequest("cannot read the domain definition: " + err.Error())
	}
	if err := missingTools(src, "qemu-img", "tar"); err != nil {
		return nil, err
	}

	disks, skip, allQcow2, need, err := inspectBackupDisks(src, m.md, "backed up")
	if err != nil {
		return nil, err
	}
	m.disks, m.skip = disks, skip
	need += importFreeMargin

	dstID := req.TargetNode
	if dstID == 0 {
		dstID = srcID
	}
	if _, ok := s.nodeStatus(dstID); !ok {
		return nil, badRequest("no such target node")
	}
	if m.dst, err = s.Nodes.Host(dstID); err != nil {
		return nil, err
	}
	m.remote = dstID != srcID
	_, pd, err := destPool(m.dst, req.Pool)
	if err != nil {
		return nil, err
	}
	m.dstDir, m.dstPool = pd.Dir, req.Pool
	if !safePathRE.MatchString(m.dstDir) {
		return nil, badRequest("unsupported characters in the pool directory " + m.dstDir)
	}
	free, err := bkFreeBytes(m.dst, pd.Dir)
	if err != nil {
		return nil, err
	}
	m.target = targetKey(m.dst.Name, req.Pool)
	m.plan = s.planIncremental(m, pd, allQcow2)
	if m.plan.Kind == kindIncremental {
		// Only the changed blocks are copied; libvirt knows how many.
		if n, ok := changedBytes(src, dom, m.plan.ParentCheckpoint); ok {
			m.changed = n
			need = n + n/8 + importFreeMargin
		}
	}
	if uint64(free) < need {
		return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s: %s available, about %s needed", pd.Dir, m.dst.Name, fmtBytes(uint64(free)), fmtBytes(need)))
	}

	m.srcDir = m.dstDir
	if m.remote {
		// The files are staged next to the VM's disks, then copied.
		dirs, err := poolDirs(src)
		if err != nil || len(dirs) == 0 {
			return nil, badRequest("the source node has no running directory-backed pool to stage the backup in")
		}
		m.srcDir = dirs[0].Dir
		for _, d := range dirs {
			if d.Dir == path.Dir(m.disks[0].Path) {
				m.srcDir = d.Dir
			}
		}
		if !safePathRE.MatchString(m.srcDir) {
			return nil, badRequest("unsupported characters in the pool directory " + m.srcDir)
		}
		if sf, err := bkFreeBytes(src, m.srcDir); err != nil || uint64(sf) < need {
			return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s to stage the backup", m.srcDir, src.Name))
		}
		dn, _ := s.nodeStatus(dstID)
		if m.ep, err = s.checkBackupTransfer(src, m.dst, dn.Node); err != nil {
			return nil, err
		}
	}
	bkRuns.Lock()
	idle := len(bkRuns.m) == 0
	bkRuns.Unlock()
	if idle { // no backup of this controller can own a work directory right now
		sweepStaleWork(m.dst, m.dstDir)
		if m.remote {
			sweepStaleWork(m.src, m.srcDir)
		}
	}
	return m, nil
}

// staleWorkMinutes is how long a backup work directory must be untouched before it counts as left behind by a
// controller that stopped (or lost its connection) while the backup ran. A running backup keeps writing.
const staleWorkMinutes = 60

// sweepStaleWork removes the work directories that backups left in dir on h when the controller stopped in the
// middle of them. The failed backup's own message names them, but only until the controller restarts; this is
// what removes them afterwards. Best effort: a node command without the verb, or a directory that will not go,
// only costs a log line.
func sweepStaleWork(h *hostconn.Host, dir string) {
	stale, err := h.StaleWork(dir, staleWorkMinutes)
	if err != nil {
		return
	}
	for _, d := range stale {
		if !strings.HasPrefix(d, strings.TrimSuffix(dir, "/")+"/.virticle-bk.") {
			continue
		}
		wd := &bkWork{h: h, path: d}
		if err := wd.remove(); err != nil {
			log.Printf("backup: cannot remove the leftover work directory %s on %s: %v", d, h.Name, err)
			continue
		}
		log.Printf("backup: removed the leftover work directory %s on %s (an interrupted backup)", d, h.Name)
	}
}

// inspectBackupDisks looks at the disks of a definition that a backup or a replication copies: the writable
// file-based ones. skip are the targets left out (read-only, CD-ROMs), allQcow2 says every copied disk has a
// qcow2 image on top, need the bytes they occupy.
func inspectBackupDisks(src *hostconn.Host, md migDomain, verb string) (disks []bkDisk, skip []string, allQcow2 bool, need uint64, err error) {
	allQcow2 = true
	for _, d := range md.Disks {
		if d.Device != "disk" || d.ReadOnly {
			skip = append(skip, d.Target)
			continue
		}
		switch {
		case d.Type != "file" || d.Path == "":
			return nil, nil, false, 0, badRequest("disk " + d.Target + " is a " + d.Type + " disk; only file-based disks can be " + verb)
		case d.Shareable:
			return nil, nil, false, 0, badRequest("disk " + d.Target + " is shareable; a shared disk is not " + verb + " with one VM")
		}
		out, err := src.QemuInfo(d.Path, true)
		if err != nil {
			return nil, nil, false, 0, badRequest("qemu-img could not inspect " + d.Path + ": " + impFirstLine(out))
		}
		chain, err := parseChain(out)
		if err != nil {
			return nil, nil, false, 0, err
		}
		bd := bkDisk{migDisk: d, File: d.Target + ".qcow2", Actual: chainActual(out), Virt: chain[0].VirtualSize, Base: chain[len(chain)-1].Path}
		if chain[0].Format != "qcow2" {
			allQcow2 = false
		}
		need += bd.Actual
		disks = append(disks, bd)
	}
	if len(disks) == 0 {
		return nil, nil, false, 0, badRequest("the VM has no writable disk to copy")
	}
	return disks, skip, allQcow2, need, nil
}

// defaultBackupReq is what a request body that leaves fields out means:
// checksums and an automatic freeze (nil), and keep the newest 7 (0 keeps everything).
func defaultBackupReq() backupReq { return backupReq{KeepLast: 7} }

func (s *Server) vmBackup(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	req := defaultBackupReq()
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.KeepLast < 0 || req.KeepLast > 1000 {
		fail(w, badRequest("keep-last must be between 0 and 1000"))
		return
	}
	srcID, _, _ := s.host(r)
	m, err := s.planBackup(srcID, h, dom, req)
	if err != nil {
		fail(w, err)
		return
	}
	id, _, err := s.launchBackup(r, h, m)
	if err != nil {
		fail(w, err)
		return
	}
	mode := "offline"
	if m.live {
		mode = "live"
	}
	writeJSON(w, backupStartAnswer(id, mode, m.plan))
}

// backupStartAnswer is the answer to starting a backup. plannedKind is what the run will try: libvirt may still
// refuse an incremental backup and the run then makes a full one, so the field is not called "kind". What was made
// is the kind in the backup's manifest (GET /api/backups).
func backupStartAnswer(task int64, mode string, plan backupPlan) map[string]any {
	return map[string]any{"task": task, "mode": mode, "plannedKind": plan.Kind, "plannedReason": plan.Reason}
}

// backupTarget names a backup task: the VM and where it goes.
func backupTarget(vm, node, pool string) string { return vm + " → " + node + ":" + pool }

// launchBackup registers the planned run (one per VM) and starts it as a
// background task; the channel receives its result.
func (s *Server) launchBackup(r *http.Request, h *hostconn.Host, m *bkRun) (int64, <-chan error, error) {
	bkRuns.Lock()
	if _, busy := bkRuns.m[m.uuid]; busy {
		bkRuns.Unlock()
		return 0, nil, badRequest("a backup of this VM is already in progress")
	}
	if _, busy := replRuns[m.uuid]; busy {
		bkRuns.Unlock()
		return 0, nil, badRequest("this VM is being copied to its standby node right now; try the backup again in a minute")
	}
	bkRuns.m[m.uuid] = m
	bkRuns.Unlock()
	id, done, err := s.startBackgroundWait(r, "backup", backupTarget(m.name, m.dst.Name, m.dstPool), h.Name, func(progress func(float64)) error {
		defer func() {
			bkRuns.Lock()
			delete(bkRuns.m, m.uuid)
			bkRuns.Unlock()
		}()
		m.progress = progress
		return m.finishFreeze(m.run())
	})
	if err != nil {
		bkRuns.Lock()
		delete(bkRuns.m, m.uuid)
		bkRuns.Unlock()
	}
	return id, done, err
}

func (s *Server) vmBackupCancel(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if _, dom, err := s.domain(r); err == nil {
		uuid = cluster.FormatUUID(dom.UUID)
	}
	bkRuns.Lock()
	m := bkRuns.m[uuid]
	bkRuns.Unlock()
	if m == nil {
		fail(w, badRequest("no backup of this VM is in progress"))
		return
	}
	m.cancel()
	writeJSON(w, map[string]bool{"ok": true})
}

func (m *bkRun) cancel() {
	m.cancelled.Store(true)
	m.stopCtx()
	if m.inJob.Load() {
		if err := m.src.DomainAbortJob(m.dom); err != nil {
			log.Printf("backup %s: abort job: %v", m.name, err) // the poll loop retries
		}
	}
	m.mu.Lock()
	wd := m.srcWork
	m.mu.Unlock()
	if wd != nil && !m.live {
		wd.remove() // kills a running qemu-img convert, then deletes its files
	}
}

func (m *bkRun) checkCancel() error {
	if m.cancelled.Load() {
		return errBackupCancelled
	}
	return nil
}

func (m *bkRun) spread(lo, hi, f float64) {
	if f > 1 {
		f = 1
	}
	m.progress(lo + (hi-lo)*f)
}

// leftWork holds the work directories a failed backup could not remove. They are
// removed by the next backup (in memory only: after a controller restart the
// message of the failed backup, which names them, is all that is left).
var leftWork = struct {
	sync.Mutex
	l []*bkWork
}{}

func keepWork(wd *bkWork) {
	leftWork.Lock()
	leftWork.l = append(leftWork.l, wd)
	leftWork.Unlock()
}

// sweepWork retries the removal of work directories left by failed backups.
func sweepWork() {
	leftWork.Lock()
	l := leftWork.l
	leftWork.l = nil
	leftWork.Unlock()
	for _, wd := range l {
		if err := wd.remove(); err != nil {
			keepWork(wd)
		}
	}
}

func (m *bkRun) run() (err error) {
	defer m.stopCtx()
	m.progress(0.01)
	stamp := time.Now().UTC()
	finalName := backupDirName(m.name, stamp)
	final := m.dstDir + "/" + finalName
	if ex, err := m.dst.Exists(final); err != nil || ex[final] {
		return fmt.Errorf("%s already exists or cannot be checked: %v", final, err)
	}

	srcWork, err := newWork(m.src, m.srcDir, "bk")
	if err != nil {
		return err
	}
	srcWork.stop = m.cancelled.Load
	m.mu.Lock()
	m.srcWork = srcWork
	m.mu.Unlock()
	var dstWork *bkWork
	defer func() {
		if err != nil {
			m.removeNewCheckpoint()
			var left []string
			for _, wd := range []*bkWork{srcWork, dstWork} {
				if wd != nil {
					if rerr := wd.remove(); rerr != nil {
						// A backup job that outlived the connection (libvirt was restarted) may
						// still write there: the next backup removes it (sweepWork).
						keepWork(wd)
						left = append(left, wd.path+" on "+wd.h.Name+" ("+rerr.Error()+")")
					}
				}
			}
			if m.cancelled.Load() && !strings.Contains(err.Error(), "cancelled") {
				err = fmt.Errorf("backup cancelled: %w", err)
			}
			if len(left) > 0 {
				err = withNote(err, "work files left behind: "+strings.Join(left, ", "))
			}
		}
	}()

	copyHi := 0.97
	if m.remote {
		copyHi = 0.75
	}
	if m.live {
		// QEMU's user must be able to enter the directory it writes into.
		if err := m.src.ChmodWork(srcWork.path, "711"); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
		if err = m.liveBackup(srcWork, stamp, 0.02, copyHi); err != nil {
			return err
		}
		if m.plan.Kind == kindIncremental {
			// QEMU leaves the file resting on the VM's own disk; cut that tie, so that a
			// backup never refers to anything outside the backups.
			for _, d := range m.disks {
				if err = m.src.DetachBacking(srcWork.path, d.File); err != nil {
					return err
				}
			}
		}
	} else if err = m.offlineBackup(srcWork, 0.02, copyHi); err != nil {
		return err
	}
	if err = m.checkCancel(); err != nil {
		return err
	}

	man := &backupManifest{
		Format: backupFormat, VM: m.name, UUID: m.uuid, Created: stamp.Format(time.RFC3339), SourceNode: m.src.Name,
		AppVersion: backupAppVersion, Live: m.live, TPM: m.md.TPM, Notes: []string{noteSnapshots, noteTPM},
		Kind: m.plan.Kind, Checkpoint: m.ckName, FullReason: m.plan.Reason,
	}
	if m.live && m.fz.Text != "" {
		frozen := m.fz.Frozen
		man.Frozen = &frozen
	}
	if m.plan.Kind == kindIncremental {
		man.Format, man.Parent, man.FullReason = backupFormatIncremental, m.plan.Parent, ""
	}
	if m.md.NVRAM != "" {
		if !safePathRE.MatchString(m.md.NVRAM) {
			return fmt.Errorf("unsupported characters in the NVRAM path %s", m.md.NVRAM)
		}
		if err := m.src.Copy(m.md.NVRAM, srcWork.file(backupNVRAMFile)); err != nil {
			return fmt.Errorf("saving the UEFI variables %s: %w", m.md.NVRAM, err)
		}
		man.NVRAM = backupNVRAMFile
		man.NVRAMFormat = m.md.NVRAMFormat
		if man.NVRAMFormat == "qcow2" {
			if err := m.checkQcow2NVRAM(srcWork.file(backupNVRAMFile)); err != nil {
				// A running VM may have been writing while the file was copied: once more.
				_ = m.src.Remove(srcWork.file(backupNVRAMFile))
				if err2 := m.src.Copy(m.md.NVRAM, srcWork.file(backupNVRAMFile)); err2 != nil {
					return fmt.Errorf("saving the UEFI variables %s: %w", m.md.NVRAM, err2)
				}
				if err := m.checkQcow2NVRAM(srcWork.file(backupNVRAMFile)); err != nil {
					return err
				}
			}
		}
	}
	if err = m.measure(srcWork); err != nil {
		return err
	}
	for _, d := range m.disks {
		man.Disks = append(man.Disks, backupDiskRec{Target: d.Target, File: d.File, Format: "qcow2", VirtualSize: d.Virt, Bytes: d.Size, SHA256: d.SHA, Source: d.Base})
	}

	dstWork = srcWork
	if m.remote {
		if dstWork, err = newWork(m.dst, m.dstDir, "bk"); err != nil {
			return err
		}
		if err = m.copyOver(srcWork, dstWork, man, 0.75, 0.97); err != nil {
			return err
		}
		srcWork.remove() // staged copy; the target has the backup now
	}
	if err = m.checkCancel(); err != nil {
		return err
	}
	if err = writeNodeFile(m.dst, dstWork.file("domain.xml"), []byte(m.domXML)); err != nil {
		return fmt.Errorf("writing domain.xml: %w", err)
	}
	mj, _ := json.MarshalIndent(man, "", "  ")
	if err = writeNodeFile(m.dst, dstWork.file("manifest.json"), mj); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}
	// The conversion's bookkeeping files must not end up in the backup (a
	// finished conversion removes them; this catches a failed one).
	m.dst.Remove(dstWork.file("rc"), dstWork.file("qpid"), dstWork.file("prog.log"), dstWork.file("out.part"))
	if err2 := m.dst.ChmodWork(dstWork.path, "700"); err2 != nil {
		err = fmt.Errorf("moving the backup into place: %w", err2)
		return err
	}
	if err2 := m.dst.Move(dstWork.path, final); err2 != nil {
		err = fmt.Errorf("moving the backup into place: %w", err2)
		return err
	}
	m.progress(0.98)
	// From here on the backup exists; the work directories are gone.
	srcWork, dstWork = nil, nil
	m.cleanCheckpoints()
	if m.req.KeepLast > 0 {
		if perr := m.s.pruneBackups(m.dst, m.dstPool, m.uuid, m.req.KeepLast, finalName); perr != nil {
			return fmt.Errorf("backup %s was written, but pruning older backups failed: %w", finalName, perr)
		}
	}
	return nil
}

// liveBackup runs libvirt's push-mode backup and follows the job. It starts
// the plan's backup, and if libvirt will not do an incremental one (bitmaps
// broken, for instance after a QEMU that was killed) it drops the VM's
// checkpoints and makes a full backup instead.
func (m *bkRun) liveBackup(wd *bkWork, stamp time.Time, lo, hi float64) error {
	files := map[string]string{}
	var plain []migDisk
	for _, d := range m.disks {
		files[d.Target] = d.File
		plain = append(plain, d.migDisk)
	}
	agent := false
	if m.req.Quiesce == nil || *m.req.Quiesce {
		agent = guestAgentConnected(m.src, m.dom)
	}
	if err := m.checkCancel(); err != nil {
		return err
	}
	m.inJob.Store(true)
	defer m.inJob.Store(false)
	begin := func() error {
		incr := ""
		if m.plan.Kind == kindIncremental {
			incr = m.plan.ParentCheckpoint
		}
		bxml := backupXML(wd.path, plain, files, m.skip, incr)
		var cxml libvirt.OptString
		name := ""
		if m.plan.Checkpoint {
			var x string
			name, x = m.newCheckpoint(stamp)
			cxml = libvirt.OptString{x}
		}
		// The guest is frozen for this one call only: the backup is the image of the moment it runs.
		out, err := withFrozenGuest(m.req.Quiesce, agent, agentFreezer{m.src, m.dom}, func() error {
			return m.src.DomainBackupBegin(m.dom, bxml, cxml, 0)
		})
		m.fz = out
		if out.ThawErr != nil {
			m.thawErr = out.ThawErr
		}
		if err == nil {
			m.ckName = name
		}
		return err
	}
	if jt, _, jerr := m.src.DomainGetJobStats(m.dom, 0); jerr == nil && jt != 0 {
		// Backup-begin would wait for that job's lock, with the guest frozen meanwhile.
		return errors.New("another job (a backup, snapshot or migration) is still running on this VM; try again when it has finished")
	}
	err := begin()
	var ff *freezeFail
	if errors.As(err, &ff) {
		return err // asked to freeze and could not: nothing was tried, so nothing is refused or dropped
	}
	if err != nil && strings.Contains(err.Error(), "cannot acquire state change lock") {
		return fmt.Errorf("starting the backup job: %w (another operation holds the VM; try again in a minute)", err)
	}
	if err != nil && m.plan.Kind == kindIncremental && strings.Contains(err.Error(), "Bitmap already exists: backup-") {
		// A backup job that was cut short by a libvirt restart left its temporary bitmap behind.
		if n := removeStaleBackupBitmaps(m.src, m.dom); n > 0 {
			log.Printf("backup %s: removed %d leftover temporary bitmap(s) of an interrupted backup; trying again", m.name, n)
			err = begin()
		}
	}
	if err != nil && m.plan.Kind == kindIncremental {
		if cerr := m.checkCancel(); cerr != nil {
			return cerr
		}
		// libvirt would not build on the checkpoint: its bitmap is broken or gone.
		log.Printf("backup %s: incremental backup refused (%v); making a full one", m.name, err)
		dropBackupCheckpoints(m.src, m.dom)
		m.plan = backupPlan{Kind: kindFull, Reason: whyRefused, Checkpoint: true}
		err = begin()
	}
	if err != nil && m.plan.Checkpoint {
		// Tracking changes is an extra; the backup itself must still work.
		log.Printf("backup %s: no checkpoint (%v); making the backup without one", m.name, err)
		m.plan = backupPlan{Kind: kindFull, Reason: whyNoCheckpoint}
		err = begin()
	}
	if err != nil {
		return fmt.Errorf("starting the backup job: %w", err)
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	aborted, misses := false, 0
	for range tick.C {
		if m.cancelled.Load() && !aborted {
			aborted = m.src.DomainAbortJob(m.dom) == nil
		}
		jt, ps, err := m.src.DomainGetJobStats(m.dom, 0)
		if err != nil {
			if misses++; misses >= 20 {
				return fmt.Errorf("lost contact with libvirt while the backup job ran: %w", err)
			}
			continue
		}
		misses = 0
		if jt == 0 {
			// The job is over; the completed-job record tells how it ended.
			ct, cps, err := m.src.DomainGetJobStats(m.dom, libvirt.DomainJobStatsCompleted)
			if err != nil {
				return fmt.Errorf("the backup job ended but libvirt has no record of its result: %w", err)
			}
			switch libvirt.DomainJobType(ct) {
			case libvirt.DomainJobCompleted:
				m.spread(lo, hi, 1) // the last counters read are rarely the final ones
				return nil
			case libvirt.DomainJobCancelled:
				return errBackupCancelled
			}
			for _, tp := range cps {
				if tp.Field == "errmsg" {
					if msg, ok := tp.Value.I.(string); ok {
						return fmt.Errorf("the backup job failed: %s", msg)
					}
				}
			}
			return fmt.Errorf("the backup job failed (job state %d)", ct)
		}
		st := map[string]float64{}
		for _, tp := range ps {
			st[tp.Field] = toFloat(tp.Value.I)
		}
		switch {
		case st["disk_total"] > 0: // what a backup job reports (virsh shows it as "File total")
			m.spread(lo, hi, st["disk_processed"]/st["disk_total"])
		case st["data_total"] > 0:
			m.spread(lo, hi, st["data_processed"]/st["data_total"])
		}
	}
	return nil
}

// offlineBackup flattens every disk into the work directory.
func (m *bkRun) offlineBackup(wd *bkWork, lo, hi float64) error {
	var total, done uint64
	for _, d := range m.disks {
		total += d.Actual + 1
	}
	for _, d := range m.disks {
		if err := m.checkCancel(); err != nil {
			return err
		}
		info, err := qemuTop(m.src, d.Path)
		if err != nil {
			return fmt.Errorf("disk %s: %w", d.Target, err)
		}
		base, span := done, d.Actual+1
		if err := wd.convert(d.Path, info, wd.file(d.File), func(p float64) {
			m.spread(lo, hi, (float64(base)+p*float64(span))/float64(total))
		}); err != nil {
			if m.cancelled.Load() {
				return errBackupCancelled
			}
			return fmt.Errorf("disk %s: %w", d.Target, err)
		}
		done += d.Actual + 1
	}
	if state, _, err := m.src.DomainGetState(m.dom, 0); err != nil || stateName(uint8(state)) != "shutoff" {
		return errors.New("the VM was started while its disks were being copied; the copy may be inconsistent, so the backup was discarded")
	}
	return nil
}

// qemuTop returns the format of the active image of a disk (the backing
// chain is followed by qemu-img itself when converting).
func qemuTop(h *hostconn.Host, file string) (string, error) {
	out, err := h.QemuInfo(file, false)
	if err != nil {
		return "", errors.New(impFirstLine(out))
	}
	var in struct {
		Format string `json:"format"`
	}
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j < i || json.Unmarshal([]byte(out[i:j+1]), &in) != nil || !importFormats[in.Format] {
		return "", fmt.Errorf("unsupported or unreadable image %s", file)
	}
	return in.Format, nil
}

// measure checks the written images and records their sizes (and checksums).
func (m *bkRun) measure(wd *bkWork) error {
	for i := range m.disks {
		d := &m.disks[i]
		f := wd.file(d.File)
		info, err := bkQemuInfo(m.src, f)
		if err != nil {
			return fmt.Errorf("backup image of %s is not usable: %w", d.Target, err)
		}
		if info.Format != "qcow2" {
			return fmt.Errorf("backup image of %s has format %s", d.Target, info.Format)
		}
		d.Virt = uint64(info.VirtualSize)
		st, err := m.src.Stat(f)
		if err != nil || !st[0].Exists {
			return fmt.Errorf("stat %s: %v", f, err)
		}
		d.Size = st[0].Size
		// A backup that may become a link of a chain is always checksummed: a restore
		// of an incremental backup checks every file it reads, not only the sizes.
		if m.req.SHA256 == nil || *m.req.SHA256 || m.plan.Checkpoint {
			if d.SHA, err = m.src.SHA256(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyOver copies the staged files to the target node, straight from node to
// node, with the node key peer transfer.
func (m *bkRun) copyOver(from, to *bkWork, man *backupManifest, lo, hi float64) error {
	var total, done uint64
	for _, d := range man.Disks {
		total += d.Bytes
	}
	if total == 0 {
		total = 1
	}
	names := []string{}
	if man.NVRAM != "" {
		names = append(names, man.NVRAM)
	}
	for _, d := range man.Disks {
		names = append(names, d.File)
	}
	sizes := map[string]uint64{}
	for _, d := range man.Disks {
		sizes[d.File] = d.Bytes
	}
	for _, n := range names {
		if err := m.checkCancel(); err != nil {
			return err
		}
		base := done
		if err := m.copyFile(from.file(n), to.file(n), func(sent uint64) {
			m.spread(lo, hi, float64(base+sent)/float64(total))
		}); err != nil {
			return fmt.Errorf("copying %s to %s failed: %w", n, m.dst.Name, err)
		}
		done += sizes[n]
		m.spread(lo, hi, float64(done)/float64(total))
	}
	return nil
}

// --- retention ---

// pruneBackups deletes the backups of VM uuid in a pool beyond the newest
// keep, except those a kept backup builds on (an incremental backup is useless
// without its parent). The backup just written (just) is never deleted.
func (s *Server) pruneBackups(h *hostconn.Host, pool, uuid string, keep int, just string) error {
	_, pd, err := destPool(h, pool)
	if err != nil {
		return err
	}
	items, err := scanPool(h, pd)
	if err != nil {
		return err
	}
	var all []retained
	for _, it := range items {
		if it.Err == "" {
			all = append(all, it.retained())
		}
	}
	var errs []string
	// Newest first, so that a failure half way leaves no backup without its parent.
	for _, key := range pruneChains(all, uuid, keep, just) {
		if err := removeBackupDir(h, pd.Dir, key); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// removeBackupDir deletes one backup directory of a pool; only a directory
// that holds a manifest is touched.
func removeBackupDir(h *hostconn.Host, poolDir, name string) error {
	if !backupDirRE.MatchString(name) {
		return badRequest("not a backup name")
	}
	dir := poolDir + "/" + name
	if err := h.RemoveTree(dir); err != nil {
		// Already gone (another request, or the later backups of a chain deleted with their parent): done.
		if ex, xerr := h.Exists(dir); xerr == nil && !ex[dir] {
			return nil
		}
		return fmt.Errorf("cannot delete %s on %s: %w", dir, h.Name, err)
	}
	return nil
}

// --- listing ---

type backupEntry struct {
	Node     int64           `json:"node"`
	NodeName string          `json:"nodeName"`
	Pool     string          `json:"pool"`
	Dir      string          `json:"dir"`
	Path     string          `json:"path"`
	Size     uint64          `json:"size"`
	Manifest *backupManifest `json:"manifest,omitempty"`
	Err      string          `json:"error,omitempty"`
	VMExists bool            `json:"vmExists"`
	// Where the backed-up VM lives now and whether it is shut off, for
	// "replace the VM" (which needs it on the target node and shut off).
	VMNode     int64  `json:"vmNode,omitempty"`
	VMNodeName string `json:"vmNodeName,omitempty"`
	VMShutOff  bool   `json:"vmShutOff,omitempty"`
	ExportName string `json:"exportName,omitempty"`
	// share identifies the NFS share the backup is on (empty for local storage); every node that mounts the
	// share lists the same backup, which is shown once.
	share string
	// Chain is how many backups a restore of this one reads (1 for a full
	// backup). ChainError says why it cannot be restored when a backup it
	// builds on is missing. Dependents is how many later backups build on this
	// one, and are deleted with it.
	Chain      int    `json:"chain,omitempty"`
	ChainError string `json:"chainError,omitempty"`
	Dependents int    `json:"dependents,omitempty"`
}

// scanPool lists the backups in one pool directory (one find on the node).
func scanPool(h *hostconn.Host, pd poolDir) ([]scanned, error) {
	marker := newScanMarker()
	out := h.ScanBackups(pd.Dir, marker) // the scan may report unreadable entries; the output is what counts
	var res []scanned
	for _, it := range parseScan(out, marker, pd.Dir) {
		sc := scanned{Dir: path.Base(it.Dir), Size: it.Size}
		if m, err := parseBackupManifest(it.Manifest); err != nil {
			sc.Err = err.Error()
		} else {
			sc.Manifest = m
		}
		res = append(res, sc)
	}
	return res, nil
}

type scanned struct {
	Dir      string
	Size     uint64
	Manifest *backupManifest
	Err      string
}

// listBackups scans every directory pool of every connected node.
func (s *Server) listBackups(uuidFilter string) []backupEntry {
	var mu sync.Mutex
	var out []backupEntry
	type where struct {
		node    int64
		name    string
		shutOff bool
	}
	exists := map[string]where{}
	var wg sync.WaitGroup
	for _, n := range s.Nodes.Status() {
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		name := n.Hostname
		if name == "" {
			name = h.Name
		}
		wg.Add(1)
		go func(id int64, h *hostconn.Host, name string) {
			defer wg.Done()
			if doms, _, err := h.ConnectListAllDomains(1, 0); err == nil {
				active := map[string]bool{}
				if act, _, err := h.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive); err == nil {
					for _, d := range act {
						active[cluster.FormatUUID(d.UUID)] = true
					}
				}
				mu.Lock()
				for _, d := range doms {
					if isReplicaOrStale(h, d) {
						continue // the standby's inert copy and the old VM after a failover are not "the VM"
					}
					u := cluster.FormatUUID(d.UUID)
					exists[u] = where{node: id, name: name, shutOff: !active[u]}
				}
				mu.Unlock()
			}
			dirs, err := poolDirs(h)
			if err != nil {
				return
			}
			descs, _ := poolDescs(h)
			for _, pd := range dirs {
				items, _ := scanPool(h, pd)
				share := shareID(h, descs[pd.Dir])
				mu.Lock()
				for _, it := range items {
					if uuidFilter != "" && (it.Manifest == nil || it.Manifest.UUID != uuidFilter) {
						continue
					}
					out = append(out, backupEntry{Node: id, NodeName: name, Pool: pd.Name, Dir: it.Dir, Path: pd.Dir + "/" + it.Dir, Size: it.Size, Manifest: it.Manifest, Err: it.Err, share: share})
				}
				mu.Unlock()
			}
		}(n.ID, h, name)
	}
	wg.Wait()
	out = dedupShared(out)
	annotateChains(out)
	for i := range out {
		if out[i].Manifest != nil {
			if w, ok := exists[out[i].Manifest.UUID]; ok {
				out[i].VMExists, out[i].VMNode, out[i].VMNodeName, out[i].VMShutOff = true, w.node, w.name, w.shutOff
			}
			out[i].ExportName = out[i].Manifest.VM + ".vmexport.tar"
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Manifest, out[j].Manifest
		if a == nil || b == nil {
			return a != nil
		}
		return a.createdTime().After(b.createdTime())
	})
	return out
}

func (s *Server) listBackupsHandler(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("uuid")
	if uuid != "" {
		if _, err := cluster.ParseUUID(uuid); err != nil {
			fail(w, badRequest("bad uuid"))
			return
		}
	}
	list := s.listBackups(uuid)
	if list == nil {
		list = []backupEntry{}
	}
	writeJSON(w, list)
}

// annotateChains fills in what the list says about incremental backups, per
// pool: the length of each chain, a missing link, and what depends on it.
func annotateChains(list []backupEntry) {
	type pool struct {
		by  map[string]*backupManifest
		all []retained
	}
	pools := map[string]*pool{}
	key := func(e backupEntry) string { return strconv.FormatInt(e.Node, 10) + "/" + e.Pool }
	for _, e := range list {
		if e.Manifest == nil {
			continue
		}
		p := pools[key(e)]
		if p == nil {
			p = &pool{by: map[string]*backupManifest{}}
			pools[key(e)] = p
		}
		p.by[e.Dir] = e.Manifest
		p.all = append(p.all, retained{Key: e.Dir, UUID: e.Manifest.UUID, Created: e.Manifest.createdTime(), Parent: e.Manifest.Parent})
	}
	for i := range list {
		e := &list[i]
		if e.Manifest == nil {
			continue
		}
		p := pools[key(*e)]
		if chain, err := chainOf(p.by, e.Dir); err != nil {
			e.ChainError = err.Error()
		} else {
			e.Chain = len(chain)
		}
		e.Dependents = len(dependentsOf(p.all, e.Dir))
	}
}

// backupRef is a backup located by the URL: node, pool and directory name.
type backupRef struct {
	h    *hostconn.Host
	id   int64
	pool string
	pd   poolDir
	name string
	dir  string
}

func (s *Server) backupFromURL(r *http.Request) (*backupRef, error) {
	id, h, err := s.host(r)
	if err != nil {
		return nil, err
	}
	b := &backupRef{h: h, id: id, pool: r.PathValue("pool"), name: r.PathValue("name")}
	if !backupDirRE.MatchString(b.name) {
		return nil, badRequest("not a backup name")
	}
	dirs, err := poolDirs(h)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if d.Name == b.pool {
			b.pd = d
		}
	}
	if b.pd.Dir == "" {
		return nil, badRequest("the pool must be a running directory-backed pool")
	}
	b.dir = b.pd.Dir + "/" + b.name
	if !safePathRE.MatchString(b.dir) {
		return nil, badRequest("unsupported characters in " + b.dir)
	}
	return b, nil
}

// load reads and vets the backup's manifest and checks that every file is
// there with the size the manifest records.
func (b *backupRef) load() (*backupManifest, error) {
	return loadBackup(b.h, b.dir)
}

func loadBackup(h *hostconn.Host, dir string) (*backupManifest, error) {
	raw, err := readLimited(h, dir+"/manifest.json", backupMaxManifest)
	if err != nil {
		return nil, badRequest("not a backup: " + err.Error())
	}
	m, err := parseBackupManifest(raw)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	files := m.files()
	sizes, err := h.FilesSize(dir, files)
	if err != nil {
		return nil, badRequest("cannot read the backup directory: " + err.Error())
	}
	bytesOf := map[string]string{}
	for i, f := range files {
		if sizes[i] == "MISSING" {
			return nil, badRequest("the backup is incomplete: " + f + " is missing")
		}
		bytesOf[f] = sizes[i]
	}
	for _, d := range m.Disks {
		if bytesOf[d.File] != strconv.FormatUint(d.Bytes, 10) {
			return nil, badRequest("the backup is damaged: " + d.File + " has " + bytesOf[d.File] + " bytes, the manifest says " + strconv.FormatUint(d.Bytes, 10))
		}
	}
	return m, nil
}

// chainLink is one backup a restore reads.
type chainLink struct {
	Dir string
	Man *backupManifest
}

func (l chainLink) path(b *backupRef) string { return b.pd.Dir + "/" + l.Dir }

func (l chainLink) diskOf(target string) (backupDiskRec, bool) {
	for _, d := range l.Man.Disks {
		if d.Target == target {
			return d, true
		}
	}
	return backupDiskRec{}, false
}

// loadChain returns the backups that restoring m needs, the full one first and
// m last (just m for a full backup). Every one of them is read and checked: a
// missing or damaged link is reported by name, not as a failure half way
// through a restore.
func (b *backupRef) loadChain(m *backupManifest) ([]chainLink, error) {
	by := map[string]*backupManifest{b.name: m}
	for cur := m; cur.kindOf() == kindIncremental; {
		if len(by) > maxChainWalk {
			return nil, badRequest("the chain of backups is too long")
		}
		if _, seen := by[cur.Parent]; seen {
			return nil, badRequest("the chain of backups loops")
		}
		pm, err := loadBackup(b.h, b.pd.Dir+"/"+cur.Parent)
		if err != nil {
			return nil, badRequest("this backup cannot be restored: the earlier backup " + cur.Parent + " it builds on is not usable (" + err.Error() + "). Restore a backup that comes before it.")
		}
		by[cur.Parent] = pm
		cur = pm
	}
	order, err := chainOf(by, b.name)
	if err != nil {
		return nil, badRequest("this backup cannot be restored: " + err.Error())
	}
	out := make([]chainLink, len(order))
	for i, d := range order {
		out[i] = chainLink{Dir: d, Man: by[d]}
		if len(order) > 1 {
			for _, dk := range by[d].Disks {
				if dk.SHA256 == "" {
					return nil, badRequest("this backup cannot be restored: " + d + " has no checksum, so the chain cannot be verified. Restore a backup that comes before it.")
				}
			}
		}
	}
	return out, nil
}

// chainBytes is how much of the disk target the chain holds.
func (p *restorePlan) chainBytes(target string) uint64 {
	var n uint64
	for _, l := range p.chain {
		if d, ok := l.diskOf(target); ok {
			n += d.Bytes
		}
	}
	return n
}

func (it scanned) retained() retained {
	return retained{Key: it.Dir, UUID: it.Manifest.UUID, Created: it.Manifest.createdTime(), Parent: it.Manifest.Parent}
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	if m, merr := b.load(); merr == nil {
		bkRuns.Lock()
		_, busy := bkRuns.m[m.UUID]
		bkRuns.Unlock()
		if busy {
			fail(w, badRequest("a backup of this VM is running and may build on this one. Delete it when the backup has finished."))
			return
		}
	}
	// Backups that build on this one are useless without it. Deleting it
	// takes them along, but only when the caller says so.
	var later []string
	if items, serr := scanPool(b.h, b.pd); serr == nil {
		var all []retained
		for _, it := range items {
			if it.Err == "" {
				all = append(all, it.retained())
			}
		}
		later = dependentsOf(all, b.name)
	}
	if len(later) > 0 && r.URL.Query().Get("withLater") != "1" {
		fail(w, badRequest(fmt.Sprintf("%d later backup(s) build on this one and would be deleted with it: %s. Delete them as well, or keep this one.", len(later), strings.Join(later, ", "))))
		return
	}
	err = s.task(r, "backup delete", b.name, b.h.Name, func(func(float64)) error {
		for _, d := range later { // newest first: no backup is ever left without its parent
			if err := removeBackupDir(b.h, b.pd.Dir, d); err != nil {
				return err
			}
		}
		return removeBackupDir(b.h, b.pd.Dir, b.name)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// checkQcow2NVRAM verifies the copy of a qcow2 UEFI variable store: it must
// read as qcow2 and not be marked corrupt (a running VM could have changed
// the file during the copy).
func (m *bkRun) checkQcow2NVRAM(file string) error {
	out, err := m.src.QemuInfo(file, false)
	if err != nil {
		return fmt.Errorf("the saved UEFI variables are not readable: %s", impFirstLine(out))
	}
	if !strings.Contains(out, `"format": "qcow2"`) || strings.Contains(out, `"corrupt": true`) {
		return fmt.Errorf("the saved UEFI variables are not a consistent qcow2 file")
	}
	return nil
}

// finishFreeze turns the end of a run into what the task shows: a guest that
// may have been left frozen is an error whatever else happened; otherwise a
// live backup ends well with one clause on whether the file systems were frozen.
func (m *bkRun) finishFreeze(err error) error {
	if m == nil {
		return err
	}
	if m.thawErr != nil {
		msg := thawProblem(m.name, m.thawErr)
		if err != nil {
			return withNote(err, msg)
		}
		return fmt.Errorf("the backup was made, but %s", msg)
	}
	if err == nil && m.fz.Text != "" {
		return doneNote(m.fz.Text)
	}
	return err
}
