package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"virticle/internal/store"
)

func logf(format string, a ...any) { log.Printf(format, a...) }

const (
	minPasswordLen = 10
	maxPasswordLen = 72 // bcrypt ignores everything after 72 bytes
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func checkUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return badRequest("the username must be 1-64 characters: letters, digits, . _ @ -")
	}
	return nil
}

// checkPassword is the whole password policy: a minimum length.
func checkPassword(p string) error {
	if utf8.RuneCountInString(p) < minPasswordLen {
		return badRequest(fmt.Sprintf("the password must be at least %d characters", minPasswordLen))
	}
	if len(p) > maxPasswordLen {
		return badRequest(fmt.Sprintf("the password must be at most %d bytes", maxPasswordLen))
	}
	return nil
}

func validRole(r string) bool { return store.RoleRank(r) > 0 }

// failAuth maps the store's account errors to status codes, and everything
// else to fail.
func failAuth(w http.ResponseWriter, err error) {
	code := 0
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrLastAdmin):
		code = http.StatusConflict
	case errors.Is(err, store.ErrBadPassword):
		code = http.StatusBadRequest
	}
	if code == 0 {
		fail(w, err)
		return
	}
	deny(w, code, err.Error())
}

func clientIP(r *http.Request) string {
	return viewOf(r).IP
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// setup creates the first administrator. It only works while no user exists.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	infoOf(r).login = true
	var c credentials
	if err := readJSON(r, &c); err != nil {
		fail(w, err)
		return
	}
	infoOf(r).user = c.Username
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, err)
		return
	}
	if n > 0 {
		fail(w, badRequest("setup has already been completed"))
		return
	}
	if err := checkUsername(c.Username); err != nil {
		fail(w, err)
		return
	}
	if err := checkPassword(c.Password); err != nil {
		fail(w, err)
		return
	}
	if _, err := s.Store.CreateFirstUser(c.Username, c.Password); err != nil {
		failAuth(w, err)
		return
	}
	s.startSession(w, r, c)
}

// Failure thresholds before a lock starts: a user from one address, an
// address on its own, and a user from anywhere.
const (
	limPair = 5
	limIP   = 30
	limUser = 30
)

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, c credentials) {
	ip := clientIP(r)
	pair, ipKey, userKey := "p:"+ip+"|"+c.Username, "i:"+ip, "u:"+c.Username
	if wait := s.limiter.locked(pair, ipKey, userKey); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		deny(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed sign-ins, try again in %d seconds", int(wait.Seconds())+1))
		return
	}
	token, _, err := s.Store.Login(c.Username, c.Password)
	if err != nil {
		s.limiter.fail(pair, limPair)
		s.limiter.fail(ipKey, limIP)
		s.limiter.fail(userKey, limUser)
		deny(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.limiter.reset(pair)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: viewOf(r).HTTPS, MaxAge: 7 * 24 * 3600,
	})
	writeJSON(w, map[string]string{"user": c.Username})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	infoOf(r).login = true
	out := map[string]any{"ok": true}
	if c, err := r.Cookie(cookieName); err == nil {
		if p, err := s.Store.SessionUser(c.Value); err == nil {
			infoOf(r).user = p.User.Username
			if u := s.endSessionURL(r, p.User); u != "" {
				out["endSession"] = u
				out["provider"] = s.signIn().OIDC.Name
			}
		}
		s.Store.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: viewOf(r).HTTPS})
	writeJSON(w, out)
}

// --- users (admin) ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.Users()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, u)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Role string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := checkUsername(req.Username); err != nil {
		fail(w, err)
		return
	}
	if err := checkPassword(req.Password); err != nil {
		fail(w, err)
		return
	}
	if !validRole(req.Role) {
		fail(w, badRequest("the role must be admin, operator or viewer"))
		return
	}
	u, err := s.Store.CreateUser(req.Username, req.Password, req.Role)
	if err != nil {
		failAuth(w, err)
		return
	}
	writeJSON(w, u)
}

func userID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, store.ErrNotFound
	}
	return id, nil
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := userID(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
		Disabled *bool   `json:"disabled"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Role != nil && !validRole(*req.Role) {
		fail(w, badRequest("the role must be admin, operator or viewer"))
		return
	}
	if target, err := s.Store.User(id); err == nil && target.Source != store.SourceLocal && (req.Role != nil || req.Password != nil) {
		fail(w, badRequest("this account comes from your identity provider or directory: its role is set by its groups and it has no password here"))
		return
	}
	if req.Password != nil {
		if err := checkPassword(*req.Password); err != nil {
			fail(w, err)
			return
		}
	}
	if err := s.Store.UpdateUser(id, store.UserUpdate{Role: req.Role, Password: req.Password, Disabled: req.Disabled}); err != nil {
		failAuth(w, err)
		return
	}
	u, err := s.Store.User(id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, u)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := userID(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.DeleteUser(id); err != nil {
		failAuth(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// --- own account ---

// accountSession returns the caller's own account, refusing API tokens: a
// leaked token must not be able to mint more tokens or change the password.
func accountSession(w http.ResponseWriter, r *http.Request) (store.Principal, string, bool) {
	info := infoOf(r)
	if info.session == "" {
		deny(w, http.StatusForbidden, "forbidden: account settings need a web session, not an API token")
		return store.Principal{}, "", false
	}
	return principalOf(r), info.session, true
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p, session, ok := accountSession(w, r)
	if !ok {
		return
	}
	var req struct{ Current, New string }
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := checkPassword(req.New); err != nil {
		fail(w, err)
		return
	}
	// Guessing the current password through a stolen session is still guessing.
	key := "pw:" + p.User.Username
	if wait := s.limiter.locked(key); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		deny(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if err := s.Store.ChangePassword(p.User.ID, req.Current, req.New, session); err != nil {
		if errors.Is(err, store.ErrBadPassword) {
			s.limiter.fail(key, limPair)
		}
		failAuth(w, err)
		return
	}
	s.limiter.reset(key)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	t, err := s.Store.Tokens(p.User.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		ExpiresDays int    `json:"expiresDays"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Name == "" || len(req.Name) > 64 {
		fail(w, badRequest("a token name of 1-64 characters is required"))
		return
	}
	if req.Role == "" {
		req.Role = p.Role
	}
	if !validRole(req.Role) || store.RoleRank(req.Role) > store.RoleRank(p.Role) {
		fail(w, badRequest("a token can have your role or a lower one"))
		return
	}
	if req.ExpiresDays < 0 || req.ExpiresDays > 3650 {
		fail(w, badRequest("expiresDays must be between 0 (never) and 3650"))
		return
	}
	var expires int64
	if req.ExpiresDays > 0 {
		expires = time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour).Unix()
	}
	t, secret, err := s.Store.CreateToken(p.User.ID, req.Name, req.Role, expires)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"token": t, "secret": secret})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	id, err := userID(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.DeleteToken(p.User.ID, id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// --- audit log (admin) ---

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(k string) int64 { n, _ := strconv.ParseInt(q.Get(k), 10, 64); return n }
	f := store.AuditFilter{
		User: q.Get("user"), Method: q.Get("method"), Path: q.Get("path"),
		Status: int(num("status")), Since: num("since"), Until: num("until"), Before: num("before"), Limit: int(num("limit")),
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	e, err := s.Store.Audit(f)
	if err != nil {
		fail(w, err)
		return
	}
	var next int64
	if len(e) == f.Limit {
		next = e[len(e)-1].ID
	}
	writeJSON(w, map[string]any{"entries": e, "next": next})
}
