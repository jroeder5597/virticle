// Package oidctest is a small OpenID Connect provider for tests: discovery,
// keys, an authorization endpoint that approves at once, and a token endpoint
// that checks PKCE and the client secret and returns a signed ID token.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	ClientID     = "virticle-test"
	ClientSecret = "test-client-secret"
)

type grant struct {
	nonce, challenge, redirect string
	claims                     map[string]any
}

// Provider is a running fake provider.
type Provider struct {
	Server *httptest.Server
	// Claims are put into the next ID tokens (sub, groups, ...).
	Claims map[string]any
	// Faults, set before sign-in, make the token wrong in one way.
	Audience, Issuer, Nonce string
	Expired                 bool
	// EndSession advertises end_session_endpoint when true.
	EndSession bool

	mu     sync.Mutex
	keys   []*rsa.PrivateKey
	kids   []string
	signer int // index of the key signing tokens
	codes  map[string]grant
}

// New starts a provider with one signing key.
func New() *Provider {
	p := &Provider{codes: map[string]grant{}, Claims: map[string]any{"sub": "user-1"}}
	p.AddKey()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/keys", p.jwks)
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	p.Server = httptest.NewServer(mux)
	return p
}

func (p *Provider) URL() string { return p.Server.URL }
func (p *Provider) Close()      { p.Server.Close() }

// AddKey adds a signing key and makes it the one that signs (rotation); the old keys stay published until DropOldKeys.
func (p *Provider) AddKey() {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, k)
	p.kids = append(p.kids, "key-"+time.Now().Format("150405.000000000"))
	p.signer = len(p.keys) - 1
}

// DropOldKeys stops publishing every key but the signing one.
func (p *Provider) DropOldKeys() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys, p.kids, p.signer = p.keys[p.signer:p.signer+1], p.kids[p.signer:p.signer+1], 0
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	u := p.Server.URL
	doc := map[string]any{
		"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token", "jwks_uri": u + "/keys",
		"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"}, "scopes_supported": []string{"openid", "profile", "email", "groups"},
		"claims_supported": []string{"sub", "email", "name", "groups"},
	}
	if p.EndSession {
		doc["end_session_endpoint"] = u + "/logout"
	}
	json.NewEncoder(w).Encode(doc)
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := jose.JSONWebKeySet{}
	for i, k := range p.keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: p.kids[i], Algorithm: "RS256", Use: "sig"})
	}
	json.NewEncoder(w).Encode(set)
}

// authorize approves at once and sends the browser back with a code.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorization request", 400)
		return
	}
	cb := make([]byte, 16)
	rand.Read(cb)
	code := base64.RawURLEncoding.EncodeToString(cb)
	claims := map[string]any{}
	p.mu.Lock()
	for k, v := range p.Claims {
		claims[k] = v
	}
	p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), claims: claims}
	p.mu.Unlock()
	back, _ := url.Parse(q.Get("redirect_uri"))
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
	}
	p.mu.Lock()
	g, found := p.codes[r.Form.Get("code")]
	delete(p.codes, r.Form.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if id != ClientID || secret != ClientSecret || !found || g.redirect != r.Form.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": p.IDToken(g.claims, g.nonce)})
}

// IDToken signs a token with the current key, honouring the faults.
func (p *Provider) IDToken(claims map[string]any, nonce string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	body := map[string]any{"iss": p.Server.URL, "aud": ClientID, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce}
	for k, v := range claims {
		body[k] = v
	}
	if p.Issuer != "" {
		body["iss"] = p.Issuer
	}
	if p.Audience != "" {
		body["aud"] = p.Audience
	}
	if p.Nonce != "" {
		body["nonce"] = p.Nonce
	}
	if p.Expired {
		body["exp"] = time.Now().Add(-time.Hour).Unix()
		body["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.keys[p.signer], KeyID: p.kids[p.signer]}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	payload, _ := json.Marshal(body)
	obj, err := sig.Sign(payload)
	if err != nil {
		panic(err)
	}
	s, _ := obj.CompactSerialize()
	return s
}
