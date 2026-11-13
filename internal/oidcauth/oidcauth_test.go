package oidcauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"virticle/internal/oidcauth/oidctest"
)

func cfg(p *oidctest.Provider) Config {
	return Config{Issuer: p.URL(), ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret}
}

// signIn runs the whole flow against the test provider the way a browser would.
func signIn(t *testing.T, p *Provider) (Identity, error) {
	t.Helper()
	req, authURL := p.Start("http://app.test/api/oidc/callback")
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Query().Get("state") != req.State {
		t.Fatalf("state not returned: %s", back)
	}
	return p.Finish(context.Background(), req, back.Query().Get("code"))
}

func TestHappyPathAndClaims(t *testing.T) {
	tp := oidctest.New()
	defer tp.Close()
	tp.Claims = map[string]any{"sub": "s1", "preferred_username": "dana", "email": "dana@example.org", "name": "Dana D", "groups": []string{"Admins", "staff"}}
	p, err := Discover(context.Background(), cfg(tp))
	if err != nil {
		t.Fatal(err)
	}
	id, err := signIn(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "s1" || id.Name != "dana" || id.Display != "Dana D" || !id.InGroup("admins") || !id.InGroup("STAFF") || id.InGroup("x") {
		t.Errorf("%+v", id)
	}
	// Name falls back to the e-mail address; a single string is one group; the claim names are settable.
	c := cfg(tp)
	c.UserClaim, c.GroupsClaim = "upn", "roles"
	tp.Claims = map[string]any{"sub": "s2", "email": "e@example.org", "roles": "solo"}
	p, _ = Discover(context.Background(), c)
	id, err = signIn(t, p)
	if err != nil || id.Name != "e@example.org" || !id.InGroup("solo") {
		t.Errorf("%+v %v", id, err)
	}
}

func TestTokenFaults(t *testing.T) {
	tp := oidctest.New()
	defer tp.Close()
	p, err := Discover(context.Background(), cfg(tp))
	if err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]func(){
		"audience": func() { tp.Audience = "someone-else" },
		"issuer":   func() { tp.Issuer = "https://evil.example.org" },
		"nonce":    func() { tp.Nonce = "replayed-nonce" },
		"expired":  func() { tp.Expired = true },
	} {
		tp.Audience, tp.Issuer, tp.Nonce, tp.Expired = "", "", "", false
		set()
		if id, err := signIn(t, p); err == nil {
			t.Errorf("%s: accepted %+v", name, id)
		}
	}
	tp.Audience, tp.Issuer, tp.Nonce, tp.Expired = "", "", "", false
	if _, err := signIn(t, p); err != nil {
		t.Errorf("clean token refused: %v", err)
	}
}

func TestStateCodeAndSecret(t *testing.T) {
	tp := oidctest.New()
	defer tp.Close()
	c := cfg(tp)
	c.ClientSecret = "wrong"
	p, _ := Discover(context.Background(), c)
	if _, err := signIn(t, p); err == nil {
		t.Error("a wrong client secret must fail")
	}
	p, _ = Discover(context.Background(), cfg(tp))
	req, _ := p.Start("http://app.test/cb")
	if _, err := p.Finish(context.Background(), req, "made-up-code"); err == nil {
		t.Error("an unknown code must fail")
	}
	// PKCE: a code obtained for one verifier does not work with another.
	req, authURL := p.Start("http://app.test/api/oidc/callback")
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := cl.Get(authURL)
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	req.Verifier = "x" + req.Verifier
	if _, err := p.Finish(context.Background(), req, back.Query().Get("code")); err == nil {
		t.Error("a wrong PKCE verifier must fail")
	}
}

func TestKeyRotation(t *testing.T) {
	tp := oidctest.New()
	defer tp.Close()
	p, _ := Discover(context.Background(), cfg(tp))
	if _, err := signIn(t, p); err != nil {
		t.Fatal(err)
	}
	// The provider rotates: new key signs, old key is withdrawn. The cached key set must refetch.
	tp.AddKey()
	tp.DropOldKeys()
	if _, err := signIn(t, p); err != nil {
		t.Errorf("after rotation: %v", err)
	}
}

func TestIssuerAndEndSession(t *testing.T) {
	for _, bad := range []string{"", "login.example.org", "http://login.example.org", "ftp://x"} {
		if ValidateIssuer(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"https://login.example.org/realms/x", "http://127.0.0.1:5556/dex", "http://localhost:8080"} {
		if err := ValidateIssuer(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	tp := oidctest.New()
	defer tp.Close()
	p, _ := Discover(context.Background(), cfg(tp))
	if p.EndSessionURL("http://app.test/") != "" {
		t.Error("no end_session_endpoint advertised")
	}
	tp.EndSession = true
	p, _ = Discover(context.Background(), cfg(tp))
	u := p.EndSessionURL("http://app.test/")
	if !strings.Contains(u, "/logout?") || !strings.Contains(u, "client_id=virticle-test") {
		t.Error(u)
	}
	if _, err := Discover(context.Background(), Config{Issuer: "http://127.0.0.1:1"}); err == nil {
		t.Error("an unreachable issuer must fail")
	}
}
