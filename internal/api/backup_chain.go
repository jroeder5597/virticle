package api

// Incremental backups: the pure decisions.
//
// A backup is "full" (every disk copied whole) or "incremental" (only the
// blocks that changed since an earlier backup of the same VM in the same pool,
// its parent). QEMU keeps track of changed blocks in dirty bitmaps that libvirt
// calls checkpoints. Each backup of a running VM creates a checkpoint, and an
// incremental backup asks libvirt for "everything changed since the checkpoint
// of the parent".
//
// Nothing here talks to a node. The functions take what was found and say what
// to do, so every fallback decision is a table test:
//
//   - decideBackup: incremental on top of which backup, or full, and why.
//   - chainOf and chainProblem: the backups a restore of one backup needs.
//   - pruneChains: retention that never deletes a backup a kept one needs.
//   - dependentsOf: what a manual delete takes with it.
//   - checkpointXML, backupXMLFor: the libvirt documents.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	kindFull        = "full"
	kindIncremental = "incremental"

	// backupFormatIncremental marks the manifest of an incremental backup. An
	// older Virticle refuses it ("unsupported backup format 2") instead of
	// restoring a file that holds only part of the disk; a full backup keeps
	// format 1, so older versions still read it.
	backupFormatIncremental = 2

	// chainBackups is the number of backups in one chain: a full backup and up
	// to six incremental ones, so a daily schedule makes a full copy weekly.
	// Short chains mean a restore reads at most seven files per disk, one bad
	// file ruins at most six later backups, and the next full copy replaces
	// whatever silent drift an incremental chain could have collected.
	chainBackups = 7

	// maxChainWalk bounds how far a chain is followed (loops, hostile manifests).
	maxChainWalk = 64

	// checkpointPrefix marks the checkpoints this program made; others are never touched.
	checkpointPrefix = "virticle-"
	// checkpointsKept bounds the checkpoints one VM holds (each costs a bitmap in
	// every disk and some work on every write): one per target, the newest ones.
	checkpointsKept = 4
)

// Why a backup is full, in words for the backup list.
const (
	whyShutOff      = "The VM was shut off, so its disks were copied whole."
	whyFormat       = "A disk is not in qcow2 format and cannot track changes, so the disks were copied whole."
	whyOldVirt      = "This node's libvirt is too old to track changes, so the disks were copied whole."
	whyFirst        = "The first backup of this VM to this pool."
	whyNoBase       = "No earlier backup could be built on (the VM was moved, restored, had a snapshot reverted or stopped unexpectedly)."
	whyLayout       = "The disks changed since the last backup (added, removed, resized or moved)."
	whyDamaged      = "An earlier backup this one would build on is missing or damaged."
	whyRotation     = "A new full copy is made after every 7 backups, which keeps restores short."
	whyNoCheckpoint = "libvirt could not start tracking changes, so the disks were copied whole."
	whyRefused      = "libvirt could not build on the earlier backup (the VM may have stopped unexpectedly), so the disks were copied whole."
)

// diskLayout is what must be unchanged for an incremental backup to make sense.
type diskLayout struct {
	Target string
	Source string // the base image of the disk's backing chain (snapshot overlays on top do not count)
	Virt   uint64
}

func sameLayout(a, b []diskLayout) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]diskLayout{}
	for _, d := range a {
		m[d.Target] = d
	}
	for _, d := range b {
		if m[d.Target] != d {
			return false
		}
	}
	return true
}

// chainBackup is one backup of the VM in the target pool, as the scan found it.
type chainBackup struct {
	Dir string
	M   *backupManifest
}

// kindOf is the kind of a manifest; backups from before incremental backups have none and are full.
func (m *backupManifest) kindOf() string {
	if m.Kind == kindIncremental {
		return kindIncremental
	}
	return kindFull
}

// layout is the disk layout a backup recorded. A backup from before the layout
// was recorded has none (nil).
func (m *backupManifest) layout() []diskLayout {
	var out []diskLayout
	for _, d := range m.Disks {
		if d.Source == "" {
			return nil
		}
		out = append(out, diskLayout{Target: d.Target, Source: d.Source, Virt: d.VirtualSize})
	}
	return out
}

// incrInput is everything decideBackup looks at.
type incrInput struct {
	Live      bool         // the VM runs, so a backup job (and a checkpoint) is possible
	AllQcow2  bool         // every disk that is backed up has a qcow2 image on top
	Supported bool         // libvirt on the node can do checkpoints
	Layout    []diskLayout // the disks as they are now
	Target    string       // identifies node and pool, see targetKey
	// Checkpoints are the names of this program's checkpoints that libvirt holds for the VM.
	Checkpoints map[string]bool
	// Backups are the readable backups of this VM in the target pool.
	Backups []chainBackup
	// Intact tells whether a backup's files are all there with the sizes its manifest records.
	Intact func(dir string) bool
}

// backupPlan is the outcome.
type backupPlan struct {
	Kind             string
	Parent           string // directory of the backup to build on (incremental)
	ParentCheckpoint string
	Checkpoint       bool   // create a checkpoint with this backup
	Reason           string // why a full backup is full (empty for the first reasons that need no words)
}

// decideBackup is the single place that chooses between an incremental and a
// full backup. Every doubt means full: a full backup is always right, an
// incremental one only when everything it relies on is intact.
func decideBackup(in incrInput) backupPlan {
	full := func(why string, ck bool) backupPlan {
		return backupPlan{Kind: kindFull, Reason: why, Checkpoint: ck}
	}
	switch {
	case !in.Live:
		return full(whyShutOff, false)
	case !in.AllQcow2:
		return full(whyFormat, false)
	case !in.Supported:
		return full(whyOldVirt, false)
	}
	// The newest backup in the target that still has its checkpoint.
	var base *chainBackup
	for i := range in.Backups {
		b := &in.Backups[i]
		if b.M.Checkpoint == "" || !in.Checkpoints[b.M.Checkpoint] {
			continue
		}
		if base == nil || b.M.createdTime().After(base.M.createdTime()) {
			base = b
		}
	}
	if base == nil {
		if len(in.Backups) == 0 {
			return full(whyFirst, true)
		}
		return full(whyNoBase, true)
	}
	if !sameLayout(base.M.layout(), in.Layout) {
		return full(whyLayout, true)
	}
	by := map[string]*backupManifest{}
	for _, b := range in.Backups {
		by[b.Dir] = b.M
	}
	chain, err := chainOf(by, base.Dir)
	if err != nil {
		return full(whyDamaged, true)
	}
	for _, d := range chain {
		if in.Intact != nil && !in.Intact(d) {
			return full(whyDamaged, true)
		}
	}
	if len(chain) >= chainBackups {
		return full(whyRotation, true)
	}
	return backupPlan{Kind: kindIncremental, Parent: base.Dir, ParentCheckpoint: base.M.Checkpoint, Checkpoint: true}
}

// chainOf lists the backups a restore of dir needs, the full backup first and
// dir last. It fails when a link is missing, belongs to another VM, is not
// older, or the chain does not end in a full backup.
func chainOf(by map[string]*backupManifest, dir string) ([]string, error) {
	var rev []string
	seen := map[string]bool{}
	cur := dir
	for {
		m, ok := by[cur]
		if !ok {
			if cur == dir {
				return nil, errors.New("not a backup")
			}
			return nil, fmt.Errorf("the earlier backup %s is missing", cur)
		}
		if seen[cur] || len(rev) >= maxChainWalk {
			return nil, errors.New("the chain of backups loops or is too long")
		}
		seen[cur] = true
		rev = append(rev, cur)
		if m.kindOf() == kindFull {
			break
		}
		p, ok := by[m.Parent]
		if m.Parent == "" {
			return nil, fmt.Errorf("the incremental backup %s names no earlier backup", cur)
		}
		if ok && (p.UUID != m.UUID || !p.createdTime().Before(m.createdTime())) {
			return nil, fmt.Errorf("the earlier backup %s does not belong to this chain", m.Parent)
		}
		cur = m.Parent
	}
	out := make([]string, len(rev))
	for i, d := range rev {
		out[len(rev)-1-i] = d
	}
	return out, nil
}

// chainProblem is the words for the list when a backup cannot be restored
// because of its chain (empty when it can).
func chainProblem(by map[string]*backupManifest, dir string) string {
	if _, err := chainOf(by, dir); err != nil {
		return err.Error()
	}
	return ""
}

// pruneChains returns the backups of VM uuid to delete so that the newest keep
// remain, newest first so that a failure half way never leaves a backup whose
// parent is already gone. A backup that a kept one builds on is kept as well,
// whatever its age: deleting it would make the kept one useless. keep < 1
// prunes nothing. just (the backup written a moment ago) is never deleted.
func pruneChains(all []retained, uuid string, keep int, just string) []string {
	if keep < 1 {
		return nil
	}
	var mine []retained
	by := map[string]retained{}
	for _, b := range all {
		if b.UUID == uuid {
			mine = append(mine, b)
			by[b.Key] = b
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Created.After(mine[j].Created) })
	if len(mine) <= keep {
		return nil
	}
	need := map[string]bool{}
	mark := func(k string) {
		for n := 0; k != "" && n < maxChainWalk; n++ {
			if need[k] {
				return
			}
			b, ok := by[k]
			if !ok {
				return
			}
			need[k] = true
			k = b.Parent
		}
	}
	for _, b := range mine[:keep] {
		mark(b.Key)
	}
	mark(just)
	var out []string
	for _, b := range mine[keep:] { // newest first
		if !need[b.Key] {
			out = append(out, b.Key)
		}
	}
	return out
}

// dependentsOf lists the backups that need dir, directly or not, newest first.
func dependentsOf(all []retained, dir string) []string {
	kids := map[string][]retained{}
	for _, b := range all {
		if b.Parent != "" {
			kids[b.Parent] = append(kids[b.Parent], b)
		}
	}
	var out []retained
	seen := map[string]bool{dir: true}
	queue := []string{dir}
	for len(queue) > 0 && len(out) < 10000 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			if !seen[k.Key] {
				seen[k.Key] = true
				out = append(out, k)
				queue = append(queue, k.Key)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	names := make([]string, len(out))
	for i, b := range out {
		names[i] = b.Key
	}
	return names
}

// --- checkpoints ---

// targetKey identifies where backups go: a checkpoint remembers the target it
// belongs to, so a one-off backup to another pool does not end the chain of the
// scheduled one.
func targetKey(nodeName, pool string) string { return nodeName + ":" + pool }

// newCheckpointName is a name no earlier checkpoint has: the time, to the second, and a random part.
func newCheckpointName(t time.Time, rnd string) string {
	return checkpointPrefix + t.UTC().Format(backupTimeLayout) + "-" + rnd
}

// checkpointXML is the <domaincheckpoint> for a new checkpoint. Disks that are
// not backed up (read-only, CD-ROMs) get no bitmap.
func checkpointXML(name, target string, backedUp, skipped []string) string {
	var b strings.Builder
	b.WriteString("<domaincheckpoint><name>" + esc(name) + "</name><description>" + esc(target) + "</description><disks>")
	for _, d := range backedUp {
		fmt.Fprintf(&b, "<disk name='%s' checkpoint='bitmap'/>", esc(d))
	}
	for _, d := range skipped {
		fmt.Fprintf(&b, "<disk name='%s' checkpoint='no'/>", esc(d))
	}
	b.WriteString("</disks></domaincheckpoint>")
	return b.String()
}

// oursOnly keeps the names of this program's checkpoints.
func oursOnly(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, checkpointPrefix) {
			out = append(out, n)
		}
	}
	return out
}

// ckInfo is a checkpoint as the cleanup sees it.
type ckInfo struct {
	Name    string
	Target  string
	Created int64
}

// staleCheckpoints returns the checkpoints to delete after a successful backup
// for target that created newName: older ones of the same target (their
// backups can no longer be built on, the new one can) and, beyond
// checkpointsKept, the oldest of all. Newest first.
func staleCheckpoints(all []ckInfo, target, newName string) []string {
	var mine []ckInfo
	for _, c := range all {
		if strings.HasPrefix(c.Name, checkpointPrefix) {
			mine = append(mine, c)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Created > mine[j].Created })
	drop := map[string]bool{}
	var keep []ckInfo
	for _, c := range mine {
		if c.Name != newName && c.Target == target {
			drop[c.Name] = true
			continue
		}
		keep = append(keep, c)
	}
	for i := checkpointsKept; i < len(keep); i++ {
		if keep[i].Name != newName {
			drop[keep[i].Name] = true
		}
	}
	var out []string
	for _, c := range mine {
		if drop[c.Name] {
			out = append(out, c.Name)
		}
	}
	return out
}
