package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"virticle/internal/dirauth"
	"virticle/internal/oidcauth"
	"virticle/internal/secrets"
	"virticle/internal/store"
)

// How sign-in is set up: who must use two-step sign-in, the identity provider
// and the directory. It is one JSON document in the settings table; the two
// secrets (client secret, bind password) are sealed and kept under their own
// keys, so no settings document or API answer ever holds one.
const (
	signInKey     = "signin"
	oidcSecretKey = "signin.oidc.secret"
	ldapSecretKey = "signin.ldap.secret"
)

// Who has to use two-step sign-in.
const (
	requireNobody   = "nobody"
	requireAdmins   = "admins"
	requireEveryone = "everyone"
)

// Role for people who sign in through a provider but are in neither group.
const (
	roleNone = "none"
)

type OIDCSettings struct {
	Enabled       bool   `json:"enabled"`
	Name          string `json:"name"` // shown on the button: "Sign in with <name>"
	Issuer        string `json:"issuer"`
	ClientID      string `json:"clientId"`
	AdminGroup    string `json:"adminGroup"`
	OperatorGroup string `json:"operatorGroup"`
	DefaultRole   string `json:"defaultRole"` // none | viewer
	GroupsClaim   string `json:"groupsClaim"`
	UserClaim     string `json:"userClaim"`
	Scopes        string `json:"scopes"`
}

type LDAPSettings struct {
	Enabled        bool   `json:"enabled"`
	URL            string `json:"url"`
	BindDN         string `json:"bindDn"`
	BaseDN         string `json:"baseDn"`
	AdminGroup     string `json:"adminGroup"`
	OperatorGroup  string `json:"operatorGroup"`
	DefaultRole    string `json:"defaultRole"`
	UserFilter     string `json:"userFilter"`
	GroupFilter    string `json:"groupFilter"`
	NameAttr       string `json:"nameAttr"`
	MemberOfAttr   string `json:"memberOfAttr"`
	CACert         string `json:"caCert"`
	AllowPlain     bool   `json:"allowPlain"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type SignInSettings struct {
	Require2FA string       `json:"require2fa"`
	OIDC       OIDCSettings `json:"oidc"`
	LDAP       LDAPSettings `json:"ldap"`
}

// signInState is the sign-in settings cache and the short-lived state of
// sign-ins in progress. All of it is in memory; a restart only asks people to start again.
type signInState struct {
	mu         sync.Mutex
	settings   *SignInSettings
	setups     map[int64]pendingSetup
	challenges map[string]*challenge
	oidcReqs   map[string]pendingOIDC
	prov       *cachedProvider
}

func (s *Server) box() (*secrets.Box, error) {
	s.boxOnce.Do(func() {
		if s.Secrets == nil {
			s.Secrets, s.boxErr = secrets.Open(s.DataDir)
		}
	})
	if s.boxErr != nil {
		return nil, s.boxErr
	}
	return s.Secrets, nil
}

// signIn returns the current sign-in settings.
func (s *Server) signIn() SignInSettings {
	s.si.mu.Lock()
	defer s.si.mu.Unlock()
	if s.si.settings == nil {
		v := SignInSettings{Require2FA: requireNobody}
		if raw, ok, err := s.Store.WebSetting(signInKey); err == nil && ok {
			json.Unmarshal([]byte(raw), &v)
		}
		if v.Require2FA != requireAdmins && v.Require2FA != requireEveryone {
			v.Require2FA = requireNobody
		}
		s.si.settings = &v
	}
	return *s.si.settings
}

func (s *Server) saveSignIn(v SignInSettings) error {
	b, _ := json.Marshal(v)
	if err := s.Store.SetWebSetting(signInKey, string(b)); err != nil {
		return err
	}
	s.si.mu.Lock()
	s.si.settings = &v
	s.si.prov = nil
	s.si.mu.Unlock()
	return nil
}

// sealedSecret reads a stored secret ("" when none is set).
func (s *Server) sealedSecret(key string) (string, error) {
	raw, ok, err := s.Store.WebSetting(key)
	if err != nil || !ok || raw == "" {
		return "", err
	}
	b, err := s.box()
	if err != nil {
		return "", err
	}
	return b.Unseal(raw)
}

func (s *Server) storeSecret(key, value string) error {
	b, err := s.box()
	if err != nil {
		return err
	}
	sealed, err := b.Seal(value)
	if err != nil {
		return err
	}
	return s.Store.SetWebSetting(key, sealed)
}

func (s *Server) secretIsSet(key string) bool {
	raw, ok, _ := s.Store.WebSetting(key)
	return ok && raw != ""
}

// twoStepRequired is the policy: must this user have two-step sign-in?
// People who sign in through an identity provider never do; the provider asks.
func twoStepRequired(require string, u store.User) bool {
	if u.Source == store.SourceOIDC {
		return false
	}
	switch require {
	case requireEveryone:
		return true
	case requireAdmins:
		return u.Role == store.RoleAdmin
	}
	return false
}

// mustSetUpTwoStep is true for a user who has to have two-step sign-in and has not set it up.
func (s *Server) mustSetUpTwoStep(u store.User) bool {
	return !u.TwoStep && twoStepRequired(s.signIn().Require2FA, u)
}

// roleFromGroups turns group membership into a role; "" means no access.
func roleFromGroups(isAdmin, isOperator bool, def string) string {
	switch {
	case isAdmin:
		return store.RoleAdmin
	case isOperator:
		return store.RoleOperator
	case def == store.RoleViewer:
		return store.RoleViewer
	}
	return ""
}

// --- the settings page ---

type oidcView struct {
	OIDCSettings
	ClientSecretSet bool   `json:"clientSecretSet"`
	RedirectURL     string `json:"redirectUrl"`
}

type ldapView struct {
	LDAPSettings
	BindPasswordSet bool `json:"bindPasswordSet"`
}

type signInView struct {
	Require2FA string   `json:"require2fa"`
	OIDC       oidcView `json:"oidc"`
	LDAP       ldapView `json:"ldap"`
	// TwoStepUsers counts the accounts that have two-step sign-in on, for the one-line status.
	TwoStepUsers int `json:"twoStepUsers"`
}

func (s *Server) signInView(r *http.Request) signInView {
	v := s.signIn()
	n := 0
	if us, err := s.Store.Users(); err == nil {
		for _, u := range us {
			if u.TwoStep {
				n++
			}
		}
	}
	return signInView{
		Require2FA:   v.Require2FA,
		OIDC:         oidcView{v.OIDC, s.secretIsSet(oidcSecretKey), redirectURL(r)},
		LDAP:         ldapView{v.LDAP, s.secretIsSet(ldapSecretKey)},
		TwoStepUsers: n,
	}
}

// redirectURL is the address to register at the identity provider: this
// controller's address as the browser reached it.
func redirectURL(r *http.Request) string {
	v := viewOf(r)
	scheme := "http"
	if v.HTTPS {
		scheme = "https"
	}
	return scheme + "://" + v.Host + "/api/oidc/callback"
}

func (s *Server) getSignIn(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.signInView(r))
}

type signInRequest struct {
	Require2FA string `json:"require2fa"`
	OIDC       struct {
		OIDCSettings
		ClientSecret string `json:"clientSecret"`
	} `json:"oidc"`
	LDAP struct {
		LDAPSettings
		BindPassword string `json:"bindPassword"`
	} `json:"ldap"`
}

func cleanRole(r *string) {
	if *r != store.RoleViewer {
		*r = roleNone
	}
}

func (s *Server) putSignIn(w http.ResponseWriter, r *http.Request) {
	var req signInRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	cur := s.signIn()
	switch req.Require2FA {
	case requireNobody, requireAdmins, requireEveryone:
	default:
		fail(w, badRequest("require2fa must be nobody, admins or everyone"))
		return
	}
	o, l := req.OIDC.OIDCSettings, req.LDAP.LDAPSettings
	for _, p := range []*string{&o.Name, &o.Issuer, &o.ClientID, &o.AdminGroup, &o.OperatorGroup, &o.GroupsClaim, &o.UserClaim, &o.Scopes,
		&l.URL, &l.BindDN, &l.BaseDN, &l.AdminGroup, &l.OperatorGroup, &l.UserFilter, &l.GroupFilter, &l.NameAttr, &l.MemberOfAttr} {
		*p = strings.TrimSpace(*p)
	}
	o.Issuer = strings.TrimRight(o.Issuer, "/")
	cleanRole(&o.DefaultRole)
	cleanRole(&l.DefaultRole)
	if len(o.Name) > 60 {
		fail(w, badRequest("the provider name is too long"))
		return
	}
	if l.TimeoutSeconds < 0 || l.TimeoutSeconds > 60 {
		fail(w, badRequest("the directory timeout must be between 1 and 60 seconds"))
		return
	}
	secretOIDC := req.OIDC.ClientSecret != "" || s.secretIsSet(oidcSecretKey)
	if o.Enabled {
		if err := oidcauth.ValidateIssuer(o.Issuer); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
		if o.ClientID == "" || !secretOIDC {
			fail(w, badRequest("the identity provider needs a client ID and a client secret"))
			return
		}
	}
	if l.Enabled {
		if err := ldapConfig(l, "").Validate(); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
		if l.BindDN != "" && req.LDAP.BindPassword == "" && !s.secretIsSet(ldapSecretKey) {
			fail(w, badRequest("the bind user needs a password"))
			return
		}
	}
	if req.OIDC.ClientSecret != "" {
		if err := s.storeSecret(oidcSecretKey, req.OIDC.ClientSecret); err != nil {
			fail(w, err)
			return
		}
	}
	if req.LDAP.BindPassword != "" {
		if err := s.storeSecret(ldapSecretKey, req.LDAP.BindPassword); err != nil {
			fail(w, err)
			return
		}
	}
	if l.BindDN == "" {
		s.Store.DeleteWebSetting(ldapSecretKey)
	}
	next := SignInSettings{Require2FA: req.Require2FA, OIDC: o, LDAP: l}
	if err := s.saveSignIn(next); err != nil {
		fail(w, err)
		return
	}
	var changed []string
	if cur.Require2FA != next.Require2FA {
		changed = append(changed, "two-step required for: "+next.Require2FA)
	}
	if cur.OIDC != next.OIDC || req.OIDC.ClientSecret != "" {
		changed = append(changed, "identity provider "+onOff(next.OIDC.Enabled))
	}
	if cur.LDAP != next.LDAP || req.LDAP.BindPassword != "" {
		changed = append(changed, "directory "+onOff(next.LDAP.Enabled))
	}
	infoOf(r).detail = "sign-in settings changed: " + strings.Join(changed, "; ")
	writeJSON(w, s.signInView(r))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// publicSignIn is what the sign-in page may know before anyone is signed in.
func (s *Server) publicSignIn() map[string]any {
	v := s.signIn()
	out := map[string]any{}
	if v.OIDC.Enabled {
		name := v.OIDC.Name
		if name == "" {
			name = "your identity provider"
		}
		out["oidc"] = name
	}
	return out
}

// --- tests of the provider and the directory ---

type step = dirauth.Step

func (s *Server) testOIDC(w http.ResponseWriter, r *http.Request) {
	var req struct{ Issuer, Scopes, GroupsClaim string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"steps": s.oidcReport(r.Context(), strings.TrimRight(strings.TrimSpace(req.Issuer), "/"), req.Scopes, req.GroupsClaim, redirectURL(r))})
}

func (s *Server) oidcReport(ctx context.Context, issuer, scopes, groupsClaim, redirect string) []step {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := oidcauth.Discover(ctx, oidcauth.Config{Issuer: issuer})
	if err != nil {
		return []step{{OK: false, Text: err.Error()}}
	}
	out := []step{{OK: true, Text: "Found the provider's discovery document at " + issuer + "/.well-known/openid-configuration."}}
	out = append(out, step{OK: true, Text: "Sign-in page: " + p.Endpoint.AuthURL + ". Token address: " + p.Endpoint.TokenURL + "."})
	n, err := oidcauth.CountKeys(ctx, p.JWKSURI)
	if err != nil {
		out = append(out, step{OK: false, Text: "The signing keys could not be fetched from " + p.JWKSURI + ": " + err.Error()})
	} else {
		out = append(out, step{OK: true, Text: fmt.Sprintf("Fetched %d signing key(s) from %s.", n, p.JWKSURI)})
	}
	gc := groupsClaim
	if gc == "" {
		gc = oidcauth.DefaultGroupsClaim
	}
	if len(p.Claims) > 0 && !contains(p.Claims, gc) {
		out = append(out, step{OK: true, Text: fmt.Sprintf("The provider does not list a %q claim. Many providers add groups to the token only when the application is set up for it; see the documentation for your provider.", gc)})
	} else if len(p.Claims) > 0 {
		out = append(out, step{OK: true, Text: fmt.Sprintf("The provider lists a %q claim.", gc)})
	}
	if p.EndSess != "" {
		out = append(out, step{OK: true, Text: "The provider supports signing out of its own session; Virticle offers that link after signing out."})
	} else {
		out = append(out, step{OK: true, Text: "The provider does not advertise a sign-out address; signing out ends only the Virticle session."})
	}
	out = append(out, step{OK: true, Text: "Register this redirect URL at the provider: " + redirect})
	return out
}

func ldapConfig(l LDAPSettings, bindPassword string) dirauth.Config {
	return dirauth.Config{
		URL: l.URL, BindDN: l.BindDN, BindPassword: bindPassword, BaseDN: l.BaseDN, UserFilter: l.UserFilter, GroupFilter: l.GroupFilter,
		NameAttr: l.NameAttr, MemberOfAttr: l.MemberOfAttr, CACert: l.CACert, AllowPlain: l.AllowPlain,
		Timeout: time.Duration(l.TimeoutSeconds) * time.Second,
	}
}

func (s *Server) testLDAP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LDAPSettings
		BindPassword string `json:"bindPassword"`
		TestUser     string `json:"testUser"`
		TestPassword string `json:"testPassword"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	pw := req.BindPassword
	if pw == "" {
		var err error
		if pw, err = s.sealedSecret(ldapSecretKey); err != nil {
			fail(w, err)
			return
		}
	}
	cfg := ldapConfig(req.LDAPSettings, pw)
	steps, res := dirauth.Test(cfg, req.TestUser, req.TestPassword)
	if res != nil {
		role := roleFromGroups(res.InGroup(req.AdminGroup), res.InGroup(req.OperatorGroup), req.DefaultRole)
		steps = append(steps, step{OK: role != "", Text: "Resulting role: " + roleText(role) + "."})
	}
	writeJSON(w, map[string]any{"steps": steps})
}

var errNoAccess = errors.New("your account is not allowed to use Virticle: ask an administrator to add you to one of its groups")

func roleText(role string) string {
	if role == "" {
		return "no access (the user is in neither group and everyone else gets no access)"
	}
	return role
}
