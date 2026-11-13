package webaccess

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"virticle/internal/tlscert"
)

// Persist is the part of the database the manager uses.
type Persist interface {
	WebSetting(key string) (string, bool, error)
	SetWebSetting(key, value string) error
	DeleteWebSetting(key string) error
}

const (
	keySettings = "web"
	keyPending  = "web.pending"

	// DefaultWindow is how long the old listener stays after a change.
	DefaultWindow = 60 * time.Second
)

// ErrLocked means the settings are fixed by command-line flags.
var ErrLocked = errors.New("the web access settings are fixed by command-line flags")

// ErrInvalid wraps a refused proposal; the checks say why.
var ErrInvalid = errors.New("the settings did not pass the checks")

// Manager owns the listeners.
type Manager struct {
	Store   Persist
	DataDir string
	Handler http.Handler
	Logf    func(string, ...any)
	// Window is the time to confirm a change; zero means DefaultWindow.
	Window time.Duration
	// OnChange is told about the settings that are in force whenever they
	// change (the API layer reads the trusted proxies, HSTS and public URL).
	OnChange func(Settings)
	// Locked, when not empty, is why the settings cannot be changed.
	Locked string
	// Notice, when not empty, is a one-line note the page shows.
	Notice string
	// Source says where the settings came from: default, stored or flags.
	Source string

	mu   sync.Mutex
	cur  Settings
	set  *listenerSet
	pend *pending
	gen  int
}

type served struct {
	kind  string // http, https, redirect
	addr  string // as configured
	srv   *http.Server
	ln    net.Listener
	su    *setup
	quiet atomic.Bool // the socket was closed on purpose
}

// close stops the server and, in case Serve has not started yet, the socket.
func (sv *served) close() {
	sv.quiet.Store(true)
	sv.srv.Close()
	sv.ln.Close()
}

// release gives up the address but lets requests in progress (the one that is
// changing the settings) and open connections finish.
func (sv *served) release() {
	sv.quiet.Store(true)
	sv.ln.Close()
}

type setup struct {
	s   Settings
	gen int
	// alias, when set, is the generation that requests on connections of a
	// released listener count as: they now belong to the listener that took
	// over the address.
	alias atomic.Int64
	tls   *tls.Config
	fc    *FileCert
	acme  *acmeState
}

type listenerSet struct {
	su      *setup
	servers []*served
}

type pending struct {
	prev     Settings
	prevSet  *listenerSet
	next     *listenerSet
	taken    []*served // old listeners closed to free their address
	deadline time.Time
	gen      int
	timer    *time.Timer
}

// Pending is what the API shows while a change waits for confirmation.
type Pending struct {
	Gen      int       `json:"gen"`
	Deadline time.Time `json:"deadline"`
	PrevMode string    `json:"previousMode"`
	// URL is where the new settings can be reached (best guess).
	URL string `json:"url"`
}

// Listener is one open listening socket.
type Listener struct {
	Kind string `json:"kind"`
	Addr string `json:"addr"`
	Gen  int    `json:"gen"`
}

// Status is everything the page needs to show.
type Status struct {
	Settings  Settings    `json:"settings"`
	Source    string      `json:"source"`
	Locked    string      `json:"locked,omitempty"`
	Notice    string      `json:"notice,omitempty"`
	Listeners []Listener  `json:"listeners"`
	Pending   *Pending    `json:"pending,omitempty"`
	Cert      *CertInfo   `json:"cert,omitempty"`
	CertError string      `json:"certError,omitempty"`
	ACME      *ACMEStatus `json:"acme,omitempty"`
	Window    int         `json:"windowSeconds"`
}

func (m *Manager) logf(f string, a ...any) {
	if m.Logf != nil {
		m.Logf(f, a...)
	}
}

func (m *Manager) window() time.Duration {
	if m.Window > 0 {
		return m.Window
	}
	return DefaultWindow
}

type pendingRecord struct {
	Prev     Settings  `json:"prev"`
	Deadline time.Time `json:"deadline"`
}

// Load returns the stored settings. A change that was never confirmed (the
// controller stopped during the window) is undone first.
func (m *Manager) Load() (Settings, bool, error) {
	if raw, ok, err := m.Store.WebSetting(keyPending); err != nil {
		return Settings{}, false, err
	} else if ok {
		var pr pendingRecord
		if json.Unmarshal([]byte(raw), &pr) == nil {
			b, _ := json.Marshal(pr.Prev)
			if err := m.Store.SetWebSetting(keySettings, string(b)); err != nil {
				return Settings{}, false, err
			}
			m.logf("web: a change was never confirmed; went back to the previous settings (%s)", pr.Prev.Mode)
		}
		if err := m.Store.DeleteWebSetting(keyPending); err != nil {
			return Settings{}, false, err
		}
	}
	raw, ok, err := m.Store.WebSetting(keySettings)
	if err != nil || !ok {
		return Settings{}, false, err
	}
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Settings{}, false, fmt.Errorf("stored web settings: %v (run: virticle web reset)", err)
	}
	return s.Normalize(), true, nil
}

// Peek is Load for a program other than the running controller (virticle
// setup): it only reads. A change that waits for confirmation is reported, not
// undone: the controller runs that change now and Load would reset the stored
// settings under it, so that confirming it later would keep the old ones.
// settings are the stored (new) settings, prev the settings the change would
// go back to, nil when no change is waiting.
func (m *Manager) Peek() (settings Settings, prev *Settings, ok bool, err error) {
	raw, ok, err := m.Store.WebSetting(keySettings)
	if err != nil || !ok {
		return Settings{}, nil, false, err
	}
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Settings{}, nil, false, fmt.Errorf("stored web settings: %v (run: virticle web reset)", err)
	}
	if praw, pok, perr := m.Store.WebSetting(keyPending); perr == nil && pok {
		var pr pendingRecord
		if json.Unmarshal([]byte(praw), &pr) == nil {
			p := pr.Prev.Normalize()
			prev = &p
		}
	}
	return s.Normalize(), prev, true, nil
}

// Start opens the listeners for s. It fails if one cannot be opened.
func (m *Manager) Start(s Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, _, err := m.open(s, nil)
	if err != nil {
		return err
	}
	m.cur, m.set = s, set
	m.change(s)
	return nil
}

// StartOrFallBack is Start, except that a TLS mode whose HTTPS address names an
// interface address this machine no longer has (EADDRNOTAVAIL) does not fail:
// it listens on all interfaces on the same port instead. warning says so, for
// the log and for the Web access page; it is empty otherwise.
func (m *Manager) StartOrFallBack(s Settings) (used Settings, warning string, err error) {
	err = m.Start(s)
	if err == nil || !s.IsTLS() || !errors.Is(err, syscall.EADDRNOTAVAIL) {
		return s, "", err
	}
	_, port, perr := SplitAddr(s.HTTPSListen)
	if perr != nil {
		return s, "", err
	}
	old := s.HTTPSListen
	s.HTTPSListen = ComposeListen("", port)
	if err2 := m.Start(s); err2 != nil {
		return s, "", err
	}
	warning = fmt.Sprintf("The address %s is no longer on this machine, so the web UI listens on all interfaces, port %d. Choose an interface below to change it.", old, port)
	return s, warning, nil
}

func (m *Manager) change(s Settings) {
	if m.OnChange != nil {
		m.OnChange(s)
	}
}

// Current returns the settings in force.
func (m *Manager) Current() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// InUse reports whether the controller already listens on addr.
func (m *Manager) InUse(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inUse(addr)
}

func (m *Manager) inUse(addr string) bool {
	for _, set := range []*listenerSet{m.set} {
		if set == nil {
			continue
		}
		for _, sv := range set.servers {
			if sv.addr == addr {
				return true
			}
		}
	}
	return false
}

type endpoint struct{ kind, addr string }

func endpoints(s Settings) []endpoint {
	if !s.IsTLS() {
		return []endpoint{{"http", s.Listen}}
	}
	eps := []endpoint{{"https", s.HTTPSListen}}
	if s.HTTPRedirect != "" {
		eps = append(eps, endpoint{"redirect", s.HTTPRedirect})
	}
	return eps
}

// prepare loads or makes the certificate source for s.
func (m *Manager) prepare(s Settings, gen int) (*setup, error) {
	su := &setup{s: s, gen: gen}
	switch s.Mode {
	case ModeSelfSigned:
		names := splitNames(s.Names)
		if len(names.Hosts) == 0 && len(names.IPs) == 0 {
			names = tlscert.NamesFor(s.HTTPSListen)
		}
		cf, kf, generated, err := tlscert.Ensure(m.DataDir, names, time.Now())
		if err != nil {
			return nil, fmt.Errorf("self-signed certificate: %v", err)
		}
		if generated {
			m.logf("web: generated a self-signed certificate in %s", cf)
		}
		if su.fc, err = NewFileCert(cf, kf); err != nil {
			return nil, err
		}
	case ModeOwn:
		fc, err := NewFileCert(s.Own.CertFile, s.Own.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("certificate: %v", err)
		}
		fc.Logf = m.Logf
		su.fc = fc
	case ModeACME:
		su.acme = newACME(s, m.DataDir)
	}
	switch {
	case su.acme != nil:
		su.tls = su.acme.tlsConfig()
	case su.fc != nil:
		su.tls = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: su.fc.Get}
	}
	return su, nil
}

func splitNames(list []string) tlscert.Names {
	var n tlscert.Names
	for _, v := range list {
		if ip := net.ParseIP(v); ip != nil {
			n.IPs = append(n.IPs, ip)
		} else {
			n.Hosts = append(n.Hosts, v)
		}
	}
	return n
}

type genKey struct{}

// GenOf says which generation of listeners accepted the request (0 if it did
// not come through a managed listener).
func GenOf(r *http.Request) int {
	g, _ := r.Context().Value(genKey{}).(int)
	return g
}

func (m *Manager) handlerFor(ep endpoint, su *setup) http.Handler {
	if ep.kind == "redirect" {
		var h http.Handler = RedirectHandler(su.s)
		if su.acme != nil {
			h = su.acme.httpHandler(h)
		}
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gen := su.gen
		if a := int(su.alias.Load()); a > 0 {
			gen = a
		}
		m.Handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), genKey{}, gen)))
	})
}

func inUseErr(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// open starts every listener of s. An address that an existing listener
// holds is taken over: that listener is closed and returned in taken, so a
// failed or reverted change can restore it.
func (m *Manager) open(s Settings, takeFrom *listenerSet) (set *listenerSet, taken []*served, err error) {
	m.gen++
	su, err := m.prepare(s, m.gen)
	if err != nil {
		return nil, nil, err
	}
	set = &listenerSet{su: su}
	undo := func() {
		for _, sv := range set.servers {
			sv.close()
		}
		for _, sv := range taken {
			takeFrom.servers = append(takeFrom.servers, m.reopen(sv))
		}
	}
	for _, ep := range endpoints(s) {
		ln, lerr := net.Listen("tcp", ep.addr)
		if lerr != nil && inUseErr(lerr) && takeFrom != nil {
			for i, old := range takeFrom.servers {
				if old.addr == ep.addr {
					old.release()
					old.su.alias.Store(int64(m.gen))
					taken = append(taken, old)
					takeFrom.servers = append(append([]*served{}, takeFrom.servers[:i]...), takeFrom.servers[i+1:]...)
					ln, lerr = net.Listen("tcp", ep.addr)
					break
				}
			}
		}
		if lerr != nil {
			undo()
			return nil, nil, fmt.Errorf("cannot listen on %s: %w", ep.addr, lerr)
		}
		set.servers = append(set.servers, m.serve(ep, ln, su))
	}
	return set, taken, nil
}

func (m *Manager) serve(ep endpoint, ln net.Listener, su *setup) *served {
	sv := &served{kind: ep.kind, addr: ep.addr, ln: ln, su: su}
	sv.srv = &http.Server{
		Handler: m.handlerFor(ep, su),
		// No Read/WriteTimeout: they would cut off console WebSockets, volume
		// uploads and long task requests. Slow header attacks are bounded here.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if ep.kind == "https" {
		sv.srv.TLSConfig = su.tls
		go func() {
			if err := sv.srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) && !sv.quiet.Load() {
				m.logf("web: https listener %s: %v", ep.addr, err)
			}
		}()
	} else {
		go func() {
			if err := sv.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !sv.quiet.Load() {
				m.logf("web: listener %s: %v", ep.addr, err)
			}
		}()
	}
	return sv
}

// reopen restarts a listener that was closed to free its address.
func (m *Manager) reopen(old *served) *served {
	var ln net.Listener
	var err error
	for i := 0; i < 20; i++ { // the port may need a moment after Close
		if ln, err = net.Listen("tcp", old.addr); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		m.logf("web: cannot restore the listener on %s: %v", old.addr, err)
		return old
	}
	old.su.alias.Store(0)
	return m.serve(endpoint{old.kind, old.addr}, ln, old.su)
}

func shutdown(set *listenerSet) {
	if set == nil {
		return
	}
	for _, sv := range set.servers {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if sv.srv.Shutdown(ctx) != nil {
			sv.close()
		}
		cancel()
	}
}

// Request is a proposed change.
type Request struct {
	Settings Settings
	// OwnCertPEM and OwnKeyPEM, when given, are stored and used for mode own.
	OwnCertPEM, OwnKeyPEM []byte
}

// Check validates a proposal without applying it.
func (m *Manager) Check(req Request, env CheckEnv) []Check {
	s := req.Settings.Normalize()
	env.OwnCertPEM, env.OwnKeyPEM = req.OwnCertPEM, req.OwnKeyPEM
	env.InUse = m.InUse
	cs := Validate(s, env)
	if m.Locked != "" {
		cs = append([]Check{{Status: "error", Title: "Settings are fixed", Detail: m.Locked}}, cs...)
	}
	return cs
}

// Apply validates and applies a proposal. The new listeners start at once; the
// old ones stay until Confirm or until the window ends, when everything goes
// back. The checks are returned either way; with ErrInvalid nothing changed.
func (m *Manager) Apply(req Request, env CheckEnv) ([]Check, *Pending, error) {
	if m.Locked != "" {
		return nil, nil, ErrLocked
	}
	s := req.Settings.Normalize()
	cs := m.Check(Request{Settings: s, OwnCertPEM: req.OwnCertPEM, OwnKeyPEM: req.OwnKeyPEM}, env)
	if HasError(cs) {
		return cs, nil, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pend != nil {
		return cs, nil, errors.New("a change is waiting for confirmation; confirm or undo it first")
	}
	if len(req.OwnCertPEM) > 0 && s.Mode == ModeOwn {
		own, err := m.storeOwn(req.OwnCertPEM, req.OwnKeyPEM)
		if err != nil {
			return cs, nil, err
		}
		s.Own = own
	}
	set, taken, err := m.open(s, m.set)
	if err != nil {
		return cs, nil, err
	}
	prev := m.cur
	deadline := time.Now().Add(m.window())
	pr, _ := json.Marshal(pendingRecord{Prev: prev, Deadline: deadline})
	nb, _ := json.Marshal(s)
	if err := m.Store.SetWebSetting(keyPending, string(pr)); err == nil {
		err = m.Store.SetWebSetting(keySettings, string(nb))
		if err != nil {
			m.Store.DeleteWebSetting(keyPending)
		}
	}
	if err != nil {
		m.abort(set, taken)
		return cs, nil, fmt.Errorf("saving the settings: %v", err)
	}
	p := &pending{prev: prev, prevSet: m.set, next: set, taken: taken, deadline: deadline, gen: set.su.gen}
	p.timer = time.AfterFunc(m.window(), func() { m.expire(p.gen) })
	m.pend, m.cur, m.set = p, s, set
	m.Notice = ""
	m.change(s)
	m.logf("web: applied mode %s; going back to %s in %s unless confirmed", s.Mode, prev.Mode, m.window())
	return cs, m.pendingInfo(env.Host), nil
}

func (m *Manager) abort(set *listenerSet, taken []*served) {
	for _, sv := range set.servers {
		sv.close()
	}
	for _, sv := range taken {
		m.set.servers = append(m.set.servers, m.reopen(sv))
	}
}

// storeOwn writes an uploaded certificate into a fresh directory, so that a
// revert still finds the previous one.
func (m *Manager) storeOwn(certPEM, keyPEM []byte) (Own, error) {
	dir := filepath.Join(m.DataDir, "web-own", strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Own{}, err
	}
	o := Own{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"), Managed: true}
	if err := os.WriteFile(o.KeyFile, keyPEM, 0o600); err != nil {
		return Own{}, err
	}
	if err := os.WriteFile(o.CertFile, certPEM, 0o644); err != nil {
		return Own{}, err
	}
	return o, nil
}

// pendingInfo describes the open window. host is the host name the admin used.
func (m *Manager) pendingInfo(host string) *Pending {
	p := m.pend
	if p == nil {
		return nil
	}
	return &Pending{Gen: p.gen, Deadline: p.deadline, PrevMode: p.prev.Mode, URL: m.urlFor(m.cur, host)}
}

// urlFor is where a browser reaches settings s, given the host it used so far.
func (m *Manager) urlFor(s Settings, host string) string {
	if s.PublicURL != "" {
		return s.PublicURL + "/"
	}
	scheme, addr := "http", s.Listen
	if s.IsTLS() {
		scheme, addr = "https", s.HTTPSListen
	}
	h, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	if !IsWildcard(addr) && h != "" {
		host = h
	}
	if host == "" {
		host = "localhost"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	u := url.URL{Scheme: scheme, Host: host}
	if !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		u.Host = host + ":" + port
	}
	return u.String() + "/"
}

// Confirm keeps the pending change. gen must be the generation of the
// listener the confirmation arrived on, which proves the new address works.
func (m *Manager) Confirm(gen int) error {
	m.mu.Lock()
	p := m.pend
	if p == nil {
		m.mu.Unlock()
		return errors.New("nothing is waiting for confirmation")
	}
	if gen != p.gen {
		m.mu.Unlock()
		return errors.New("confirm from the new address, not the old one")
	}
	p.timer.Stop()
	m.pend = nil
	m.Store.DeleteWebSetting(keyPending)
	old, taken := p.prevSet, &listenerSet{servers: p.taken}
	m.mu.Unlock()
	shutdown(old)
	shutdown(taken)
	m.cleanOwn()
	m.logf("web: settings confirmed (mode %s)", m.Current().Mode)
	return nil
}

func (m *Manager) expire(gen int) {
	m.mu.Lock()
	p := m.pend
	m.mu.Unlock()
	if p != nil && p.gen == gen {
		m.logf("web: not confirmed in time; going back")
		m.Revert()
	}
}

// Revert undoes the pending change.
func (m *Manager) Revert() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pend
	if p == nil {
		return errors.New("nothing is waiting for confirmation")
	}
	p.timer.Stop()
	m.pend = nil
	for _, sv := range p.next.servers {
		sv.close()
	}
	set := p.prevSet
	for _, sv := range p.taken {
		set.servers = append(set.servers, m.reopen(sv))
	}
	b, _ := json.Marshal(p.prev)
	m.Store.SetWebSetting(keySettings, string(b))
	m.Store.DeleteWebSetting(keyPending)
	m.cur, m.set = p.prev, set
	m.change(p.prev)
	return nil
}

// cleanOwn removes uploaded certificates nothing refers to any more.
func (m *Manager) cleanOwn() {
	root := filepath.Join(m.DataDir, "web-own")
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	keep := filepath.Dir(m.Current().Own.CertFile)
	for _, e := range ents {
		if d := filepath.Join(root, e.Name()); d != keep {
			os.RemoveAll(d)
		}
	}
}

// Reload re-reads certificate files (SIGHUP).
func (m *Manager) Reload() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, set := range []*listenerSet{m.set} {
		if set != nil && set.su.fc != nil {
			if err := set.su.fc.Reload(); err != nil {
				m.logf("web: reloading the certificate failed, keeping the old one: %v", err)
			} else {
				m.logf("web: certificate reloaded")
			}
		}
	}
}

// Renew makes a new self-signed certificate and starts serving it.
func (m *Manager) Renew() (CertInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Locked != "" {
		return CertInfo{}, ErrLocked
	}
	if m.cur.Mode != ModeSelfSigned || m.set == nil || m.set.su.fc == nil {
		return CertInfo{}, errors.New("the controller is not using a self-signed certificate")
	}
	names := splitNames(m.cur.Names)
	if len(names.Hosts) == 0 && len(names.IPs) == 0 {
		names = tlscert.NamesFor(m.cur.HTTPSListen)
	}
	if _, _, err := tlscert.Generate(m.DataDir, names, time.Now()); err != nil {
		return CertInfo{}, err
	}
	if err := m.set.su.fc.Reload(); err != nil {
		return CertInfo{}, err
	}
	leaf, _ := m.set.su.fc.Leaf()
	m.logf("web: renewed the self-signed certificate")
	return Describe(leaf), nil
}

// RequestACME asks for the certificates now, in the background.
func (m *Manager) RequestACME() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur.Mode != ModeACME || m.set == nil || m.set.su.acme == nil {
		return errors.New("the controller is not in ACME mode")
	}
	go m.set.su.acme.Request()
	return nil
}

// CertFile is the certificate file in use, for the expiry alert; "" for
// modes without one.
func (m *Manager) CertFile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set != nil && m.set.su.fc != nil {
		return m.set.su.fc.CertFile
	}
	return ""
}

// Status describes the running state.
func (m *Manager) Status(host string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Normalized so that the lists are [] and never null in the JSON: the first-run
	// defaults have nil lists and the page calls join on them.
	st := Status{Settings: m.cur.Normalize(), Source: m.Source, Locked: m.Locked, Notice: m.Notice, Pending: m.pendingInfo(host), Window: int(m.window() / time.Second)}
	for _, set := range []*listenerSet{m.set, m.prevSet()} {
		if set == nil {
			continue
		}
		for _, sv := range set.servers {
			st.Listeners = append(st.Listeners, Listener{Kind: sv.kind, Addr: sv.ln.Addr().String(), Gen: set.su.gen})
		}
	}
	sort.SliceStable(st.Listeners, func(i, j int) bool { return st.Listeners[i].Gen > st.Listeners[j].Gen })
	if m.set != nil {
		if fc := m.set.su.fc; fc != nil {
			leaf, err := fc.Leaf()
			if leaf != nil {
				ci := Describe(leaf)
				st.Cert = &ci
			}
			if err != nil {
				st.CertError = err.Error()
			}
		}
		if a := m.set.su.acme; a != nil {
			as := a.status()
			st.ACME = &as
		}
	}
	return st
}

func (m *Manager) prevSet() *listenerSet {
	if m.pend != nil {
		return m.pend.prevSet
	}
	return nil
}
