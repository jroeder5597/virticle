package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
	"virticle/internal/snapsched"
)

// Scheduled snapshots keep no state of their own: the schedule lives in the
// domain's <metadata>, "last run" is the creation time of the newest scheduled
// snapshot libvirt holds, and pruning only touches snapshots that carry the
// scheduler's description marker and a "<prefix>-<timestamp>" name.

const (
	schedTick  = 20 * time.Second
	schedRetry = 5 * time.Minute // wait after a failed attempt (kept in memory only)
)

var schedOnce sync.Once

func (s *Server) startSnapshotScheduler() {
	if s.NoScheduler {
		return
	}
	schedOnce.Do(func() { go s.runSnapshotScheduler(context.Background()) })
}

func (s *Server) runSnapshotScheduler(ctx context.Context) {
	failed := map[string]time.Time{}
	t := time.NewTicker(schedTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scheduleTick(failed)
		}
	}
}

// schedRequest builds the request context s.task wants for work that no user
// started.
func schedRequest() *http.Request {
	r, _ := http.NewRequestWithContext(context.WithValue(context.Background(), ctxKey{}, "scheduler"), http.MethodPost, "/", nil)
	return r
}

func (s *Server) scheduleTick(failed map[string]time.Time) {
	conflicts := map[int64]*schedConflict{}
	scanned := map[int64]bool{}
	for _, n := range s.Nodes.Status() {
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		doms, _, err := h.ConnectListAllDomains(1, 0)
		if err != nil {
			continue
		}
		scanned[n.ID] = true
		for _, dom := range doms {
			meta, err := domainMeta(h, dom)
			if err != nil || meta == "" {
				continue
			}
			key := h.Name + "/" + dom.Name
			if parseReplicaMeta(meta) != nil || parseStale(meta) != nil {
				continue // the standby's inert copy and the old VM after a failover take no part in schedules
			}
			if rc := parseReplication(meta); rc != nil && rc.Enabled {
				s.runReplicaSchedule(n.ID, h, dom, *rc)
			}
			// An unparsable schedule is reported by the GET endpoint.
			if sch, err := snapsched.Parse(meta); err == nil && sch != nil && sch.Enabled {
				if other := s.runSchedule(h, dom, *sch, key, failed); other != "" {
					c := conflicts[n.ID]
					if c == nil {
						name := n.Hostname
						if name == "" {
							name = h.Name
						}
						c = &schedConflict{Node: n.ID, NodeName: name, Other: other}
						conflicts[n.ID] = c
					}
					c.VMs = append(c.VMs, dom.Name)
				}
			}
			if bs, err := parseBackupSchedule(meta); err == nil && bs != nil && bs.Enabled {
				s.runBackupSchedule(n.ID, h, dom, *bs, "bk:"+key)
			}
		}
	}
	s.setConflicts(conflicts, scanned)
}

// schedConflict records that another controller schedules snapshots on a node.
type schedConflict struct {
	Node     int64
	NodeName string
	Other    string // the other controller's instance id
	VMs      []string
}

// setConflicts replaces the recorded conflicts of the nodes this pass could
// read; those of unreachable nodes are kept.
func (s *Server) setConflicts(found map[int64]*schedConflict, scanned map[int64]bool) {
	s.condMu.Lock()
	defer s.condMu.Unlock()
	if s.conflicts == nil {
		s.conflicts = map[int64]schedConflict{}
	}
	for id := range scanned {
		delete(s.conflicts, id)
	}
	for id, c := range found {
		s.conflicts[id] = *c
	}
}

func (s *Server) currentConflicts() []schedConflict {
	s.condMu.Lock()
	defer s.condMu.Unlock()
	out := make([]schedConflict, 0, len(s.conflicts))
	for _, c := range s.conflicts {
		out = append(out, c)
	}
	return out
}

// runSchedule takes and prunes the scheduled snapshots of one VM. It returns
// the instance id of another controller when that one took a scheduled
// snapshot of this VM recently.
func (s *Server) runSchedule(h *hostconn.Host, dom libvirt.Domain, sch snapsched.Schedule, key string, failed map[string]time.Time) (other string) {
	st, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil || stateName(st) == "shutoff" {
		return "" // nothing changes on a stopped VM, so nothing is taken
	}
	list, err := snapshotList(h, dom)
	if err != nil {
		return ""
	}
	snaps := toSnaps(list)
	now := time.Now()
	other = snapsched.Foreign(snaps, sch.Prefix, s.InstanceID, now.Add(-sch.ForeignWindow()))
	r := schedRequest()
	if sch.Due(now, snapsched.Newest(snaps, sch.Prefix)) && now.Sub(failed[key]) >= schedRetry {
		// Read the list again right before creating: another controller may
		// have taken the snapshot since the first look.
		if list, err = snapshotList(h, dom); err != nil {
			return other
		}
		snaps = toSnaps(list)
		if !sch.Due(time.Now(), snapsched.Newest(snaps, sch.Prefix)) {
			return other
		}
		name := snapsched.Name(sch.Prefix, now)
		_, err := s.createSnapshot(r, h, dom, snapshotRequest{
			Name: name, Description: snapsched.Stamp(s.InstanceID), Memory: sch.Memory, // the freeze is automatic (Quiesce left unset)
		})
		if err != nil {
			// createSnapshot only records a task once validation passed;
			// make validation failures (e.g. no guest agent) visible too.
			var br badRequest
			if errors.As(err, &br) {
				err = badRequest("The schedule cannot take a snapshot: " + br.Error())
				s.recordFailure(r, "snapshot create", dom.Name, h.Name, err)
			}
			failed[key] = now
			log.Printf("scheduled snapshot of %s on %s failed: %v", dom.Name, h.Name, err)
			return other
		}
		delete(failed, key)
		if list, err = snapshotList(h, dom); err != nil {
			return other
		}
		snaps = toSnaps(list)
	}
	for _, name := range snapsched.Prune(snaps, sch.Prefix, sch.Keep) {
		sn, err := h.DomainSnapshotLookupByName(dom, name, 0)
		if err != nil {
			continue
		}
		err = s.task(r, "snapshot prune", dom.Name+"@"+name, h.Name, func(func(float64)) error {
			defer refreshPools(h)
			return h.DomainSnapshotDelete(sn, 0)
		})
		if err != nil {
			log.Printf("pruning snapshot %s of %s on %s failed: %v", name, dom.Name, h.Name, err)
			return other // retry on the next tick instead of hammering a failing delete
		}
	}
	return other
}

func (s *Server) recordFailure(r *http.Request, kind, target, node string, cause error) {
	s.task(r, kind, target, node, func(func(float64)) error { return cause })
}

func toSnaps(list []snapInfo) []snapsched.Snap {
	out := make([]snapsched.Snap, len(list))
	for i, si := range list {
		out[i] = snapsched.Snap{Name: si.Name, Description: si.Description, Created: time.Unix(si.Created, 0)}
	}
	return out
}

// metaFlags picks the config the metadata is read from and written to: the
// persistent definition when there is one (and the live domain too if running).
func metaFlags(h *hostconn.Host, dom libvirt.Domain, write bool) libvirt.DomainModificationImpact {
	if p, err := h.DomainIsPersistent(dom); err != nil || p == 0 {
		return libvirt.DomainAffectLive
	}
	f := libvirt.DomainAffectConfig
	if write {
		if st, _, _, _, _, err := h.DomainGetInfo(dom); err == nil && stateName(st) != "shutoff" {
			f |= libvirt.DomainAffectLive
		}
	}
	return f
}

func (s *Server) getSnapshotSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	sch, err := snapsched.Parse(meta)
	if err != nil {
		fail(w, err)
		return
	}
	resp := map[string]any{"schedule": sch}
	if sch != nil {
		var last time.Time
		if list, err := snapshotList(h, dom); err == nil {
			if last = snapsched.Newest(toSnaps(list), sch.Prefix); !last.IsZero() {
				resp["last"] = last.Unix()
			}
		}
		nodeID, _, _ := s.host(r)
		s.addNextRun(resp, nodeID, dom.Name, *sch, last)
		if t := s.lastSchedulerTask(dom.Name); t != nil {
			resp["lastRun"] = t
		}
	}
	writeJSON(w, resp)
}

// addNextRun adds what the page needs to say when the next run is: the time the scheduler itself would compute
// (nextRun, Unix seconds), and who runs schedules ("this" controller, "disabled" by -no-scheduler, or "other").
func (s *Server) addNextRun(resp map[string]any, node int64, vm string, sch snapsched.Schedule, last time.Time) {
	runner := "this"
	if s.NoScheduler {
		runner = "disabled"
	}
	for _, c := range s.currentConflicts() {
		if c.Node == node && !s.NoScheduler {
			for _, n := range c.VMs {
				if n == vm {
					runner = "other"
					resp["otherController"] = c.Other
				}
			}
		}
	}
	resp["runner"] = runner
	if next, ok := sch.NextRun(time.Now(), last); ok {
		resp["nextRun"] = next.Unix()
	}
	resp["tick"] = int(schedTick.Seconds())
}

// lastSchedulerTask is the newest snapshot task the scheduler ran for a VM in the past week (its result), or nil.
func (s *Server) lastSchedulerTask(vm string) map[string]any {
	tasks, err := s.Store.TasksBy("scheduler", time.Now().Add(-7*24*time.Hour).Unix())
	if err != nil {
		return nil
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		t := tasks[i]
		if (t.Kind == "snapshot create" || t.Kind == "snapshot prune") && (t.Target == vm || strings.HasPrefix(t.Target, vm+"@")) && t.Status != "running" {
			return map[string]any{"kind": t.Kind, "time": t.Finished, "ok": t.Status == "ok", "error": t.Error}
		}
	}
	return nil
}

// defaultSnapSchedule holds what a saved schedule gets for fields the client leaves out: enabled, named "auto-<timestamp>".
func defaultSnapSchedule() snapsched.Schedule {
	return snapsched.Schedule{Enabled: true, Prefix: "auto", Keep: 7}
}

func (s *Server) putSnapshotSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	// Fields the client leaves out: a saved schedule is enabled and names its snapshots "auto-<timestamp>".
	sch := defaultSnapSchedule()
	if err := readJSON(r, &sch); err != nil {
		fail(w, err)
		return
	}
	if err := sch.Validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	s.updateSchedule(w, r, h, dom, &sch)
}

func (s *Server) deleteSnapshotSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	s.updateSchedule(w, r, h, dom, nil)
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, h *hostconn.Host, dom libvirt.Domain, sch *snapsched.Schedule) {
	kind := "snapshot schedule set"
	if sch == nil {
		kind = "snapshot schedule remove"
	}
	err := s.task(r, kind, dom.Name, h.Name, func(func(float64)) error {
		meta, err := domainMeta(h, dom)
		if err != nil {
			return err
		}
		updated, err := snapsched.Update(meta, sch)
		if err != nil {
			return badRequest(err.Error())
		}
		if snapsched.Empty(updated) {
			updated = ""
		}
		return setDomainMeta(h, dom, updated)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
