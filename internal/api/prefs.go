package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// User preferences: small UI choices that follow the user across browsers. Only the keys below exist;
// everything else is refused, so the table cannot become a dumping ground.
var prefAllowed = map[string][]string{
	"tasksDock":    {"open", "closed"}, // the docked Tasks panel
	"consoleClick": {"tab", "inline"},  // what a click on the console preview does
}

const prefMaxKeys = 16

// validatePrefs checks a partial update. Values must be strings from the allow-list.
func validatePrefs(in map[string]json.RawMessage) (map[string]string, error) {
	if len(in) == 0 {
		return nil, badRequest("no preferences given")
	}
	if len(in) > prefMaxKeys {
		return nil, badRequest("too many preferences in one request")
	}
	out := make(map[string]string, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		allowed, ok := prefAllowed[k]
		if !ok {
			return nil, badRequest(fmt.Sprintf("unknown preference %q", clip(k, 40)))
		}
		var v string
		if err := json.Unmarshal(in[k], &v); err != nil {
			return nil, badRequest(fmt.Sprintf("preference %s must be a string", k))
		}
		good := false
		for _, a := range allowed {
			good = good || a == v
		}
		if !good {
			return nil, badRequest(fmt.Sprintf("preference %s must be one of %v", k, allowed))
		}
		out[k] = v
	}
	return out, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// GET /api/me/prefs. Every role, but a web session only: an API token acts for a script, has no
// preferences, and is answered 403 like the other account endpoints.
func (s *Server) getPrefs(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	m, err := s.Store.UserPrefs(p.User.ID)
	if err != nil {
		fail(w, err)
		return
	}
	for k := range m { // a key retired from the allow-list is not reported
		if _, ok := prefAllowed[k]; !ok {
			delete(m, k)
		}
	}
	writeJSON(w, m)
}

// PUT /api/me/prefs with a partial object; the answer is the full set.
func (s *Server) putPrefs(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	var in map[string]json.RawMessage
	if err := readJSON(r, &in); err != nil {
		fail(w, err)
		return
	}
	set, err := validatePrefs(in)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.SetUserPrefs(p.User.ID, set); err != nil {
		fail(w, err)
		return
	}
	s.getPrefs(w, r)
}
