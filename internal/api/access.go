package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"virticle/internal/store"
)

// route is one registered pattern; Handler records them so a test can check
// that every route has an access level.
type route struct {
	pattern string
	public  bool // reachable without signing in
}

const (
	viewer   = store.RoleViewer
	operator = store.RoleOperator
	admin    = store.RoleAdmin
)

// routeLevels is the role a route needs. It is the single place access is
// decided: requireAuth enforces it before any handler runs. A route that is
// not listed requires admin (default deny), and TestEveryRouteHasALevel fails
// until it is listed on purpose.
//
// Rule of thumb: viewer is GET only and never a console (a console is
// control); operator manages VMs, storage and networks; admin also manages
// nodes, users and the audit log.
var routeLevels = map[string]string{
	"GET /api/web":               admin,
	"POST /api/web/check":        admin,
	"PUT /api/web":               admin,
	"POST /api/web/confirm":      admin,
	"POST /api/web/revert":       admin,
	"POST /api/web/snippets":     admin,
	"POST /api/web/renew":        admin,
	"POST /api/web/acme/request": admin,

	"GET /api/controller-backup":           admin,
	"PUT /api/controller-backup":           admin,
	"POST /api/controller-backup/download": admin,

	"GET /api/version": viewer,
	"GET /api/events":  viewer,
	"GET /api/tasks":   viewer,
	"GET /api/alerts":  viewer,

	"GET /api/notify/destinations":            admin,
	"POST /api/notify/destinations":           admin,
	"PUT /api/notify/destinations/{id}":       admin,
	"DELETE /api/notify/destinations/{id}":    admin,
	"POST /api/notify/destinations/{id}/test": admin,

	"GET /api/nodes":                         viewer,
	"GET /api/local-node":                    admin,
	"GET /api/cluster/cpu-baseline":          viewer,
	"POST /api/nodes":                        admin,
	"DELETE /api/nodes/{node}":               admin,
	"PUT /api/nodes/{node}":                  admin,
	"POST /api/nodes/{node}/test-connection": admin,
	"POST /api/nodes/{node}/forget-host-key": admin,
	"POST /api/prepare":                      admin,
	"GET /api/prepare/{task}":                admin,
	"GET /api/nodes/{node}":                  viewer,

	"POST /api/nodes/{node}/evacuate/check":  operator,
	"POST /api/nodes/{node}/evacuate":        admin,
	"GET /api/nodes/{node}/evacuate":         viewer,
	"POST /api/nodes/{node}/evacuate/cancel": admin,
	"PUT /api/nodes/{node}/maintenance":      admin,
	"GET /api/nodes/{node}/updates":          viewer,
	"GET /api/nodes/{node}/interfaces":       viewer,

	// Host bridges change the network the controller reaches the node over: admin, even for reading the plan.
	"GET /api/nodes/{node}/hostnet":           admin,
	"POST /api/nodes/{node}/hostnet/plan":     admin,
	"POST /api/nodes/{node}/hostnet/apply":    admin,
	"POST /api/nodes/{node}/hostnet/rollback": admin,
	"GET /api/nodes/{node}/hostnet/peers":     admin,

	"POST /api/users/{id}/reset-2fa": admin,
	"GET /api/signin":                admin,
	"PUT /api/signin":                admin,
	"POST /api/signin/oidc/test":     admin,
	"POST /api/signin/ldap/test":     admin,

	"GET /api/users":         admin,
	"POST /api/users":        admin,
	"PUT /api/users/{id}":    admin,
	"DELETE /api/users/{id}": admin,
	"GET /api/audit":         admin,

	// Own account: every role, session only (see accountSession).
	"GET /api/account/2fa":                 viewer,
	"POST /api/account/2fa/begin":          viewer,
	"POST /api/account/2fa/confirm":        viewer,
	"POST /api/account/2fa/disable":        viewer,
	"POST /api/account/2fa/recovery-codes": viewer,
	"POST /api/account/password":           viewer,
	"GET /api/account/tokens":              viewer,
	"POST /api/account/tokens":             viewer,
	"DELETE /api/account/tokens/{id}":      viewer,

	// Own UI preferences: every role, session only.
	"GET /api/me/prefs": viewer,
	"PUT /api/me/prefs": viewer,

	// Profiles hold password hashes and keys: even reading them needs operator.
	"GET /api/cloudinit-profiles":         operator,
	"POST /api/cloudinit-profiles":        operator,
	"PUT /api/cloudinit-profiles/{id}":    operator,
	"DELETE /api/cloudinit-profiles/{id}": operator,

	"GET /api/vms":                                    viewer,
	"GET /api/pools":                                  viewer,
	"GET /api/storage/shared":                         viewer,
	"POST /api/storage/shared":                        operator,
	"DELETE /api/storage/shared/{name}":               operator,
	"GET /api/networks":                               viewer,
	"GET /api/topology":                               viewer,
	"GET /api/cluster/network-drift":                  viewer,
	"POST /api/nodes/{node}/vms":                      operator,
	"GET /api/nodes/{node}/vms/{uuid}":                viewer,
	"POST /api/nodes/{node}/vms/{uuid}/action":        operator,
	"PUT /api/nodes/{node}/vms/{uuid}/xml":            operator,
	"PUT /api/nodes/{node}/vms/{uuid}/autostart":      operator,
	"POST /api/nodes/{node}/vms/{uuid}/agent-channel": operator,
	"DELETE /api/nodes/{node}/vms/{uuid}":             operator,
	"PUT /api/nodes/{node}/vms/{uuid}/template":       operator,
	"POST /api/nodes/{node}/vms/{uuid}/seal":          operator,
	"GET /api/nodes/{node}/vms/{uuid}/clone-info":     viewer,
	"POST /api/nodes/{node}/vms/{uuid}/clone":         operator,
	"GET /api/nodes/{node}/vms/{uuid}/vnc":            operator,
	"GET /api/nodes/{node}/vms/{uuid}/serial":         operator,
	"GET /api/nodes/{node}/vms/{uuid}/screenshot":     operator,
	"POST /api/nodes/{node}/vms/{uuid}/sendkey":       operator,

	"POST /api/nodes/{node}/vms/{uuid}/serial-console":   operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/serial-console": operator,
	"GET /api/nodes/{node}/vms/{uuid}/clock":             viewer,
	"PUT /api/nodes/{node}/vms/{uuid}/clock":             operator,
	"POST /api/nodes/{node}/vms/{uuid}/clock/sync":       operator,

	"PUT /api/nodes/{node}/vms/{uuid}/cpu":                         operator,
	"PUT /api/nodes/{node}/vms/{uuid}/memory":                      operator,
	"POST /api/nodes/{node}/vms/{uuid}/disks":                      operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/disks/{target}":           operator,
	"POST /api/nodes/{node}/vms/{uuid}/disks/{target}/resize":      operator,
	"GET /api/nodes/{node}/vms/{uuid}/disks/{target}/move/plan":    viewer,
	"POST /api/nodes/{node}/vms/{uuid}/disks/{target}/move":        operator,
	"POST /api/nodes/{node}/vms/{uuid}/disks/{target}/move/cancel": operator,
	"POST /api/nodes/{node}/vms/{uuid}/cdrom":                      operator,
	"PUT /api/nodes/{node}/vms/{uuid}/cdrom/{target}":              operator,
	"POST /api/nodes/{node}/vms/{uuid}/nics":                       operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/nics/{mac}":               operator,
	"PUT /api/nodes/{node}/vms/{uuid}/nics/{mac}":                  operator,
	"PUT /api/nodes/{node}/vms/{uuid}/boot":                        operator,
	"GET /api/nodes/{node}/vms/{uuid}/tuning":                      viewer,
	"PUT /api/nodes/{node}/vms/{uuid}/tuning/pinning":              operator,
	"PUT /api/nodes/{node}/vms/{uuid}/tuning/cpu":                  operator,
	"PUT /api/nodes/{node}/vms/{uuid}/tuning/hugepages":            operator,
	"PUT /api/nodes/{node}/vms/{uuid}/tuning/numa":                 operator,
	"GET /api/nodes/{node}/vms/{uuid}/hostdevs":                    viewer,
	"POST /api/nodes/{node}/vms/{uuid}/hostdevs":                   operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/hostdevs/{id}":            operator,
	"POST /api/nodes/{node}/vms/{uuid}/migrate/check":              operator,
	"POST /api/nodes/{node}/vms/{uuid}/migrate":                    operator,
	"POST /api/nodes/{node}/vms/{uuid}/migrate/cancel":             operator,
	"GET /api/nodes/{node}/migration-network":                      viewer,
	"PUT /api/nodes/{node}/migration-network":                      admin,
	"GET /api/nodes/{node}/migration-tls":                          viewer,
	"POST /api/nodes/{node}/migration-tls":                         admin,
	"DELETE /api/nodes/{node}/migration-tls":                       admin,

	"GET /api/nodes/{node}/vms/{uuid}/snapshots":                viewer,
	"GET /api/nodes/{node}/vms/{uuid}/snapshot-options":         viewer,
	"POST /api/nodes/{node}/vms/{uuid}/snapshots":               operator,
	"POST /api/nodes/{node}/vms/{uuid}/snapshots/{name}/revert": operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/snapshots/{name}":      operator,
	"POST /api/nodes/{node}/vms/{uuid}/snapshots/delete":        operator,
	"POST /api/nodes/{node}/vms/{uuid}/snapshots/delete-plan":   operator,
	"GET /api/nodes/{node}/vms/{uuid}/snapshot-schedule":        viewer,
	"PUT /api/nodes/{node}/vms/{uuid}/snapshot-schedule":        operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/snapshot-schedule":     operator,

	"GET /api/nodes/{node}/vms/{uuid}/backup-schedule":    viewer,
	"PUT /api/nodes/{node}/vms/{uuid}/backup-schedule":    operator,
	"DELETE /api/nodes/{node}/vms/{uuid}/backup-schedule": operator,
	"POST /api/nodes/{node}/vms/{uuid}/backup":            operator,
	"POST /api/nodes/{node}/vms/{uuid}/backup/cancel":     operator,
	"GET /api/backups":                                          viewer,
	"POST /api/nodes/{node}/backups/import":                     operator,
	"POST /api/nodes/{node}/backups/{pool}/{name}/restore":      operator,
	"GET /api/nodes/{node}/backups/{pool}/{name}/export":        operator, // the export holds the disk contents
	"DELETE /api/nodes/{node}/backups/{pool}/{name}":            operator,
	"GET /api/nodes/{node}/pools":                               viewer,
	"POST /api/nodes/{node}/pools":                              operator,
	"POST /api/nodes/{node}/pools/{pool}/action":                operator,
	"GET /api/nodes/{node}/pools/{pool}/volumes":                viewer,
	"GET /api/nodes/{node}/pools/{pool}/volumes/{vol}/firmware": viewer,
	"POST /api/nodes/{node}/pools/{pool}/volumes":               operator,
	"POST /api/nodes/{node}/pools/{pool}/volumes/download":      operator,
	"PUT /api/nodes/{node}/pools/{pool}/volumes/{vol}":          operator,
	"POST /api/nodes/{node}/pools/{pool}/volumes/{vol}/action":  operator,
	"DELETE /api/nodes/{node}/pools/{pool}/volumes/{vol}":       operator,
	"GET /api/images": viewer,
	"GET /api/nodes/{node}/pools/{pool}/images":                viewer,
	"POST /api/nodes/{node}/pools/{pool}/images":               operator,
	"POST /api/nodes/{node}/pools/{pool}/images/cancel":        operator,
	"GET /api/nodes/{node}/networks":                           viewer,
	"PUT /api/nodes/{node}/vms/{uuid}/tpm":                     operator,
	"GET /api/tags":                                            viewer,
	"PUT /api/tags/{name}/color":                               operator,
	"DELETE /api/tags/{name}/color":                            operator,
	"PUT /api/nodes/{node}/vms/{uuid}/tags":                    operator,
	"GET /api/nodes/{node}/firmware-caps":                      viewer,
	"POST /api/nodes/{node}/import/inspect":                    operator,
	"POST /api/nodes/{node}/import":                            operator,
	"GET /api/stats":                                           viewer,
	"GET /api/nodes/{node}/stats":                              viewer,
	"GET /api/nodes/{node}/vms/{uuid}/stats":                   viewer,
	"GET /api/metrics/query":                                   viewer,
	"GET /api/metrics/overview":                                viewer,
	"GET /metrics":                                             viewer,
	"GET /api/nodes/{node}/vms/{uuid}/guest":                   viewer,
	"GET /api/nodes/{node}/vms/{uuid}/agent-help":              viewer,
	"POST /api/nodes/{node}/networks":                          operator,
	"GET /api/nodes/{node}/networks/{net}":                     viewer,
	"PUT /api/nodes/{node}/networks/{net}/xml":                 operator,
	"POST /api/nodes/{node}/networks/{net}/action":             operator,
	"POST /api/nodes/{node}/networks/{net}/dhcp-hosts":         operator,
	"PUT /api/nodes/{node}/networks/{net}/dhcp-hosts/{mac}":    operator,
	"DELETE /api/nodes/{node}/networks/{net}/dhcp-hosts/{mac}": operator,
}

// auditedGets are read-only routes that are still recorded: opening a console
// is a control action.
var auditedGets = map[string]bool{
	// A sign-in through the identity provider ends here; it is a GET only because the browser is redirected back.
	"GET /api/oidc/callback":                  true,
	"GET /api/nodes/{node}/vms/{uuid}/vnc":    true,
	"GET /api/nodes/{node}/vms/{uuid}/serial": true,
	// An export hands out disk contents.
	"GET /api/nodes/{node}/backups/{pool}/{name}/export": true,
}

// levelFor returns the role a pattern needs; unlisted patterns need admin.
func levelFor(pattern string) string {
	if l, ok := routeLevels[pattern]; ok {
		return l
	}
	return admin
}

// reqInfo is filled in as a request moves through the middleware and read
// back when the request is audited.
type reqInfo struct {
	user, token string
	pattern     string
	login       bool   // a login, setup or logout request: audited even without a session
	session     string // cookie session token; empty for bearer-token requests
	userID      int64
	detail      string // what the request did, for the audit log (never a secret)
}

type infoKey struct{}

func infoOf(r *http.Request) *reqInfo {
	i, _ := r.Context().Value(infoKey{}).(*reqInfo)
	if i == nil {
		i = &reqInfo{} // handler used without wrap; nothing reads it
	}
	return i
}

type principalKey struct{}

// authenticate resolves the caller from a bearer token or the session cookie.
// A bearer header that does not check out is an error; it never falls back to
// the cookie.
func (s *Server) authenticate(r *http.Request) (store.Principal, string, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return store.Principal{}, "", store.ErrNotFound
		}
		p, err := s.Store.TokenUser(strings.TrimSpace(tok))
		return p, "", err
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return store.Principal{}, "", store.ErrNotFound
	}
	p, err := s.Store.SessionUser(c.Value)
	return p, c.Value, err
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}

func (s *Server) requireAuth(pattern string, next http.HandlerFunc) http.HandlerFunc {
	need := levelFor(pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		info := infoOf(r)
		info.pattern = pattern
		p, session, err := s.authenticate(r)
		if err != nil {
			deny(w, http.StatusUnauthorized, "not signed in")
			return
		}
		info.user, info.token, info.session, info.userID = p.User.Username, p.Token, session, p.User.ID
		if session != "" && !twoStepRoutes[pattern] && s.mustSetUpTwoStep(p.User) {
			deny(w, http.StatusForbidden, "two-step sign-in must be set up first: open the Account page")
			return
		}
		if store.RoleRank(p.Role) < store.RoleRank(need) {
			deny(w, http.StatusForbidden, fmt.Sprintf("forbidden: this needs the %s role, you are %s", need, p.Role))
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, p.User.Username)
		ctx = context.WithValue(ctx, principalKey{}, p)
		next(w, r.WithContext(ctx))
	}
}

func principalOf(r *http.Request) store.Principal {
	p, _ := r.Context().Value(principalKey{}).(store.Principal)
	return p
}

// --- middleware: headers, CSRF check, audit ---

var hostRe = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]{1,260}$`)

func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	connect := "'self'"
	v := viewOf(r)
	if hostRe.MatchString(v.Host) {
		// Older browsers do not let 'self' cover WebSockets. Behind a trusted
		// proxy the scheme is known, so only the matching one is listed.
		switch {
		case !v.Forwarded:
			connect += " ws://" + v.Host + " wss://" + v.Host
		case v.HTTPS:
			connect += " wss://" + v.Host
		default:
			connect += " ws://" + v.Host
		}
	}
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data: blob:; font-src 'self' data:; connect-src "+connect+"; worker-src 'self' blob:; "+
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	if v.HTTPS && v.HSTS {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
	}
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossSite reports whether a browser says the request came from another
// site. Non-browser clients send neither header and pass.
func crossSite(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		v := viewOf(r)
		if err != nil || u.Host == "" || !(strings.EqualFold(u.Host, v.Host) || (v.Public != "" && strings.EqualFold(u.Host, v.Public))) {
			return true
		}
	}
	return false
}

// recorder remembers the status for the audit log and keeps WebSocket
// upgrades working by passing Hijack and Flush through.
type recorder struct {
	http.ResponseWriter
	status   int
	onHijack func()
}

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijacking not supported")
	}
	c, rw, err := hj.Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
		w.onHijack() // a console can stay open for hours; record it when it opens
	}
	return c, rw, err
}

func (s *Server) wrap(next http.Handler) http.Handler {
	if s.limiter == nil {
		s.limiter = newLimiter()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withView(r, s.computeView(r))
		securityHeaders(w, r)
		if !isSafeMethod(r.Method) && r.Header.Get("Authorization") == "" && crossSite(r) {
			deny(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		info := &reqInfo{}
		r = r.WithContext(context.WithValue(r.Context(), infoKey{}, info))
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		var once sync.Once
		record := func() { once.Do(func() { s.audit(r, info, rec.status, start) }) }
		rec.onHijack = record
		defer record()
		next.ServeHTTP(rec, r)
	})
}

// audit records who asked for what. It never sees request bodies or queries.
func (s *Server) audit(r *http.Request, info *reqInfo, status int, start time.Time) {
	if status == 0 {
		status = http.StatusOK
	}
	if isSafeMethod(r.Method) && !auditedGets[info.pattern] {
		return
	}
	if info.user == "" && !info.login {
		return // unauthenticated noise would only push real entries out
	}
	if status == http.StatusTooManyRequests {
		return
	}
	err := s.Store.AddAudit(store.AuditEntry{
		Time: start.Unix(), User: info.user, Token: info.token, IP: viewOf(r).IP, Method: r.Method, Path: r.URL.Path, Status: status, Detail: info.detail,
	})
	if err != nil {
		logf("audit: %v", err)
	}
}
