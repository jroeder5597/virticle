// Package oidcauth is the relying-party side of OpenID Connect sign-in: the
// authorization code flow with PKCE, discovery from the issuer URL, and ID
// token validation (signature against the provider's rotating keys, issuer,
// audience, expiry and nonce). The protocol work is done by go-oidc and
// oauth2; this package fixes the choices and reads the claims.
package oidcauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Defaults for the settings a user may leave empty.
const (
	DefaultScopes      = "openid profile email"
	DefaultGroupsClaim = "groups"
	DefaultUserClaim   = "preferred_username"
)

// Config is one identity provider.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       string // space separated
	GroupsClaim  string
	UserClaim    string
}

func (c Config) scopes() []string {
	f := strings.Fields(c.Scopes)
	if len(f) == 0 {
		f = strings.Fields(DefaultScopes)
	}
	for _, s := range f {
		if s == oidc.ScopeOpenID {
			return f
		}
	}
	return append([]string{oidc.ScopeOpenID}, f...)
}

// ValidateIssuer accepts https addresses, and plain http only on a loopback
// host (for a provider running on the same machine).
func ValidateIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return errors.New("the issuer URL must look like https://login.example.org")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return errors.New("the issuer URL must use https (plain http is only accepted for localhost)")
	}
	return errors.New("the issuer URL must look like https://login.example.org")
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func withClient(ctx context.Context) context.Context { return oidc.ClientContext(ctx, httpClient) }

// Provider is a discovered provider. It keeps the key set, which refetches the
// keys when a token names a key it has not seen (key rotation).
type Provider struct {
	cfg      Config
	p        *oidc.Provider
	EndSess  string // end_session_endpoint, may be empty
	JWKSURI  string
	Endpoint oauth2.Endpoint
	Scopes   []string // scopes the provider says it supports
	Claims   []string // claims the provider says it supports
}

// Discover reads the provider's discovery document.
func Discover(ctx context.Context, cfg Config) (*Provider, error) {
	if err := ValidateIssuer(cfg.Issuer); err != nil {
		return nil, err
	}
	p, err := oidc.NewProvider(withClient(ctx), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("could not read %s/.well-known/openid-configuration: %w", strings.TrimSuffix(cfg.Issuer, "/"), err)
	}
	var doc struct {
		EndSession string   `json:"end_session_endpoint"`
		JWKS       string   `json:"jwks_uri"`
		Scopes     []string `json:"scopes_supported"`
		Claims     []string `json:"claims_supported"`
	}
	if err := p.Claims(&doc); err != nil {
		return nil, err
	}
	return &Provider{cfg: cfg, p: p, EndSess: doc.EndSession, JWKSURI: doc.JWKS, Endpoint: p.Endpoint(), Scopes: doc.Scopes, Claims: doc.Claims}, nil
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Request is the state of one sign-in attempt, kept by the caller between the
// redirect to the provider and the return.
type Request struct {
	State, Nonce, Verifier string
	Redirect               string
}

// Start builds the address to send the browser to.
func (p *Provider) Start(redirect string) (Request, string) {
	r := Request{State: random(24), Nonce: random(24), Verifier: oauth2.GenerateVerifier(), Redirect: redirect}
	u := p.oauth(redirect).AuthCodeURL(r.State, oauth2.S256ChallengeOption(r.Verifier), oidc.Nonce(r.Nonce))
	return r, u
}

func (p *Provider) oauth(redirect string) *oauth2.Config {
	return &oauth2.Config{ClientID: p.cfg.ClientID, ClientSecret: p.cfg.ClientSecret, Endpoint: p.Endpoint, RedirectURL: redirect, Scopes: p.cfg.scopes()}
}

// Identity is what the ID token says about the user.
type Identity struct {
	Subject string
	Issuer  string
	Name    string // sign-in name from the user claim
	Display string // full name
	Email   string
	Groups  []string
	Raw     map[string]any
}

// Finish exchanges the code and validates the ID token.
func (p *Provider) Finish(ctx context.Context, req Request, code string) (Identity, error) {
	ctx = withClient(ctx)
	tok, err := p.oauth(req.Redirect).Exchange(ctx, code, oauth2.VerifierOption(req.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("the provider refused the sign-in code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, errors.New("the provider returned no ID token; check that the scope openid is allowed")
	}
	return p.Verify(ctx, raw, req.Nonce)
}

// Verify checks a raw ID token: signature, issuer, audience, expiry and nonce.
func (p *Provider) Verify(ctx context.Context, raw, nonce string) (Identity, error) {
	idt, err := p.p.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(withClient(ctx), raw)
	if err != nil {
		return Identity{}, fmt.Errorf("the ID token is not valid: %w", err)
	}
	if nonce == "" || idt.Nonce != nonce {
		return Identity{}, errors.New("the ID token does not belong to this sign-in (nonce differs)")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, err
	}
	return p.cfg.identity(idt.Issuer, idt.Subject, claims), nil
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (c Config) identity(iss, sub string, claims map[string]any) Identity {
	id := Identity{Subject: sub, Issuer: iss, Raw: claims, Email: str(claims, "email"), Display: str(claims, "name")}
	uc := c.UserClaim
	if uc == "" {
		uc = DefaultUserClaim
	}
	id.Name = str(claims, uc)
	if id.Name == "" {
		id.Name = id.Email
	}
	gc := c.GroupsClaim
	if gc == "" {
		gc = DefaultGroupsClaim
	}
	switch v := claims[gc].(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	case string:
		id.Groups = []string{v}
	}
	return id
}

// InGroup reports whether the identity lists want (any case).
func (i Identity) InGroup(want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for _, g := range i.Groups {
		if strings.EqualFold(g, want) {
			return true
		}
	}
	return false
}

// EndSessionURL is the provider's sign-out address with a return address, or "".
func (p *Provider) EndSessionURL(returnTo string) string {
	if p.EndSess == "" {
		return ""
	}
	u, err := url.Parse(p.EndSess)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_id", p.cfg.ClientID)
	q.Set("post_logout_redirect_uri", returnTo)
	u.RawQuery = q.Encode()
	return u.String()
}

// CountKeys fetches a JWKS address and counts the keys in it.
func CountKeys(ctx context.Context, jwksURI string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the provider answered %s", resp.Status)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return 0, err
	}
	return len(set.Keys), nil
}
