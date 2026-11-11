package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"virticle/internal/cluster"
	"virticle/internal/store"
)

const testPW = "correct horse battery"

type env struct {
	t       *testing.T
	srv     *Server
	h       http.Handler
	cookies map[string]string // username -> session cookie value
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := &Server{
		Store:  st,
		Nodes:  cluster.NewManager(filepath.Join(dir, "key"), filepath.Join(dir, "known_hosts")),
		Static: fstest.MapFS{"index.html": {Data: []byte("<html></html>")}},
	}
	return &env{t: t, srv: srv, h: srv.Handler(), cookies: map[string]string{}}
}

// user creates a user and signs it in, remembering the session cookie.
func (e *env) user(name, role string) store.User {
	e.t.Helper()
	u, err := e.srv.Store.CreateUser(name, testPW, role)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, _, err := e.srv.Store.Login(name, testPW)
	if err != nil {
		e.t.Fatal(err)
	}
	e.cookies[name] = tok
	return u
}

type opt func(*http.Request)

func cookieOf(e *env, user string) opt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: e.cookies[user]}) }
}
func bearer(tok string) opt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func header(k, v string) opt { return func(r *http.Request) { r.Header.Set(k, v) } }

func (e *env) do(method, path string, body any, opts ...opt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "192.0.2.7:4444"
	for _, o := range opts {
		if o != nil {
			o(r)
		}
	}
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != nil {
				e.t.Errorf("%s %s panicked: %v", method, path, p)
			}
		}()
		e.h.ServeHTTP(w, r)
	}()
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON %q: %v", w.Body.String(), err)
	}
}

func TestEveryRouteHasALevel(t *testing.T) {
	e := newEnv(t)
	registered := map[string]bool{}
	for _, rt := range e.srv.routes {
		if rt.public {
			continue
		}
		registered[rt.pattern] = true
		lvl, ok := routeLevels[rt.pattern]
		if !ok {
			t.Errorf("route %q has no entry in routeLevels (it would require admin)", rt.pattern)
			continue
		}
		method, path, _ := strings.Cut(rt.pattern, " ")
		account := strings.HasPrefix(path, "/api/account/") || strings.HasPrefix(path, "/api/me/")
		if method != "GET" && !account && store.RoleRank(lvl) < store.RoleRank(operator) {
			t.Errorf("%s lets a viewer change things", rt.pattern)
		}
		if auditedGets[rt.pattern] && store.RoleRank(lvl) < store.RoleRank(operator) {
			t.Errorf("%s is a console and must need operator", rt.pattern)
		}
		if strings.HasSuffix(path, "/vnc") || strings.HasSuffix(path, "/serial") {
			if !auditedGets[rt.pattern] {
				t.Errorf("%s is a console and must be audited", rt.pattern)
			}
		}
		if (strings.HasPrefix(path, "/api/users") || strings.HasPrefix(path, "/api/audit") || (path == "/api/nodes" && method != "GET") || (strings.HasPrefix(path, "/api/nodes/{node}") && strings.Count(path, "/") == 3 && method != "GET")) && lvl != admin {
			t.Errorf("%s must be admin only, is %s", rt.pattern, lvl)
		}
	}
	for p := range routeLevels {
		if !registered[p] {
			t.Errorf("routeLevels lists %q, which is not a registered route", p)
		}
	}
	if len(registered) < 60 {
		t.Errorf("only %d routes registered; the table walk is not seeing the mux", len(registered))
	}
}

func TestRoleMatrix(t *testing.T) {
	e := newEnv(t)
	for _, r := range []string{viewer, operator, admin} {
		e.user(r, r)
	}
	for _, rt := range e.srv.routes {
		if rt.public {
			continue
		}
		method, path, _ := strings.Cut(rt.pattern, " ")
		for _, ph := range []string{"{node}", "{uuid}", "{id}", "{target}", "{mac}", "{name}", "{pool}", "{vol}"} {
			path = strings.ReplaceAll(path, ph, "99999")
		}
		need := levelFor(rt.pattern)
		for _, role := range []string{viewer, operator, admin} {
			var body any
			if method != "GET" && method != "DELETE" {
				body = map[string]any{}
			}
			w := e.do(method, path, body, cookieOf(e, role))
			wantDenied := store.RoleRank(role) < store.RoleRank(need)
			switch {
			case wantDenied && w.Code != http.StatusForbidden:
				t.Errorf("%s as %s: got %d, want 403", rt.pattern, role, w.Code)
			case !wantDenied && (w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized):
				t.Errorf("%s as %s: got %d, want the handler to run", rt.pattern, role, w.Code)
			}
		}
		if w := e.do(method, path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: got %d, want 401", rt.pattern, w.Code)
		}
	}
}

func TestViewerCannotMutate(t *testing.T) {
	e := newEnv(t)
	e.user("v", viewer)
	for _, rt := range e.srv.routes {
		method, _, _ := strings.Cut(rt.pattern, " ")
		if rt.public || method == "GET" || strings.Contains(rt.pattern, "/account/") || strings.Contains(rt.pattern, "/api/me/") {
			continue
		}
		if store.RoleRank(levelFor(rt.pattern)) <= store.RoleRank(viewer) {
			t.Errorf("viewer may call %s", rt.pattern)
		}
	}
}

func TestOperatorCannotManageUsersOrNodes(t *testing.T) {
	e := newEnv(t)
	e.user("op", operator)
	for _, c := range []struct{ m, p string }{
		{"POST", "/api/users"}, {"PUT", "/api/users/1"}, {"DELETE", "/api/users/1"}, {"GET", "/api/users"}, {"GET", "/api/audit"},
		{"POST", "/api/nodes"}, {"DELETE", "/api/nodes/1"},
	} {
		if w := e.do(c.m, c.p, map[string]any{}, cookieOf(e, "op")); w.Code != 403 {
			t.Errorf("operator %s %s: %d", c.m, c.p, w.Code)
		}
	}
	if w := e.do("GET", "/api/vms", nil, cookieOf(e, "op")); w.Code != 200 {
		t.Errorf("operator list VMs: %d", w.Code)
	}
}

func TestTokens(t *testing.T) {
	e := newEnv(t)
	u := e.user("ann", admin)

	// Created through the API with a session; shown once.
	w := e.do("POST", "/api/account/tokens", map[string]any{"name": "ci", "role": "viewer"}, cookieOf(e, "ann"))
	if w.Code != 200 {
		t.Fatalf("create token: %d %s", w.Code, w.Body)
	}
	var created struct {
		Secret string
		Token  store.APIToken
	}
	decode(t, w, &created)
	if !strings.HasPrefix(created.Secret, "virticle_") || created.Token.Role != viewer {
		t.Fatalf("unexpected token %+v", created)
	}
	list := e.do("GET", "/api/account/tokens", nil, cookieOf(e, "ann"))
	if strings.Contains(list.Body.String(), created.Secret) {
		t.Error("the token secret is listed after creation")
	}

	// A viewer token reads but cannot write or manage users, even for an admin.
	if w := e.do("GET", "/api/vms", nil, bearer(created.Secret)); w.Code != 200 {
		t.Errorf("viewer token GET: %d", w.Code)
	}
	if w := e.do("POST", "/api/nodes", map[string]any{}, bearer(created.Secret)); w.Code != 403 {
		t.Errorf("viewer token POST nodes: %d", w.Code)
	}
	if w := e.do("POST", "/api/nodes/1/vms/x/action", map[string]any{}, bearer(created.Secret)); w.Code != 403 {
		t.Errorf("viewer token VM action: %d", w.Code)
	}
	// Tokens cannot mint tokens or change the password.
	if w := e.do("POST", "/api/account/tokens", map[string]any{"name": "x"}, bearer(created.Secret)); w.Code != 403 {
		t.Errorf("token minting a token: %d", w.Code)
	}
	// A bad bearer token is refused even with a valid cookie.
	if w := e.do("GET", "/api/vms", nil, bearer("vp_nope"), cookieOf(e, "ann")); w.Code != 401 {
		t.Errorf("bad bearer with cookie: %d", w.Code)
	}

	// A token cannot exceed its owner's role.
	e.user("bob", operator)
	if w := e.do("POST", "/api/account/tokens", map[string]any{"name": "x", "role": "admin"}, cookieOf(e, "bob")); w.Code != 400 {
		t.Errorf("operator minting an admin token: %d", w.Code)
	}

	// Full-role token works; demoting the owner demotes the token.
	_, full, _ := e.srv.Store.CreateToken(u.ID, "full", admin, 0)
	if w := e.do("GET", "/api/users", nil, bearer(full)); w.Code != 200 {
		t.Errorf("admin token GET users: %d", w.Code)
	}
	e.user("root2", admin) // so ann may be demoted
	op := operator
	if err := e.srv.Store.UpdateUser(u.ID, store.UserUpdate{Role: &op}); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/api/users", nil, bearer(full)); w.Code != 403 {
		t.Errorf("admin token of a demoted user: %d", w.Code)
	}

	// Expiry and disabling.
	_, expired, _ := e.srv.Store.CreateToken(u.ID, "old", viewer, time.Now().Add(-time.Minute).Unix())
	if w := e.do("GET", "/api/vms", nil, bearer(expired)); w.Code != 401 {
		t.Errorf("expired token: %d", w.Code)
	}
	_, soon, _ := e.srv.Store.CreateToken(u.ID, "soon", viewer, time.Now().Add(time.Hour).Unix())
	if w := e.do("GET", "/api/vms", nil, bearer(soon)); w.Code != 200 {
		t.Errorf("unexpired token: %d", w.Code)
	}
	dis := true
	e.srv.Store.UpdateUser(u.ID, store.UserUpdate{Disabled: &dis})
	if w := e.do("GET", "/api/vms", nil, bearer(soon)); w.Code != 401 {
		t.Errorf("token of a disabled user: %d", w.Code)
	}

	// Revoking.
	if w := e.do("DELETE", "/api/account/tokens/"+itoa(created.Token.ID), nil, cookieOf(e, "bob")); w.Code != 404 {
		t.Errorf("deleting someone else's token: %d", w.Code)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestLastAdminProtected(t *testing.T) {
	e := newEnv(t)
	a := e.user("root", admin)
	url := "/api/users/" + itoa(a.ID)
	if w := e.do("DELETE", url, nil, cookieOf(e, "root")); w.Code != 409 {
		t.Errorf("deleting the last admin: %d", w.Code)
	}
	if w := e.do("PUT", url, map[string]any{"role": "operator"}, cookieOf(e, "root")); w.Code != 409 {
		t.Errorf("demoting the last admin: %d", w.Code)
	}
	if w := e.do("PUT", url, map[string]any{"disabled": true}, cookieOf(e, "root")); w.Code != 409 {
		t.Errorf("disabling the last admin: %d", w.Code)
	}
	// A disabled admin does not count as another admin.
	b := e.user("second", admin)
	e.do("PUT", "/api/users/"+itoa(b.ID), map[string]any{"disabled": true}, cookieOf(e, "root"))
	if w := e.do("DELETE", url, nil, cookieOf(e, "root")); w.Code != 409 {
		t.Errorf("deleting the only enabled admin: %d", w.Code)
	}
	e.do("PUT", "/api/users/"+itoa(b.ID), map[string]any{"disabled": false}, cookieOf(e, "root"))
	if w := e.do("PUT", url, map[string]any{"role": "viewer"}, cookieOf(e, "root")); w.Code != 200 {
		t.Errorf("demoting with another admin present: %d %s", w.Code, w.Body)
	}
}

func TestUserManagement(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	c := cookieOf(e, "root")
	if w := e.do("POST", "/api/users", map[string]any{"username": "x", "password": "short", "role": "viewer"}, c); w.Code != 400 {
		t.Errorf("short password: %d", w.Code)
	}
	if w := e.do("POST", "/api/users", map[string]any{"username": "x y", "password": testPW, "role": "viewer"}, c); w.Code != 400 {
		t.Errorf("bad username: %d", w.Code)
	}
	if w := e.do("POST", "/api/users", map[string]any{"username": "x", "password": testPW, "role": "god"}, c); w.Code != 400 {
		t.Errorf("bad role: %d", w.Code)
	}
	var u store.User
	w := e.do("POST", "/api/users", map[string]any{"username": "carol", "password": testPW, "role": "viewer"}, c)
	decode(t, w, &u)
	if w.Code != 200 || u.Role != viewer {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/users", map[string]any{"username": "carol", "password": testPW, "role": "viewer"}, c); w.Code != 409 {
		t.Errorf("duplicate: %d", w.Code)
	}
	if w := e.do("POST", "/api/login", credentials{"carol", testPW}); w.Code != 200 {
		t.Errorf("login: %d", w.Code)
	}
	e.do("PUT", "/api/users/"+itoa(u.ID), map[string]any{"disabled": true}, c)
	if w := e.do("POST", "/api/login", credentials{"carol", testPW}); w.Code != 401 {
		t.Errorf("login while disabled: %d", w.Code)
	}
	newPW := "another long password"
	e.do("PUT", "/api/users/"+itoa(u.ID), map[string]any{"disabled": false, "password": newPW}, c)
	if w := e.do("POST", "/api/login", credentials{"carol", newPW}); w.Code != 200 {
		t.Errorf("login with reset password: %d", w.Code)
	}
}

func TestChangePasswordEndsOtherSessions(t *testing.T) {
	e := newEnv(t)
	e.user("ann", admin)
	other, _, _ := e.srv.Store.Login("ann", testPW)
	w := e.do("POST", "/api/account/password", map[string]any{"current": "wrong wrong wrong", "new": "brand new password"}, cookieOf(e, "ann"))
	if w.Code != 400 {
		t.Errorf("wrong current password: %d", w.Code)
	}
	w = e.do("POST", "/api/account/password", map[string]any{"current": testPW, "new": "short"}, cookieOf(e, "ann"))
	if w.Code != 400 {
		t.Errorf("short new password: %d", w.Code)
	}
	w = e.do("POST", "/api/account/password", map[string]any{"current": testPW, "new": "brand new password"}, cookieOf(e, "ann"))
	if w.Code != 200 {
		t.Fatalf("change: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/vms", nil, cookieOf(e, "ann")); w.Code != 200 {
		t.Errorf("own session after change: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/vms", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: other})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Errorf("other session after change: %d", rec.Code)
	}
}

func TestCSRFOriginCheck(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	c := cookieOf(e, "root")
	body := map[string]any{"username": "u1", "password": testPW, "role": "viewer"}
	cases := []struct {
		name string
		opts []opt
		want int
	}{
		{"no headers (curl)", nil, 200},
		{"same origin", []opt{header("Origin", "http://example.com")}, 200},
		{"same origin fetch metadata", []opt{header("Sec-Fetch-Site", "same-origin")}, 200},
		{"cross origin", []opt{header("Origin", "https://evil.example")}, 403},
		{"null origin", []opt{header("Origin", "null")}, 403},
		{"cross-site fetch metadata", []opt{header("Sec-Fetch-Site", "cross-site")}, 403},
		{"same-site fetch metadata", []opt{header("Sec-Fetch-Site", "same-site")}, 403},
		{"cross origin wins over same-origin metadata", []opt{header("Sec-Fetch-Site", "same-origin"), header("Origin", "https://evil.example")}, 403},
	}
	for i, tc := range cases {
		body["username"] = "u" + itoa(int64(i))
		w := e.do("POST", "/api/users", body, append([]opt{c}, tc.opts...)...)
		if w.Code != tc.want {
			t.Errorf("%s: got %d want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
	}
	// Safe methods are not checked here (WebSockets check Origin themselves).
	if w := e.do("GET", "/api/users", nil, c, header("Origin", "https://evil.example")); w.Code != 200 {
		t.Errorf("GET with foreign origin: %d", w.Code)
	}
	// Bearer requests carry no ambient credentials.
	_, tok, _ := e.srv.Store.CreateToken(1, "t", admin, 0)
	if w := e.do("POST", "/api/users", map[string]any{"username": "viaToken", "password": testPW, "role": "viewer"}, bearer(tok), header("Origin", "https://evil.example")); w.Code != 200 {
		t.Errorf("bearer with foreign origin: %d", w.Code)
	}
}

func TestWebSocketOriginRefused(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	ts := httptest.NewServer(e.h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/events"
	dial := func(origin string) error {
		h := http.Header{"Cookie": {cookieName + "=" + e.cookies["root"]}}
		if origin != "" {
			h.Set("Origin", origin)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
		if err == nil {
			c.CloseNow()
		}
		return err
	}
	if err := dial("https://evil.example"); err == nil {
		t.Error("cross-origin WebSocket was accepted")
	}
	if err := dial(ts.URL); err != nil {
		t.Errorf("same-origin WebSocket refused: %v", err)
	}
	if err := dial(""); err != nil {
		t.Errorf("WebSocket without Origin refused: %v", err)
	}
}

func TestAuditLog(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	e.user("view", viewer)
	secret := "S3cret-body-value"
	e.do("POST", "/api/login", credentials{"root", "wrong password!!"})
	e.do("POST", "/api/users", map[string]any{"username": "zed", "password": secret + secret, "role": "viewer"}, cookieOf(e, "root"))
	e.do("GET", "/api/vms", nil, cookieOf(e, "root"))
	e.do("POST", "/api/nodes", map[string]any{"addr": secret}, cookieOf(e, "view")) // 403, still recorded
	e.do("POST", "/api/nodes", map[string]any{}, nil)                               // unauthenticated: not recorded
	_, tok, _ := e.srv.Store.CreateToken(1, "ci", admin, 0)
	e.do("DELETE", "/api/users/99999", nil, bearer(tok))

	w := e.do("GET", "/api/audit", nil, cookieOf(e, "root"))
	if w.Code != 200 {
		t.Fatalf("audit: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Error("a request body leaked into the audit log")
	}
	var res struct {
		Entries []store.AuditEntry
	}
	decode(t, w, &res)
	var got []string
	for _, a := range res.Entries {
		got = append(got, a.User+"|"+a.Token+"|"+a.Method+" "+a.Path+"|"+itoa(int64(a.Status)))
		if a.IP != "192.0.2.7" || a.Time == 0 {
			t.Errorf("bad entry %+v", a)
		}
	}
	want := []string{
		"root|ci|DELETE /api/users/99999|404",
		"view||POST /api/nodes|403",
		"root||POST /api/users|200",
		"root||POST /api/login|401",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if w := e.do("GET", "/api/audit", nil, cookieOf(e, "view")); w.Code != 403 {
		t.Errorf("viewer reading the audit log: %d", w.Code)
	}
	// Filters and paging.
	w = e.do("GET", "/api/audit?user=view", nil, cookieOf(e, "root"))
	decode(t, w, &res)
	if len(res.Entries) != 1 || res.Entries[0].User != "view" {
		t.Errorf("user filter: %+v", res.Entries)
	}
	var page struct {
		Entries []store.AuditEntry
		Next    int64
	}
	w = e.do("GET", "/api/audit?limit=2", nil, cookieOf(e, "root"))
	decode(t, w, &page)
	if len(page.Entries) != 2 || page.Next == 0 {
		t.Fatalf("paging: %+v", page)
	}
	w = e.do("GET", "/api/audit?limit=2&before="+itoa(page.Next), nil, cookieOf(e, "root"))
	decode(t, w, &page)
	if len(page.Entries) == 0 {
		t.Errorf("second page: %+v", page)
	}
}

func TestSetupAndLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/api/setup", credentials{"admin", "short"}); w.Code != 400 {
		t.Errorf("short setup password: %d", w.Code)
	}
	if w := e.do("POST", "/api/setup", credentials{"admin", testPW}); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/setup", credentials{"admin2", testPW}); w.Code != 400 {
		t.Errorf("second setup: %d", w.Code)
	}
	for i := 0; i < limPair; i++ {
		if w := e.do("POST", "/api/login", credentials{"admin", "wrong password"}); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := e.do("POST", "/api/login", credentials{"admin", testPW})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Errorf("locked login: %d %v", w.Code, w.Header())
	}
	// Another user from the same address is not locked by the pair rule.
	if w := e.do("POST", "/api/login", credentials{"nobody", "x"}); w.Code != 401 {
		t.Errorf("other user: %d", w.Code)
	}
}

func TestSecurityHeadersAndCookie(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/api/setup", credentials{"admin", testPW})

	w := e.do("GET", "/", nil)
	h := w.Header()
	for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Referrer-Policy"} {
		if h.Get(k) == "" {
			t.Errorf("missing %s", k)
		}
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP: %s", h.Get("Content-Security-Policy"))
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}

	login := func(tlsOn bool) *http.Cookie {
		b, _ := json.Marshal(credentials{"admin", testPW})
		r := httptest.NewRequest("POST", "/api/login", bytes.NewReader(b))
		r.RemoteAddr = "198.51.100.1:1"
		if tlsOn {
			r.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		if tlsOn && rec.Header().Get("Strict-Transport-Security") == "" {
			t.Error("no HSTS over TLS")
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == cookieName {
				return c
			}
		}
		t.Fatal("no session cookie")
		return nil
	}
	if c := login(false); c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("plain cookie: %+v", c)
	}
	if c := login(true); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("TLS cookie: %+v", c)
	}
}

func TestStatusReportsRole(t *testing.T) {
	e := newEnv(t)
	e.user("op", operator)
	var st struct{ User, Role string }
	decode(t, e.do("GET", "/api/status", nil, cookieOf(e, "op")), &st)
	if st.User != "op" || st.Role != operator {
		t.Errorf("status: %+v", st)
	}
}
