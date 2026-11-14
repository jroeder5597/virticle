package api

// Replication of a VM to a standby node: the pure decisions.
//
// What the standby holds. For every replicated VM one hidden work directory
// (.virticle-rp.XXXXXX) in a pool directory of the standby node, the replica
// directory. For every disk it holds
//
//	b<G>-<disk>.qcow2    the base image of generation G (a full copy)
//	p<S>-<disk>.qcow2    the overlay of point S: only the blocks that changed since
//	                     the point before (S-1), its backing file is that point's
//	                     overlay, or the base for the first point of a generation
//	p<S>.json            the manifest of point S, written last and never changed
//	n<S>.nvram           the UEFI variables as they were at point S, if the VM has any
//
// A point is complete when its p<S>.json exists. Everything else is either
// part of the chain of a complete point or leftover that may be deleted. The
// rules that keep the only copy safe:
//
//  1. Nothing of a complete point is touched until its successor is complete.
//  2. A point's files are written under names no point uses yet (in<S>-...),
//     checked, linked to the point before it with a header-only rebase, renamed,
//     and only then does the manifest appear.
//  3. Merging the oldest point into the base removes its manifest first, so the
//     point can no longer be picked, then writes its overlay into the base
//     (cutting that short leaves the chain above reading the same data) and
//     only then rebases the next overlay and deletes the merged one. Each step
//     can run again.
//  4. A full copy is a new generation. The points of older generations are
//     dropped (manifests first) only after the first point of the new one is
//     complete.
//
// Nothing here talks to a node.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	replFormat = 1
	// replKeep is the number of points a replica keeps: the newest, and one before it, so a
	// failover can use the older one when the newest turns out damaged.
	replKeep = 2
	// replMaxInterval bounds "every N minutes" (a week).
	replMinInterval = 5
	replMaxInterval = 7 * 24 * 60
	// replTargetPrefix starts the target of the checkpoints replication makes (their description),
	// so backups and replication each keep and drop only their own.
	replTargetPrefix = "replica:"
)

// Why a replication run copies everything, in words for the status.
const (
	whyReplFirst    = "The first copy of this VM to the standby node."
	whyReplShutOff  = "The VM is shut off and libvirt cannot track changes for it now, so the disks were copied whole."
	whyReplNoBase   = "The changes since the last copy could not be tracked (the VM was moved, had a snapshot reverted or stopped unexpectedly), so the disks were copied whole."
	whyReplLayout   = "The disks changed since the last copy (added, removed, resized or moved), so the disks were copied whole."
	whyReplDamaged  = "The copy on the standby node was missing or damaged, so the disks were copied whole."
	whyReplRefused  = "libvirt could not build on the earlier copy (the VM may have stopped unexpectedly), so the disks were copied whole."
	whyReplNoCkpt   = "libvirt could not start tracking changes, so the disks were copied whole."
	whyReplOldVirt  = "This node's libvirt is too old to track changes, so the disks were copied whole."
	whyReplFormat   = "A disk is not in qcow2 format and cannot track changes, so the disks were copied whole."
	whyReplGenAgain = "The standby holds no usable copy, so the disks were copied whole."
)

// replTarget is the checkpoint target of replication to node id.
func replTarget(nodeID int64) string { return replTargetPrefix + strconv.FormatInt(nodeID, 10) }

func isReplTarget(t string) bool { return strings.HasPrefix(t, replTargetPrefix) }

// replDisk is one disk of a point.
type replDisk struct {
	Target string `json:"target"`
	File   string `json:"file"`   // the overlay of this point
	Bytes  uint64 `json:"bytes"`  // size of the file
	SHA256 string `json:"sha256"` // of the file as it was sent
	Virt   uint64 `json:"virtualSize"`
	Source string `json:"source"` // base image on the source node, with Virt the layout of the disk
	Vol    string `json:"vol"`    // volume file name the disk gets when the replica is started
}

// replPoint is the manifest of one point.
type replPoint struct {
	Format     int        `json:"format"`
	Seq        int        `json:"seq"`
	Gen        int        `json:"gen"`
	Time       string     `json:"time"` // RFC3339 UTC: the moment the copy is a copy of
	Kind       string     `json:"kind"` // full | incremental
	Checkpoint string     `json:"checkpoint,omitempty"`
	Offline    bool       `json:"offline,omitempty"` // taken while the VM was shut off
	VM         string     `json:"vm"`
	UUID       string     `json:"uuid"`
	SourceNode string     `json:"sourceNode"`
	SourceID   int64      `json:"sourceId"`
	Interval   int        `json:"everyMinutes"`
	Pool       string     `json:"pool"`
	Disks      []replDisk `json:"disks"`
	DomainXML  string     `json:"domainXml"`           // the definition for the standby, disks at the paths they get when started
	NVRAM      string     `json:"nvram,omitempty"`     // file in the replica directory
	NVRAMPath  string     `json:"nvramPath,omitempty"` // where it goes on failover
	TPM        bool       `json:"tpm,omitempty"`
	TPMState   string     `json:"tpmState,omitempty"` // the emulated TPM's state, a file in the replica directory
	Autostart  bool       `json:"autostart,omitempty"`
	Sent       uint64     `json:"sent"`             // bytes sent for this point
	Stamps     []string   `json:"stamps,omitempty"` // "mtime|size" of the source's disk files after an offline run
	Created    string     `json:"created"`
}

func (p *replPoint) timeOf() time.Time {
	t, _ := time.Parse(time.RFC3339, p.Time)
	return t
}

func (p *replPoint) diskOf(target string) (replDisk, bool) {
	for _, d := range p.Disks {
		if d.Target == target {
			return d, true
		}
	}
	return replDisk{}, false
}

func (p *replPoint) layout() []diskLayout {
	var out []diskLayout
	for _, d := range p.Disks {
		out = append(out, diskLayout{Target: d.Target, Source: d.Source, Virt: d.Virt})
	}
	return out
}

// files lists the names of the files this point has of its own: the overlays and the manifest.
func (p *replPoint) files() []string {
	var out []string
	for _, d := range p.Disks {
		out = append(out, d.File)
	}
	out = append(out, replManifestName(p.Seq))
	if p.NVRAM != "" {
		out = append(out, p.NVRAM)
	}
	if p.TPMState != "" {
		out = append(out, p.TPMState)
	}
	return out
}

func parseReplPoint(b []byte) (*replPoint, error) {
	var p replPoint
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	if p.Format != replFormat {
		return nil, fmt.Errorf("unsupported replica format %d", p.Format)
	}
	if p.Seq < 1 || p.Gen < 1 || len(p.Disks) == 0 || p.timeOf().IsZero() {
		return nil, fmt.Errorf("incomplete point manifest")
	}
	for _, d := range p.Disks {
		if !replFileRE.MatchString(d.File) || !replFileRE.MatchString(d.Vol) {
			return nil, fmt.Errorf("unsupported file name in the point manifest")
		}
	}
	return &p, nil
}

// replFileRE is what a name in a replica directory looks like (plain, no dot first).
var replFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,119}$`)

var (
	rePointJSON = regexp.MustCompile(`^p([0-9]+)\.json$`)
	rePointFile = regexp.MustCompile(`^p([0-9]+)-.+\.qcow2$`)
	reBaseFile  = regexp.MustCompile(`^b([0-9]+)-.+\.qcow2$`)
	reNVRAMFile = regexp.MustCompile(`^n([0-9]+)\.nvram$`)
	reTPMFile   = regexp.MustCompile(`^t([0-9]+)\.tpm$`)
	reInFile    = regexp.MustCompile(`^in([0-9]+)-.+$`)
)

func replManifestName(seq int) string           { return fmt.Sprintf("p%d.json", seq) }
func replOverlayName(seq int, t string) string  { return fmt.Sprintf("p%d-%s.qcow2", seq, t) }
func replBaseName(gen int, t string) string     { return fmt.Sprintf("b%d-%s.qcow2", gen, t) }
func replIncomingName(seq int, t string) string { return fmt.Sprintf("in%d-%s.qcow2", seq, t) }
func replNVRAMName(seq int) string              { return fmt.Sprintf("n%d.nvram", seq) }
func replTPMName(seq int) string                { return fmt.Sprintf("t%d.tpm", seq) }

// replVolName is the file name disk number i (0 first) of VM name gets when the replica is started:
// the names a restore uses.
func replVolName(vm, target string, i int) string {
	if i == 0 {
		return vm + ".qcow2"
	}
	return vm + "-" + target + ".qcow2"
}

// replInput is everything decideReplica looks at.
type replInput struct {
	Backupable bool // the VM runs or can be started paused, so libvirt can run a backup job
	AllQcow2   bool
	Supported  bool         // libvirt on the source can do checkpoints
	Layout     []diskLayout // the disks as they are now
	// Checkpoints are the names of this program's checkpoints that libvirt holds for the VM.
	Checkpoints map[string]bool
	// Latest is the newest usable point on the standby, nil when there is none.
	Latest *replPoint
	// Intact tells whether the files of Latest are all there.
	Intact bool
}

// replPlan is the outcome.
type replPlan struct {
	Kind             string
	ParentCheckpoint string
	Checkpoint       bool // create a checkpoint with this run
	Reason           string
}

// decideReplica chooses between an incremental and a full copy. Every doubt means full: a full copy
// is always right, an incremental one only when everything it relies on is intact.
func decideReplica(in replInput) replPlan {
	full := func(why string, ck bool) replPlan { return replPlan{Kind: kindFull, Reason: why, Checkpoint: ck} }
	switch {
	case !in.Backupable:
		return full(whyReplShutOff, false)
	case !in.AllQcow2:
		return full(whyReplFormat, false)
	case !in.Supported:
		return full(whyReplOldVirt, false)
	case in.Latest == nil:
		return full(whyReplFirst, true)
	case !in.Intact:
		return full(whyReplDamaged, true)
	case in.Latest.Checkpoint == "" || !in.Checkpoints[in.Latest.Checkpoint]:
		return full(whyReplNoBase, true)
	case !sameLayout(in.Latest.layout(), in.Layout):
		return full(whyReplLayout, true)
	}
	return replPlan{Kind: kindIncremental, ParentCheckpoint: in.Latest.Checkpoint, Checkpoint: true}
}

// --- what a directory listing means ---

// replScan is what the file names of a replica directory say.
type replScan struct {
	Points []int // sequence numbers that have a manifest, ascending
	// Strays are files that no point can need: incoming files, files of points without a manifest.
	// Files of older points and bases that a point's chain still runs through are not in it: the
	// caller decides that from the chains (see keepSet).
	Maybe []string // every file that is not a manifest, for the caller to sort by chain
}

// scanReplNames reads a directory listing.
func scanReplNames(names []string) replScan {
	var s replScan
	for _, n := range names {
		if m := rePointJSON.FindStringSubmatch(n); m != nil {
			v, _ := strconv.Atoi(m[1])
			s.Points = append(s.Points, v)
			continue
		}
		s.Maybe = append(s.Maybe, n)
	}
	sort.Ints(s.Points)
	return s
}

// nextSeq is the sequence number of the next point: above every file name that carries one, so a
// name left by an interrupted run is never used twice.
func nextSeq(names []string) int {
	top := 0
	for _, n := range names {
		for _, re := range []*regexp.Regexp{rePointJSON, rePointFile, reNVRAMFile, reTPMFile, reInFile} {
			if m := re.FindStringSubmatch(n); m != nil {
				if v, _ := strconv.Atoi(m[1]); v > top {
					top = v
				}
			}
		}
	}
	return top + 1
}

// nextGen is the generation number of the next full copy.
func nextGen(names []string) int {
	top := 0
	for _, n := range names {
		if m := reBaseFile.FindStringSubmatch(n); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > top {
				top = v
			}
		}
	}
	return top + 1
}

// livePoints picks, of the points with a manifest, the ones that stay: those of the newest generation
// that has any, newest first, at most keep. The rest are to be dropped.
func livePoints(points []*replPoint, keep int) (kept, dropped []*replPoint) {
	if len(points) == 0 {
		return nil, nil
	}
	sorted := append([]*replPoint{}, points...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq > sorted[j].Seq })
	gen := sorted[0].Gen
	for _, p := range sorted {
		if p.Gen == gen && len(kept) < keep {
			kept = append(kept, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	return kept, dropped // dropped: newest first
}

// dropOrder is the order in which dropped points lose their manifests: oldest first, so the
// points that stay never depend on a point that is gone... the chain of a kept point only runs
// through older overlays, which are deleted after (see keepSet), never before, their manifests.
func dropOrder(dropped []*replPoint) []*replPoint {
	out := append([]*replPoint{}, dropped...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// keepSet is the set of file names to keep: the files of the kept points' manifests plus every file
// on their chains. chains maps a point's overlay file to the names of the files below it
// (oldest last), as the images' headers say.
func keepSet(kept []*replPoint, chains map[string][]string) map[string]bool {
	keep := map[string]bool{}
	for _, p := range kept {
		for _, f := range p.files() {
			keep[f] = true
		}
		for _, d := range p.Disks {
			for _, f := range chains[d.File] {
				keep[f] = true
			}
		}
	}
	return keep
}

// strays lists the files of names that are not in keep, oldest sequence first, so a cleanup that
// stops half way leaves the newest files.
func strays(names []string, keep map[string]bool) []string {
	var out []string
	for _, n := range names {
		if !keep[n] {
			out = append(out, n)
		}
	}
	seq := func(n string) int {
		for _, re := range []*regexp.Regexp{rePointJSON, rePointFile, reNVRAMFile, reTPMFile, reInFile, reBaseFile} {
			if m := re.FindStringSubmatch(n); m != nil {
				v, _ := strconv.Atoi(m[1])
				return v
			}
		}
		return 0
	}
	sort.SliceStable(out, func(i, j int) bool { return seq(out[i]) < seq(out[j]) })
	return out
}

// mergeStep is one disk of a merge of an older overlay into its base.
type mergeStep struct {
	Disk string
	Old  string // overlay to write into the base and delete
	Base string
	Next string // overlay that sat on Old and is rebased onto the base ("" when none)
}

// planMerge says what merging chain (oldest first: base, overlay, overlay, ...) down to keep overlays
// takes: nothing when the chain is short enough, else the steps for the overlay right above the base.
// It is called again until it returns nothing.
func planMerge(disk string, chain []string, keepOverlays int) *mergeStep {
	if len(chain)-1 <= keepOverlays || len(chain) < 3 {
		return nil
	}
	return &mergeStep{Disk: disk, Old: chain[1], Base: chain[0], Next: chain[2]}
}

// --- checkpoints shared with backups ---

// staleReplCheckpoints returns the replication checkpoints for target to delete once a run has
// produced newName: every other one of the same target, newest first. It never touches the
// checkpoints of backups.
func staleReplCheckpoints(all []ckInfo, target, newName string) []string {
	var mine []ckInfo
	for _, c := range all {
		if c.Name != newName && c.Target == target {
			mine = append(mine, c)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Created > mine[j].Created })
	out := make([]string, len(mine))
	for i, c := range mine {
		out[i] = c.Name
	}
	return out
}

// replIntervalOK checks "every N minutes".
func replIntervalOK(n int) error {
	if n < replMinInterval || n > replMaxInterval {
		return fmt.Errorf("the interval must be between %d minutes and 7 days", replMinInterval)
	}
	return nil
}

// ageText is a short age for messages.
func ageText(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return "a minute"
	case d < 90*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Round(time.Minute)/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Round(time.Hour)/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d.Round(24*time.Hour)/(24*time.Hour)))
}
