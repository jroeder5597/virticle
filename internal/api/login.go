package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"virticle/internal/dirauth"
	"virticle/internal/store"
)

// login is the first step of signing in with a name and password: a local
// account first, then the directory. An account with two-step sign-in on gets
// a second page before the session exists.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	info := infoOf(r)
	info.login = true
	var c credentials
	if err := readJSON(r, &c); err != nil {
		fail(w, err)
		return
	}
	if len(c.Username) > 256 {
		c.Username = c.Username[:256]
	}
	info.user = c.Username
	ip := clientIP(r)
	pair, ipKey, userKey := "p:"+ip+"|"+c.Username, "i:"+ip, "u:"+c.Username
	if wait := s.limiter.locked(pair, ipKey, userKey); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		deny(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed sign-ins, try again in %d seconds", int(wait.Seconds())+1))
		return
	}
	u, err := s.Store.CheckPassword(c.Username, c.Password)
	dirDown := false
	if err != nil {
		u, err = s.directoryLogin(c.Username, c.Password)
		dirDown = errors.Is(err, errDirectoryDown)
	}
	switch {
	case errors.Is(err, errNoAccess):
		// The password was right; the directory says this person has no role here.
		info.detail = "directory sign-in refused: no access"
		deny(w, http.StatusForbidden, errNoAccess.Error())
		return
	case err != nil:
		if !dirDown {
			s.limiter.fail(pair, limPair)
			s.limiter.fail(ipKey, limIP)
			s.limiter.fail(userKey, limUser)
		}
		msg := "wrong username or password"
		if dirDown {
			msg += " (the directory could not be reached; local accounts still work)"
			info.detail = "directory unreachable"
		}
		deny(w, http.StatusUnauthorized, msg)
		return
	}
	if !u.TwoStep {
		s.limiter.reset(pair) // with a second step the failures stay counted until it passes
	}
	s.afterPassword(w, r, u)
}

// directoryLogin checks name and password in the directory and creates or
// updates the account, with the role from the group mapping. A name that
// belongs to a local or provider account is never taken over by the directory.
func (s *Server) directoryLogin(name, password string) (store.User, error) {
	l := s.signIn().LDAP
	if !l.Enabled {
		return store.User{}, store.ErrNotFound
	}
	if ex, err := s.Store.UserByName(name); err == nil && ex.Source != store.SourceLDAP {
		return store.User{}, store.ErrNotFound
	}
	bindPw, err := s.sealedSecret(ldapSecretKey)
	if err != nil {
		logf("directory sign-in: %v", err)
		return store.User{}, errDirectoryDown
	}
	res, err := dirauth.Authenticate(ldapConfig(l, bindPw), name, password)
	var ue *dirauth.UnreachableError
	switch {
	case errors.As(err, &ue):
		logf("directory sign-in: %v", err)
		return store.User{}, errDirectoryDown
	case err != nil:
		return store.User{}, store.ErrNotFound
	}
	uname := sanitizeName(res.Name)
	if uname == "" {
		return store.User{}, store.ErrNotFound
	}
	role := roleFromGroups(res.InGroup(l.AdminGroup), res.InGroup(l.OperatorGroup), l.DefaultRole)
	if role == "" {
		if ex, err := s.Store.UserByName(uname); err == nil && ex.Source == store.SourceLDAP {
			s.Store.EndSessions(ex.ID)
		}
		return store.User{}, errNoAccess
	}
	u, err := s.Store.UpsertExternal(store.SourceLDAP, res.DN, uname, role)
	if err != nil || u.Disabled {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}
