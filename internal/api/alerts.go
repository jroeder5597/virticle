package api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/tlscert"
)

// AlertSettings are the thresholds and options set by server flags; zero
// values mean the defaults (see alertConfig.withDefaults).
type AlertSettings struct {
	PoolWarn, PoolCrit float64 // percent
	Mem                float64 // percent
	CertDays           int
	// Webhook is the deprecated -alert-webhook flag: one more destination of the
	// Notifications page, which cannot be changed there.
	Webhook string
	// CertFile is the TLS certificate the UI is served with, if the server
	// generated or was given one; its expiry is watched.
	CertFile string
}

// ValidateWebhookURL accepts http and https URLs only.
func ValidateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("the alert webhook must be an http:// or https:// URL")
	}
	return nil
}

const alertEvery = 60 * time.Second

var (
	alertOnce sync.Once
	alertHub  = struct {
		sync.Mutex
		list []Alert
	}{}
)

func (s *Server) startAlerts() {
	alertOnce.Do(func() { go s.runAlerts(context.Background(), alertEvery, 10*time.Second) })
}

func (s *Server) runAlerts(ctx context.Context, every, first time.Duration) {
	tr := newAlertTracker(alertConfig{PoolWarn: s.Alerts.PoolWarn, PoolCrit: s.Alerts.PoolCrit, Mem: s.Alerts.Mem, CertDays: s.Alerts.CertDays})
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = every
		raised, cleared, all := tr.step(s.gatherAlertInput(time.Now()), time.Now())
		alertHub.Lock()
		alertHub.list = all
		alertHub.Unlock()
		if len(raised)+len(cleared) > 0 {
			s.Nodes.Publish(cluster.Event{Type: "alert"})
		}
		s.dispatchAlerts(raised, cleared)
	}
}

func (s *Server) gatherAlertInput(now time.Time) alertInput {
	in := alertInput{Fresh: map[int64]bool{}}
	linksRead := map[int64]bool{}
	online := 0
	for _, n := range s.Nodes.Status() {
		name := n.Hostname
		if name == "" {
			name = n.Addr
		}
		in.Nodes = append(in.Nodes, alertNode{ID: n.ID, Name: name, Online: n.State == "online", Maintenance: n.Maintenance})
		if n.State != "online" {
			continue
		}
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		in.Fresh[n.ID] = true
		online++
		if links, err := readLinkAlerts(h, n.ID, name, n.Addr); err == nil {
			linksRead[n.ID] = true
			in.Debounced = append(in.Debounced, links...)
		}
		pools, storage := gatherPools(h, n.ID, name)
		healSharedPools(h)
		in.Pools = append(in.Pools, pools...)
		in.Debounced = append(in.Debounced, storage...)
		in.VMs = append(in.VMs, gatherBadVMs(h, n.ID, name)...)
		if t, ok := s.migCertExpiry(h); ok {
			in.MigCerts = append(in.MigCerts, alertMigCert{Node: n.ID, NodeName: name, NotAfter: t})
		}
		if m, ok := latestHostMem(n.ID); ok {
			m.NodeName = name
			in.Mem = append(in.Mem, m)
		}
	}
	driftRead := true
	if online >= 2 {
		rep := s.computeDrift(false)
		driftRead = len(rep.Unread) == 0
		in.Debounced = append(in.Debounced, driftAlert(rep)...)
	}
	in.Unread = func(a Alert) bool {
		if a.Kind == "net-drift" {
			return !driftRead
		}
		return !linksRead[a.Node]
	}
	in.Conflicts = s.currentConflicts()
	in.Rolling = s.rollingAlert()
	if jobs, err := s.Store.TasksBy("scheduler", now.Add(-24*time.Hour).Unix()); err == nil {
		in.Jobs = jobs
	} else {
		log.Printf("alerts: reading scheduler tasks: %v", err)
	}
	certFile := s.Alerts.CertFile
	if s.Web != nil {
		certFile = s.Web.CertFile()
	}
	if certFile != "" {
		if t, err := tlscert.NotAfter(certFile); err == nil {
			in.CertExpiry = t
		}
	}
	return in
}

// gatherPools reads the usage of the active pools. A directory pool's numbers
// are only as fresh as its last refresh, so each pool is refreshed first. An NFS
// pool is probed before anything else (poolhealth.go): one whose mount does not
// answer is not refreshed, because that would wait for the server as long as it is
// away, and becomes a storage alert instead.
func gatherPools(h *hostconn.Host, node int64, nodeName string) ([]alertPool, []Alert) {
	pools, _, err := h.ConnectListAllStoragePools(1, 0)
	if err != nil {
		if errors.Is(err, hostconn.ErrStorageBusy) {
			// A node with one libvirt daemon (Debian, Ubuntu) cannot list its pools while a call on a dead NFS
			// share is stuck, which is exactly when the alert is needed: probe the shares seen last time.
			return nil, knownNFSAlerts(h, node, nodeName)
		}
		return nil, nil
	}
	type active struct {
		pl libvirt.StoragePool
		x  poolXML
	}
	var act []active
	var mounts []string
	var known []knownNFS
	for _, pl := range pools {
		if a, err := h.StoragePoolIsActive(pl); err != nil || a == 0 {
			continue
		}
		var x poolXML
		if raw, err := h.StoragePoolGetXMLDesc(pl, 0); err == nil {
			xml.Unmarshal([]byte(raw), &x)
		}
		act = append(act, active{pl, x})
		if x.Type == "netfs" && x.Target.Path != "" {
			mounts = append(mounts, x.Target.Path)
			known = append(known, knownNFS{Pool: pl.Name, Server: x.Source.Host.Name, Path: x.Target.Path})
		}
	}
	rememberNFS(node, known)
	health := nfsProber.peek(h, mounts)
	var out []alertPool
	var alerts []Alert
	for _, a := range act {
		if st, probed := health[a.x.Target.Path]; probed && st != mountUp {
			alerts = append(alerts, storageAlert(node, nodeName, a.pl.Name, a.x.Source.Host.Name, st))
			continue
		}
		pl := a.pl
		// An NFS pool is never refreshed from here: a refresh that meets a server going away waits for it
		// while holding the node's storage daemon, and every storage call of that node queues behind it.
		if a.x.Type != "netfs" {
			h.StoragePoolRefresh(pl, 0)
		}
		_, capacity, alloc, avail, err := h.StoragePoolGetInfo(pl)
		if err != nil || capacity == 0 {
			continue
		}
		used := alloc
		if capacity > avail && capacity-avail > used {
			used = capacity - avail
		}
		out = append(out, alertPool{Node: node, NodeName: nodeName, Name: pl.Name, Capacity: capacity, Used: used})
	}
	return out, alerts
}

func gatherBadVMs(h *hostconn.Host, node int64, nodeName string) []alertVM {
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil
	}
	var out []alertVM
	for _, d := range doms {
		state, reason, err := h.DomainGetState(d, 0)
		if err != nil {
			continue
		}
		if why := badVMState(state, reason); why != "" {
			out = append(out, alertVM{Node: node, NodeName: nodeName, UUID: cluster.FormatUUID(d.UUID), Name: d.Name, Why: why})
		}
	}
	return out
}

// badVMState names the states that need a human: a crashed guest and a guest
// that libvirt paused because of a disk I/O error.
func badVMState(state, reason int32) string {
	switch libvirt.DomainState(state) {
	case libvirt.DomainCrashed:
		return "has crashed"
	case libvirt.DomainPaused:
		if libvirt.DomainPausedReason(reason) == libvirt.DomainPausedIoerror {
			return "is paused after a disk I/O error"
		}
	}
	return ""
}

func latestHostMem(node int64) (alertMem, bool) {
	hub.mu.Lock()
	ns := hub.nodes[node]
	hub.mu.Unlock()
	if ns == nil {
		return alertMem{}, false
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	p, ok := ns.host.Last()
	if !ok || p.MemUsedKiB == nil || p.MemTotalKiB == nil {
		return alertMem{}, false
	}
	return alertMem{Node: node, UsedKiB: *p.MemUsedKiB, TotalKiB: *p.MemTotalKiB}, true
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	alertHub.Lock()
	out := append([]Alert{}, alertHub.list...)
	alertHub.Unlock()
	// The pool-usage thresholds travel along so pickers can colour pools by the same limits.
	c := alertConfig{PoolWarn: s.Alerts.PoolWarn, PoolCrit: s.Alerts.PoolCrit}.withDefaults()
	writeJSON(w, map[string]any{"alerts": out, "poolWarn": c.PoolWarn, "poolCritical": c.PoolCrit})
}

// knownNFS is an NFS pool of a node as the last successful look at its pools saw it.
type knownNFS struct{ Pool, Server, Path string }

var lastNFS sync.Map // node id -> []knownNFS

func rememberNFS(node int64, k []knownNFS) { lastNFS.Store(node, k) }

// knownNFSAlerts is the storage alerts of a node whose pools cannot be listed: the shares seen last time are probed
// (the probe is a shell command on the node and never waits for libvirt).
func knownNFSAlerts(h *hostconn.Host, node int64, nodeName string) []Alert {
	v, ok := lastNFS.Load(node)
	if !ok {
		return nil
	}
	k := v.([]knownNFS)
	paths := make([]string, len(k))
	for i, p := range k {
		paths[i] = p.Path
	}
	return storageAlertsFor(node, nodeName, k, nfsProber.peek(h, paths))
}

// storageAlertsFor turns the probe answers for the known shares into alerts: one for each that does not answer.
func storageAlertsFor(node int64, nodeName string, k []knownNFS, health map[string]mountState) []Alert {
	var out []Alert
	for _, p := range k {
		if st, probed := health[p.Path]; probed && st != mountUp {
			out = append(out, storageAlert(node, nodeName, p.Pool, p.Server, st))
		}
	}
	return out
}
