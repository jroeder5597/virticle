package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
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
	// Webhook is a URL chosen by the operator. When set, every alert raise and
	// clear is POSTed there as JSON. Nothing is sent anywhere otherwise.
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

// webhookKey is where the webhook URL set in the UI is kept.
const webhookKey = "alert.webhook"

// webhookURL is the address alerts are POSTed to: the -alert-webhook flag when
// given (then the UI cannot change it), else the one stored in the database.
func (s *Server) webhookURL() (url string, fromFlag bool) {
	if s.Alerts.Webhook != "" {
		return s.Alerts.Webhook, true
	}
	if s.Store != nil {
		if v, ok, err := s.Store.WebSetting(webhookKey); err == nil && ok {
			return v, false
		}
	}
	return "", false
}

func (s *Server) getAlertSettings(w http.ResponseWriter, r *http.Request) {
	u, flag := s.webhookURL()
	writeJSON(w, map[string]any{"webhook": u, "fromFlag": flag})
}

func (s *Server) putAlertSettings(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Webhook string `json:"webhook"`
	}
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	if _, flag := s.webhookURL(); flag {
		fail(w, badRequest("the webhook is set by the -alert-webhook flag; remove the flag to change it here"))
		return
	}
	q.Webhook = strings.TrimSpace(q.Webhook)
	if q.Webhook != "" {
		if err := ValidateWebhookURL(q.Webhook); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
	}
	var err error
	if q.Webhook == "" {
		err = s.Store.DeleteWebSetting(webhookKey)
	} else {
		err = s.Store.SetWebSetting(webhookKey, q.Webhook)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"webhook": q.Webhook, "fromFlag": false})
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
	var wh *webhook
	whURL := ""
	defer func() {
		if wh != nil {
			wh.stop()
		}
	}()
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = every
		// The webhook can be changed on the Alerts page at any time.
		if u, _ := s.webhookURL(); u != whURL {
			if wh != nil {
				wh.stop()
				wh = nil
			}
			if whURL = u; u != "" {
				wh = newWebhook(u)
			}
		}
		raised, cleared, all := tr.step(s.gatherAlertInput(time.Now()), time.Now())
		alertHub.Lock()
		alertHub.list = all
		alertHub.Unlock()
		if len(raised)+len(cleared) > 0 {
			s.Nodes.Publish(cluster.Event{Type: "alert"})
		}
		if wh != nil {
			for _, a := range raised {
				wh.send("raise", a)
			}
			for _, a := range cleared {
				wh.send("clear", a)
			}
		}
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
		in.Pools = append(in.Pools, gatherPools(h, n.ID, name)...)
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
// are only as fresh as its last refresh, so each pool is refreshed first.
func gatherPools(h *hostconn.Host, node int64, nodeName string) []alertPool {
	pools, _, err := h.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil
	}
	var out []alertPool
	for _, pl := range pools {
		if act, err := h.StoragePoolIsActive(pl); err != nil || act == 0 {
			continue
		}
		h.StoragePoolRefresh(pl, 0)
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
	return out
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

// --- webhook ---

type webhook struct {
	url     string
	client  *http.Client
	delays  []time.Duration // waits before the 2nd, 3rd ... attempt
	queue   chan webhookMsg
	dropped func(webhookMsg)
}

type webhookMsg struct {
	Event string `json:"event"` // raise | clear
	Alert Alert  `json:"alert"`
	Time  int64  `json:"time"`
}

func newWebhook(u string) *webhook {
	w := &webhook{url: u, client: &http.Client{Timeout: 10 * time.Second}, delays: []time.Duration{2 * time.Second, 10 * time.Second},
		queue: make(chan webhookMsg, 64)}
	go w.run()
	return w
}

// send queues a message and returns at once; a full queue (the receiver has
// been down for a long time) drops the message and logs it.
func (w *webhook) send(event string, a Alert) {
	m := webhookMsg{Event: event, Alert: a, Time: time.Now().Unix()}
	select {
	case w.queue <- m:
	default:
		log.Printf("alert webhook: queue full, dropping %s of %s", event, a.ID)
		if w.dropped != nil {
			w.dropped(m)
		}
	}
}

// stop ends the delivery goroutine after the queued messages. Only the
// goroutine that calls send may call it.
func (w *webhook) stop() { close(w.queue) }

func (w *webhook) run() {
	for m := range w.queue {
		w.deliver(m)
	}
}

func (w *webhook) deliver(m webhookMsg) {
	body, _ := json.Marshal(m)
	var err error
	for attempt := 0; attempt <= len(w.delays); attempt++ {
		if attempt > 0 {
			time.Sleep(w.delays[attempt-1])
		}
		if err = w.post(body); err == nil {
			return
		}
	}
	log.Printf("alert webhook: giving up on %s of %s: %v", m.Event, m.Alert.ID, err)
}

func (w *webhook) post(body []byte) error {
	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "virticle-alerts")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("receiver answered %s", resp.Status)
	}
	return nil
}
