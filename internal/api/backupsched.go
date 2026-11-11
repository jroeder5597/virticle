package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/snapsched"
)

// Scheduled backups. Like scheduled snapshots they keep no state of their
// own: the schedule is the "backup-schedule" child of the VM's shared
// metadata root, "last run" is the creation time in the manifest of the
// newest backup of the VM in the target pool, and retention is the keepLast
// pruning the backup itself already does. Timing (interval or daily slot) is
// snapsched's.

const (
	backupSchedChild = "backup-schedule"
	// backupCheckEvery bounds how often the target pool is listed to find the
	// newest backup; it is a scan over SSH, not a libvirt call.
	backupCheckEvery = time.Minute
)

type backupSchedule struct {
	Enabled bool `json:"enabled"`
	// Mode is "interval" (every EveryMinutes) or "daily" (at At, UTC).
	Mode         string `json:"mode"`
	EveryMinutes int    `json:"everyMinutes,omitempty"`
	At           string `json:"at,omitempty"`
	TargetNode   int64  `json:"targetNode"` // 0 = the node the VM is on
	Pool         string `json:"pool"`
	KeepLast     int    `json:"keepLast"`
}

var poolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (b backupSchedule) timing() snapsched.Schedule {
	return snapsched.Schedule{Enabled: b.Enabled, Mode: b.Mode, EveryMinutes: b.EveryMinutes, At: b.At, Keep: 1, Prefix: "x"}
}

func (b backupSchedule) validate() error {
	if err := b.timing().Validate(); err != nil {
		return err
	}
	switch {
	case b.TargetNode < 0:
		return fmt.Errorf("bad target node")
	case !poolNameRE.MatchString(b.Pool):
		return fmt.Errorf("a target pool is required")
	case b.KeepLast < 1 || b.KeepLast > 1000:
		return fmt.Errorf("keep-last must be between 1 and 1000 (a schedule without retention would fill the pool)")
	}
	return nil
}

// due reports whether a backup should start now, given the creation time of
// the newest backup of the VM in the target (zero if none).
func (b backupSchedule) due(now, last time.Time) bool { return b.timing().Due(now, last) }

func (b backupSchedule) element() string {
	el := fmt.Sprintf(`<%s enabled=%q mode=%q`, backupSchedChild, yesNo(b.Enabled), b.Mode)
	if b.Mode == "interval" {
		el += fmt.Sprintf(` every-minutes="%d"`, b.EveryMinutes)
	} else {
		el += fmt.Sprintf(` at=%q`, b.At)
	}
	return el + fmt.Sprintf(` target-node="%d" pool=%q keep-last="%d"/>`, b.TargetNode, b.Pool, b.KeepLast)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// parseBackupSchedule reads the schedule from the application metadata; nil
// when there is none.
func parseBackupSchedule(meta string) (*backupSchedule, error) {
	if meta == "" {
		return nil, nil
	}
	var root struct {
		B *struct {
			Enabled string `xml:"enabled,attr"`
			Mode    string `xml:"mode,attr"`
			Every   int    `xml:"every-minutes,attr"`
			At      string `xml:"at,attr"`
			Node    int64  `xml:"target-node,attr"`
			Pool    string `xml:"pool,attr"`
			Keep    int    `xml:"keep-last,attr"`
		} `xml:"backup-schedule"`
	}
	if err := xml.Unmarshal([]byte(meta), &root); err != nil {
		return nil, fmt.Errorf("metadata is not well-formed XML: %w", err)
	}
	if root.B == nil {
		return nil, nil
	}
	b := &backupSchedule{Enabled: root.B.Enabled == "yes", Mode: root.B.Mode, EveryMinutes: root.B.Every, At: root.B.At,
		TargetNode: root.B.Node, Pool: root.B.Pool, KeepLast: root.B.Keep}
	return b, b.validate()
}

// newestBackup is the creation time of the newest backup of VM uuid in all.
func newestBackup(all []retained, uuid string) time.Time {
	var last time.Time
	for _, b := range all {
		if b.UUID == uuid && b.Created.After(last) {
			last = b.Created
		}
	}
	return last
}

// lastBackupTime lists the target pool and returns the newest backup of the
// VM there (zero if none).
func lastBackupTime(dst *hostconn.Host, pool, uuid string) (time.Time, error) {
	_, pd, err := destPool(dst, pool)
	if err != nil {
		return time.Time{}, err
	}
	items, err := scanPool(dst, pd)
	if err != nil {
		return time.Time{}, err
	}
	var all []retained
	for _, it := range items {
		if it.Err == "" {
			all = append(all, retained{Key: it.Dir, UUID: it.Manifest.UUID, Created: it.Manifest.createdTime()})
		}
	}
	return newestBackup(all, uuid), nil
}

// bkSchedState is the scheduler's in-memory pacing; losing it on restart
// only means one extra look at the target.
type bkSchedState struct {
	mu      sync.Mutex
	checked map[string]time.Time // last time the target was listed
	failed  map[string]time.Time // last failed attempt
}

func (st *bkSchedState) get(m *map[string]time.Time, key string) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return (*m)[key]
}

func (st *bkSchedState) set(m *map[string]time.Time, key string, t time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if *m == nil {
		*m = map[string]time.Time{}
	}
	if t.IsZero() {
		delete(*m, key)
	} else {
		(*m)[key] = t
	}
}

// runBackupSchedule starts a backup of dom if its schedule says one is due.
// The backup runs in the background; this only plans and launches it.
func (s *Server) runBackupSchedule(srcID int64, h *hostconn.Host, dom libvirt.Domain, bs backupSchedule, key string) {
	uuid := cluster.FormatUUID(dom.UUID)
	bkRuns.Lock()
	_, busy := bkRuns.m[uuid]
	bkRuns.Unlock()
	if busy {
		return
	}
	now := time.Now()
	if now.Sub(s.bk.get(&s.bk.failed, key)) < schedRetry || now.Sub(s.bk.get(&s.bk.checked, key)) < backupCheckEvery {
		return
	}
	s.bk.set(&s.bk.checked, key, now)
	dstID := bs.TargetNode
	if dstID == 0 {
		dstID = srcID
	}
	r := schedRequest()
	target := backupTarget(dom.Name, fmt.Sprintf("node %d", dstID), bs.Pool)
	failed := func(err error) {
		s.bk.set(&s.bk.failed, key, now)
		s.recordFailure(r, "backup", target, h.Name, err)
		logf("scheduled backup of %s on %s failed: %v", dom.Name, h.Name, err)
	}
	dst, err := s.Nodes.Host(dstID)
	if err != nil {
		failed(err)
		return
	}
	target = backupTarget(dom.Name, dst.Name, bs.Pool) // the name a real run's task carries, so a success clears the alert
	last, err := lastBackupTime(dst, bs.Pool, uuid)
	if err != nil {
		failed(err)
		return
	}
	if !bs.due(now, last) {
		return
	}
	m, err := s.planBackup(srcID, h, dom, backupReq{TargetNode: bs.TargetNode, Pool: bs.Pool, KeepLast: bs.KeepLast})
	if err != nil {
		failed(err)
		return
	}
	_, done, err := s.launchBackup(r, h, m)
	if err != nil {
		if _, isBusy := err.(badRequest); !isBusy {
			failed(err)
		}
		return
	}
	go func() {
		if err := <-done; err != nil {
			s.bk.set(&s.bk.failed, key, time.Now())
		} else {
			s.bk.set(&s.bk.failed, key, time.Time{})
		}
	}()
}

// --- API ---

func (s *Server) getBackupSchedule(w http.ResponseWriter, r *http.Request) {
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
	bs, err := parseBackupSchedule(meta)
	if err != nil {
		fail(w, err)
		return
	}
	resp := map[string]any{"schedule": bs}
	if bs != nil {
		var lastT time.Time
		srcID, _, _ := s.host(r)
		dstID := bs.TargetNode
		if dstID == 0 {
			dstID = srcID
		}
		if dst, err := s.Nodes.Host(dstID); err != nil {
			resp["lastError"] = err.Error()
		} else if last, err := lastBackupTime(dst, bs.Pool, cluster.FormatUUID(dom.UUID)); err != nil {
			resp["lastError"] = err.Error()
		} else if !last.IsZero() {
			resp["last"] = last.Unix()
			lastT = last
		}
		// Backups run for shut-off VMs too; the timing is the same snapsched code the scheduler decides with.
		resp["runner"] = "this"
		if s.NoScheduler {
			resp["runner"] = "disabled"
		}
		if next, ok := bs.timing().NextRun(time.Now(), lastT); ok {
			resp["nextRun"] = next.Unix()
		}
		resp["tick"] = int(schedTick.Seconds())
	}
	writeJSON(w, resp)
}

func (s *Server) putBackupSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var bs backupSchedule
	if err := readJSON(r, &bs); err != nil {
		fail(w, err)
		return
	}
	if bs.Mode == "interval" {
		bs.At = ""
	} else {
		bs.EveryMinutes = 0
	}
	if err := bs.validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	srcID, _, _ := s.host(r)
	dstID := bs.TargetNode
	if dstID == 0 {
		dstID = srcID
	}
	dst, err := s.Nodes.Host(dstID)
	if err != nil {
		fail(w, err)
		return
	}
	if _, _, err := destPool(dst, bs.Pool); err != nil {
		fail(w, err)
		return
	}
	s.updateBackupSchedule(w, r, h, dom, bs.element(), "backup schedule set")
}

func (s *Server) deleteBackupSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	s.updateBackupSchedule(w, r, h, dom, "", "backup schedule remove")
}

func (s *Server) updateBackupSchedule(w http.ResponseWriter, r *http.Request, h *hostconn.Host, dom libvirt.Domain, elem, kind string) {
	err := s.task(r, kind, dom.Name, h.Name, func(func(float64)) error {
		old, err := domainMeta(h, dom)
		if err != nil {
			return err
		}
		meta, err := metaSetChild(old, backupSchedChild, elem)
		if err != nil {
			return badRequest(err.Error())
		}
		return setDomainMeta(h, dom, meta)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
