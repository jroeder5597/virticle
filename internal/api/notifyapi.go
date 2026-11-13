package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"virticle/internal/store"
)

// The Notifications page: GET/POST /api/notify/destinations, PUT/DELETE
// /api/notify/destinations/{id} and POST .../{id}/test. Administrators only
// (routes not listed in routeLevels need admin). The address is never returned
// after it was saved: only its host and last characters.

func (s *Server) notifyRoutes(auth func(string, http.HandlerFunc)) {
	auth("GET /api/notify/destinations", s.listDests)
	auth("POST /api/notify/destinations", s.createDest)
	auth("PUT /api/notify/destinations/{id}", s.updateDest)
	auth("DELETE /api/notify/destinations/{id}", s.deleteDest)
	auth("POST /api/notify/destinations/{id}/test", s.testDest)
}

type destJSON struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	Tail      string `json:"tail"`
	Insecure  bool   `json:"insecure,omitempty"` // an http:// address
	Severity  string `json:"severity"`
	Cleared   bool   `json:"cleared"`
	HasSecret bool   `json:"hasSecret"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	State     string `json:"state"`            // idle | ok | failing
	Error     string `json:"error,omitempty"`  // when failing
	At        int64  `json:"at,omitempty"`     // of the failure
	LastOK    int64  `json:"lastOk,omitempty"` // last delivery
}

func hostOf(raw string) (host string, insecure bool) {
	if u, err := url.Parse(raw); err == nil {
		return u.Host, u.Scheme == "http"
	}
	return raw, false
}

func (s *Server) destJSON(d notifyDest, host, tail string) destJSON {
	h, insecure := hostOf(host) // host is kept as scheme://host
	j := destJSON{ID: d.ID, Type: d.Kind, Name: d.Name, Host: h, Tail: tail, Insecure: insecure, Severity: d.Severity,
		Cleared: d.Cleared, HasSecret: d.Secret != "", ReadOnly: d.ReadOnly, State: "idle"}
	if w := s.notifier().worker(d.ID); w != nil {
		st := w.status()
		switch {
		case st.Failing:
			j.State, j.Error, j.At = "failing", st.Error, st.At.Unix()
		case !st.LastOK.IsZero():
			j.State, j.LastOK = "ok", st.LastOK.Unix()
		}
	}
	return j
}

func (s *Server) listDests(w http.ResponseWriter, r *http.Request) {
	out := []destJSON{}
	n := s.notifier()
	rows, err := s.Store.NotifyDests()
	if err != nil {
		fail(w, err)
		return
	}
	n.sync()
	live := map[string]notifyDest{}
	for _, d := range s.loadDests() {
		live[d.ID] = d
	}
	for _, r := range rows {
		id := strconv.FormatInt(r.ID, 10)
		out = append(out, s.destJSON(live[id], r.Host, r.Tail))
	}
	if d, ok := live["flag"]; ok {
		host, tail := legacyParts(d.URL)
		out = append(out, s.destJSON(d, host, tail))
	}
	writeJSON(w, map[string]any{"destinations": out, "links": s.publicBase() != ""})
}

type destRequest struct {
	Type        string  `json:"type"`
	Name        *string `json:"name"`
	URL         *string `json:"url"` // empty or absent when changing: keep the saved address
	Severity    *string `json:"severity"`
	Cleared     *bool   `json:"cleared"`
	Secret      *string `json:"secret"` // webhook only; empty or absent keeps the saved one
	ClearSecret bool    `json:"clearSecret"`
}

func cleanName(p *string, def string) (string, error) {
	name := def
	if p != nil {
		name = strings.TrimSpace(*p)
	}
	if name == "" {
		name = def
	}
	if len([]rune(name)) > 60 {
		return "", badRequest("The name is too long (60 characters at most).")
	}
	return name, nil
}

func cleanSeverity(p *string, def string) (string, error) {
	if p == nil || *p == "" {
		return def, nil
	}
	if *p != "warning" && *p != "critical" {
		return "", badRequest("Severity must be \"warning\" (warnings and critical) or \"critical\".")
	}
	return *p, nil
}

func (s *Server) createDest(w http.ResponseWriter, r *http.Request) {
	var q destRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	if _, ok := destKinds[q.Type]; !ok {
		fail(w, badRequest("Type must be slack, mattermost, discord or webhook."))
		return
	}
	if q.URL == nil || strings.TrimSpace(*q.URL) == "" {
		fail(w, badRequest("Enter the address."))
		return
	}
	d := store.NotifyDest{Kind: q.Type, Cleared: true, Severity: "warning"}
	var err error
	if d.Name, err = cleanName(q.Name, destKinds[q.Type]); err != nil {
		fail(w, err)
		return
	}
	if d.Severity, err = cleanSeverity(q.Severity, "warning"); err != nil {
		fail(w, err)
		return
	}
	if q.Cleared != nil {
		d.Cleared = *q.Cleared
	}
	if d.URLEnc, d.Host, d.Tail, err = s.sealURL(q.Type, strings.TrimSpace(*q.URL)); err != nil {
		fail(w, err)
		return
	}
	if q.Secret != nil && *q.Secret != "" {
		if q.Type != "webhook" {
			fail(w, badRequest("Only a webhook can sign its messages."))
			return
		}
		box, err := s.secrets()
		if err == nil {
			d.SecretEnc, err = box.Seal(*q.Secret)
		}
		if err != nil {
			fail(w, err)
			return
		}
	}
	id, err := s.Store.CreateNotifyDest(d, time.Now().Unix())
	if err != nil {
		fail(w, err)
		return
	}
	s.notifier().sync()
	writeJSON(w, map[string]any{"id": strconv.FormatInt(id, 10)})
}

func (s *Server) storedDest(r *http.Request) (store.NotifyDest, error) {
	if r.PathValue("id") == "flag" {
		return store.NotifyDest{}, badRequest("This destination comes from the -alert-webhook flag. Remove the flag to change it.")
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.NotifyDest{}, store.ErrNotFound
	}
	return s.Store.NotifyDest(id)
}

func (s *Server) updateDest(w http.ResponseWriter, r *http.Request) {
	d, err := s.storedDest(r)
	if err != nil {
		fail(w, err)
		return
	}
	var q destRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	if d.Name, err = cleanName(q.Name, d.Name); err != nil {
		fail(w, err)
		return
	}
	if d.Severity, err = cleanSeverity(q.Severity, d.Severity); err != nil {
		fail(w, err)
		return
	}
	if q.Cleared != nil {
		d.Cleared = *q.Cleared
	}
	if q.URL != nil && strings.TrimSpace(*q.URL) != "" {
		if d.URLEnc, d.Host, d.Tail, err = s.sealURL(d.Kind, strings.TrimSpace(*q.URL)); err != nil {
			fail(w, err)
			return
		}
	}
	if q.Secret != nil && *q.Secret != "" || q.ClearSecret {
		if d.Kind != "webhook" {
			fail(w, badRequest("Only a webhook can sign its messages."))
			return
		}
		d.SecretEnc = ""
		if q.Secret != nil && *q.Secret != "" && !q.ClearSecret {
			box, err := s.secrets()
			if err == nil {
				d.SecretEnc, err = box.Seal(*q.Secret)
			}
			if err != nil {
				fail(w, err)
				return
			}
		}
	}
	if err := s.Store.UpdateNotifyDest(d); err != nil {
		fail(w, err)
		return
	}
	s.notifier().sync()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) deleteDest(w http.ResponseWriter, r *http.Request) {
	d, err := s.storedDest(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.DeleteNotifyDest(d.ID); err != nil {
		fail(w, err)
		return
	}
	s.notifier().sync()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) testDest(w http.ResponseWriter, r *http.Request) {
	wk := s.notifier().worker(r.PathValue("id"))
	if wk == nil {
		fail(w, store.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout+2*time.Second)
	defer cancel()
	res := wk.sendTest(ctx)
	writeJSON(w, map[string]any{"ok": res.OK, "status": res.Status, "message": res.Message})
}
