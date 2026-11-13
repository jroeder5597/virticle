package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"virticle/internal/oidcauth"
	"virticle/internal/store"
)

const (
	oidcCookie = "virt_oidc"
	oidcTTL    = 10 * time.Minute
)

type pendingOIDC struct {
	req oidcauth.Request
	exp time.Time
}

type cachedProvider struct {
	key string
	p   *oidcauth.Provider
	at  time.Time
}

func (s *Server) oidcConfig() (oidcauth.Config, OIDCSettings, error) {
	o := s.signIn().OIDC
	if !o.Enabled {
		return oidcauth.Config{}, o, fmt.Errorf("sign-in with an identity provider is not turned on")
	}
	secret, err := s.sealedSecret(oidcSecretKey)
	if err != nil {
		return oidcauth.Config{}, o, err
	}
	return oidcauth.Config{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: secret, Scopes: o.Scopes, GroupsClaim: o.GroupsClaim, UserClaim: o.UserClaim}, o, nil
}

// oidcProvider returns the discovered provider, reusing it (and its cached
// signing keys) for an hour so a sign-in needs no discovery round trip.
func (s *Server) oidcProvider(ctx context.Context, cfg oidcauth.Config) (*oidcauth.Provider, error) {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%v", cfg))))
	s.si.mu.Lock()
	c := s.si.prov
	s.si.mu.Unlock()
	if c != nil && c.key == key && time.Since(c.at) < time.Hour {
		return c.p, nil
	}
	p, err := oidcauth.Discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s.si.mu.Lock()
	s.si.prov = &cachedProvider{key, p, time.Now()}
	s.si.mu.Unlock()
	return p, nil
}

// signInError sends the browser back to the sign-in page with a message.
func signInError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?signin_error="+url.QueryEscape(msg), http.StatusFound)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	infoOf(r).login = true
	cfg, _, err := s.oidcConfig()
	if err != nil {
		signInError(w, r, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	p, err := s.oidcProvider(ctx, cfg)
	if err != nil {
		logf("oidc: %v", err)
		signInError(w, r, "The identity provider could not be reached. Local accounts still work.")
		return
	}
	req, authURL := p.Start(redirectURL(r))
	s.si.mu.Lock()
	if s.si.oidcReqs == nil {
		s.si.oidcReqs = map[string]pendingOIDC{}
	}
	now := time.Now()
	for k, v := range s.si.oidcReqs {
		if now.After(v.exp) {
			delete(s.si.oidcReqs, k)
		}
	}
	if len(s.si.oidcReqs) > 1000 { // someone is only starting sign-ins; keep memory bounded
		s.si.oidcReqs = map[string]pendingOIDC{}
	}
	s.si.oidcReqs[req.State] = pendingOIDC{req, now.Add(oidcTTL)}
	s.si.mu.Unlock()
	// Lax, not Strict: the provider sends the browser back from another site, and a Strict cookie would not come along.
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: req.State, Path: "/api/oidc", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: viewOf(r).HTTPS, MaxAge: int(oidcTTL.Seconds())})
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	info := infoOf(r)
	info.login = true
	ip := "i:" + clientIP(r)
	if s.limiter.locked(ip) > 0 {
		signInError(w, r, "Too many failed sign-ins. Try again in a few minutes.")
		return
	}
	failWith := func(detail, msg string) {
		s.limiter.fail(ip, limIP)
		info.detail = "OIDC sign-in failed: " + detail
		signInError(w, r, msg)
	}
	q := r.URL.Query()
	state := q.Get("state")
	ck, err := r.Cookie(oidcCookie)
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Path: "/api/oidc", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: viewOf(r).HTTPS})
	s.si.mu.Lock()
	pend, found := s.si.oidcReqs[state]
	delete(s.si.oidcReqs, state) // a state is good for one return
	s.si.mu.Unlock()
	if state == "" || err != nil || ck.Value != state || !found || time.Now().After(pend.exp) {
		failWith("unknown or expired state", "That sign-in was not started here or has expired. Try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		failWith("provider said "+e, "The identity provider did not sign you in ("+truncate(strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return -1
			}
			return r
		}, e), 40)+").")
		return
	}
	cfg, o, err := s.oidcConfig()
	if err != nil {
		failWith("not configured", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	p, err := s.oidcProvider(ctx, cfg)
	if err != nil {
		logf("oidc: %v", err)
		signInError(w, r, "The identity provider could not be reached. Local accounts still work.")
		return
	}
	id, err := p.Finish(ctx, pend.req, q.Get("code"))
	if err != nil {
		logf("oidc: %v", err)
		failWith("token not accepted", "The identity provider's answer was not accepted. See the controller log for the reason.")
		return
	}
	name := sanitizeName(id.Name)
	if name == "" {
		failWith("no user name", "The identity provider sent no user name (claim "+firstNonEmpty(o.UserClaim, oidcauth.DefaultUserClaim)+" or email). See Settings, Sign-in.")
		return
	}
	info.user = name
	role := roleFromGroups(id.InGroup(o.AdminGroup), id.InGroup(o.OperatorGroup), o.DefaultRole)
	if role == "" {
		if ex, err := s.Store.UserByName(name); err == nil && ex.Source == store.SourceOIDC {
			s.Store.EndSessions(ex.ID) // removed from the group: takes effect now
		}
		info.detail = "OIDC sign-in refused: no access"
		signInError(w, r, errNoAccess.Error())
		return
	}
	u, err := s.Store.UpsertExternal(store.SourceOIDC, id.Issuer+"|"+id.Subject, name, role)
	if err != nil || u.Disabled {
		failWith("account refused", "This account cannot sign in. The name may already be used by another account, or the account is disabled.")
		return
	}
	token, err := s.Store.NewSession(u.ID, store.ExternalSessionTTL)
	if err != nil {
		fail(w, err)
		return
	}
	info.detail = "OIDC sign-in, role " + role
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: viewOf(r).HTTPS, MaxAge: int(store.ExternalSessionTTL.Seconds())})
	http.Redirect(w, r, "/", http.StatusFound)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// endSessionURL is the identity provider's sign-out address for a user who
// signed in through it, or "".
func (s *Server) endSessionURL(r *http.Request, u store.User) string {
	if u.Source != store.SourceOIDC {
		return ""
	}
	cfg, _, err := s.oidcConfig()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	p, err := s.oidcProvider(ctx, cfg)
	if err != nil {
		return ""
	}
	v := viewOf(r)
	scheme := "http"
	if v.HTTPS {
		scheme = "https"
	}
	return p.EndSessionURL(scheme + "://" + v.Host + "/")
}
