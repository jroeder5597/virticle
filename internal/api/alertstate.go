package api

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"virticle/internal/store"
)

// Alerts are derived from live data on every check; nothing about them is
// stored. This file is the pure part: given the facts gathered in one pass it
// decides which alerts exist, and the tracker remembers just enough between
// passes (when an alert first appeared, since when a node has been offline) to
// report "since" and to tell raise from clear.

type Alert struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`     // node-down | pool-usage | host-memory | failed-job | vm-state | tls-expiry | scheduler-conflict | net-link | net-bond | net-drift | storage-unavailable | rolling-update | replica-age | stale-copy
	Severity string `json:"severity"` // warning | critical
	Title    string `json:"title"`
	Message  string `json:"message"`
	Node     int64  `json:"node,omitempty"`
	Link     string `json:"link,omitempty"` // UI route of the affected object
	Since    int64  `json:"since"`
}

type alertConfig struct {
	PoolWarn, PoolCrit float64       // percent
	Mem                float64       // percent
	CertDays           int           // warn when the UI certificate expires within this many days
	NodeDown           time.Duration // offline for longer than this raises node-down
}

func (c alertConfig) withDefaults() alertConfig {
	if c.PoolWarn <= 0 {
		c.PoolWarn = 85
	}
	if c.PoolCrit <= 0 {
		c.PoolCrit = 95
	}
	if c.Mem <= 0 {
		c.Mem = 90
	}
	if c.CertDays <= 0 {
		c.CertDays = 30
	}
	if c.NodeDown <= 0 {
		c.NodeDown = 2 * time.Minute
	}
	return c
}

type alertNode struct {
	ID          int64
	Name        string
	Online      bool
	Maintenance bool
}

type alertPool struct {
	Node           int64
	NodeName, Name string
	Capacity, Used uint64
}

type alertMem struct {
	Node              int64
	NodeName          string
	UsedKiB, TotalKiB uint64
}

// alertReplica is a standby's copy of a VM: how old it is against its interval. alertStale is the old VM
// after a failover.
type alertReplica struct {
	Node           int64
	NodeName, UUID string
	Name, Source   string
	Last           time.Time // the newest point, or the last time the copy was found current
	Every          int
}

type alertStale struct {
	Node           int64
	NodeName, UUID string
	Name, To       string
}

type alertVM struct {
	Node           int64
	NodeName, UUID string
	Name, Why      string
}

// alertInput is what one pass gathered. Fresh lists the nodes whose pools, memory
// and VMs could be read; alerts about the others are kept as they were rather
// than cleared, because an unreachable node says nothing about its pools.
// alertMigCert is the expiry of a node's migration TLS certificate.
type alertMigCert struct {
	Node     int64
	NodeName string
	NotAfter time.Time
}

type alertInput struct {
	MigCerts   []alertMigCert
	Nodes      []alertNode
	Fresh      map[int64]bool
	Pools      []alertPool
	Mem        []alertMem
	VMs        []alertVM
	Replicas   []alertReplica
	Stale      []alertStale
	Jobs       []store.Task    // tasks of the scheduler in the window
	Conflicts  []schedConflict // nodes where another controller takes scheduled snapshots
	Rolling    *Alert          // a rolling update that stopped and was neither continued, put back nor dismissed
	CertExpiry time.Time       // zero when the UI does not use a certificate of its own
	// Debounced are alerts about network state (card links, bonds, drift between nodes) that flap easily: one is
	// raised only after it was seen on two consecutive passes, and cleared only after it was missing on two.
	Debounced []Alert
	// Unread says that an alert from an earlier pass cannot be judged now (its node could not be read); it is kept.
	Unread func(Alert) bool
}

// debouncedKind is true for the alerts that go through Debounced.
func debouncedKind(kind string) bool {
	return kind == "net-link" || kind == "net-bond" || kind == "net-drift" || kind == "storage-unavailable"
}

type alertTracker struct {
	cfg          alertConfig
	offlineSince map[int64]time.Time
	active       map[string]Alert
	// How many passes in a row a debounced alert was seen (before it is raised) or missing (before it is cleared).
	seen, gone map[string]int
}

func newAlertTracker(cfg alertConfig) *alertTracker {
	return &alertTracker{cfg: cfg.withDefaults(), offlineSince: map[int64]time.Time{}, active: map[string]Alert{}, seen: map[string]int{}, gone: map[string]int{}}
}

// step evaluates one pass. raised holds new alerts and alerts whose severity
// changed; cleared holds alerts that are gone. all is the current set.
func (t *alertTracker) step(in alertInput, now time.Time) (raised, cleared, all []Alert) {
	cur := map[string]Alert{}
	add := func(a Alert) { cur[a.ID] = a }

	for _, n := range in.Nodes {
		if n.Online {
			delete(t.offlineSince, n.ID)
			continue
		}
		if _, ok := t.offlineSince[n.ID]; !ok {
			t.offlineSince[n.ID] = now
		}
		since := t.offlineSince[n.ID]
		// A node that was put into maintenance is expected to be away.
		if n.Maintenance || now.Sub(since) <= t.cfg.NodeDown {
			continue
		}
		add(Alert{ID: "node-down:" + strconv.FormatInt(n.ID, 10), Kind: "node-down", Severity: "critical",
			Title: n.Name + " is offline", Node: n.ID, Link: "/nodes",
			Message: fmt.Sprintf("%s has been unreachable since %s.", n.Name, since.Format("15:04:05"))})
	}
	for _, p := range in.Pools {
		if p.Capacity == 0 {
			continue
		}
		pct := float64(p.Used) / float64(p.Capacity) * 100
		sev := ""
		switch {
		case pct >= t.cfg.PoolCrit:
			sev = "critical"
		case pct >= t.cfg.PoolWarn:
			sev = "warning"
		}
		if sev != "" {
			add(Alert{ID: fmt.Sprintf("pool:%d:%s", p.Node, p.Name), Kind: "pool-usage", Severity: sev, Node: p.Node,
				Title: fmt.Sprintf("Pool %s is %.0f%% full", p.Name, pct), Link: "/storage",
				Message: fmt.Sprintf("Storage pool %s on %s uses %s of %s.", p.Name, p.NodeName, fmtBytes(p.Used), fmtBytes(p.Capacity))})
		}
	}
	for _, m := range in.Mem {
		if m.TotalKiB == 0 {
			continue
		}
		pct := float64(m.UsedKiB) / float64(m.TotalKiB) * 100
		if pct >= t.cfg.Mem {
			add(Alert{ID: "mem:" + strconv.FormatInt(m.Node, 10), Kind: "host-memory", Severity: "warning", Node: m.Node,
				Title: fmt.Sprintf("%s memory is %.0f%% used", m.NodeName, pct), Link: "/nodes",
				Message: fmt.Sprintf("%s uses %s of %s (buffers and cache excluded).", m.NodeName, fmtBytes(m.UsedKiB*1024), fmtBytes(m.TotalKiB*1024))})
		}
	}
	for _, v := range in.VMs {
		add(Alert{ID: fmt.Sprintf("vm:%d:%s", v.Node, v.UUID), Kind: "vm-state", Severity: "critical", Node: v.Node,
			Title: v.Name + " " + v.Why, Link: fmt.Sprintf("/vm/%d/%s", v.Node, v.UUID),
			Message: fmt.Sprintf("VM %s on %s is %s.", v.Name, v.NodeName, v.Why)})
	}
	for _, rp := range in.Replicas {
		if rp.Last.IsZero() || rp.Every <= 0 {
			continue // the first copy is still on its way
		}
		age := now.Sub(rp.Last)
		if age <= 2*time.Duration(rp.Every)*time.Minute+time.Minute {
			continue
		}
		add(Alert{ID: fmt.Sprintf("replica:%d:%s", rp.Node, rp.UUID), Kind: "replica-age", Severity: "warning", Node: rp.Node,
			Title: fmt.Sprintf("The replica of %s on %s is %s old", rp.Name, rp.NodeName, ageText(age)), Link: fmt.Sprintf("/vm/%d/%s", rp.Node, rp.UUID),
			Message: fmt.Sprintf("The copy of %s from %s on %s was last brought up to date at %s, more than twice its interval of %d minutes ago. A failover now would lose what changed since. Check the standby node, the source node and the failed runs under Tasks; the alert clears when a run succeeds.",
				rp.Name, rp.Source, rp.NodeName, rp.Last.Format("2006-01-02 15:04"), rp.Every)})
	}
	for _, st := range in.Stale {
		add(Alert{ID: fmt.Sprintf("stale:%d:%s", st.Node, st.UUID), Kind: "stale-copy", Severity: "warning", Node: st.Node,
			Title: fmt.Sprintf("%s on %s is an old copy", st.Name, st.NodeName), Link: fmt.Sprintf("/vm/%d/%s", st.Node, st.UUID),
			Message: fmt.Sprintf("%s was started on %s from its replica. This VM is the old one: it is kept shut off and cannot be started. Delete it, or use Replicate back on the running VM to make it the new replica.", st.Name, st.To)})
	}
	for _, c := range in.Conflicts {
		add(Alert{ID: "sched-conflict:" + strconv.FormatInt(c.Node, 10), Kind: "scheduler-conflict", Severity: "warning", Node: c.Node,
			Title: "Another controller is running schedules on node " + c.NodeName, Link: "/nodes",
			Message: fmt.Sprintf("Scheduled snapshots of %s on %s were recently taken by controller %s, not by this one. Both controllers run every schedule on these nodes; start one of them with -no-scheduler.",
				strings.Join(c.VMs, ", "), c.NodeName, c.Other)})
	}
	if in.Rolling != nil {
		add(*in.Rolling)
	}
	// A job is in trouble when its newest run failed; a later success clears it.
	latest := map[string]store.Task{}
	for _, j := range in.Jobs {
		k := j.Kind + "\x00" + jobVM(j.Target) + "\x00" + j.Node
		if old, ok := latest[k]; !ok || j.ID > old.ID {
			latest[k] = j
		}
	}
	for _, j := range latest {
		if j.Status != "failed" {
			continue
		}
		if j.Kind == cbTaskKind {
			add(Alert{ID: "job:" + cbTaskKind, Kind: "failed-job", Severity: "warning", Title: "The daily backup of the controller failed", Link: "/controller-backup",
				Message: fmt.Sprintf("The backup written to the backups folder failed at %s: %s", time.Unix(j.Finished, 0).Format("2006-01-02 15:04:05"), j.Error)})
			continue
		}
		title := fmt.Sprintf("Scheduled %s of %s failed", j.Kind, jobVM(j.Target))
		if j.Kind == "replicate" {
			title = fmt.Sprintf("Replication of %s failed", jobVM(j.Target))
		}
		add(Alert{ID: fmt.Sprintf("job:%s:%s:%s", j.Kind, j.Node, jobVM(j.Target)), Kind: "failed-job", Severity: "warning",
			Title: title, Link: "/tasks",
			Message: fmt.Sprintf("%s on %s failed at %s: %s", j.Kind, j.Node, time.Unix(j.Finished, 0).Format("2006-01-02 15:04:05"), j.Error)})
	}
	for _, c := range in.MigCerts {
		left := c.NotAfter.Sub(now)
		if left >= time.Duration(t.cfg.CertDays)*24*time.Hour {
			continue
		}
		sev, title := "warning", fmt.Sprintf("The migration certificate of %s expires in %d days", c.NodeName, int(left.Hours()/24))
		if left <= 0 {
			sev, title = "critical", fmt.Sprintf("The migration certificate of %s has expired", c.NodeName)
		}
		add(Alert{ID: "migtls-expiry:" + strconv.FormatInt(c.Node, 10), Kind: "tls-expiry", Severity: sev, Title: title, Node: c.Node, Link: "/nodes",
			Message: "Valid until " + c.NotAfter.Format("2006-01-02") + ". Until it is issued again, live migrations of this node are no longer encrypted. Use the re-issue action on the Nodes page."})
	}
	if !in.CertExpiry.IsZero() {
		left := in.CertExpiry.Sub(now)
		if left < time.Duration(t.cfg.CertDays)*24*time.Hour {
			sev, title := "warning", fmt.Sprintf("The web UI certificate expires in %d days", int(left.Hours()/24))
			if left <= 0 {
				sev, title = "critical", "The web UI certificate has expired"
			}
			add(Alert{ID: "tls-expiry", Kind: "tls-expiry", Severity: sev, Title: title,
				Message: "Valid until " + in.CertExpiry.Format("2006-01-02") + ". Replace the certificate, or delete the generated one in the data directory to have a new one made at the next start."})
		}
	}

	t.debounce(in, cur)

	// Keep what cannot be re-evaluated: alerts about a node whose data was not read.
	for id, a := range t.active {
		if _, ok := cur[id]; ok || a.Node == 0 || a.Kind == "node-down" || in.Fresh[a.Node] {
			continue
		}
		cur[id] = a
	}

	for id, a := range cur {
		old, had := t.active[id]
		if had {
			a.Since = old.Since
			cur[id] = a
			if old.Severity != a.Severity {
				raised = append(raised, a)
			}
		} else {
			a.Since = now.Unix()
			cur[id] = a
			raised = append(raised, a)
		}
	}
	for id, a := range t.active {
		if _, ok := cur[id]; !ok {
			cleared = append(cleared, a)
		}
	}
	t.active = cur
	for _, a := range cur {
		all = append(all, a)
	}
	sortAlerts(all)
	sortAlerts(raised)
	sortAlerts(cleared)
	return raised, cleared, all
}

// debounce adds the debounced alerts of this pass to cur: a new one only on its second pass in a row, an old one
// until it has been missing for two passes.
func (t *alertTracker) debounce(in alertInput, cur map[string]Alert) {
	now := map[string]Alert{}
	for _, a := range in.Debounced {
		now[a.ID] = a
	}
	for id, a := range now {
		t.gone[id] = 0
		if _, raised := t.active[id]; raised {
			cur[id] = a
			continue
		}
		if t.seen[id]++; t.seen[id] >= 2 {
			cur[id] = a
		}
	}
	for id := range t.seen {
		if _, ok := now[id]; !ok {
			delete(t.seen, id)
		}
	}
	for id, old := range t.active {
		if _, ok := now[id]; ok || !debouncedKind(old.Kind) {
			continue
		}
		if in.Unread != nil && in.Unread(old) {
			cur[id] = old
			continue
		}
		if t.gone[id]++; t.gone[id] < 2 {
			cur[id] = old
		} else {
			delete(t.gone, id)
		}
	}
}

// jobVM is the VM a scheduler task is about. Snapshot tasks are named "vm"
// when a run is refused before it starts and "vm@snapshot" once a snapshot
// is involved; both belong to the same job.
func jobVM(target string) string {
	vm, _, _ := strings.Cut(target, "@")
	vm, _, _ = strings.Cut(vm, " → ") // replicate: "vm → node"
	return vm
}

func sortAlerts(a []Alert) {
	rank := func(s string) int {
		if s == "critical" {
			return 0
		}
		return 1
	}
	sort.Slice(a, func(i, j int) bool {
		if rank(a[i].Severity) != rank(a[j].Severity) {
			return rank(a[i].Severity) < rank(a[j].Severity)
		}
		if a[i].Since != a[j].Since {
			return a[i].Since < a[j].Since
		}
		return strings.Compare(a[i].ID, a[j].ID) < 0
	})
}

// storageAlert is the alert for an NFS pool that does not answer on a node. It goes through the debounced path:
// raised on the second check in a row, cleared on the second check that finds the share answering again.
func storageAlert(node int64, nodeName, pool, server string, st mountState) Alert {
	title := fmt.Sprintf("Storage %s is unavailable on %s", pool, nodeName)
	msg := fmt.Sprintf("The NFS storage %s is unavailable on %s: %s. Check the NFS server and the network between it and the node. This alert clears by itself when the storage answers.", pool, nodeName, unavailableText(server, st))
	return Alert{ID: fmt.Sprintf("nfs:%d:%s", node, pool), Kind: "storage-unavailable", Severity: "critical", Title: title, Message: msg, Node: node, Link: "/storage"}
}
