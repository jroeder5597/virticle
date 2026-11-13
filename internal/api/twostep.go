package api

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"virticle/internal/dirauth"
	"virticle/internal/store"
	"virticle/internal/totp"
)

const (
	issuerName        = "Virticle"
	challengeTTL      = 5 * time.Minute
	maxChallengeFails = 5
	setupTTL          = 15 * time.Minute
	recoveryCount     = 10
)

// challenge is a sign-in that passed the password and waits for the code.
type challenge struct {
	userID   int64
	username string
	exp      time.Time
	fails    int
}

// pendingSetup is a secret shown to a user who has not yet confirmed it with a code.
type pendingSetup struct {
	secret string
	exp    time.Time
}

// totpNow is the time codes are checked against; tests move it.
func (s *Server) totpNow() time.Time {
	if s.TOTPClock != nil {
		return s.TOTPClock()
	}
	return time.Now()
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// recoveryAlphabet leaves out look-alike characters (0/o, 1/l/i).
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func newRecoveryCodes() []string {
	out := make([]string, recoveryCount)
	max := big.NewInt(int64(len(recoveryAlphabet)))
	for i := range out {
		var b strings.Builder
		for j := 0; j < 10; j++ {
			n, _ := rand.Int(rand.Reader, max)
			b.WriteByte(recoveryAlphabet[n.Int64()])
			if j == 4 {
				b.WriteByte('-')
			}
		}
		out[i] = b.String()
	}
	return out
}

func recoveryHashes(userID int64, codes []string) []string {
	h := make([]string, len(codes))
	for i, c := range codes {
		h[i] = store.HashRecoveryCode(userID, c)
	}
	return h
}

var sixDigits = regexp.MustCompile(`^\d{6}$`)

// checkSecondStep accepts a current authenticator code or a recovery code for
// the user. used says which one it was (for the audit log); a recovery code is
// consumed.
func (s *Server) checkSecondStep(userID int64, code string) (ok bool, recovery bool, err error) {
	ts, err := s.Store.TwoStep(userID)
	if err != nil || !ts.Enabled {
		return false, false, err
	}
	code = strings.TrimSpace(code)
	if compact := strings.ReplaceAll(code, " ", ""); sixDigits.MatchString(compact) {
		b, err := s.box()
		if err != nil {
			return false, false, err
		}
		secret, err := b.Unseal(ts.Secret)
		if err != nil {
			return false, false, err
		}
		step, good := totp.Verify(secret, compact, s.totpNow(), ts.Step)
		if !good {
			return false, false, nil
		}
		ok, err = s.Store.UseTwoStepStep(userID, step) // false: the same code was used a moment ago
		return ok, false, err
	}
	if len(code) < 8 || len(code) > 16 {
		return false, false, nil
	}
	ok, err = s.Store.UseRecoveryCode(userID, store.HashRecoveryCode(userID, code))
	return ok, ok, err
}

// openSession starts the browser session after every step has passed.
func (s *Server) openSession(w http.ResponseWriter, r *http.Request, u store.User) {
	ttl := 7 * 24 * time.Hour
	if u.Source != store.SourceLocal {
		ttl = store.ExternalSessionTTL
	}
	token, err := s.Store.NewSession(u.ID, ttl)
	if err != nil {
		fail(w, err)
		return
	}
	infoOf(r).user = u.Username
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: viewOf(r).HTTPS, MaxAge: int(ttl.Seconds()),
	})
	writeJSON(w, map[string]any{"user": u.Username, "setup": s.mustSetUpTwoStep(u)})
}

// afterPassword continues a sign-in whose first step passed: it asks for the
// code when the user has two-step sign-in on, else opens the session.
func (s *Server) afterPassword(w http.ResponseWriter, r *http.Request, u store.User) {
	if !u.TwoStep {
		s.openSession(w, r, u)
		return
	}
	tok := randomToken()
	s.si.mu.Lock()
	if s.si.challenges == nil {
		s.si.challenges = map[string]*challenge{}
	}
	now := time.Now()
	for k, c := range s.si.challenges {
		if now.After(c.exp) {
			delete(s.si.challenges, k)
		}
	}
	s.si.challenges[tok] = &challenge{userID: u.ID, username: u.Username, exp: now.Add(challengeTTL)}
	s.si.mu.Unlock()
	writeJSON(w, map[string]any{"second": true, "challenge": tok})
}

func (s *Server) loginSecond(w http.ResponseWriter, r *http.Request) {
	info := infoOf(r)
	info.login = true
	var req struct{ Challenge, Code string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	s.si.mu.Lock()
	c := s.si.challenges[req.Challenge]
	if c != nil && time.Now().After(c.exp) {
		delete(s.si.challenges, req.Challenge)
		c = nil
	}
	s.si.mu.Unlock()
	if c == nil {
		deny(w, http.StatusUnauthorized, "this sign-in has expired, start again with your password")
		return
	}
	info.user = c.username
	ip := clientIP(r)
	pair, ipKey, userKey := "p:"+ip+"|"+c.username, "i:"+ip, "u:"+c.username
	if wait := s.limiter.locked(pair, ipKey, userKey); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		deny(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed sign-ins, try again in %d seconds", int(wait.Seconds())+1))
		return
	}
	ok, recovery, err := s.checkSecondStep(c.userID, req.Code)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		s.limiter.fail(pair, limPair)
		s.limiter.fail(ipKey, limIP)
		s.limiter.fail(userKey, limUser)
		info.detail = "second step failed"
		s.si.mu.Lock()
		if c.fails++; c.fails >= maxChallengeFails {
			delete(s.si.challenges, req.Challenge) // start again with the password
		}
		s.si.mu.Unlock()
		deny(w, http.StatusUnauthorized, "that code is not right")
		return
	}
	s.si.mu.Lock()
	delete(s.si.challenges, req.Challenge)
	s.si.mu.Unlock()
	u, err := s.Store.User(c.userID)
	if err != nil || u.Disabled {
		deny(w, http.StatusUnauthorized, "this account cannot sign in")
		return
	}
	s.limiter.reset(pair)
	if recovery {
		left, _ := s.Store.RecoveryCodesLeft(u.ID)
		info.detail = fmt.Sprintf("recovery code used (%d left)", left)
	}
	s.openSession(w, r, u)
}

// --- own account ---

// twoStepRoutes are the only requests a user who must set up two-step sign-in may make until done.
var twoStepRoutes = map[string]bool{
	"GET /api/account/2fa":          true,
	"POST /api/account/2fa/begin":   true,
	"POST /api/account/2fa/confirm": true,
}

func (s *Server) getTwoStep(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	left, _ := s.Store.RecoveryCodesLeft(p.User.ID)
	writeJSON(w, map[string]any{
		"enabled":      p.User.TwoStep,
		"required":     twoStepRequired(s.signIn().Require2FA, p.User),
		"recoveryLeft": left,
		"external":     p.User.Source == store.SourceOIDC,
		"source":       p.User.Source,
	})
}

// qrPath draws the code as one SVG path of unit squares: the page scales it.
func qrPath(content string) (size int, path string, err error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return 0, "", err
	}
	bm := q.Bitmap() // includes the quiet border
	var b strings.Builder
	for y, row := range bm {
		for x := 0; x < len(row); {
			if !row[x] {
				x++
				continue
			}
			start := x
			for x < len(row) && row[x] {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start, y, x-start, x-start)
		}
	}
	return len(bm), b.String(), nil
}

func (s *Server) beginTwoStep(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	if p.User.Source == store.SourceOIDC {
		deny(w, http.StatusBadRequest, "your identity provider handles the second step for you")
		return
	}
	if p.User.TwoStep {
		deny(w, http.StatusConflict, "two-step sign-in is already on")
		return
	}
	secret, err := totp.NewSecret()
	if err != nil {
		fail(w, err)
		return
	}
	uri := totp.URI(issuerName, p.User.Username, secret)
	size, path, err := qrPath(uri)
	if err != nil {
		fail(w, err)
		return
	}
	s.si.mu.Lock()
	if s.si.setups == nil {
		s.si.setups = map[int64]pendingSetup{}
	}
	s.si.setups[p.User.ID] = pendingSetup{secret, time.Now().Add(setupTTL)}
	s.si.mu.Unlock()
	writeJSON(w, map[string]any{"secret": secret, "uri": uri, "qr": map[string]any{"size": size, "path": path}})
}

func (s *Server) confirmTwoStep(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	var req struct{ Code string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	key := "2fa:" + p.User.Username
	if wait := s.limiter.locked(key); wait > 0 {
		deny(w, http.StatusTooManyRequests, "too many wrong codes, try again later")
		return
	}
	s.si.mu.Lock()
	pend, found := s.si.setups[p.User.ID]
	s.si.mu.Unlock()
	if !found || time.Now().After(pend.exp) {
		deny(w, http.StatusBadRequest, "this set-up has expired, start again")
		return
	}
	step, good := totp.Verify(pend.secret, req.Code, s.totpNow(), 0)
	if !good {
		s.limiter.fail(key, limPair)
		deny(w, http.StatusBadRequest, "that code is not right; check the clock of your phone and try the next code")
		return
	}
	b, err := s.box()
	if err != nil {
		fail(w, err)
		return
	}
	sealed, err := b.Seal(pend.secret)
	if err != nil {
		fail(w, err)
		return
	}
	codes := newRecoveryCodes()
	if err := s.Store.EnableTwoStep(p.User.ID, sealed, step, recoveryHashes(p.User.ID, codes)); err != nil {
		failAuth(w, err)
		return
	}
	s.limiter.reset(key)
	s.si.mu.Lock()
	delete(s.si.setups, p.User.ID)
	s.si.mu.Unlock()
	infoOf(r).detail = "two-step sign-in turned on"
	writeJSON(w, map[string]any{"codes": codes})
}

// verifyPassword checks the account's own password: the local one, or the
// directory's for a directory account.
func (s *Server) verifyPassword(u store.User, pw string) bool {
	switch u.Source {
	case store.SourceLocal:
		_, err := s.Store.CheckPassword(u.Username, pw)
		return err == nil
	case store.SourceLDAP:
		l := s.signIn().LDAP
		if !l.Enabled || pw == "" {
			return false
		}
		bindPw, err := s.sealedSecret(ldapSecretKey)
		if err != nil {
			return false
		}
		res, err := dirauth.Authenticate(ldapConfig(l, bindPw), u.Username, pw)
		return err == nil && sanitizeName(res.Name) == u.Username
	}
	return false
}

func (s *Server) disableTwoStep(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	var req struct{ Password, Code string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if twoStepRequired(s.signIn().Require2FA, p.User) {
		deny(w, http.StatusForbidden, "an administrator requires two-step sign-in for your account")
		return
	}
	key := "2fa:" + p.User.Username
	if wait := s.limiter.locked(key); wait > 0 {
		deny(w, http.StatusTooManyRequests, "too many wrong attempts, try again later")
		return
	}
	good := s.verifyPassword(p.User, req.Password)
	if good {
		var err error
		if good, _, err = s.checkSecondStep(p.User.ID, req.Code); err != nil {
			fail(w, err)
			return
		}
	}
	if !good {
		s.limiter.fail(key, limPair)
		deny(w, http.StatusBadRequest, "the password or the code is wrong")
		return
	}
	if err := s.Store.DisableTwoStep(p.User.ID); err != nil {
		fail(w, err)
		return
	}
	s.limiter.reset(key)
	infoOf(r).detail = "two-step sign-in turned off"
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) newRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	var req struct{ Password, Code string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	key := "2fa:" + p.User.Username
	if wait := s.limiter.locked(key); wait > 0 {
		deny(w, http.StatusTooManyRequests, "too many wrong attempts, try again later")
		return
	}
	good := p.User.TwoStep && s.verifyPassword(p.User, req.Password)
	if good {
		var err error
		if good, _, err = s.checkSecondStep(p.User.ID, req.Code); err != nil {
			fail(w, err)
			return
		}
	}
	if !good {
		s.limiter.fail(key, limPair)
		deny(w, http.StatusBadRequest, "the password or the code is wrong")
		return
	}
	codes := newRecoveryCodes()
	if err := s.Store.ReplaceRecoveryCodes(p.User.ID, recoveryHashes(p.User.ID, codes)); err != nil {
		fail(w, err)
		return
	}
	infoOf(r).detail = "new recovery codes made"
	writeJSON(w, map[string]any{"codes": codes})
}

// resetTwoStep is the administrator's way out for a user who lost the phone.
func (s *Server) resetTwoStepUser(w http.ResponseWriter, r *http.Request) {
	id, err := userID(r)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := s.Store.User(id)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.ResetTwoStep(id); err != nil {
		fail(w, err)
		return
	}
	infoOf(r).detail = "two-step sign-in reset for " + u.Username
	writeJSON(w, map[string]bool{"ok": true})
}

var nameChars = regexp.MustCompile(`[^A-Za-z0-9._@-]`)

// sanitizeName makes a name from a provider fit the user name rules.
func sanitizeName(n string) string {
	n = nameChars.ReplaceAllString(strings.TrimSpace(n), "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

var errDirectoryDown = errors.New("the directory could not be reached")
