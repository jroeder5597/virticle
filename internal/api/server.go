// Package api is the HTTP layer: a JSON REST API, WebSockets for events and
// consoles, and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/cluster"
	"virt-proj/internal/hostconn"
	"virt-proj/internal/store"
)

const cookieName = "virt_session"

type Server struct {
	Store  *store.Store
	Nodes  *cluster.Manager
	PubKey string // controller's SSH public key, shown when adding a node
	Static fs.FS  // built web UI
}

type ctxKey struct{}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	pub := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	auth := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAuth(h)) }

	pub("GET /api/status", s.status)
	pub("POST /api/setup", s.setup)
	pub("POST /api/login", s.login)
	pub("POST /api/logout", s.logout)

	auth("GET /api/events", s.events)
	auth("GET /api/tasks", s.tasks)

	auth("GET /api/nodes", s.listNodes)
	auth("POST /api/nodes", s.addNode)
	auth("DELETE /api/nodes/{node}", s.deleteNode)
	auth("GET /api/nodes/{node}", s.nodeInfo)

	auth("GET /api/vms", s.listVMs)
	auth("POST /api/nodes/{node}/vms", s.createVM)
	auth("GET /api/nodes/{node}/vms/{uuid}", s.vmDetail)
	auth("POST /api/nodes/{node}/vms/{uuid}/action", s.vmAction)
	auth("PUT /api/nodes/{node}/vms/{uuid}/xml", s.vmSetXML)
	auth("PUT /api/nodes/{node}/vms/{uuid}/autostart", s.vmAutostart)
	auth("POST /api/nodes/{node}/vms/{uuid}/agent-channel", s.vmAddAgentChannel)
	auth("DELETE /api/nodes/{node}/vms/{uuid}", s.vmDelete)
	auth("GET /api/nodes/{node}/vms/{uuid}/vnc", s.vmVNC)
	auth("GET /api/nodes/{node}/vms/{uuid}/serial", s.vmSerial)

	auth("POST /api/nodes/{node}/vms/{uuid}/migrate/check", s.migrateCheck)
	auth("POST /api/nodes/{node}/vms/{uuid}/migrate", s.migrate)
	auth("POST /api/nodes/{node}/vms/{uuid}/migrate/cancel", s.migrateCancel)

	auth("GET /api/nodes/{node}/pools", s.listPools)
	auth("POST /api/nodes/{node}/pools", s.createPool)
	auth("POST /api/nodes/{node}/pools/{pool}/action", s.poolAction)
	auth("GET /api/nodes/{node}/pools/{pool}/volumes", s.listVolumes)
	auth("POST /api/nodes/{node}/pools/{pool}/volumes", s.createVolume)
	auth("PUT /api/nodes/{node}/pools/{pool}/volumes/{vol}", s.uploadVolume)
	auth("POST /api/nodes/{node}/pools/{pool}/volumes/{vol}/action", s.volumeAction)
	auth("DELETE /api/nodes/{node}/pools/{pool}/volumes/{vol}", s.deleteVolume)

	auth("GET /api/nodes/{node}/networks", s.listNetworks)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	mux.Handle("/", s.spa())
	return mux
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
	code := http.StatusInternalServerError
	var lv libvirt.Error
	var br badRequest
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, cluster.ErrOffline):
		code = http.StatusServiceUnavailable
	case errors.As(err, &br):
		code = http.StatusBadRequest
	case errors.As(err, &lv):
		// libvirt rejected the request; its message is what the user needs.
		code = http.StatusUnprocessableEntity
		if lv.Code == uint32(libvirt.ErrNoDomain) || lv.Code == uint32(libvirt.ErrNoStoragePool) || lv.Code == uint32(libvirt.ErrNoStorageVol) {
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

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			http.Error(w, `{"error":"not signed in"}`, http.StatusUnauthorized)
			return
		}
		user, err := s.Store.SessionUser(c.Value)
		if err != nil {
			http.Error(w, `{"error":"not signed in"}`, http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	}
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

// --- session ---

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, err)
		return
	}
	user := ""
	if c, err := r.Cookie(cookieName); err == nil {
		user, _ = s.Store.SessionUser(c.Value)
	}
	writeJSON(w, map[string]any{"needsSetup": n == 0, "user": user})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// setup creates the first administrator. It only works while no user exists.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := readJSON(r, &c); err != nil {
		fail(w, err)
		return
	}
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, err)
		return
	}
	if n > 0 {
		fail(w, badRequest("setup has already been completed"))
		return
	}
	if c.Username == "" || len(c.Password) < 8 {
		fail(w, badRequest("a username and a password of at least 8 characters are required"))
		return
	}
	if err := s.Store.CreateUser(c.Username, c.Password); err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, c)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := readJSON(r, &c); err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, c)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, c credentials) {
	token, err := s.Store.Login(c.Username, c.Password)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"wrong username or password"}`)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 7 * 24 * 3600,
	})
	writeJSON(w, map[string]string{"user": c.Username})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Store.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Tasks(100)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, t)
}
