package api

// Backing up a VM to a directory-backed pool on this or another node.
//
//   - A running VM is backed up with libvirt's backup API (push mode, full,
//     no checkpoint): QEMU copies the disks while the guest keeps running and
//     nothing is left behind in the domain (no snapshot, bitmap or checkpoint).
//     The job is polled for progress and can be aborted.
//   - A shut-off VM is backed up with qemu-img convert, which also flattens
//     backing chains.
//   - Files are first written to a hidden work directory inside a pool on the
//     node that has the disks; for another node they are copied with scp (as
//     migration does) into a work directory in the target pool. The manifest
//     is written last and the directory renamed into place, so a backup either
//     exists completely or not at all, and cancelling removes everything.

import (
	"bytes"
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

	"virt-proj/internal/cluster"
	"virt-proj/internal/hostconn"
)

type backupReq struct {
	TargetNode int64  `json:"targetNode"` // 0 = the node the VM is on
	Pool       string `json:"pool"`
	KeepLast   int    `json:"keepLast"` // 0 = keep everything
	SHA256     bool   `json:"sha256"`
	Quiesce    bool   `json:"quiesce"` // freeze guest file systems through the guest agent (running VM)
}

// bkDisk is a writable disk that goes into the backup.
type bkDisk struct {
	migDisk
	File   string // file name inside the backup
	Actual uint64 // bytes allocated by its whole chain
	Size   uint64 // bytes of the backup file, once written
	Virt   uint64
	SHA    string
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
	user     string
	host     string
	remote   bool
	progress func(float64)

	mu        sync.Mutex
	srcWork   *workDir
	cancelled atomic.Bool
	inJob     atomic.Bool
}

var bkRuns = struct {
	sync.Mutex
	m map[string]*bkRun
}{m: map[string]*bkRun{}}

var errBackupCancelled = errors.New("backup cancelled")

func newWork(h *hostconn.Host, dir, tag string) (*workDir, error) {
	prefix := dir + "/.virt-proj-" + tag + "."
	out, err := h.Run("umask 077 && mktemp -d -- " + shq(prefix+"XXXXXX"))
	if err != nil || !strings.HasPrefix(out, prefix) || hasControl(out) {
		return nil, fmt.Errorf("cannot create a work directory in %s on %s: %s", dir, h.Name, impFirstLine(out))
	}
	return &workDir{h: h, path: out}, nil
}

// readLimited returns a node file's content; larger files are refused.
func readLimited(h *hostconn.Host, file string, max int) ([]byte, error) {
	out, err := h.Run("head -c " + strconv.Itoa(max+1) + " -- " + shq(file))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %s", file, impFirstLine(out))
	}
	if len(out) > max {
		return nil, fmt.Errorf("%s is larger than %d KiB", file, max>>10)
	}
	return []byte(out), nil
}

func writeNodeFile(h *hostconn.Host, file string, data []byte) error {
	return h.Pipe(context.Background(), "umask 077; cat > "+shq(file), bytes.NewReader(data), nil)
}

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
	if pers, err := src.DomainIsPersistent(dom); err != nil || pers == 0 {
		return nil, badRequest("this VM is transient (only running, not defined); define it first")
	}
	state, _, err := src.DomainGetState(dom, 0)
	if err != nil {
		return nil, err
	}
	m := &bkRun{s: s, src: src, dom: dom, req: req, name: dom.Name, uuid: cluster.FormatUUID(dom.UUID), live: stateName(uint8(state)) != "shutoff"}
	if m.domXML, err = src.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive); err != nil {
		return nil, err
	}
	if len(m.domXML) > backupMaxXML {
		return nil, badRequest("the domain definition is unreasonably large")
	}
	if m.md, err = parseMigDomain(m.domXML); err != nil {
		return nil, badRequest("cannot read the domain definition: " + err.Error())
	}
	if out, err := src.Run(`for t in qemu-img tar; do command -v "$t" >/dev/null 2>&1 || echo "$t"; done`); err != nil || out != "" {
		return nil, badRequest("missing on " + src.Name + ": " + strings.Join(strings.Fields(out), ", "))
	}

	var need uint64
	for _, d := range m.md.Disks {
		if d.Device != "disk" || d.ReadOnly {
			m.skip = append(m.skip, d.Target)
			continue
		}
		switch {
		case d.Type != "file" || d.Path == "":
			return nil, badRequest("disk " + d.Target + " is a " + d.Type + " disk; only file-based disks can be backed up")
		case d.Shareable:
			return nil, badRequest("disk " + d.Target + " is shareable; a shared disk is not backed up with one VM")
		}
		out, err := src.Run("qemu-img info -U --backing-chain --output=json -- " + shq(d.Path) + " 2>&1")
		if err != nil {
			return nil, badRequest("qemu-img could not inspect " + d.Path + ": " + impFirstLine(out))
		}
		chain, err := parseChain(out)
		if err != nil {
			return nil, err
		}
		bd := bkDisk{migDisk: d, File: d.Target + ".qcow2", Actual: chainActual(out), Virt: chain[0].VirtualSize}
		need += bd.Actual
		m.disks = append(m.disks, bd)
	}
	if len(m.disks) == 0 {
		return nil, badRequest("the VM has no writable disk to back up")
	}
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
	free, err := freeBytes(m.dst, pd.Dir)
	if err != nil {
		return nil, err
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
		if sf, err := freeBytes(src, m.srcDir); err != nil || uint64(sf) < need {
			return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s to stage the backup", m.srcDir, src.Name))
		}
		dn, _ := s.nodeStatus(dstID)
		var c checkList
		s.checkSSHTrust(src, m.dst, dn, &c)
		if c.blocked() {
			for _, k := range c {
				if !k.OK && k.Severity == "error" {
					return nil, badRequest(k.Message)
				}
			}
		}
		if m.user, m.host, err = sshTarget(m.dst, dn); err != nil {
			return nil, badRequest(err.Error())
		}
	}
	return m, nil
}

func (s *Server) vmBackup(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req backupReq
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
	writeJSON(w, map[string]any{"task": id, "mode": mode})
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
	bkRuns.m[m.uuid] = m
	bkRuns.Unlock()
	id, done, err := s.startBackgroundWait(r, "backup", backupTarget(m.name, m.dst.Name, m.dstPool), h.Name, func(progress func(float64)) error {
		defer func() {
			bkRuns.Lock()
			delete(bkRuns.m, m.uuid)
			bkRuns.Unlock()
		}()
		m.progress = progress
		return m.run()
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

func (m *bkRun) run() (err error) {
	m.progress(0.01)
	stamp := time.Now().UTC()
	finalName := backupDirName(m.name, stamp)
	final := m.dstDir + "/" + finalName
	if out, err := m.dst.Run("test ! -e " + shq(final)); err != nil {
		return fmt.Errorf("%s already exists: %s", final, out)
	}

	srcWork, err := newWork(m.src, m.srcDir, "bk")
	if err != nil {
		return err
	}
	srcWork.stop = m.cancelled.Load
	m.mu.Lock()
	m.srcWork = srcWork
	m.mu.Unlock()
	var dstWork *workDir
	defer func() {
		if err != nil {
			var left []string
			for _, wd := range []*workDir{srcWork, dstWork} {
				if wd != nil {
					if rerr := wd.remove(); rerr != nil {
						left = append(left, wd.path+" on "+wd.h.Name)
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
		if out, err := m.src.Run("chmod 711 -- " + shq(srcWork.path)); err != nil {
			return fmt.Errorf("chmod: %s", out)
		}
		if err = m.liveBackup(srcWork, 0.02, copyHi); err != nil {
			return err
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
	}
	if m.md.NVRAM != "" {
		if !safePathRE.MatchString(m.md.NVRAM) {
			return fmt.Errorf("unsupported characters in the NVRAM path %s", m.md.NVRAM)
		}
		if out, err := m.src.Run("cp -- " + shq(m.md.NVRAM) + " " + shq(srcWork.file(backupNVRAMFile))); err != nil {
			return fmt.Errorf("saving the UEFI variables %s: %s", m.md.NVRAM, impFirstLine(out))
		}
		man.NVRAM = backupNVRAMFile
	}
	if err = m.measure(srcWork); err != nil {
		return err
	}
	for _, d := range m.disks {
		man.Disks = append(man.Disks, backupDiskRec{Target: d.Target, File: d.File, Format: "qcow2", VirtualSize: d.Virt, Bytes: d.Size, SHA256: d.SHA})
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
	// The conversion helper's bookkeeping files must not end up in the backup.
	m.dst.Run("rm -f -- " + shq(dstWork.file("rc")) + " " + shq(dstWork.file("qpid")) + " " + shq(dstWork.file("prog.log")) + " " + shq(dstWork.file("out.part")))
	if out, err2 := m.dst.Run("chmod 700 -- " + shq(dstWork.path) + " && mv -T -- " + shq(dstWork.path) + " " + shq(final)); err2 != nil {
		err = fmt.Errorf("moving the backup into place: %s", impFirstLine(out))
		return err
	}
	m.progress(0.98)
	// From here on the backup exists; the work directories are gone.
	srcWork, dstWork = nil, nil
	if m.req.KeepLast > 0 {
		if perr := m.s.pruneBackups(m.dst, m.dstPool, m.uuid, m.req.KeepLast, finalName); perr != nil {
			return fmt.Errorf("backup %s was written, but pruning older backups failed: %w", finalName, perr)
		}
	}
	return nil
}

// liveBackup runs libvirt's push-mode backup and follows the job.
func (m *bkRun) liveBackup(wd *workDir, lo, hi float64) error {
	files := map[string]string{}
	var plain []migDisk
	for _, d := range m.disks {
		files[d.Target] = d.File
		plain = append(plain, d.migDisk)
	}
	var flags libvirt.DomainBackupBeginFlags
	if m.req.Quiesce {
		flags |= backupBeginQuiesce
	}
	if err := m.checkCancel(); err != nil {
		return err
	}
	m.inJob.Store(true)
	defer m.inJob.Store(false)
	if err := m.src.DomainBackupBegin(m.dom, backupXML(wd.path, plain, files, m.skip), libvirt.OptString{}, flags); err != nil {
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
		case st["file_total"] > 0:
			m.spread(lo, hi, st["file_processed"]/st["file_total"])
		case st["data_total"] > 0:
			m.spread(lo, hi, st["data_processed"]/st["data_total"])
		}
	}
	return nil
}

// offlineBackup flattens every disk into the work directory.
func (m *bkRun) offlineBackup(wd *workDir, lo, hi float64) error {
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
	out, err := h.Run("qemu-img info -U --output=json -- " + shq(file) + " 2>&1")
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
func (m *bkRun) measure(wd *workDir) error {
	for i := range m.disks {
		d := &m.disks[i]
		f := wd.file(d.File)
		info, err := qemuInfo(m.src, f)
		if err != nil {
			return fmt.Errorf("backup image of %s is not usable: %w", d.Target, err)
		}
		if info.Format != "qcow2" {
			return fmt.Errorf("backup image of %s has format %s", d.Target, info.Format)
		}
		d.Virt = uint64(info.VirtualSize)
		out, err := m.src.Run("stat -c %s -- " + shq(f))
		if err != nil {
			return fmt.Errorf("stat %s: %s", f, out)
		}
		d.Size, _ = strconv.ParseUint(strings.TrimSpace(out), 10, 64)
		if m.req.SHA256 {
			if d.SHA, err = sha256Of(m.src, f); err != nil {
				return err
			}
		}
	}
	return nil
}

func sha256Of(h *hostconn.Host, file string) (string, error) {
	out, err := h.Run("sha256sum -- " + shq(file))
	f := strings.Fields(out)
	if err != nil || len(f) < 1 || !sha256RE.MatchString(f[0]) {
		return "", fmt.Errorf("sha256sum %s: %s", file, impFirstLine(out))
	}
	return f[0], nil
}

// copyOver copies the staged files to the target node with scp, run on the
// source node like migration does.
func (m *bkRun) copyOver(from, to *workDir, man *backupManifest, lo, hi float64) error {
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
		cmd := "scp -q -p -o BatchMode=yes -- " + shq(from.file(n)) + " " + shq(m.user+"@"+m.host+":"+to.file(n))
		if out, err := m.src.Run(cmd); err != nil {
			return fmt.Errorf("copying %s to %s failed: %w: %s", n, m.dst.Name, err, impFirstLine(out))
		}
		done += sizes[n]
		m.spread(lo, hi, float64(done)/float64(total))
	}
	return nil
}

// --- retention ---

// pruneBackups deletes the backups of VM uuid in a pool beyond the newest
// keep. The backup just written (just) is never deleted.
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
			all = append(all, retained{Key: it.Dir, UUID: it.Manifest.UUID, Created: it.Manifest.createdTime()})
		}
	}
	var errs []string
	for _, b := range pruneList(all, uuid, keep) {
		if b.Key == just {
			continue
		}
		if err := removeBackupDir(h, pd.Dir, b.Key); err != nil {
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
	out, err := h.Run("[ -f " + shq(dir+"/manifest.json") + " ] && [ ! -L " + shq(dir) + " ] && rm -rf -- " + shq(dir))
	if err != nil {
		return fmt.Errorf("cannot delete %s on %s: %s", dir, h.Name, impFirstLine(out))
	}
	return nil
}

// --- listing ---

type backupEntry struct {
	Node       int64           `json:"node"`
	NodeName   string          `json:"nodeName"`
	Pool       string          `json:"pool"`
	Dir        string          `json:"dir"`
	Path       string          `json:"path"`
	Size       uint64          `json:"size"`
	Manifest   *backupManifest `json:"manifest,omitempty"`
	Err        string          `json:"error,omitempty"`
	VMExists   bool            `json:"vmExists"`
	ExportName string          `json:"exportName,omitempty"`
}

// scanPool lists the backups in one pool directory (one find on the node).
func scanPool(h *hostconn.Host, pd poolDir) ([]scanned, error) {
	marker := newScanMarker()
	out, _ := h.Run(scanScript(pd.Dir, marker)) // find exits non-zero for unreadable entries; the output is what counts
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
	exists := map[string]bool{}
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
				mu.Lock()
				for _, d := range doms {
					exists[cluster.FormatUUID(d.UUID)] = true
				}
				mu.Unlock()
			}
			dirs, err := poolDirs(h)
			if err != nil {
				return
			}
			for _, pd := range dirs {
				items, _ := scanPool(h, pd)
				mu.Lock()
				for _, it := range items {
					if uuidFilter != "" && (it.Manifest == nil || it.Manifest.UUID != uuidFilter) {
						continue
					}
					out = append(out, backupEntry{Node: id, NodeName: name, Pool: pd.Name, Dir: it.Dir, Path: pd.Dir + "/" + it.Dir, Size: it.Size, Manifest: it.Manifest, Err: it.Err})
				}
				mu.Unlock()
			}
		}(n.ID, h, name)
	}
	wg.Wait()
	for i := range out {
		if out[i].Manifest != nil {
			out[i].VMExists = exists[out[i].Manifest.UUID]
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
	raw, err := readLimited(b.h, b.dir+"/manifest.json", backupMaxManifest)
	if err != nil {
		return nil, badRequest("not a backup: " + err.Error())
	}
	m, err := parseBackupManifest(raw)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	var script strings.Builder
	script.WriteString("cd " + shq(b.dir) + " || exit 1; for f in")
	for _, f := range m.files() {
		script.WriteString(" " + shq(f))
	}
	script.WriteString(`; do if [ -f "$f" ] && [ ! -L "$f" ]; then stat -c %s -- "$f"; else echo MISSING; fi; done`)
	out, err := b.h.Run(script.String())
	if err != nil {
		return nil, badRequest("cannot read the backup directory: " + impFirstLine(out))
	}
	sizes := strings.Fields(out)
	files := m.files()
	if len(sizes) != len(files) {
		return nil, badRequest("unexpected answer while checking the backup files")
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

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	err = s.task(r, "backup delete", b.name, b.h.Name, func(func(float64)) error {
		return removeBackupDir(b.h, b.pd.Dir, b.name)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// VIR_DOMAIN_BACKUP_BEGIN_QUIESCE, which go-libvirt does not name.
const backupBeginQuiesce libvirt.DomainBackupBeginFlags = 2
