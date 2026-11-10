package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/hostconn"
	"virt-proj/internal/snapsched"
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
	for _, n := range s.Nodes.Status() {
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		doms, _, err := h.ConnectListAllDomains(1, 0)
		if err != nil {
			continue
		}
		for _, dom := range doms {
			meta, err := domainMeta(h, dom)
			if err != nil || meta == "" {
				continue
			}
			sch, err := snapsched.Parse(meta)
			if err != nil || sch == nil || !sch.Enabled {
				continue // an unparsable schedule is reported by the GET endpoint
			}
			key := h.Name + "/" + dom.Name
			s.runSchedule(h, dom, *sch, key, failed)
		}
	}
}

func (s *Server) runSchedule(h *hostconn.Host, dom libvirt.Domain, sch snapsched.Schedule, key string, failed map[string]time.Time) {
	st, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil || stateName(st) == "shutoff" {
		return // nothing changes on a stopped VM, so nothing is taken
	}
	list, err := snapshotList(h, dom)
	if err != nil {
		return
	}
	snaps := toSnaps(list)
	now := time.Now()
	r := schedRequest()
	if sch.Due(now, snapsched.Newest(snaps, sch.Prefix)) && now.Sub(failed[key]) >= schedRetry {
		name := snapsched.Name(sch.Prefix, now)
		_, err := s.createSnapshot(r, h, dom, snapshotRequest{
			Name: name, Description: snapsched.Marker, Memory: sch.Memory, Quiesce: sch.Quiesce,
		})
		if err != nil {
			// createSnapshot only records a task once validation passed;
			// make validation failures (e.g. no guest agent) visible too.
			var br badRequest
			if errors.As(err, &br) {
				s.recordFailure(r, "snapshot create", dom.Name, h.Name, err)
			}
			failed[key] = now
			log.Printf("scheduled snapshot of %s on %s failed: %v", dom.Name, h.Name, err)
			return
		}
		delete(failed, key)
		if list, err = snapshotList(h, dom); err != nil {
			return
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
			return // retry on the next tick instead of hammering a failing delete
		}
	}
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

func domainMeta(h *hostconn.Host, dom libvirt.Domain) (string, error) {
	m, err := h.DomainGetMetadata(dom, int32(libvirt.DomainMetadataElement), libvirt.OptString{snapsched.NS}, metaFlags(h, dom, false))
	var lv libvirt.Error
	if errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomainMetadata) {
		return "", nil
	}
	return m, err
}

func setDomainMeta(h *hostconn.Host, dom libvirt.Domain, meta string) error {
	var m libvirt.OptString
	if meta != "" {
		m = libvirt.OptString{meta}
	}
	return h.DomainSetMetadata(dom, int32(libvirt.DomainMetadataElement), m, libvirt.OptString{"vp"}, libvirt.OptString{snapsched.NS}, metaFlags(h, dom, true))
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
		if list, err := snapshotList(h, dom); err == nil {
			if last := snapsched.Newest(toSnaps(list), sch.Prefix); !last.IsZero() {
				resp["last"] = last.Unix()
			}
		}
	}
	writeJSON(w, resp)
}

func (s *Server) putSnapshotSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var sch snapsched.Schedule
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
