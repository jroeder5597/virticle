package api

// The standby side of replication: a replica directory and what is done in it
// (the rules are in replica_points.go). Every method works through the node
// command's verbs on plain file names in that directory, so it behaves the same
// for root and service-account nodes, and every one of them can be run again
// after it was cut short.

import (
	"encoding/json"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"

	"virticle/internal/hostconn"
)

const replManifestMax = 1 << 20

type replStore struct {
	h   *hostconn.Host
	dir string
}

func (st replStore) file(n string) string { return st.dir + "/" + n }

func (st replStore) list() (map[string]uint64, error) { return st.h.ReplList(st.dir) }

func sortedNames(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// points reads the manifests of the complete points, oldest first. A manifest that cannot be read is
// not a point (it is left in place, the next cleanup treats its files as strays only when the
// listing shows a newer point; never in the same call).
func (st replStore) points(names map[string]uint64) ([]*replPoint, []string) {
	var out []*replPoint
	var bad []string
	for _, seq := range scanReplNames(sortedNames(names)).Points {
		n := replManifestName(seq)
		b, err := st.h.ReadLimited(st.file(n), replManifestMax)
		if err != nil {
			bad = append(bad, n+": "+err.Error())
			continue
		}
		p, err := parseReplPoint(b)
		if err != nil || p.Seq != seq {
			bad = append(bad, n+": not a usable manifest")
			continue
		}
		out = append(out, p)
	}
	return out, bad
}

// chain returns the file names below (and including) top, as the images' headers say, top first. It
// fails when a header points outside the directory or at a file that is not there.
func (st replStore) chain(top string, names map[string]uint64) ([]string, error) {
	out, err := st.h.QemuInfo(st.file(top), true)
	if err != nil {
		return nil, fmt.Errorf("%s is not readable: %s", top, impFirstLine(out))
	}
	imgs, err := parseChain(out)
	if err != nil {
		return nil, err
	}
	var res []string
	for _, im := range imgs {
		if path.Dir(im.Path) != st.dir {
			return nil, fmt.Errorf("%s rests on %s, outside the replica", top, im.Path)
		}
		n := path.Base(im.Path)
		if _, ok := names[n]; !ok {
			return nil, fmt.Errorf("%s rests on %s, which is missing", top, n)
		}
		if im.Format != "qcow2" {
			return nil, fmt.Errorf("%s is not a qcow2 image", n)
		}
		res = append(res, n)
	}
	return res, nil
}

// usable says whether the point can be started from: every overlay is there and its chain reads.
func (st replStore) usable(p *replPoint, names map[string]uint64) (map[string][]string, error) {
	chains := map[string][]string{}
	for _, d := range p.Disks {
		if _, ok := names[d.File]; !ok {
			return nil, fmt.Errorf("%s is missing", d.File)
		}
		c, err := st.chain(d.File, names)
		if err != nil {
			return nil, err
		}
		chains[d.File] = c[1:]
	}
	return chains, nil
}

// settle brings the directory into order after anything that may have been cut short, and returns
// the points that are usable, newest first. It never deletes a file that a usable point needs.
//   - points beyond replKeep and points of older generations lose their manifests, oldest first;
//   - an overlay that sits between the base and the oldest kept overlay (a merge that stopped) is
//     written into the base and removed;
//   - files that belong to no kept point are deleted, oldest first.
func (st replStore) settle() ([]*replPoint, map[string]uint64, error) {
	names, err := st.list()
	if err != nil {
		return nil, nil, err
	}
	all, bad := st.points(names)
	for _, b := range bad {
		log.Printf("replica %s: %s", st.dir, b)
	}
	// Usable points only: a point whose chain does not read is no point. Newest first.
	var good []*replPoint
	for i := len(all) - 1; i >= 0; i-- {
		if _, err := st.usable(all[i], names); err != nil {
			log.Printf("replica %s: point %d is not usable: %v", st.dir, all[i].Seq, err)
			continue
		}
		good = append(good, all[i])
	}
	kept, dropped := livePoints(good, replKeep)
	// A point that is not usable has no business in the directory only once a usable newer one exists.
	unusable := map[int]bool{}
	for _, p := range all {
		unusable[p.Seq] = true
	}
	for _, p := range good {
		delete(unusable, p.Seq)
	}
	if len(kept) > 0 {
		for _, p := range all {
			if unusable[p.Seq] && p.Seq < kept[0].Seq {
				dropped = append(dropped, p)
			}
		}
	}
	for _, p := range dropOrder(dropped) {
		if err := st.h.Remove(st.file(replManifestName(p.Seq))); err != nil {
			return nil, nil, err
		}
		delete(names, replManifestName(p.Seq))
	}
	if len(kept) == 0 {
		return nil, names, nil
	}
	// Half done merges: an overlay between the base and the oldest kept overlay.
	oldest := kept[len(kept)-1]
	for {
		stepped := false
		for _, d := range oldest.Disks {
			c, err := st.chain(d.File, names)
			if err != nil {
				return nil, nil, err
			}
			// c is top first; make it base first.
			rev := make([]string, len(c))
			for i, n := range c {
				rev[len(c)-1-i] = n
			}
			if step := planMerge(d.Target, rev, 1); step != nil {
				if err := st.mergeStep(step, names); err != nil {
					return nil, nil, err
				}
				stepped = true
			}
		}
		if !stepped {
			break
		}
	}
	chains := map[string][]string{}
	for _, p := range kept {
		cs, err := st.usable(p, names)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range cs {
			chains[k] = v
		}
	}
	keep := keepSet(kept, chains)
	for _, p := range all {
		if unusable[p.Seq] && p.Seq > kept[0].Seq {
			// Newer than every usable point: the failure may be a passing one. Leave its files alone.
			for _, f := range p.files() {
				keep[f] = true
			}
		}
	}
	for _, n := range strays(sortedNames(names), keep) {
		if err := st.h.Remove(st.file(n)); err != nil {
			return nil, nil, err
		}
		delete(names, n)
	}
	return kept, names, nil
}

// mergeStep writes the overlay above the base into the base, rebases the overlay above it onto the base
// and deletes the written one. Each call may be repeated.
func (st replStore) mergeStep(s *mergeStep, names map[string]uint64) error {
	if _, ok := names[s.Old]; ok {
		if err := st.h.ReplCommit(st.dir, s.Old, ""); err != nil {
			return err
		}
	}
	if s.Next != "" {
		if err := st.h.ReplRebase(st.dir, s.Next, s.Base); err != nil {
			return err
		}
	}
	if _, ok := names[s.Old]; ok {
		if err := st.h.Remove(st.file(s.Old)); err != nil {
			return err
		}
		delete(names, s.Old)
	}
	return nil
}

// writePoint stores the manifest of a point. It is the moment the point exists.
func (st replStore) writePoint(p *replPoint) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > replManifestMax {
		return fmt.Errorf("the point manifest is unreasonably large")
	}
	return st.h.WriteFile(st.file(replManifestName(p.Seq)), b, replManifestMax, "600")
}

// diskBytes is the space the replica directory uses.
func (st replStore) diskBytes(names map[string]uint64) uint64 {
	var sum uint64
	for _, n := range sortedNames(names) {
		sum += names[n]
	}
	return sum
}

func isReplFile(n string) bool {
	return strings.HasSuffix(n, ".qcow2") || strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".nvram")
}

// replDamagedMarker is what the node command puts into the error of a commit whose base could not be made clean.
const replDamagedMarker = "replica-damaged"

func isReplicaDamaged(err error) bool {
	return err != nil && strings.Contains(err.Error(), replDamagedMarker)
}

// wipe deletes every file of the replica directory (the points, the images, the UEFI variables). The next run then
// starts as the first one: a full copy.
func (st replStore) wipe() error {
	names, err := st.list()
	if err != nil {
		return err
	}
	for _, n := range sortedNames(names) {
		if !isReplFile(n) {
			continue
		}
		if err := st.h.Remove(st.file(n)); err != nil {
			return err
		}
	}
	return nil
}
