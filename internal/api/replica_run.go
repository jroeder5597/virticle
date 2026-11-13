package api

// One run of replication: take the changes of a VM since the last point (or
// every block, when that is not possible), send them to the standby node, add
// them there as a new point and tidy up. libvirt's backup job (the code of
// backups, bkRun.liveBackup) produces the files; what happens to them is in
// replica_points.go and replica_store.go.
//
// A VM that is shut off is copied too, because its last writes happened after
// the last run: the VM is started paused (the guest never runs, so nothing is
// written), the backup job runs as for a live VM, and QEMU is asked to end
// cleanly, which saves the dirty bitmaps again. If that is not possible the
// disks are converted whole.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// replRuns are the replication runs under way, by VM UUID; replNodes the source nodes that have one.
// Both are guarded by bkRuns' mutex, so a backup and a replication run of one VM exclude each other
// (libvirt allows one backup job per VM).
var (
	replRuns  = map[string]*replRun{}
	replNodes = map[int64]string{}
)

type replRun struct {
	s       *Server
	srcID   int64
	src     *hostconn.Host
	dom     libvirt.Domain
	dstID   int64
	dst     *hostconn.Host
	dstNode store.Node
	cfg     replConfig
	bk      *bkRun
	name    string
	uuid    string
	pd      poolDir // the standby pool
	ep      migEndpoint
	st      replStore
	// final: the run of a planned failover (the VM is shut off and the copy must be exact)
	final bool
	live  bool

	progress func(float64)

	// results, for the task and the status
	Kind    string
	Reason  string
	Sent    uint64
	Skipped bool
	Point   time.Time
	Took    time.Duration
}

var errNoReplica = errors.New("replication of this VM is not set up")

// pausedCopies are the VMs that are started paused for a copy right now (guarded by bkRuns' mutex). Start and
// Resume of such a VM are refused: the guest must not run while its disks are read.
var pausedCopies = map[string]bool{}

func setPausedCopy(uuid string, on bool) {
	bkRuns.Lock()
	defer bkRuns.Unlock()
	if on {
		pausedCopies[uuid] = true
	} else {
		delete(pausedCopies, uuid)
	}
}

// pausedCopyGuard refuses to start or resume a VM that is paused for its copy.
func pausedCopyGuard(dom libvirt.Domain) error {
	bkRuns.Lock()
	on := pausedCopies[cluster.FormatUUID(dom.UUID)]
	bkRuns.Unlock()
	if on {
		return badRequest(dom.Name + " is shut off and is being copied to its standby node right now (it is held paused for the copy and ends by itself within a few minutes). Start it when the copy is done")
	}
	return nil
}

// replStatus is what the controller remembers of a replicated VM between runs (in memory only: what
// is lost in a restart is the text of the last failure, the copy itself says everything else).
var replStatus = struct {
	sync.Mutex
	err    map[string]string // uuid -> last failure
	errAt  map[string]time.Time
	failed map[string]time.Time // last failed attempt, for the retry pause
	prog   map[string]float64
}{err: map[string]string{}, errAt: map[string]time.Time{}, failed: map[string]time.Time{}, prog: map[string]float64{}}

func setReplError(uuid string, err error) {
	replStatus.Lock()
	defer replStatus.Unlock()
	if err == nil {
		delete(replStatus.err, uuid)
		delete(replStatus.errAt, uuid)
		delete(replStatus.failed, uuid)
		return
	}
	replStatus.err[uuid] = err.Error()
	replStatus.errAt[uuid] = time.Now()
	replStatus.failed[uuid] = time.Now()
}

func replErrorOf(uuid string) (string, time.Time) {
	replStatus.Lock()
	defer replStatus.Unlock()
	return replStatus.err[uuid], replStatus.errAt[uuid]
}

// claimRepl registers a run for the VM (and, unless final, its source node). It fails when a backup or
// another run of the VM is going, or another VM is being copied from the same node.
func claimRepl(r *replRun) error {
	bkRuns.Lock()
	defer bkRuns.Unlock()
	if _, busy := bkRuns.m[r.uuid]; busy {
		return badRequest("a backup of this VM is in progress; replication runs when it is done")
	}
	if _, busy := replRuns[r.uuid]; busy {
		return badRequest("a replication run of this VM is already in progress")
	}
	migRuns.Lock()
	_, moving := migRuns.m[r.uuid]
	migRuns.Unlock()
	if moving && !r.final {
		return badRequest("this VM is being migrated right now; replication runs when it is done")
	}
	if other, busy := replNodes[r.srcID]; busy && !r.final {
		return badRequest("another VM (" + other + ") is being copied from this node right now; one run at a time per node")
	}
	replRuns[r.uuid] = r
	if !r.final {
		replNodes[r.srcID] = r.name
	}
	return nil
}

func releaseRepl(r *replRun) {
	bkRuns.Lock()
	defer bkRuns.Unlock()
	if replRuns[r.uuid] == r {
		delete(replRuns, r.uuid)
	}
	if replNodes[r.srcID] == r.name && !r.final {
		delete(replNodes, r.srcID)
	}
}

func replRunning(uuid string) bool {
	bkRuns.Lock()
	defer bkRuns.Unlock()
	_, busy := replRuns[uuid]
	return busy
}

// replicaDefinition turns the source's inactive definition into the standby's: the disks at the files
// they get when the replica is started, no install media, no backing chains, none of the source's
// metadata (schedules, tags). Everything else (CPU, memory, networks, firmware, NVRAM path, TPM) is kept.
func replicaDefinition(srcXML string, md migDomain, poolDir string) (string, map[string]string, error) {
	vols := map[string]string{}
	m := map[string]string{}
	i := 0
	for _, d := range md.Disks {
		if d.Path == "" {
			continue
		}
		if d.Device == "cdrom" || d.Device == "floppy" || d.ReadOnly {
			m[d.Path] = ""
			continue
		}
		v := replVolName(md.Name, d.Target, i)
		if !volNameRE.MatchString(v) {
			return "", nil, badRequest("volume name " + v + " is not valid")
		}
		vols[d.Target] = v
		m[d.Path] = poolDir + "/" + v
		i++
	}
	x := stripBackingStore(srcXML)
	x = remapDisks(x, m)
	x = reMetadataEl.ReplaceAllString(x, "")
	return x, vols, nil
}

var reMetadataEl = regexp.MustCompile(`(?s)<metadata>.*?</metadata>|<metadata\s*/>`)

// planReplica inspects the VM and the standby and builds the run. Everything that can be refused is
// refused here, before a task exists.
func (s *Server) planReplica(srcID int64, src *hostconn.Host, dom libvirt.Domain, cfg replConfig, final bool) (*replRun, error) {
	sweepWork()
	s.sweepReplicaOrphans(srcID, src)
	if pers, err := src.DomainIsPersistent(dom); err != nil || pers == 0 {
		return nil, badRequest("this VM is transient (only running, not defined); define it first")
	}
	state, _, err := src.DomainGetState(dom, 0)
	if err != nil {
		return nil, err
	}
	live := stateName(uint8(state)) != "shutoff"
	if cfg.TargetNode == srcID {
		return nil, badRequest("the standby node must be another node than the one the VM is on")
	}
	if _, ok := s.nodeStatus(cfg.TargetNode); !ok {
		return nil, badRequest("no such standby node")
	}
	dst, err := s.Nodes.Host(cfg.TargetNode)
	if err != nil {
		return nil, badRequest("the standby node is not connected: " + err.Error())
	}
	dn, _ := s.nodeStatus(cfg.TargetNode)
	s.sweepReplicaOrphans(cfg.TargetNode, dst)
	// The setting names the standby by number and by name. A controller that was set up again (nodes added in another
	// order) numbers its nodes differently: a number that now belongs to another node must not receive the copy.
	if cfg.TargetName != "" && dst.Name != cfg.TargetName {
		return nil, badRequest("the standby " + cfg.TargetName + " of this VM is not the node with that number any more (it is " + dst.Name + " now, the nodes were numbered again). Stop replication and set it up again")
	}
	ctx, stop := context.WithCancel(context.Background())
	m := &bkRun{ctx: ctx, stopCtx: stop, s: s, src: src, dom: dom, name: dom.Name, uuid: cluster.FormatUUID(dom.UUID), live: live}
	no := false
	_ = no // quiesce stays automatic: a short freeze through the guest agent when there is one
	if m.domXML, err = src.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive); err != nil {
		return nil, err
	}
	if len(m.domXML) > backupMaxXML {
		return nil, badRequest("the domain definition is unreasonably large")
	}
	if m.md, err = parseMigDomain(m.domXML); err != nil {
		return nil, badRequest("cannot read the domain definition: " + err.Error())
	}
	if m.md.Hostdevs > 0 {
		return nil, badRequest("this VM has passthrough devices (PCI, USB or mediated); they belong to this node, so the VM cannot be replicated. Remove them first")
	}
	if err := refuseSharedDisks(src, dst, m.md); err != nil {
		return nil, err
	}
	if err := missingTools(src, "qemu-img"); err != nil {
		return nil, err
	}
	if err := missingTools(dst, "qemu-img"); err != nil {
		return nil, err
	}
	disks, skip, allQcow2, _, err := inspectBackupDisks(src, m.md, "replicated")
	if err != nil {
		return nil, err
	}
	if !allQcow2 {
		return nil, badRequest("a disk of this VM is not a qcow2 image: only qcow2 disks can track changes, and copying a raw disk whole every time would take the standby's bandwidth and the node's I/O for nothing. Convert the disk to qcow2 first (move it to a pool as qcow2)")
	}
	m.disks, m.skip = disks, skip
	m.target = replTarget(cfg.TargetNode)
	m.req = backupReq{}

	_, pd, err := destPool(dst, cfg.Pool)
	if err != nil {
		return nil, err
	}
	if !safePathRE.MatchString(pd.Dir) {
		return nil, badRequest("unsupported characters in the pool directory " + pd.Dir)
	}
	r := &replRun{s: s, srcID: srcID, src: src, dom: dom, dstID: cfg.TargetNode, dst: dst, dstNode: dn.Node, cfg: cfg, bk: m,
		name: dom.Name, uuid: m.uuid, pd: pd, final: final, live: live}

	// The standby may hold this VM only as its replica.
	if err := r.checkStandbyIdentity(); err != nil {
		return nil, err
	}
	// Where the VM's files wait on the source before they are sent.
	dirs, err := poolDirs(src)
	if err != nil || len(dirs) == 0 {
		return nil, badRequest("the source node has no running directory-backed pool to stage the copy in")
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
	if r.ep, err = s.checkBackupTransfer(src, dst, dn.Node); err != nil {
		return nil, err
	}
	return r, nil
}

// standbyDomain finds the VM's domain on the standby node by UUID; ok is false when there is none.
func (r *replRun) standbyDomain() (libvirt.Domain, bool) {
	u, _ := cluster.ParseUUID(r.uuid)
	d, err := r.dst.DomainLookupByUUID(u)
	return d, err == nil
}

// checkStandbyIdentity refuses a standby that holds this UUID or name as anything but the replica (or,
// for "replicate back", the stale copy).
func (r *replRun) checkStandbyIdentity() error {
	if d, ok := r.standbyDomain(); ok {
		meta, _ := domainMeta(r.dst, d)
		switch {
		case parseReplicaMeta(meta) != nil:
		case parseStale(meta) != nil && r.cfg.ReplaceStale:
		default:
			return badRequest("the node " + r.dst.Name + " already has a VM with this identity that is not a replica of it (" + d.Name + "). If it is an old copy from before a failover, use \"Replicate back\" on the running VM; otherwise delete it or choose another standby")
		}
		return nil
	}
	if d, err := r.dst.DomainLookupByName(r.name); err == nil {
		return badRequest("the node " + r.dst.Name + " has another VM named " + d.Name + "; the replica takes the same name, so delete or rename that one or choose another standby")
	}
	return nil
}

// ensureStandby makes the replica directory and the (inert) replica domain exist, and returns the
// directory's store.
func (r *replRun) ensureStandby() error {
	dir := r.cfg.Dir
	if dir != "" {
		if ex, err := r.dst.Exists(dir); err != nil || !ex[dir] {
			dir = ""
		}
	}
	if dir == "" {
		if d, ok := r.standbyDomain(); ok {
			meta, _ := domainMeta(r.dst, d)
			if rm := parseReplicaMeta(meta); rm != nil && rm.Dir != "" {
				if ex, err := r.dst.Exists(rm.Dir); err == nil && ex[rm.Dir] {
					dir = rm.Dir
				}
			}
		}
	}
	if dir == "" {
		w, err := r.dst.MkWorkDir(r.pd.Dir, "rp")
		if err != nil {
			return err
		}
		dir = w
	}
	r.cfg.Dir = dir
	r.st = replStore{h: r.dst, dir: dir}
	if err := r.setSourceConfig(nil); err != nil {
		return err
	}
	if _, ok := r.standbyDomain(); !ok && !r.cfg.ReplaceStale {
		return r.defineReplica(nil)
	}
	return nil
}

// definition is the standby's definition for the VM as it is now.
func (r *replRun) definition() (string, map[string]string, error) {
	return replicaDefinition(r.bk.domXML, r.bk.md, r.pd.Dir)
}

// defineReplica defines (or redefines, when the definition changed) the replica domain on the standby
// and writes its marker. last is the newest point, if any.
func (r *replRun) defineReplica(last *replPoint) error {
	x, _, err := r.definition()
	if err != nil {
		return err
	}
	dom, exists := r.standbyDomain()
	if exists {
		if cur, err := r.dst.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive); err == nil && sameDefinition(cur, x) {
			return r.markReplica(dom, last)
		}
	}
	dom, err = r.dst.DomainDefineXMLFlags(x, libvirt.DomainDefineValidate)
	if err != nil {
		return fmt.Errorf("defining the replica on %s: %w", r.dst.Name, err)
	}
	if err := r.dst.DomainSetAutostart(dom, 0); err != nil {
		log.Printf("replica %s: autostart off: %v", r.name, err)
	}
	return r.markReplica(dom, last)
}

// sameDefinition compares two definitions as far as the replica cares: libvirt adds elements (addresses,
// ids, seclabels) when it stores a definition, so what is compared is the text it would store again.
func sameDefinition(stored, wanted string) bool {
	norm := func(x string) string {
		x = reMetadataEl.ReplaceAllString(x, "")
		return strings.Join(strings.Fields(x), " ")
	}
	return norm(stored) == norm(wanted)
}

// markReplica sets the replica marker of the standby's domain.
func (r *replRun) markReplica(dom libvirt.Domain, last *replPoint) error {
	rm := replicaMeta{Dir: r.cfg.Dir, Pool: r.cfg.Pool, SourceNode: r.srcID, SourceName: r.src.Name, Every: r.cfg.EveryMinutes}
	if last != nil {
		rm.Last = last.timeOf().Unix()
		rm.Checked = time.Now().Unix()
	}
	if names, err := r.st.list(); err == nil {
		pts, _ := r.st.points(names)
		rm.Points = len(pts)
		rm.Bytes = r.st.diskBytes(names)
	}
	if sm, err := domainMeta(r.src, r.dom); err == nil {
		if fo := parseFailedOver(sm); fo != nil {
			rm.SrcGen = fo.Gen
		}
	}
	old, err := domainMeta(r.dst, dom)
	if err != nil {
		return err
	}
	meta, err := setMetaChildren(old, map[string]string{replicaChild: rm.element()})
	if err != nil {
		return err
	}
	return setDomainMeta(r.dst, dom, meta)
}

// setSourceConfig stores the replication of the VM on the VM; mutate changes the config first.
func (r *replRun) setSourceConfig(mutate func(*replConfig)) error {
	if mutate != nil {
		mutate(&r.cfg)
	}
	old, err := domainMeta(r.src, r.dom)
	if err != nil {
		return err
	}
	meta, err := setMetaChildren(old, map[string]string{replChild: r.cfg.element()})
	if err != nil {
		return err
	}
	return setDomainMeta(r.src, r.dom, meta)
}

func (r *replRun) cancel() { r.bk.cancel() }

func (r *replRun) spread(lo, hi, f float64) { r.bk.progress = r.progress; r.bk.spread(lo, hi, f) }

// stamps are the modification time and size of the files the VM's disks are in, to tell later whether a
// shut-off VM was started in between.
func (r *replRun) stamps() ([]string, error) {
	var paths []string
	for _, d := range r.bk.disks {
		paths = append(paths, d.Path)
	}
	st, err := r.src.Stamps(paths...)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(st))
	for i, s := range st {
		if !s.Exists {
			return nil, fmt.Errorf("the disk file %s is missing", paths[i])
		}
		out[i] = strconv.FormatInt(s.Mtime, 10) + "|" + strconv.FormatInt(s.Size, 10)
	}
	return out, nil
}

func sameStamps(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// startPaused starts the shut-off VM with its virtual CPUs stopped, so a backup job can read its disks.
func (r *replRun) startPaused() error {
	_, err := r.src.DomainCreateWithFlags(r.dom, uint32(libvirt.DomainStartPaused))
	return err
}

// stopPaused ends the paused VM cleanly (QEMU saves its bitmaps when it is asked, not when it is killed).
func (r *replRun) stopPaused() error {
	// Only a VM that is still paused is ended: one that somebody resumed meanwhile (the controller's guard cannot
	// stop a libvirt client of its own) runs a guest, which a power-off would cut.
	if st, _, err := r.src.DomainGetState(r.dom, 0); err != nil || stateName(uint8(st)) != "paused" {
		return errors.New("the VM was not paused any more when the copy ended: somebody started it. It was left running and the copy was discarded")
	}
	if err := r.src.DomainDestroyFlags(r.dom, libvirt.DomainDestroyGraceful); err != nil {
		return err
	}
	for i := 0; i < 60; i++ {
		if st, _, err := r.src.DomainGetState(r.dom, 0); err == nil && stateName(uint8(st)) == "shutoff" {
			return nil
		}
		time.Sleep(time.Second)
	}
	return errors.New("the VM did not end after it was asked to")
}

// finishPausedCopy ends a VM that a run started paused and could not end (the controller stopped, or QEMU did
// not end in time). The record on the VM says a copy had it paused: a VM that is still paused is ended the same
// way the run would have; one that is shut off or running (somebody started it) is left alone. Then the record goes.
func finishPausedCopy(h *hostconn.Host, dom libvirt.Domain) {
	if st, _, err := h.DomainGetState(dom, 0); err == nil && stateName(uint8(st)) == "paused" {
		if err := h.DomainDestroyFlags(dom, libvirt.DomainDestroyGraceful); err != nil {
			log.Printf("replication %s: ending the paused VM: %v", dom.Name, err)
			return
		}
		log.Printf("replication %s: the VM was left paused by an interrupted copy; it was ended", dom.Name)
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		return
	}
	cfg := parseReplication(meta)
	if cfg == nil {
		return
	}
	cfg.PausedCopy = false
	if nm, err := setMetaChildren(meta, map[string]string{replChild: cfg.element()}); err == nil {
		_ = setDomainMeta(h, dom, nm)
	}
}

// run does it all. The new point exists once its manifest is written; before that a failure leaves the
// replica as it was, after it a failure only leaves tidying for the next run.
func (r *replRun) run() (err error) {
	started := time.Now()
	defer func() { r.Took = time.Since(started) }()
	m := r.bk
	m.progress = r.progress
	defer m.stopCtx()
	r.progress(0.01)
	stamp := time.Now().UTC()

	if err := r.ensureStandby(); err != nil {
		return err
	}
	kept, names, err := r.st.settle()
	if err != nil && isReplicaDamaged(err) {
		// A base image that cannot be made clean again is not repaired further: the copy is started over, whole.
		// The source is intact, so nothing is lost but the old points, which could not be started from anyway.
		log.Printf("replication %s: the replica on %s is damaged (%v); it is discarded and copied again", r.name, r.dst.Name, err)
		if werr := r.st.wipe(); werr != nil {
			return fmt.Errorf("the replica on %s is damaged (%v) and could not be cleared: %w", r.dst.Name, err, werr)
		}
		kept, names, err = r.st.settle()
	}
	if err != nil {
		return fmt.Errorf("checking the replica on %s: %w", r.dst.Name, err)
	}
	var latest *replPoint
	if len(kept) > 0 {
		latest = kept[0]
	}

	// A shut-off VM that has not been started since the last copy has nothing new.
	var stamps0 []string
	if !r.live {
		if stamps0, err = r.stamps(); err != nil {
			return err
		}
		if latest != nil && latest.Offline && sameStamps(latest.Stamps, stamps0) {
			r.Skipped, r.Point = true, latest.timeOf()
			r.progress(1)
			if d, ok := r.standbyDomain(); ok {
				_ = r.markReplica(d, latest)
			}
			r.Took = time.Since(started)
			return r.finishStatus(latest, nil)
		}
	}

	// Plan.
	in := replInput{Backupable: true, AllQcow2: true, Layout: nil, Checkpoints: map[string]bool{}, Latest: latest, Intact: latest != nil}
	for _, d := range m.disks {
		in.Layout = append(in.Layout, diskLayout{Target: d.Target, Source: d.Base, Virt: d.Virt})
	}
	if v, verr := r.src.ConnectGetLibVersion(); verr == nil && v >= minCheckpointLibvirt {
		in.Supported = true
	}
	cks, cerr := listCheckpoints(r.src, r.dom)
	if cerr != nil {
		log.Printf("replication %s: listing checkpoints: %v", r.name, cerr)
		in.Supported = false
	}
	for _, c := range cks {
		in.Checkpoints[c.Name] = true
	}
	plan := decideReplica(in)
	m.plan = backupPlan{Kind: plan.Kind, ParentCheckpoint: plan.ParentCheckpoint, Checkpoint: plan.Checkpoint, Reason: plan.Reason}

	// Space: on the standby the new files are written next to the old ones.
	need := uint64(importFreeMargin)
	for _, d := range m.disks {
		need += d.Actual
	}
	if plan.Kind == kindIncremental {
		if n, ok := changedBytes(r.src, r.dom, plan.ParentCheckpoint); ok {
			need = n + n/8 + importFreeMargin
		}
	}
	if free, ferr := bkFreeBytes(r.dst, r.pd.Dir); ferr != nil {
		return ferr
	} else if uint64(free) < need {
		return fmt.Errorf("not enough free space in %s on %s: %s available, about %s needed. The earlier copy is kept as it is; free some space there or choose a bigger pool", r.pd.Dir, r.dst.Name, fmtBytes(uint64(free)), fmtBytes(need))
	}
	if free, ferr := bkFreeBytes(r.src, m.srcDir); ferr != nil || uint64(free) < need {
		return fmt.Errorf("not enough free space in %s on %s to stage the copy: about %s needed", m.srcDir, r.src.Name, fmtBytes(need))
	}

	seq := nextSeq(sortedNames(names))
	gen := nextGen(sortedNames(names))
	if plan.Kind == kindIncremental {
		gen = latest.Gen
	}

	srcWork, err := newWork(r.src, m.srcDir, "rp")
	if err != nil {
		return err
	}
	srcWork.stop = m.cancelled.Load
	m.mu.Lock()
	m.srcWork = srcWork
	m.mu.Unlock()
	pointDone := false
	defer func() {
		if !pointDone {
			m.removeNewCheckpoint()
			r.cleanIncoming(seq)
		}
		if rerr := srcWork.remove(); rerr != nil {
			keepWork(srcWork)
		}
	}()

	// Produce the files on the source.
	if err = m.checkCancel(); err != nil {
		return err
	}
	if err = r.produce(srcWork, stamp, plan); err != nil {
		return err
	}
	if m.plan.Kind == kindIncremental {
		for _, d := range m.disks {
			if err = r.src.DetachBacking(srcWork.path, d.File); err != nil {
				return err
			}
		}
	}
	nvramFile := ""
	if m.md.NVRAM != "" {
		if nvramFile, err = r.copyNVRAM(srcWork); err != nil {
			return err
		}
	}
	if err = m.measure(srcWork); err != nil {
		return err
	}
	if !r.live {
		// After QEMU has ended: its last act changes the files.
		if stamps0, err = r.stamps(); err != nil {
			return err
		}
	}

	// Send.
	var sent uint64
	for _, d := range m.disks {
		sent += d.Size
	}
	r.Sent = sent
	if err = r.send(srcWork, seq, nvramFile, sent); err != nil {
		return err
	}
	if err = m.checkCancel(); err != nil {
		return err
	}

	// Add the point.
	kind := m.plan.Kind
	if kind == kindFull {
		gen = nextGen(sortedNames(names))
	}
	pt, err := r.addPoint(latest, seq, gen, kind, stamp, nvramFile != "", stamps0)
	if err != nil {
		return err
	}
	pointDone = true
	r.Kind, r.Reason, r.Point = kind, m.plan.Reason, stamp
	r.progress(0.93)

	// From here on the point exists; problems are reported but cost nothing.
	var problems []string
	note := func(e error) {
		if e != nil {
			problems = append(problems, e.Error())
			log.Printf("replication %s: %v", r.name, e)
		}
	}
	kept, _, serr := r.st.settle()
	note(serr)
	if len(kept) == 0 {
		kept = []*replPoint{pt}
	}
	note(r.afterPoint(kept[0]))
	note(r.dropOldCheckpoints())
	r.Took = time.Since(started)
	note(r.finishStatus(pt, nil))
	if len(problems) > 0 {
		return fmt.Errorf("the copy of %s is complete, but afterwards: %s", stamp.Format("15:04"), strings.Join(problems, "; "))
	}
	return nil
}

// produce makes the files of the point in the work directory on the source.
func (r *replRun) produce(wd *bkWork, stamp time.Time, plan replPlan) error {
	m := r.bk
	if r.live {
		if err := r.src.ChmodWork(wd.path, "711"); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
		return m.liveBackup(wd, stamp, 0.02, 0.5)
	}
	// Shut off. Only the copy of what changed since the last point is worth starting QEMU for: a planned failover
	// has to be quick whatever the size of the disks. The VM is started paused (its virtual CPUs never run, so
	// nothing is written), the same backup job runs as for a live VM, and QEMU is asked to end, which saves the
	// dirty bitmaps. Start and Resume are refused meanwhile and a restart of the controller ends the paused VM
	// (finishPausedCopy). Everything else (a first copy, a copy with no usable bitmap, a node that cannot start the
	// VM) converts the disks whole, which starts nothing.
	if plan.Kind == kindIncremental && plan.Checkpoint {
		if err := r.src.ChmodWork(wd.path, "711"); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
		if err := r.setSourceConfig(func(c *replConfig) { c.PausedCopy = true }); err != nil {
			return err
		}
		setPausedCopy(r.uuid, true)
		defer setPausedCopy(r.uuid, false)
		if err := r.startPaused(); err == nil {
			err := m.liveBackup(wd, stamp, 0.02, 0.5)
			serr := r.stopPaused()
			if serr == nil {
				_ = r.setSourceConfig(func(c *replConfig) { c.PausedCopy = false })
			} else {
				log.Printf("replication %s: ending the paused VM: %v", r.name, serr)
				if err == nil {
					err = fmt.Errorf("the VM was started paused for the copy and could not be ended cleanly: %v. It may be running paused; the next scheduler round ends it", serr)
				}
			}
			return err
		} else {
			_ = r.setSourceConfig(func(c *replConfig) { c.PausedCopy = false })
			log.Printf("replication %s: cannot start the shut-off VM paused (%v); converting the disks whole", r.name, err)
		}
	}
	m.plan = backupPlan{Kind: kindFull, Reason: whyReplShutOff}
	m.live = false
	if err := m.offlineBackup(wd, 0.02, 0.5); err != nil {
		return err
	}
	return nil
}

// copyNVRAM stages the UEFI variables next to the disk files.
func (r *replRun) copyNVRAM(wd *bkWork) (string, error) {
	m := r.bk
	if !safePathRE.MatchString(m.md.NVRAM) {
		return "", fmt.Errorf("unsupported characters in the NVRAM path %s", m.md.NVRAM)
	}
	f := wd.file(backupNVRAMFile)
	if err := r.src.Copy(m.md.NVRAM, f); err != nil {
		return "", fmt.Errorf("saving the UEFI variables %s: %w", m.md.NVRAM, err)
	}
	if m.md.NVRAMFormat == "qcow2" {
		if err := m.checkQcow2NVRAM(f); err != nil {
			_ = r.src.Remove(f)
			if err2 := r.src.Copy(m.md.NVRAM, f); err2 != nil {
				return "", fmt.Errorf("saving the UEFI variables %s: %w", m.md.NVRAM, err2)
			}
			if err := m.checkQcow2NVRAM(f); err != nil {
				return "", err
			}
		}
	}
	return backupNVRAMFile, nil
}

// send copies the staged files into the replica directory under names no point uses, and checks them.
func (r *replRun) send(wd *bkWork, seq int, nvram string, total uint64) error {
	m := r.bk
	if total == 0 {
		total = 1
	}
	var done uint64
	for _, d := range m.disks {
		if err := m.checkCancel(); err != nil {
			return err
		}
		base := done
		dst := r.st.file(replIncomingName(seq, d.Target))
		if err := copyToNode(m.ctx, r.src, r.dst, r.ep, wd.file(d.File), dst, "600", func(sent uint64) {
			r.spread(0.5, 0.9, float64(base+sent)/float64(total))
		}); err != nil {
			if m.cancelled.Load() {
				return errBackupCancelled
			}
			if free, ferr := bkFreeBytes(r.dst, r.pd.Dir); ferr == nil && free < 64<<20 {
				return fmt.Errorf("%s ran out of space in %s while the copy was being written: %s are left. The earlier copy is kept as it is; free some space there or choose a bigger pool", r.dst.Name, r.pd.Dir, fmtBytes(uint64(free)))
			}
			return fmt.Errorf("the copy of %s to %s was interrupted (the connection or the node command on %s stopped): %w. The earlier copy is kept as it is; the next run sends it again", d.File, r.dst.Name, r.dst.Name, err)
		}
		done += d.Size
		if d.SHA != "" {
			sum, err := r.dst.SHA256(dst)
			if err != nil {
				return err
			}
			if sum != d.SHA {
				return fmt.Errorf("%s arrived damaged on %s (SHA-256 %s, sent %s); nothing was changed there", d.File, r.dst.Name, sum, d.SHA)
			}
		}
	}
	if nvram != "" {
		if err := copyToNode(m.ctx, r.src, r.dst, r.ep, wd.file(nvram), r.st.file(replNVRAMName(seq)), "600", nil); err != nil {
			return fmt.Errorf("sending the UEFI variables to %s failed: %w", r.dst.Name, err)
		}
	}
	return nil
}

// addPoint links the received files into the chain and writes the manifest.
func (r *replRun) addPoint(latest *replPoint, seq, gen int, kind string, stamp time.Time, hasNVRAM bool, stamps []string) (*replPoint, error) {
	m := r.bk
	x, vols, err := r.definition()
	if err != nil {
		return nil, err
	}
	p := &replPoint{Format: replFormat, Seq: seq, Gen: gen, Time: stamp.Format(time.RFC3339), Kind: kind, Checkpoint: m.ckName,
		Offline: !r.live, VM: r.name, UUID: r.uuid, SourceNode: r.src.Name, SourceID: r.srcID, Interval: r.cfg.EveryMinutes,
		Pool: r.cfg.Pool, DomainXML: x, TPM: m.md.TPM, Sent: r.Sent, Stamps: stamps, Created: time.Now().UTC().Format(time.RFC3339)}
	if as, err := r.src.DomainGetAutostart(r.dom); err == nil {
		p.Autostart = as != 0
	}
	if hasNVRAM {
		p.NVRAM = replNVRAMName(seq)
		p.NVRAMPath = m.md.NVRAM
	}
	for _, d := range m.disks {
		in := replIncomingName(seq, d.Target)
		top := replOverlayName(seq, d.Target)
		switch kind {
		case kindIncremental:
			prev, ok := latest.diskOf(d.Target)
			if !ok {
				return nil, fmt.Errorf("the last copy has no disk %s", d.Target)
			}
			if err := r.dst.ReplRebase(r.st.dir, in, prev.File); err != nil {
				return nil, err
			}
			if err := r.dst.Move(r.st.file(in), r.st.file(top)); err != nil {
				return nil, err
			}
		default:
			base := replBaseName(gen, d.Target)
			if err := r.dst.Move(r.st.file(in), r.st.file(base)); err != nil {
				return nil, err
			}
			if err := r.dst.ReplOverlay(r.st.dir, top, base); err != nil {
				return nil, err
			}
		}
		p.Disks = append(p.Disks, replDisk{Target: d.Target, File: top, Bytes: d.Size, SHA256: d.SHA, Virt: d.Virt, Source: d.Base, Vol: vols[d.Target]})
	}
	if err := r.st.writePoint(p); err != nil {
		return nil, err
	}
	return p, nil
}

// cleanIncoming removes what a run that failed left in the replica directory.
func (r *replRun) cleanIncoming(seq int) {
	if r.st.h == nil {
		return
	}
	for _, d := range r.bk.disks {
		_ = r.dst.Remove(r.st.file(replIncomingName(seq, d.Target)), r.st.file(replOverlayName(seq, d.Target)))
	}
	_ = r.dst.Remove(r.st.file(replNVRAMName(seq)))
}

// afterPoint updates the standby's domain to the definition of the newest point.
func (r *replRun) afterPoint(p *replPoint) error {
	if r.cfg.ReplaceStale {
		if err := r.swapStale(); err != nil {
			return err
		}
	}
	return r.defineReplica(p)
}

// swapStale removes the old copy of this VM on the standby (the one a failover left behind) once the new
// replica is complete, so that the replica can take its place.
func (r *replRun) swapStale() error {
	d, ok := r.standbyDomain()
	if !ok {
		r.cfg.ReplaceStale = false
		return nil
	}
	meta, _ := domainMeta(r.dst, d)
	if parseStale(meta) == nil {
		if parseReplicaMeta(meta) != nil {
			r.cfg.ReplaceStale = false
			return nil
		}
		return fmt.Errorf("the VM %s on %s is not marked as an old copy; it was not removed", d.Name, r.dst.Name)
	}
	if st, _, err := r.dst.DomainGetState(d, 0); err != nil || stateName(uint8(st)) != "shutoff" {
		return fmt.Errorf("the old copy %s on %s is not shut off; it was not removed", d.Name, r.dst.Name)
	}
	if err := removeVMAndDisks(r.dst, d); err != nil {
		return err
	}
	r.cfg.ReplaceStale = false
	return nil
}

// dropOldCheckpoints deletes the replication checkpoints the new point has replaced, newest first.
func (r *replRun) dropOldCheckpoints() error {
	cks, err := listCheckpoints(r.src, r.dom)
	if err != nil {
		return err
	}
	for _, n := range staleReplCheckpoints(cks, replTarget(r.dstID), r.bk.ckName) {
		if err := deleteCheckpoint(r.src, r.dom, n); err != nil {
			log.Printf("replication %s: removing checkpoint %s: %v", r.name, n, err)
		}
	}
	return nil
}

// finishStatus records the run on the VM.
func (r *replRun) finishStatus(p *replPoint, _ error) error {
	return r.setSourceConfig(func(c *replConfig) {
		c.LastOK = p.timeOf().Unix()
		if !r.Skipped {
			c.LastBytes, c.LastKind, c.LastSecs = r.Sent, r.Kind, int(r.Took.Seconds())
		}
	})
}

// --- starting runs ---

func replTarget2(vm, node string) string { return vm + " → " + node }

// launchReplica claims the VM and runs the plan in the background as a task; the channel gets its result.
func (s *Server) launchReplica(req *http.Request, r *replRun) (int64, <-chan error, error) {
	if err := claimRepl(r); err != nil {
		return 0, nil, err
	}
	id, done, err := s.startBackgroundWait(req, "replicate", replTarget2(r.name, r.dst.Name), r.src.Name, func(progress func(float64)) error {
		defer releaseRepl(r)
		r.progress = progress
		err := r.run()
		setReplError(r.uuid, err)
		return err
	})
	if err != nil {
		releaseRepl(r)
	}
	return id, done, err
}
