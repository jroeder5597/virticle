// Package api is the HTTP layer: a JSON REST API, WebSockets for events and
// consoles, and the embedded web UI.
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/buildinfo"
	"virticle/internal/catalog"
	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
	"virticle/internal/webaccess"
)

const cookieName = "virt_session"

type Server struct {
	Store  *store.Store
	Nodes  *cluster.Manager
	PubKey string // controller's SSH public key, shown when adding a node
	Static fs.FS  // built web UI
	Alerts AlertSettings
	// DataDir is the controller's data directory (the migration CA lives in it).
	DataDir string

	// TrustedProxies are the peers whose X-Forwarded-* headers are believed
	// (see proxy.go); empty means the headers are never read.
	TrustedProxies []netip.Prefix
	// Web runs the listeners and the TLS mode (see webaccess.go); nil in tests.
	Web  *webaccess.Manager
	webv atomic.Pointer[webView]
	// NoScheduler turns off the background snapshot and backup schedulers.
	NoScheduler bool
	// InstanceID identifies this controller in the descriptions of the
	// snapshots it schedules (see snapschedule.go).
	InstanceID string

	bk        bkSchedState
	catMu     sync.Mutex
	cat       *catalog.Source // image catalogue, see images.go
	condMu    sync.Mutex
	conflicts map[int64]schedConflict // nodes where another controller schedules snapshots

	routes  []route // every pattern registered by Handler, for the access-level test
	limiter *limiter
	shots   shotCache
	cpuBase baselineState
	migAuto *migTLSAuto // automatic encrypted-migration setup (migtlsauto.go)
}

type ctxKey struct{}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.startSnapshotScheduler()
	s.startAlerts()
	s.startMigAuto()
	s.startCPUBaseline()
	s.routes = nil
	pub := func(pattern string, h http.HandlerFunc) {
		s.routes = append(s.routes, route{pattern: pattern, public: true})
		mux.HandleFunc(pattern, h)
	}
	auth := func(pattern string, h http.HandlerFunc) {
		s.routes = append(s.routes, route{pattern: pattern})
		mux.HandleFunc(pattern, s.requireAuth(pattern, h))
	}
	s.startStatsSampler()

	pub("GET /api/status", s.status)
	pub("POST /api/setup", s.setup)
	pub("POST /api/login", s.login)
	pub("POST /api/logout", s.logout)

	auth("GET /api/version", s.version)
	auth("GET /api/events", s.events)
	auth("GET /api/alerts", s.listAlerts)
	auth("GET /api/alerts/settings", s.getAlertSettings)
	auth("PUT /api/alerts/settings", s.putAlertSettings)
	auth("GET /api/tasks", s.tasks)

	auth("GET /api/users", s.listUsers)
	auth("POST /api/users", s.createUser)
	auth("PUT /api/users/{id}", s.updateUser)
	auth("DELETE /api/users/{id}", s.deleteUser)
	auth("POST /api/account/password", s.changePassword)
	auth("GET /api/account/tokens", s.listTokens)
	auth("POST /api/account/tokens", s.createToken)
	auth("DELETE /api/account/tokens/{id}", s.deleteToken)
	auth("GET /api/me/prefs", s.getPrefs)
	auth("PUT /api/me/prefs", s.putPrefs)
	auth("GET /api/audit", s.auditLog)
	s.webRoutes(auth)

	auth("GET /api/nodes", s.listNodes)
	auth("GET /api/local-node", s.localNode)
	auth("GET /api/cluster/cpu-baseline", s.cpuBaseline)
	auth("POST /api/nodes", s.addNode)
	auth("DELETE /api/nodes/{node}", s.deleteNode)
	auth("PUT /api/nodes/{node}", s.editNode)
	auth("POST /api/nodes/{node}/test-connection", s.testNodeConnection)
	auth("POST /api/nodes/{node}/forget-host-key", s.forgetHostKey)
	auth("POST /api/prepare", s.prepareStart)
	auth("GET /api/prepare/{task}", s.prepareStatus)
	auth("GET /api/nodes/{node}", s.nodeInfo)
	auth("POST /api/nodes/{node}/evacuate/check", s.evacuateCheck)
	auth("POST /api/nodes/{node}/evacuate", s.evacuate)
	auth("GET /api/nodes/{node}/evacuate", s.evacuateStatus)
	auth("POST /api/nodes/{node}/evacuate/cancel", s.evacuateCancel)
	auth("PUT /api/nodes/{node}/maintenance", s.putMaintenance)
	auth("GET /api/nodes/{node}/updates", s.nodeUpdates)
	auth("GET /api/nodes/{node}/interfaces", s.nodeInterfaces)
	auth("GET /api/nodes/{node}/hostnet", s.hostnetGet)
	auth("POST /api/nodes/{node}/hostnet/plan", s.hostnetPlan)
	auth("POST /api/nodes/{node}/hostnet/apply", s.hostnetApply)
	auth("POST /api/nodes/{node}/hostnet/rollback", s.hostnetRollback)
	auth("GET /api/nodes/{node}/hostnet/peers", s.hostnetPeers)
	auth("GET /api/stats", s.allStats)
	auth("GET /api/nodes/{node}/stats", s.nodeStats)
	auth("GET /api/nodes/{node}/vms/{uuid}/stats", s.vmStats)
	auth("GET /api/metrics/query", s.metricsQuery)
	auth("GET /api/metrics/overview", s.metricsOverview)
	auth("GET /metrics", s.promMetrics)
	auth("GET /api/nodes/{node}/vms/{uuid}/guest", s.vmGuest)
	auth("GET /api/nodes/{node}/vms/{uuid}/agent-help", s.vmAgentHelp)

	auth("GET /api/cloudinit-profiles", s.listCloudProfiles)
	auth("POST /api/cloudinit-profiles", s.saveCloudProfile)
	auth("PUT /api/cloudinit-profiles/{id}", s.saveCloudProfile)
	auth("DELETE /api/cloudinit-profiles/{id}", s.deleteCloudProfile)

	auth("GET /api/vms", s.listVMs)
	auth("GET /api/pools", s.listAllPools)
	auth("GET /api/networks", s.listAllNetworks)
	auth("GET /api/topology", s.topology)
	auth("POST /api/nodes/{node}/vms", s.createVM)
	auth("GET /api/nodes/{node}/vms/{uuid}", s.vmDetail)
	auth("POST /api/nodes/{node}/vms/{uuid}/action", s.vmAction)
	auth("PUT /api/nodes/{node}/vms/{uuid}/xml", s.vmSetXML)
	auth("PUT /api/nodes/{node}/vms/{uuid}/autostart", s.vmAutostart)
	auth("POST /api/nodes/{node}/vms/{uuid}/agent-channel", s.vmAddAgentChannel)
	auth("DELETE /api/nodes/{node}/vms/{uuid}", s.vmDelete)
	auth("PUT /api/nodes/{node}/vms/{uuid}/template", s.vmTemplate)
	auth("POST /api/nodes/{node}/vms/{uuid}/seal", s.vmSeal)
	auth("GET /api/nodes/{node}/vms/{uuid}/clone-info", s.cloneInfo)
	auth("POST /api/nodes/{node}/vms/{uuid}/clone", s.vmClone)
	auth("GET /api/nodes/{node}/vms/{uuid}/vnc", s.vmVNC)
	auth("GET /api/nodes/{node}/vms/{uuid}/serial", s.vmSerial)
	auth("POST /api/nodes/{node}/vms/{uuid}/serial-console", s.vmSerialAdd)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/serial-console", s.vmSerialRemove)
	auth("GET /api/nodes/{node}/vms/{uuid}/clock", s.vmClock)
	auth("PUT /api/nodes/{node}/vms/{uuid}/clock", s.hwClock)
	auth("POST /api/nodes/{node}/vms/{uuid}/clock/sync", s.vmClockSync)
	auth("GET /api/nodes/{node}/vms/{uuid}/screenshot", s.vmScreenshot)
	auth("POST /api/nodes/{node}/vms/{uuid}/sendkey", s.sendKeys)

	auth("PUT /api/nodes/{node}/vms/{uuid}/cpu", s.hwCPU)
	auth("PUT /api/nodes/{node}/vms/{uuid}/memory", s.hwMemory)
	auth("POST /api/nodes/{node}/vms/{uuid}/disks", s.hwDiskAdd)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/disks/{target}", s.hwDiskDelete)
	auth("POST /api/nodes/{node}/vms/{uuid}/disks/{target}/resize", s.hwDiskResize)
	auth("GET /api/nodes/{node}/vms/{uuid}/disks/{target}/move/plan", s.diskMovePlan)
	auth("POST /api/nodes/{node}/vms/{uuid}/disks/{target}/move", s.needs("diskMove", s.diskMove))
	auth("POST /api/nodes/{node}/vms/{uuid}/disks/{target}/move/cancel", s.diskMoveCancel)
	auth("POST /api/nodes/{node}/vms/{uuid}/cdrom", s.hwCDROMAdd)
	auth("PUT /api/nodes/{node}/vms/{uuid}/cdrom/{target}", s.hwCDROMMedia)
	auth("POST /api/nodes/{node}/vms/{uuid}/nics", s.hwNICAdd)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/nics/{mac}", s.hwNICDelete)
	auth("PUT /api/nodes/{node}/vms/{uuid}/nics/{mac}", s.hwNICUpdate)
	auth("PUT /api/nodes/{node}/vms/{uuid}/boot", s.hwBoot)
	auth("GET /api/nodes/{node}/vms/{uuid}/tuning", s.vmTuning)
	auth("PUT /api/nodes/{node}/vms/{uuid}/tuning/pinning", s.tuningPinning)
	auth("PUT /api/nodes/{node}/vms/{uuid}/tuning/cpu", s.tuningCPU)
	auth("PUT /api/nodes/{node}/vms/{uuid}/tuning/hugepages", s.tuningHugepages)
	auth("PUT /api/nodes/{node}/vms/{uuid}/tuning/numa", s.tuningNUMA)
	auth("GET /api/nodes/{node}/vms/{uuid}/hostdevs", s.vmHostdevs)
	auth("POST /api/nodes/{node}/vms/{uuid}/hostdevs", s.vmHostdevAttach)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/hostdevs/{id}", s.vmHostdevDetach)
	auth("PUT /api/nodes/{node}/vms/{uuid}/tpm", s.vmTPM)
	auth("PUT /api/nodes/{node}/vms/{uuid}/tags", s.vmTags)
	auth("GET /api/tags", s.listTags)
	auth("PUT /api/tags/{name}/color", s.putTagColor)
	auth("DELETE /api/tags/{name}/color", s.deleteTagColor)
	auth("GET /api/nodes/{node}/firmware-caps", s.firmwareCaps)
	auth("POST /api/nodes/{node}/import/inspect", s.needs("writePools", s.importInspect))
	auth("POST /api/nodes/{node}/import", s.needs("writePools", s.importDisk))
	auth("POST /api/nodes/{node}/vms/{uuid}/migrate/check", s.needs("migration", s.migrateCheck))
	auth("POST /api/nodes/{node}/vms/{uuid}/migrate", s.needs("migration", s.migrate))
	auth("POST /api/nodes/{node}/vms/{uuid}/migrate/cancel", s.migrateCancel)
	auth("GET /api/nodes/{node}/migration-tls", s.migTLSStatus)
	auth("POST /api/nodes/{node}/migration-tls", s.migTLSSetup)
	auth("DELETE /api/nodes/{node}/migration-tls", s.migTLSRemove)
	auth("GET /api/nodes/{node}/vms/{uuid}/snapshots", s.listSnapshots)
	auth("GET /api/nodes/{node}/vms/{uuid}/snapshot-options", s.snapshotOptionsHandler)
	auth("POST /api/nodes/{node}/vms/{uuid}/snapshots", s.createSnapshotHandler)
	auth("POST /api/nodes/{node}/vms/{uuid}/snapshots/{name}/revert", s.revertSnapshot)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/snapshots/{name}", s.deleteSnapshot)
	auth("POST /api/nodes/{node}/vms/{uuid}/snapshots/delete", s.deleteSnapshotsHandler)
	auth("POST /api/nodes/{node}/vms/{uuid}/snapshots/delete-plan", s.planSnapshotsHandler)
	auth("GET /api/nodes/{node}/vms/{uuid}/snapshot-schedule", s.getSnapshotSchedule)
	auth("PUT /api/nodes/{node}/vms/{uuid}/snapshot-schedule", s.putSnapshotSchedule)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/snapshot-schedule", s.deleteSnapshotSchedule)

	auth("GET /api/nodes/{node}/vms/{uuid}/backup-schedule", s.getBackupSchedule)
	auth("PUT /api/nodes/{node}/vms/{uuid}/backup-schedule", s.putBackupSchedule)
	auth("DELETE /api/nodes/{node}/vms/{uuid}/backup-schedule", s.deleteBackupSchedule)
	auth("POST /api/nodes/{node}/vms/{uuid}/backup", s.needs("backups", s.vmBackup))
	auth("POST /api/nodes/{node}/vms/{uuid}/backup/cancel", s.vmBackupCancel)
	auth("GET /api/backups", s.listBackupsHandler)
	auth("POST /api/nodes/{node}/backups/import", s.needs("backups", s.importBackup))
	auth("POST /api/nodes/{node}/backups/{pool}/{name}/restore", s.needs("backups", s.restoreBackup))
	auth("GET /api/nodes/{node}/backups/{pool}/{name}/export", s.needs("backups", s.exportBackup))
	auth("DELETE /api/nodes/{node}/backups/{pool}/{name}", s.needs("backups", s.deleteBackup))

	auth("GET /api/nodes/{node}/pools", s.listPools)
	auth("POST /api/nodes/{node}/pools", s.createPool)
	auth("POST /api/nodes/{node}/pools/{pool}/action", s.poolAction)
	auth("GET /api/nodes/{node}/pools/{pool}/volumes", s.listVolumes)
	auth("POST /api/nodes/{node}/pools/{pool}/volumes", s.createVolume)
	auth("POST /api/nodes/{node}/pools/{pool}/volumes/download", s.needs("writePools", s.downloadVolume))
	auth("PUT /api/nodes/{node}/pools/{pool}/volumes/{vol}", s.uploadVolume)
	auth("POST /api/nodes/{node}/pools/{pool}/volumes/{vol}/action", s.volumeAction)
	auth("DELETE /api/nodes/{node}/pools/{pool}/volumes/{vol}", s.deleteVolume)
	auth("GET /api/nodes/{node}/pools/{pool}/volumes/{vol}/firmware", s.volumeFirmware)
	auth("GET /api/images", s.listImages)
	auth("GET /api/nodes/{node}/pools/{pool}/images", s.listPoolImages)
	auth("POST /api/nodes/{node}/pools/{pool}/images", s.needs("writePools", s.downloadImage))
	auth("POST /api/nodes/{node}/pools/{pool}/images/cancel", s.cancelImage)

	auth("GET /api/nodes/{node}/networks", s.listNetworks)
	auth("POST /api/nodes/{node}/networks", s.createNetwork)
	auth("GET /api/nodes/{node}/networks/{net}", s.getNetwork)
	auth("PUT /api/nodes/{node}/networks/{net}/xml", s.putNetworkXML)
	auth("POST /api/nodes/{node}/networks/{net}/action", s.networkAction)
	auth("POST /api/nodes/{node}/networks/{net}/dhcp-hosts", s.dhcpHostAdd)
	auth("PUT /api/nodes/{node}/networks/{net}/dhcp-hosts/{mac}", s.dhcpHostUpdate)
	auth("DELETE /api/nodes/{node}/networks/{net}/dhcp-hosts/{mac}", s.dhcpHostDelete)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	mux.Handle("/", s.spa())
	return s.wrap(mux)
}

// spa serves the built UI and falls back to index.html for client-side routes.
func (s *Server) spa() http.Handler {
	files := http.FileServerFS(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.Static, p); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

func fail(w http.ResponseWriter, err error) {
	var ac accepted
	if errors.As(err, &ac) { // not a failure: the work goes on as a background task
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"accepted": true, "task": ac.Task})
		return
	}
	code := http.StatusInternalServerError
	var lv libvirt.Error
	var br badRequest
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, cluster.ErrOffline):
		code = http.StatusServiceUnavailable
	case errors.Is(err, hostconn.ErrNoPrivilege):
		code = http.StatusConflict
	case errors.As(err, &br):
		code = http.StatusBadRequest
	case errors.As(err, &lv):
		// libvirt rejected the request; its message is what the user needs.
		code = http.StatusUnprocessableEntity
		if lv.Code == uint32(libvirt.ErrNoDomain) || lv.Code == uint32(libvirt.ErrNoStoragePool) || lv.Code == uint32(libvirt.ErrNoStorageVol) || lv.Code == uint32(libvirt.ErrNoNetwork) {
			code = http.StatusNotFound
		}
	}
	if code == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v); err != nil {
		return badRequest("invalid request body: " + err.Error())
	}
	return nil
}

func userOf(r *http.Request) string {
	u, _ := r.Context().Value(ctxKey{}).(string)
	return u
}

func (s *Server) host(r *http.Request) (int64, *hostconn.Host, error) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		return 0, nil, store.ErrNotFound
	}
	h, err := s.Nodes.Host(id)
	return id, h, err
}

func (s *Server) domain(r *http.Request) (*hostconn.Host, libvirt.Domain, error) {
	_, h, err := s.host(r)
	if err != nil {
		return nil, libvirt.Domain{}, err
	}
	u, err := cluster.ParseUUID(r.PathValue("uuid"))
	if err != nil {
		return nil, libvirt.Domain{}, store.ErrNotFound
	}
	dom, err := h.DomainLookupByUUID(u)
	return h, dom, err
}

// task runs fn, recording it in the task log and announcing start and finish
// to connected UIs. fn may report progress in the range 0..1.
func (s *Server) task(r *http.Request, kind, target, node string, fn func(progress func(float64)) error) error {
	t, err := s.Store.StartTask(kind, target, node, userOf(r))
	if err != nil {
		return err
	}
	s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
	last := 0.0
	err = fn(func(p float64) {
		if p-last >= 0.02 {
			last = p
			s.Store.TaskProgress(t.ID, p)
			t.Progress = p
			s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
		}
	})
	s.Store.FinishTask(&t, err)
	s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
	return err
}

// accepted is returned by taskAsync when the work was started in the background; fail answers it with 202.
type accepted struct{ Task int64 }

func (a accepted) Error() string { return "accepted as a background task" }

// taskAsync is s.task for work that can take long and whose handler needs nothing from its result. A request
// with ?async=1 (the web UI) gets its answer as soon as the task is recorded: 202 with the task id, and the
// outcome arrives through the task log and the event stream. Without it the call waits, as scripts expect.
// fn must not use the request: it may run after the handler has returned.
func (s *Server) taskAsync(r *http.Request, kind, target, node string, fn func(progress func(float64)) error) error {
	if r == nil || r.URL == nil || r.URL.Query().Get("async") != "1" {
		return s.task(r, kind, target, node, fn)
	}
	id, err := s.startBackground(r, kind, target, node, fn)
	if err != nil {
		return err
	}
	return accepted{Task: id}
}

// withJobProgress runs call and, while it is in flight, reports the domain job's progress (a snapshot that
// saves memory, for example). Never reports 1: the caller's return finishes the task.
func withJobProgress(h *hostconn.Host, dom libvirt.Domain, progress func(float64), call func() error) error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			jt, ps, err := h.DomainGetJobStats(dom, 0)
			if err != nil || jt == 0 {
				continue
			}
			st := map[string]float64{}
			for _, tp := range ps {
				st[tp.Field] = toFloat(tp.Value.I)
			}
			total, got := st["data_total"], st["data_processed"]
			if total == 0 {
				total, got = st["memory_total"], st["memory_processed"]
			}
			if total > 0 {
				progress(min(0.99, got/total))
			}
		}
	}
}

// --- session ---

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, err)
		return
	}
	user, role := "", ""
	if c, err := r.Cookie(cookieName); err == nil {
		if p, err := s.Store.SessionUser(c.Value); err == nil {
			user, role = p.User.Username, p.Role
		}
	}
	writeJSON(w, map[string]any{"needsSetup": n == 0, "user": user, "role": role})
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, buildinfo.Get())
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Tasks(100)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, t)
}
