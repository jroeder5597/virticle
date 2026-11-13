package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"virticle/internal/dirauth/ldaptest"
	"virticle/internal/oidcauth/oidctest"
	"virticle/internal/store"
	"virticle/internal/totp"
)

// signInEnv is an env with a data directory (for the secret key) and a clock tests can move.
func signInEnv(t *testing.T) (*env, *time.Time) {
	t.Helper()
	e := newEnv(t)
	e.srv.DataDir = t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	e.srv.TOTPClock = func() time.Time { return now }
	return e, &now
}

func sessionCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func (e *env) as(cookie string) opt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie}) }
}

// turnOnTwoStep sets it up for the signed-in user through the API and returns the secret and recovery codes.
func (e *env) turnOnTwoStep(cookie string, now time.Time) (secret string, codes []string) {
	e.t.Helper()
	w := e.do("POST", "/api/account/2fa/begin", nil, e.as(cookie))
	var b struct {
		Secret, URI string
		QR          struct {
			Size int
			Path string
		}
	}
	decode(e.t, w, &b)
	if w.Code != 200 || b.Secret == "" || !strings.HasPrefix(b.URI, "otpauth://totp/Virticle:") || b.QR.Size < 20 || b.QR.Path == "" {
		e.t.Fatalf("begin: %d %s", w.Code, w.Body)
	}
	code, _ := totp.Code(b.Secret, totp.Step(now))
	w = e.do("POST", "/api/account/2fa/confirm", map[string]string{"code": code}, e.as(cookie))
	var c struct{ Codes []string }
	decode(e.t, w, &c)
	if w.Code != 200 || len(c.Codes) != 10 {
		e.t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	return b.Secret, c.Codes
}

func codeAt(secret string, t time.Time) string {
	c, _ := totp.Code(secret, totp.Step(t))
	return c
}

func TestTwoStepSignIn(t *testing.T) {
	e, now := signInEnv(t)
	e.user("admin", admin)
	u := e.user("alice", operator)
	secret, codes := e.turnOnTwoStep(e.cookies["alice"], *now)

	// The seed is sealed in the database and never shown again.
	if ts, err := e.srv.Store.TwoStep(u.ID); err != nil || ts.Secret == "" || strings.Contains(ts.Secret, secret) {
		t.Fatalf("stored seed %q (%v)", ts.Secret, err)
	}
	if w := e.do("GET", "/api/account/2fa", nil, e.as(e.cookies["alice"])); strings.Contains(w.Body.String(), secret) {
		t.Fatal("the seed is shown again")
	}

	// Step one alone opens no session.
	w := e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW})
	var l struct {
		Second    bool
		Challenge string
	}
	decode(t, w, &l)
	if w.Code != 200 || !l.Second || l.Challenge == "" || sessionCookie(w) != "" {
		t.Fatalf("step one: %d %s", w.Code, w.Body)
	}
	// The code that confirmed the set-up is spent; a wrong one fails; the next step's code works once.
	for _, bad := range []string{codeAt(secret, *now), "000000", "", "abc"} {
		w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": bad})
		if w.Code != 401 || sessionCookie(w) != "" {
			t.Fatalf("code %q: %d %s", bad, w.Code, w.Body)
		}
	}
	*now = now.Add(30 * time.Second)
	good := codeAt(secret, *now)
	w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": good})
	cookie := sessionCookie(w)
	if w.Code != 200 || cookie == "" {
		t.Fatalf("good code: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/version", nil, e.as(cookie)); w.Code != 200 {
		t.Fatalf("session: %d", w.Code)
	}
	// Replay: the same code with a new challenge is refused.
	w = e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW})
	decode(t, w, &l)
	if w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": good}); w.Code != 401 {
		t.Fatalf("replay: %d", w.Code)
	}
	// A recovery code works once, in any spelling, and the audit log says so.
	rc := strings.ToUpper(codes[0])
	w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": " " + rc + " "})
	if w.Code != 200 || sessionCookie(w) == "" {
		t.Fatalf("recovery: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW})
	decode(t, w, &l)
	if w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": codes[0]}); w.Code != 401 {
		t.Fatalf("recovery reuse: %d", w.Code)
	}
	var seen = map[string]bool{}
	entries, _ := e.srv.Store.Audit(store.AuditFilter{User: "alice", Limit: 100})
	for _, en := range entries {
		seen[en.Detail] = true
	}
	for _, want := range []string{"two-step sign-in turned on", "second step failed", "recovery code used (9 left)"} {
		if !seen[want] {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}
	// A wrong password never reaches step two.
	if w = e.do("POST", "/api/login", map[string]string{"username": "alice", "password": "wrong password!"}); w.Code != 401 || strings.Contains(w.Body.String(), "challenge") {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body)
	}
}

func TestSecondStepFailuresAreRateLimited(t *testing.T) {
	e, now := signInEnv(t)
	e.user("alice", operator)
	secret, _ := e.turnOnTwoStep(e.cookies["alice"], *now)
	*now = now.Add(30 * time.Second)
	var l struct{ Challenge string }
	for i := 0; i < limPair; i++ {
		w := e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW})
		decode(t, w, &l)
		if w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": "111111"}); w.Code != 401 {
			t.Fatalf("try %d: %d", i, w.Code)
		}
	}
	// Locked now: even the right password and the right code get a 429.
	if w := e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW}); w.Code != 429 {
		t.Fatalf("after %d failed codes: %d", limPair, w.Code)
	}
	_ = secret
}

func TestChallengeBurnsAfterFiveWrongCodes(t *testing.T) {
	e, now := signInEnv(t)
	e.user("alice", operator)
	secret, _ := e.turnOnTwoStep(e.cookies["alice"], *now)
	e.srv.limiter = newLimiter() // isolate from the per-user lock
	w := e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW})
	var l struct{ Challenge string }
	decode(t, w, &l)
	*now = now.Add(30 * time.Second)
	for i := 0; i < maxChallengeFails; i++ {
		e.srv.limiter = newLimiter()
		e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": "111111"})
	}
	e.srv.limiter = newLimiter()
	if w = e.do("POST", "/api/login/second", map[string]string{"challenge": l.Challenge, "code": codeAt(secret, *now)}); w.Code != 401 || sessionCookie(w) != "" {
		t.Fatalf("a burnt challenge accepted the right code: %d", w.Code)
	}
}

func TestTurnOffAndAdminReset(t *testing.T) {
	e, now := signInEnv(t)
	e.user("admin", admin)
	e.user("alice", operator)
	secret, codes := e.turnOnTwoStep(e.cookies["alice"], *now)
	*now = now.Add(30 * time.Second)
	off := func(pw, code string) int {
		return e.do("POST", "/api/account/2fa/disable", map[string]string{"password": pw, "code": code}, e.as(e.cookies["alice"])).Code
	}
	if c := off("wrong password!", codeAt(secret, *now)); c != 400 {
		t.Fatalf("wrong password: %d", c)
	}
	if c := off(testPW, "123456"); c != 400 {
		t.Fatalf("wrong code: %d", c)
	}
	if c := off(testPW, codeAt(secret, *now)); c != 200 {
		t.Fatalf("right both: %d", c)
	}
	if w := e.do("POST", "/api/login", map[string]string{"username": "alice", "password": testPW}); w.Code != 200 || strings.Contains(w.Body.String(), "challenge") {
		t.Fatalf("after off: %d %s", w.Code, w.Body)
	}

	// Admin reset: the user lost the phone.
	*now = now.Add(60 * time.Second)
	e.turnOnTwoStep(e.cookies["alice"], *now)
	u, _ := e.srv.Store.UserByName("alice")
	if !u.TwoStep {
		t.Fatal("not on")
	}
	if w := e.do("POST", "/api/users/"+itoa(u.ID)+"/reset-2fa", nil, e.as(e.cookies["alice"])); w.Code != 403 {
		t.Fatalf("a user resetting herself through the admin route: %d", w.Code)
	}
	if w := e.do("POST", "/api/users/"+itoa(u.ID)+"/reset-2fa", nil, e.as(e.cookies["admin"])); w.Code != 200 {
		t.Fatalf("reset: %d", w.Code)
	}
	if w := e.do("GET", "/api/version", nil, e.as(e.cookies["alice"])); w.Code != 401 {
		t.Fatal("a reset must end the user's sessions")
	}
	u, _ = e.srv.Store.UserByName("alice")
	if u.TwoStep {
		t.Fatal("still on")
	}
	_ = codes
	// The list shows who has it on.
	var us []store.User
	decode(t, e.do("GET", "/api/users", nil, e.as(e.cookies["admin"])), &us)
	for _, x := range us {
		if x.TwoStep {
			t.Errorf("%s shows two-step on after the reset", x.Username)
		}
	}
}

func TestRequiredPolicy(t *testing.T) {
	cases := []struct {
		require string
		user    store.User
		want    bool
	}{
		{requireNobody, store.User{Role: admin, Source: store.SourceLocal}, false},
		{requireAdmins, store.User{Role: admin, Source: store.SourceLocal}, true},
		{requireAdmins, store.User{Role: operator, Source: store.SourceLocal}, false},
		{requireAdmins, store.User{Role: admin, Source: store.SourceLDAP}, true},
		{requireEveryone, store.User{Role: viewer, Source: store.SourceLocal}, true},
		{requireEveryone, store.User{Role: viewer, Source: store.SourceLDAP}, true},
		{requireEveryone, store.User{Role: admin, Source: store.SourceOIDC}, false}, // the provider asked
		{requireAdmins, store.User{Role: admin, Source: store.SourceOIDC}, false},
		{"", store.User{Role: admin, Source: store.SourceLocal}, false},
	}
	for _, c := range cases {
		if got := twoStepRequired(c.require, c.user); got != c.want {
			t.Errorf("%s / %s %s: %v want %v", c.require, c.user.Source, c.user.Role, got, c.want)
		}
	}
}

func putSignIn(e *env, cookie string, body map[string]any) *httptest.ResponseRecorder {
	return e.do("PUT", "/api/signin", body, e.as(cookie))
}

func TestRequiredTwoStepForcesSetup(t *testing.T) {
	e, now := signInEnv(t)
	e.user("admin", admin)
	e.user("op", operator)
	ac := e.cookies["admin"]
	// Only admins must have it. The admin's existing session is limited at once; the operator is not.
	if w := putSignIn(e, ac, map[string]any{"require2fa": "admins"}); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/version", nil, e.as(e.cookies["op"])); w.Code != 200 {
		t.Fatalf("operator: %d", w.Code)
	}
	if w := e.do("GET", "/api/nodes", nil, e.as(ac)); w.Code != 403 {
		t.Fatalf("admin without two-step may still work: %d", w.Code)
	}
	var st struct{ TwoStepSetup bool }
	decode(t, e.do("GET", "/api/status", nil, e.as(ac)), &st)
	if !st.TwoStepSetup {
		t.Fatal("status does not send the admin to the set-up page")
	}
	// Signing in again gives a session that can set up and do nothing else, tokens included.
	w := e.do("POST", "/api/login", map[string]string{"username": "admin", "password": testPW})
	var l struct{ Setup bool }
	decode(t, w, &l)
	fresh := sessionCookie(w)
	if !l.Setup || fresh == "" {
		t.Fatalf("login: %s", w.Body)
	}
	if w := e.do("POST", "/api/account/tokens", map[string]any{"name": "x"}, e.as(fresh)); w.Code != 403 {
		t.Fatalf("a token from a session that skipped the second step: %d", w.Code)
	}
	if w := e.do("POST", "/api/account/2fa/disable", map[string]any{}, e.as(fresh)); w.Code != 403 {
		t.Fatalf("turning it off while required: %d", w.Code)
	}
	e.turnOnTwoStep(fresh, *now)
	if w := e.do("GET", "/api/nodes", nil, e.as(fresh)); w.Code != 200 {
		t.Fatalf("after set-up: %d", w.Code)
	}
	if w := e.do("POST", "/api/account/tokens", map[string]any{"name": "x"}, e.as(fresh)); w.Code != 200 {
		t.Fatalf("token after set-up: %d %s", w.Code, w.Body)
	}
	// API tokens are for machines: unaffected.
	_, secret, _ := e.srv.Store.CreateToken(1, "machine", admin, 0)
	e.srv.Store.ResetTwoStep(1)
	if w := e.do("GET", "/api/nodes", nil, bearer(secret)); w.Code != 200 {
		t.Fatalf("token while the user must set up two-step: %d", w.Code)
	}
}

func TestSettingsAreAuditedAndSecretsHidden(t *testing.T) {
	e, _ := signInEnv(t)
	e.user("admin", admin)
	ac := e.cookies["admin"]
	w := putSignIn(e, ac, map[string]any{"require2fa": "nobody",
		"oidc": map[string]any{"enabled": true, "name": "Test IdP", "issuer": "https://idp.example.org", "clientId": "cid", "clientSecret": "SUPER-SECRET-VALUE"},
		"ldap": map[string]any{"enabled": true, "url": "ldaps://dc.example.org", "bindDn": "cn=svc", "baseDn": "dc=example,dc=org", "bindPassword": "BIND-PASSWORD-VALUE"}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, body := range []string{w.Body.String(), e.do("GET", "/api/signin", nil, e.as(ac)).Body.String(), e.do("GET", "/api/status", nil).Body.String()} {
		if strings.Contains(body, "SUPER-SECRET") || strings.Contains(body, "BIND-PASSWORD") {
			t.Fatalf("a secret leaked: %s", body)
		}
	}
	var v signInView
	decode(t, e.do("GET", "/api/signin", nil, e.as(ac)), &v)
	if !v.OIDC.ClientSecretSet || !v.LDAP.BindPasswordSet || v.OIDC.RedirectURL != "http://example.com/api/oidc/callback" {
		t.Errorf("%+v", v)
	}
	// Nothing in the database holds the plain secrets either.
	for _, k := range []string{signInKey, oidcSecretKey, ldapSecretKey} {
		raw, _, _ := e.srv.Store.WebSetting(k)
		if strings.Contains(raw, "SUPER-SECRET") || strings.Contains(raw, "BIND-PASSWORD") {
			t.Errorf("%s holds a plain secret", k)
		}
	}
	// A save without a new secret keeps the old one.
	putSignIn(e, ac, map[string]any{"require2fa": "nobody", "oidc": map[string]any{"enabled": true, "issuer": "https://idp.example.org", "clientId": "cid2"}})
	if s, _ := e.srv.sealedSecret(oidcSecretKey); s != "SUPER-SECRET-VALUE" {
		t.Errorf("secret lost: %q", s)
	}
	entries, _ := e.srv.Store.Audit(store.AuditFilter{Method: "PUT", Limit: 10})
	if len(entries) < 2 || !strings.Contains(entries[0].Detail, "sign-in settings changed") || strings.Contains(entries[1].Detail, "SECRET") {
		t.Errorf("audit: %+v", entries)
	}
	// Refusals: bad issuer, enabling without a secret, a non-admin.
	for name, body := range map[string]map[string]any{
		"http issuer":  {"require2fa": "nobody", "oidc": map[string]any{"enabled": true, "issuer": "http://idp.example.org", "clientId": "c", "clientSecret": "s"}},
		"bad policy":   {"require2fa": "sometimes"},
		"plain filter": {"require2fa": "nobody", "ldap": map[string]any{"enabled": true, "url": "ldaps://x", "baseDn": "dc=x", "userFilter": "(uid=x)"}},
	} {
		if w := putSignIn(e, ac, body); w.Code != 400 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	e.user("op", operator)
	if w := e.do("GET", "/api/signin", nil, e.as(e.cookies["op"])); w.Code != 403 {
		t.Errorf("operator reads sign-in settings: %d", w.Code)
	}
}

// --- identity provider ---

func oidcEnv(t *testing.T) (*env, *oidctest.Provider) {
	t.Helper()
	e, _ := signInEnv(t)
	e.user("admin", admin)
	tp := oidctest.New()
	t.Cleanup(tp.Close)
	w := putSignIn(e, e.cookies["admin"], map[string]any{"require2fa": "nobody",
		"oidc": map[string]any{"enabled": true, "name": "Test IdP", "issuer": tp.URL(), "clientId": oidctest.ClientID, "clientSecret": oidctest.ClientSecret,
			"adminGroup": "virticle-admins", "operatorGroup": "virticle-ops", "defaultRole": "none"}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return e, tp
}

// oidcLogin walks the flow the way a browser does and returns the callback's answer.
func oidcLogin(t *testing.T, e *env) *httptest.ResponseRecorder {
	t.Helper()
	w := e.do("GET", "/api/oidc/start", nil)
	if w.Code != 302 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var state string
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcCookie {
			state = c.Value
		}
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	return e.do("GET", "/api/oidc/callback?"+back.RawQuery, nil, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: oidcCookie, Value: state}) })
}

func TestOIDCSignInAndRoleMapping(t *testing.T) {
	e, tp := oidcEnv(t)
	var st map[string]any
	decode(t, e.do("GET", "/api/status", nil), &st)
	if st["oidc"] != "Test IdP" {
		t.Fatalf("status: %v", st)
	}
	cur := e.srv.signIn()
	cur.Require2FA = requireEveryone
	e.srv.saveSignIn(cur) // even so, the provider's own second factor is the one that counts
	tp.Claims = map[string]any{"sub": "u-1", "preferred_username": "erin", "groups": []string{"virticle-ops", "other"}}
	w := oidcLogin(t, e)
	if w.Code != 302 || w.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s", w.Code, w.Header().Get("Location"))
	}
	c := sessionCookie(w)
	var me struct {
		User, Role, Source string
		TwoStepSetup       bool
	}
	decode(t, e.do("GET", "/api/status", nil, e.as(c)), &me)
	if me.User != "erin" || me.Role != operator || me.Source != "oidc" {
		t.Fatalf("%+v", me)
	}
	if me.TwoStepSetup {
		t.Error("the local two-step is not asked of someone who signed in through the provider, even when required for everyone")
	}
	// The session is no longer than 12 hours.
	for _, ck := range w.Result().Cookies() {
		if ck.Name == cookieName && ck.MaxAge > 12*3600 {
			t.Errorf("cookie lives %d s", ck.MaxAge)
		}
	}
	// Promoted at the next sign-in.
	tp.Claims["groups"] = []string{"virticle-admins"}
	w = oidcLogin(t, e)
	decode(t, e.do("GET", "/api/status", nil, e.as(sessionCookie(w))), &me)
	if me.Role != admin {
		t.Fatalf("promotion: %+v", me)
	}
	// Removed from every group: refused, and the session they had ends at once.
	tp.Claims["groups"] = []string{"other"}
	w = oidcLogin(t, e)
	if sessionCookie(w) != "" || !strings.Contains(w.Header().Get("Location"), "signin_error") {
		t.Fatalf("no-group user: %d %s", w.Code, w.Header().Get("Location"))
	}
	if r := e.do("GET", "/api/version", nil, e.as(c)); r.Code != 401 {
		t.Errorf("old session survives the loss of the group: %d", r.Code)
	}
	// "Everyone else gets Viewer" is a setting.
	cur.Require2FA = requireNobody
	e.srv.saveSignIn(cur)
	putSignIn(e, e.cookies["admin"], map[string]any{"require2fa": "nobody",
		"oidc": map[string]any{"enabled": true, "name": "Test IdP", "issuer": tp.URL(), "clientId": oidctest.ClientID, "adminGroup": "virticle-admins", "defaultRole": "viewer"}})
	w = oidcLogin(t, e)
	decode(t, e.do("GET", "/api/status", nil, e.as(sessionCookie(w))), &me)
	if me.Role != viewer {
		t.Fatalf("default viewer: %+v", me)
	}
	// The account has no password and its role cannot be edited by hand.
	u, _ := e.srv.Store.UserByName("erin")
	for _, body := range []map[string]any{{"role": "admin"}, {"password": "a new long password"}} {
		if r := e.do("PUT", "/api/users/"+itoa(u.ID), body, e.as(e.cookies["admin"])); r.Code != 400 {
			t.Errorf("editing an external account: %d", r.Code)
		}
	}
	if _, _, err := e.srv.Store.Login("erin", "!"); err == nil {
		t.Error("an external account signed in locally")
	}
	var list []store.User
	decode(t, e.do("GET", "/api/users", nil, e.as(e.cookies["admin"])), &list)
	found := false
	for _, x := range list {
		found = found || (x.Username == "erin" && x.Source == "oidc")
	}
	if !found {
		t.Error("the Users list does not show the source")
	}
}

func TestOIDCRefusals(t *testing.T) {
	e, tp := oidcEnv(t)
	tp.Claims = map[string]any{"sub": "u-2", "preferred_username": "frank", "groups": []string{"virticle-admins"}}
	for name, set := range map[string]func(){
		"audience": func() { tp.Audience = "other-app" },
		"issuer":   func() { tp.Issuer = "https://elsewhere.example.org" },
		"nonce":    func() { tp.Nonce = "old" },
		"expired":  func() { tp.Expired = true },
	} {
		tp.Audience, tp.Issuer, tp.Nonce, tp.Expired = "", "", "", false
		set()
		w := oidcLogin(t, e)
		if sessionCookie(w) != "" || !strings.Contains(w.Header().Get("Location"), "signin_error") {
			t.Errorf("%s: accepted (%d %s)", name, w.Code, w.Header().Get("Location"))
		}
	}
	tp.Audience, tp.Issuer, tp.Nonce, tp.Expired = "", "", "", false
	// A callback that was not started by this browser (wrong or missing state cookie) is refused.
	w := e.do("GET", "/api/oidc/start", nil)
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := cl.Get(w.Header().Get("Location"))
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	for _, ck := range []*http.Cookie{nil, {Name: oidcCookie, Value: "forged"}} {
		w = e.do("GET", "/api/oidc/callback?"+back.RawQuery, nil, func(r *http.Request) {
			if ck != nil {
				r.AddCookie(ck)
			}
		})
		if sessionCookie(w) != "" || !strings.Contains(w.Header().Get("Location"), "signin_error") {
			t.Errorf("login CSRF accepted with cookie %v", ck)
		}
	}
	// A local account keeps its name: the provider cannot take it over.
	tp.Claims = map[string]any{"sub": "u-3", "preferred_username": "admin", "groups": []string{"virticle-admins"}}
	if w := oidcLogin(t, e); sessionCookie(w) != "" {
		t.Error("the provider signed in as the local administrator")
	}
	// Turned off: the start page says so.
	putSignIn(e, e.cookies["admin"], map[string]any{"require2fa": "nobody"})
	if w := e.do("GET", "/api/oidc/start", nil); !strings.Contains(w.Header().Get("Location"), "signin_error") {
		t.Error("start works while the provider is off")
	}
}

func TestOIDCTestButtonAndLogout(t *testing.T) {
	e, tp := oidcEnv(t)
	tp.EndSession = true
	w := e.do("POST", "/api/signin/oidc/test", map[string]any{"issuer": tp.URL()}, e.as(e.cookies["admin"]))
	body := w.Body.String()
	for _, want := range []string{"discovery document", "signing key(s)", "signing out of its own session", "/api/oidc/callback"} {
		if !strings.Contains(body, want) {
			t.Errorf("report lacks %q: %s", want, body)
		}
	}
	w = e.do("POST", "/api/signin/oidc/test", map[string]any{"issuer": "http://127.0.0.1:1"}, e.as(e.cookies["admin"]))
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("unreachable issuer: %s", w.Body)
	}
	tp.Claims = map[string]any{"sub": "u-9", "preferred_username": "gina", "groups": []string{"virticle-ops"}}
	c := sessionCookie(oidcLogin(t, e))
	w = e.do("POST", "/api/logout", nil, e.as(c))
	var out struct{ EndSession, Provider string }
	decode(t, w, &out)
	if !strings.HasPrefix(out.EndSession, tp.URL()+"/logout?") || out.Provider != "Test IdP" {
		t.Errorf("logout: %s", w.Body)
	}
	// A local user's logout offers nothing.
	out.EndSession = ""
	decode(t, e.do("POST", "/api/logout", nil, e.as(e.cookies["admin"])), &out)
	if out.EndSession != "" {
		t.Error("offered a provider sign-out to a local user")
	}
}

// --- directory ---

func ldapEnv(t *testing.T) (*env, *ldaptest.Server) {
	t.Helper()
	e, _ := signInEnv(t)
	e.user("admin", admin)
	d, err := ldaptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	base := "dc=example,dc=org"
	d.Entries = []ldaptest.Entry{
		{DN: base, Attrs: map[string][]string{}},
		{DN: "cn=svc," + base, Attrs: map[string][]string{}},
		{DN: "uid=hana,ou=p," + base, Attrs: map[string][]string{"uid": {"hana"}, "memberOf": {"cn=vadmins,ou=g," + base}}},
		{DN: "uid=ivan,ou=p," + base, Attrs: map[string][]string{"uid": {"ivan"}}},
		{DN: "cn=vops,ou=g," + base, Attrs: map[string][]string{"cn": {"vops"}, "member": {"uid=ivan,ou=p," + base}}},
		{DN: "uid=jo,ou=p," + base, Attrs: map[string][]string{"uid": {"jo"}}},
	}
	d.Passwords = map[string]string{"cn=svc," + base: "svc-pw", "uid=hana,ou=p," + base: "hana-password", "uid=ivan,ou=p," + base: "ivan-password", "uid=jo,ou=p," + base: "jo-password"}
	w := putSignIn(e, e.cookies["admin"], map[string]any{"require2fa": "nobody", "ldap": map[string]any{
		"enabled": true, "url": d.TLSURL(), "bindDn": "cn=svc," + base, "bindPassword": "svc-pw", "baseDn": base, "caCert": d.CAPEM,
		"adminGroup": "vadmins", "operatorGroup": "vops", "defaultRole": "none", "timeoutSeconds": 2}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return e, d
}

func TestLDAPSignIn(t *testing.T) {
	e, d := ldapEnv(t)
	login := func(u, p string) *httptest.ResponseRecorder {
		e.srv.limiter = newLimiter()
		return e.do("POST", "/api/login", map[string]string{"username": u, "password": p})
	}
	roleOf := func(w *httptest.ResponseRecorder) string {
		var me struct{ Role, Source string }
		decode(t, e.do("GET", "/api/status", nil, e.as(sessionCookie(w))), &me)
		return me.Role + "/" + me.Source
	}
	if got := roleOf(login("hana", "hana-password")); got != "admin/ldap" {
		t.Errorf("memberOf admin: %s", got)
	}
	if got := roleOf(login("ivan", "ivan-password")); got != "operator/ldap" {
		t.Errorf("group search operator: %s", got)
	}
	// In neither group: no access, said plainly, and not counted as a wrong password.
	w := login("jo", "jo-password")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "not allowed") || sessionCookie(w) != "" {
		t.Errorf("no group: %d %s", w.Code, w.Body)
	}
	for _, c := range [][2]string{{"hana", "wrong-password"}, {"nobody", "x"}, {"hana", ""}} {
		if w := login(c[0], c[1]); w.Code != 401 || sessionCookie(w) != "" {
			t.Errorf("%v: %d", c, w.Code)
		}
	}
	// Role follows the directory at every sign-in; a user who loses the group is shut out and signed out.
	hana := sessionCookie(login("hana", "hana-password"))
	d.Entries[2].Attrs["memberOf"] = nil
	if w := login("hana", "hana-password"); w.Code != 403 {
		t.Errorf("hana after losing the group: %d", w.Code)
	}
	if w := e.do("GET", "/api/version", nil, e.as(hana)); w.Code != 401 {
		t.Errorf("old session of a user who lost the group: %d", w.Code)
	}
	// Directory users can use the built-in two-step sign-in, and the policy applies to them.
	d.Entries[2].Attrs["memberOf"] = []string{"cn=vadmins,ou=g,dc=example,dc=org"}
	c := sessionCookie(login("hana", "hana-password"))
	now := e.srv.TOTPClock()
	secret, _ := e.turnOnTwoStep(c, now)
	w = login("hana", "hana-password")
	if !strings.Contains(w.Body.String(), "challenge") {
		t.Fatalf("no second step for a directory user: %s", w.Body)
	}
	// Turning it off asks for the directory password.
	nowp := now.Add(30 * time.Second)
	e.srv.TOTPClock = func() time.Time { return nowp }
	if r := e.do("POST", "/api/account/2fa/disable", map[string]string{"password": "wrong", "code": codeAt(secret, nowp)}, e.as(c)); r.Code != 400 {
		t.Errorf("wrong directory password: %d", r.Code)
	}
	if r := e.do("POST", "/api/account/2fa/disable", map[string]string{"password": "hana-password", "code": codeAt(secret, nowp)}, e.as(c)); r.Code != 200 {
		t.Errorf("right directory password: %d %s", r.Code, r.Body)
	}
	// The local account of the same name cannot be taken over, and the local administrator signs in without the directory.
	if w := login("admin", testPW); w.Code != 200 {
		t.Errorf("local admin: %d", w.Code)
	}
	// Unreachable directory: local accounts still work, and the message says why.
	d.Close()
	if w := login("admin", testPW); w.Code != 200 {
		t.Errorf("local admin with the directory down: %d", w.Code)
	}
	start := time.Now()
	w = login("hana", "hana-password")
	if w.Code != 401 || !strings.Contains(w.Body.String(), "directory could not be reached") || time.Since(start) > 5*time.Second {
		t.Errorf("directory down: %d %s (%v)", w.Code, w.Body, time.Since(start))
	}
}

func TestLDAPTestButton(t *testing.T) {
	e, d := ldapEnv(t)
	ac := e.as(e.cookies["admin"])
	base := "dc=example,dc=org"
	body := map[string]any{"url": d.TLSURL(), "bindDn": "cn=svc," + base, "baseDn": base, "caCert": d.CAPEM, "adminGroup": "vadmins", "operatorGroup": "vops",
		"defaultRole": "none", "testUser": "ivan", "testPassword": "ivan-password"}
	// The stored bind password is used when the form leaves it empty.
	w := e.do("POST", "/api/signin/ldap/test", body, ac)
	for _, want := range []string{"Connected over LDAPS", "Bound as the bind user", "User found as uid=ivan", "The password is correct", "Resulting role: operator"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("report lacks %q: %s", want, w.Body)
		}
	}
	if strings.Contains(w.Body.String(), "svc-pw") || strings.Contains(w.Body.String(), "ivan-password") {
		t.Error("a password is in the report")
	}
	body["testUser"], body["testPassword"] = "jo", "jo-password"
	if w := e.do("POST", "/api/signin/ldap/test", body, ac); !strings.Contains(w.Body.String(), "Resulting role: no access") {
		t.Errorf("jo: %s", w.Body)
	}
}
