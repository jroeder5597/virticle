package api

import (
	"net/http"
	"net/netip"
	"time"

	"virticle/internal/webaccess"
)

// webView is what the request path needs from the web access settings; it is
// swapped atomically whenever the settings change.
type webView struct {
	proxies   []netip.Prefix
	proxySet  bool // mode proxy: these replace the -trusted-proxy flag
	hsts      bool
	publicURL string
}

// UseWebSettings tells the server which web access settings are in force.
func (s *Server) UseWebSettings(ws webaccess.Settings) {
	v := &webView{hsts: ws.HSTS, publicURL: ws.PublicHost()}
	if ws.Mode == webaccess.ModeProxy {
		v.proxies, v.proxySet = ws.Trusted(), true
	}
	s.webv.Store(v)
}

// trustedProxies is the list of peers whose forwarding headers are believed:
// the configured ones in proxy mode, else those of the -trusted-proxy flag.
func (s *Server) trustedProxies() []netip.Prefix {
	if v := s.webv.Load(); v != nil && v.proxySet {
		return v.proxies
	}
	return s.TrustedProxies
}

func (s *Server) webExtras() (publicHost string, hsts bool) {
	if v := s.webv.Load(); v != nil {
		return v.publicURL, v.hsts
	}
	return "", false
}

func (s *Server) webRoutes(auth func(string, http.HandlerFunc)) {
	auth("GET /api/web", s.webStatus)
	auth("POST /api/web/check", s.webCheck)
	auth("PUT /api/web", s.webApply)
	auth("POST /api/web/confirm", s.webConfirm)
	auth("POST /api/web/revert", s.webRevert)
	auth("POST /api/web/snippets", s.webSnippets)
	auth("POST /api/web/renew", s.webRenew)
	auth("POST /api/web/acme/request", s.webACMERequest)
}

type webRequest struct {
	Settings webaccess.Settings `json:"settings"`
	// CertPEM and KeyPEM are a pasted or uploaded certificate chain and key
	// for mode own. They are never returned.
	CertPEM string `json:"certPem"`
	KeyPEM  string `json:"keyPem"`
}

func (s *Server) webManager(w http.ResponseWriter) *webaccess.Manager {
	if s.Web == nil {
		http.Error(w, `{"error":"web access settings are not available"}`, http.StatusServiceUnavailable)
		return nil
	}
	return s.Web
}

func (s *Server) webEnv(r *http.Request) webaccess.CheckEnv {
	env := webaccess.CheckEnv{Now: time.Now(), Host: r.Host, ForwardedProto: r.Header.Get("X-Forwarded-Proto")}
	if a, err := netip.ParseAddr(directView(r).IP); err == nil {
		env.Peer = a
	}
	return env
}

func (s *Server) webStatus(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	st := m.Status(r.Host)
	writeJSON(w, map[string]any{
		"status":         st,
		"onNewListener":  st.Pending != nil && webaccess.GenOf(r) == st.Pending.Gen,
		"tls":            viewOf(r).HTTPS,
		"snippets":       webaccess.Snippets(st.Settings),
		"suggestedNames": webaccess.SuggestedNames(),
	})
}

func (s *Server) webCheck(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	var q webRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	cs := m.Check(webaccess.Request{Settings: q.Settings, OwnCertPEM: []byte(q.CertPEM), OwnKeyPEM: []byte(q.KeyPEM)}, s.webEnv(r))
	writeJSON(w, map[string]any{"checks": cs, "ok": !webaccess.HasError(cs)})
}

func (s *Server) webApply(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	var q webRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	cs, p, err := m.Apply(webaccess.Request{Settings: q.Settings, OwnCertPEM: []byte(q.CertPEM), OwnKeyPEM: []byte(q.KeyPEM)}, s.webEnv(r))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		msg := err.Error()
		if err == webaccess.ErrInvalid {
			msg = "the settings did not pass the checks"
		}
		writeJSON(w, map[string]any{"error": msg, "checks": cs})
		return
	}
	writeJSON(w, map[string]any{"checks": cs, "pending": p})
}

func (s *Server) webConfirm(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	if err := m.Confirm(webaccess.GenOf(r)); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) webRevert(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	if m.Status("").Pending == nil {
		fail(w, badRequest("nothing is waiting for confirmation"))
		return
	}
	// The undo may arrive through the listener it closes: answer first.
	go func() {
		time.Sleep(300 * time.Millisecond)
		m.Revert()
	}()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) webSnippets(w http.ResponseWriter, r *http.Request) {
	var q webRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"snippets": webaccess.Snippets(q.Settings.Normalize())})
}

func (s *Server) webRenew(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	ci, err := m.Renew()
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, map[string]any{"cert": ci})
}

func (s *Server) webACMERequest(w http.ResponseWriter, r *http.Request) {
	m := s.webManager(w)
	if m == nil {
		return
	}
	if err := m.RequestACME(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"ok": true})
}
