package webaccess

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// testCert makes a certificate and key as PEM.
func testCert(t *testing.T, names []string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func statuses(cs []Check, title string) []string {
	var out []string
	for _, c := range cs {
		if c.Title == title {
			out = append(out, c.Status)
		}
	}
	return out
}

func has(cs []Check, title, status string) bool {
	for _, s := range statuses(cs, title) {
		if s == status {
			return true
		}
	}
	return false
}

var noBind = CheckEnv{Bind: func(string) error { return nil }}

func TestNormalize(t *testing.T) {
	s := Settings{Mode: " Proxy ", Names: []string{"a.example, B.example", "a.EXAMPLE"}, PublicURL: "https://x.example/"}.Normalize()
	if s.Mode != "proxy" || s.Listen != DefaultListen || s.HTTPSListen != ":8443" || s.PublicURL != "https://x.example" {
		t.Errorf("%+v", s)
	}
	if len(s.Names) != 2 {
		t.Errorf("names %v", s.Names)
	}
	if s.ACME.Directory != LetsEncrypt {
		t.Errorf("directory %q", s.ACME.Directory)
	}
}

func TestValidateOff(t *testing.T) {
	cs := Validate(Settings{Mode: "off", Listen: "127.0.0.1:8080"}.Normalize(), noBind)
	if HasError(cs) || has(cs, "Sign-ins travel unencrypted", "warning") {
		t.Errorf("loopback http: %+v", cs)
	}
	cs = Validate(Settings{Mode: "off", Listen: "0.0.0.0:8080"}.Normalize(), noBind)
	if !has(cs, "Sign-ins travel unencrypted", "warning") {
		t.Errorf("no warning for a public plain http listener: %+v", cs)
	}
	cs = Validate(Settings{Mode: "off", Listen: "nonsense"}.Normalize(), noBind)
	if !HasError(cs) {
		t.Error("bad address accepted")
	}
	if !HasError(Validate(Settings{Mode: "bogus"}, noBind)) {
		t.Error("bad mode accepted")
	}
}

func TestValidateBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := Settings{Mode: "off", Listen: ln.Addr().String()}.Normalize()
	if cs := Validate(s, CheckEnv{}); !has(cs, "HTTP address", "error") {
		t.Errorf("a busy port passed: %+v", cs)
	}
	if cs := Validate(s, CheckEnv{InUse: func(string) bool { return true }}); HasError(cs) {
		t.Errorf("our own port must pass: %+v", cs)
	}
}

func TestValidateSelfSigned(t *testing.T) {
	s := Settings{Mode: "selfsigned", HTTPSListen: "127.0.0.1:8443", PublicURL: "https://vm.example", Names: []string{"vm.example", "10.0.0.5"}}.Normalize()
	cs := Validate(s, noBind)
	if HasError(cs) {
		t.Errorf("%+v", cs)
	}
	s.Names = nil
	if !has(Validate(s, noBind), "Names", "error") {
		t.Error("no names accepted")
	}
	s.Names = []string{"bad name!"}
	if !has(Validate(s, noBind), "Names", "error") {
		t.Error("bad name accepted")
	}
	s.Names = []string{"other.example"}
	if !has(Validate(s, noBind), "Names", "warning") {
		t.Error("public URL not in names should warn")
	}
	s.PublicURL = "http://vm.example"
	if !has(Validate(s, noBind), "Public URL", "error") {
		t.Error("http public URL accepted in a TLS mode")
	}
}

func TestValidateOwn(t *testing.T) {
	now := time.Now()
	good, key := testCert(t, []string{"vm.example", "10.0.0.5"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	s := Settings{Mode: "own", HTTPSListen: "127.0.0.1:8443", PublicURL: "https://vm.example", Names: []string{"vm.example"}}.Normalize()
	env := noBind
	env.OwnCertPEM, env.OwnKeyPEM = good, key
	if cs := Validate(s, env); HasError(cs) {
		t.Errorf("good cert: %+v", cs)
	}
	// Key of another certificate.
	_, otherKey := testCert(t, []string{"vm.example"}, now.Add(-time.Hour), now.Add(time.Hour))
	env.OwnKeyPEM = otherKey
	if cs := Validate(s, env); !has(cs, "Certificate", "error") {
		t.Errorf("mismatched key accepted: %+v", cs)
	}
	// Expired.
	old, oldKey := testCert(t, []string{"vm.example"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	env.OwnCertPEM, env.OwnKeyPEM = old, oldKey
	if cs := Validate(s, env); !has(cs, "Expiry", "error") {
		t.Errorf("expired cert accepted: %+v", cs)
	}
	// Names not covered.
	env.OwnCertPEM, env.OwnKeyPEM = good, key
	s.Names = []string{"vm.example", "missing.example"}
	if cs := Validate(s, env); !has(cs, "Names", "error") {
		t.Errorf("uncovered name accepted: %+v", cs)
	}
	s.Names = nil
	s.PublicURL = "https://other.example"
	if cs := Validate(s, env); !has(cs, "Public URL", "error") {
		t.Errorf("public URL outside the certificate accepted: %+v", cs)
	}
	// Nothing given.
	if cs := Validate(Settings{Mode: "own", HTTPSListen: "127.0.0.1:8443"}.Normalize(), noBind); !has(cs, "Certificate", "error") {
		t.Error("no certificate accepted")
	}
	// From files.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "c.pem"), good, 0o644)
	os.WriteFile(filepath.Join(dir, "k.pem"), key, 0o600)
	s = Settings{Mode: "own", HTTPSListen: "127.0.0.1:8443", Own: Own{CertFile: filepath.Join(dir, "c.pem"), KeyFile: filepath.Join(dir, "k.pem")}}.Normalize()
	if cs := Validate(s, noBind); HasError(cs) {
		t.Errorf("files: %+v", cs)
	}
}

func TestValidateACME(t *testing.T) {
	s := Settings{Mode: "acme", HTTPSListen: "127.0.0.1:8443", ACME: ACME{Domains: []string{"vm.example"}, AcceptTerms: true}}.Normalize()
	cs := Validate(s, noBind)
	if HasError(cs) {
		t.Errorf("%+v", cs)
	}
	if !has(cs, "Port 443", "warning") {
		t.Error("port 443 hint missing")
	}
	s.ACME.AcceptTerms = false
	if !has(Validate(s, noBind), "Terms", "error") {
		t.Error("terms not required")
	}
	s.ACME.AcceptTerms = true
	for _, d := range []string{"*.example.org", "10.0.0.1", "bad name"} {
		s.ACME.Domains = []string{d}
		if !has(Validate(s, noBind), "Domains", "error") {
			t.Errorf("domain %q accepted", d)
		}
	}
	s.ACME.Domains = nil
	if !has(Validate(s, noBind), "Domains", "error") {
		t.Error("no domains accepted")
	}
	s.ACME.Domains = []string{"vm.example"}
	s.ACME.Directory = "http://insecure"
	if !has(Validate(s, noBind), "Directory URL", "error") {
		t.Error("http directory accepted")
	}
	s.ACME.Directory = LetsEncryptStaging
	if !has(Validate(s, noBind), "Directory URL", "info") {
		t.Error("staging not mentioned")
	}
	s.PublicURL = "https://elsewhere.example"
	if !has(Validate(s, noBind), "Public URL", "error") {
		t.Error("public URL outside the domains accepted")
	}
}

func TestValidateProxy(t *testing.T) {
	s := Settings{Mode: "proxy", Listen: "127.0.0.1:8080", PublicURL: "https://vm.example", TrustedProxies: []string{"127.0.0.1"}}.Normalize()
	env := noBind
	env.Peer = netip.MustParseAddr("127.0.0.1")
	env.ForwardedProto = "https"
	cs := Validate(s, env)
	if HasError(cs) || !has(cs, "Proxy headers", "ok") {
		t.Errorf("%+v", cs)
	}
	env.ForwardedProto = ""
	if !has(Validate(s, env), "Proxy headers", "warning") {
		t.Error("missing X-Forwarded-Proto not flagged")
	}
	env.Peer = netip.MustParseAddr("203.0.113.9")
	if !has(Validate(s, env), "Proxy headers", "info") {
		t.Error("direct request should say not tested")
	}
	s.TrustedProxies = nil
	if !has(Validate(s, noBind), "Trusted proxies", "error") {
		t.Error("empty trusted list accepted")
	}
	s.TrustedProxies = []string{"nope"}
	if !has(Validate(s, noBind), "Trusted proxies", "error") {
		t.Error("bad trusted entry accepted")
	}
	s.TrustedProxies = []string{"0.0.0.0/0"}
	if !has(Validate(s, noBind), "Trusted proxies", "warning") {
		t.Error("/0 not flagged")
	}
	s.TrustedProxies = []string{"10.0.0.0/8"}
	s.PublicURL = ""
	if !has(Validate(s, noBind), "Public URL", "error") {
		t.Error("proxy mode without public URL accepted")
	}
	s.PublicURL = "https://vm.example"
	s.Listen = "0.0.0.0:8080"
	if !has(Validate(s, noBind), "HTTP address", "warning") {
		t.Error("non-loopback backend not flagged")
	}
}

func TestSnippets(t *testing.T) {
	s := Settings{Mode: "proxy", Listen: "127.0.0.1:8080", PublicURL: "https://vm.example.org", TrustedProxies: []string{"127.0.0.1"}}.Normalize()
	got := Snippets(s)
	if len(got) != 5 {
		t.Fatalf("%d snippets", len(got))
	}
	for _, sn := range got {
		if !strings.Contains(sn.Text, "vm.example.org") || !strings.Contains(sn.Text, "127.0.0.1:8080") {
			t.Errorf("%s lacks the host or backend", sn.ID)
		}
		if strings.Contains(sn.Text, "@") {
			t.Errorf("%s has an unreplaced placeholder", sn.ID)
		}
		if !strings.Contains(strings.ToLower(sn.Text), "x-forwarded-proto") && sn.ID != "caddy" && sn.ID != "traefik" {
			t.Errorf("%s does not set X-Forwarded-Proto", sn.ID)
		}
		golden := filepath.Join("testdata", sn.ID+".golden")
		if *update {
			os.MkdirAll("testdata", 0o755)
			os.WriteFile(golden, []byte(sn.Text), 0o644)
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != sn.Text {
			t.Errorf("%s differs from %s (run go test -update)", sn.ID, golden)
		}
	}
	// A wildcard backend address is reached through loopback.
	s.Listen = "0.0.0.0:9000"
	if txt := Snippets(s)[0].Text; !strings.Contains(txt, "127.0.0.1:9000") {
		t.Errorf("wildcard backend: %s", txt)
	}
	// Without a public URL the sample name shows.
	if txt := Snippets(Settings{}.Normalize())[0].Text; !strings.Contains(txt, SampleHost) {
		t.Error("sample host missing")
	}
}

func TestRedirectHandler(t *testing.T) {
	h := RedirectHandler(Settings{HTTPSListen: ":8443"})
	r := httptest.NewRequest("GET", "http://vm.example:8080/vms?x=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "https://vm.example:8443/vms?x=1" {
		t.Errorf("%d %s", w.Code, w.Header().Get("Location"))
	}
	h = RedirectHandler(Settings{HTTPSListen: ":443"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "http://vm.example/api/login", nil))
	if w.Header().Get("Location") != "https://vm.example/api/login" || w.Code != 307 {
		t.Errorf("443: %d %s", w.Code, w.Header().Get("Location"))
	}
	h = RedirectHandler(Settings{HTTPSListen: ":8443", PublicURL: "https://public.example"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://10.1.1.1/", nil))
	if w.Header().Get("Location") != "https://public.example/" {
		t.Errorf("public: %s", w.Header().Get("Location"))
	}
	h = RedirectHandler(Settings{HTTPSListen: ":8443"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://[::1]:80/", nil))
	if w.Header().Get("Location") != "https://[::1]:8443/" {
		t.Errorf("ipv6: %s", w.Header().Get("Location"))
	}
}

func TestFileCertReload(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	c1, k1 := testCert(t, []string{"one.example"}, now.Add(-time.Hour), now.Add(time.Hour))
	cf, kf := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cf, c1, 0o644)
	os.WriteFile(kf, k1, 0o600)
	fc, err := NewFileCert(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := fc.Leaf(); l.VerifyHostname("one.example") != nil {
		t.Fatal("wrong first cert")
	}
	c2, k2 := testCert(t, []string{"two.example"}, now.Add(-time.Hour), now.Add(time.Hour))
	os.WriteFile(cf, c2, 0o644)
	os.WriteFile(kf, k2, 0o600)
	if err := fc.Reload(); err != nil { // SIGHUP
		t.Fatal(err)
	}
	if l, _ := fc.Leaf(); l.VerifyHostname("two.example") != nil {
		t.Fatal("not reloaded")
	}
	// A broken file keeps the old certificate.
	os.WriteFile(cf, []byte("junk"), 0o644)
	if err := fc.Reload(); err == nil {
		t.Fatal("junk accepted")
	}
	if c, _ := fc.Get(nil); c == nil {
		t.Fatal("lost the certificate")
	}
	if l, err := fc.Leaf(); l == nil || err == nil {
		t.Errorf("leaf %v err %v", l, err)
	}
}

func TestACMEStatusFromCache(t *testing.T) {
	dir := t.TempDir()
	s := Settings{Mode: "acme", ACME: ACME{Domains: []string{"vm.example"}, AcceptTerms: true, Directory: LetsEncryptStaging}}.Normalize()
	a := newACME(s, dir)
	if st := a.status(); st.LastIssued != nil || st.Expires != nil {
		t.Fatalf("fresh: %+v", st)
	}
	exp := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	cert, key := testCert(t, []string{"vm.example"}, time.Now().Add(-time.Hour), exp)
	// autocert stores the key, then the chain, in one entry under the domain.
	if err := a.mgr.Cache.Put(context.Background(), "vm.example", append(key, cert...)); err != nil {
		t.Fatal(err)
	}
	if err := a.mgr.Cache.Put(context.Background(), "other.example", append(key, cert...)); err != nil {
		t.Fatal(err)
	}
	st := a.status()
	if st.LastIssued == nil || st.Expires == nil || !st.Expires.Equal(exp) {
		t.Errorf("after issuance: %+v", st)
	}
	// A fresh start reads the expiry back from the cache.
	b := newACME(s, dir)
	if st := b.status(); st.Expires == nil || st.LastIssued != nil {
		t.Errorf("reload: %+v", st)
	}
	// A request for a name that is not listed fails and is not recorded.
	if _, err := a.getCertificate(&tls.ClientHelloInfo{ServerName: "scanner.example"}); err == nil {
		t.Error("an unlisted name got a certificate")
	}
	if a.status().LastError != "" {
		t.Error("noise recorded as error")
	}
	// The listed name has a cached certificate, so no CA is contacted.
	if c, err := a.getCertificate(&tls.ClientHelloInfo{ServerName: "vm.example", SupportedProtos: []string{"h2"}, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}, SupportedCurves: []tls.CurveID{tls.CurveP256}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}}); err != nil || c == nil {
		t.Errorf("cached certificate not served: %v", err)
	}
	// HTTP-01 handler: challenge paths are for autocert, the rest redirects.
	var hit bool
	h := a.httpHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://vm.example/other", nil))
	if !hit {
		t.Error("fallback not used")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://vm.example/.well-known/acme-challenge/nope", nil))
	if w.Code != 403 && w.Code != 404 {
		t.Errorf("unknown token: %d", w.Code)
	}
}

// --- manager ---

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memStore) WebSetting(k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}
func (s *memStore) SetWebSetting(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[k] = v
	return nil
}
func (s *memStore) DeleteWebSetting(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, k)
	return nil
}

func newManager(t *testing.T) (*Manager, *memStore) {
	t.Helper()
	st := &memStore{}
	m := &Manager{Store: st, DataDir: t.TempDir(), Window: 400 * time.Millisecond,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "gen="+string(rune('0'+GenOf(r))))
		})}
	return m, st
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func addrOf(t *testing.T, m *Manager, kind string) string {
	t.Helper()
	for _, l := range m.Status("").Listeners {
		if l.Kind == kind && l.Gen == m.gen {
			return l.Addr
		}
	}
	t.Fatalf("no %s listener in %+v", kind, m.Status("").Listeners)
	return ""
}

var insecureClient = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func TestManagerSwitchConfirmAndRevert(t *testing.T) {
	m, st := newManager(t)
	if err := m.Start(Settings{Mode: "off", Listen: "127.0.0.1:0"}.Normalize()); err != nil {
		t.Fatal(err)
	}
	oldAddr := addrOf(t, m, "http")
	if c, body := get(t, insecureClient, "http://"+oldAddr+"/"); c != 200 || body != "gen=1" {
		t.Fatalf("old: %d %s", c, body)
	}

	// Switch to self-signed. Both listeners answer during the window.
	req := Request{Settings: Settings{Mode: "selfsigned", HTTPSListen: "127.0.0.1:0", HTTPRedirect: freeAddr(t), Names: []string{"localhost", "127.0.0.1"}}}
	_, p, err := m.Apply(req, noBind)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.PrevMode != "off" || !strings.HasPrefix(p.URL, "https://127.0.0.1:") {
		t.Fatalf("pending: %+v", p)
	}
	httpsAddr := addrOf(t, m, "https")
	if c, body := get(t, insecureClient, "https://"+httpsAddr+"/"); c != 200 || body != "gen=2" {
		t.Fatalf("new: %d %s", c, body)
	}
	if c, _ := get(t, insecureClient, "http://"+oldAddr+"/"); c != 200 {
		t.Fatalf("old listener must stay during the window: %d", c)
	}
	if c, _ := get(t, insecureClient, "http://"+addrOf(t, m, "redirect")+"/x"); c != 307 {
		t.Fatalf("redirect listener: %d", c)
	}
	if _, _, err := m.Apply(req, noBind); err == nil {
		t.Error("a second change was accepted while one is pending")
	}
	if err := m.Confirm(1); err == nil {
		t.Error("confirmation from the old listener accepted")
	}
	if raw, _, _ := st.WebSetting("web.pending"); raw == "" {
		t.Error("pending record not stored")
	}

	// Not confirmed: back to the old settings.
	deadline := time.Now().Add(5 * time.Second)
	for m.Status("").Pending != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Current().Mode != "off" {
		t.Fatalf("mode after timeout: %s", m.Current().Mode)
	}
	if c, _ := get(t, insecureClient, "http://"+oldAddr+"/"); c != 200 {
		t.Errorf("old listener gone after revert: %d", c)
	}
	if c, _ := get(t, insecureClient, "https://"+httpsAddr+"/"); c != 0 {
		t.Errorf("new listener still open after revert: %d", c)
	}
	if raw, _, _ := st.WebSetting("web"); !strings.Contains(raw, `"mode":"off"`) {
		t.Errorf("stored settings not restored: %s", raw)
	}
	if _, ok, _ := st.WebSetting("web.pending"); ok {
		t.Error("pending record left behind")
	}

	// Confirmed from the new listener: the old one goes.
	m.Window = time.Minute
	if _, _, err = m.Apply(req, noBind); err != nil {
		t.Fatal(err)
	}
	if err := m.Confirm(m.gen); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if c, _ := get(t, insecureClient, "http://"+oldAddr+"/"); c != 0 {
		t.Errorf("old listener still open after confirm: %d", c)
	}
	if c, _ := get(t, insecureClient, "https://"+addrOf(t, m, "https")+"/"); c != 200 {
		t.Errorf("new listener: %d", c)
	}
	if raw, _, _ := st.WebSetting("web"); !strings.Contains(raw, `"mode":"selfsigned"`) {
		t.Errorf("not stored: %s", raw)
	}
}

func TestManagerRejectsInvalidAndLocked(t *testing.T) {
	m, _ := newManager(t)
	if err := m.Start(Settings{Mode: "off", Listen: "127.0.0.1:0"}.Normalize()); err != nil {
		t.Fatal(err)
	}
	cs, _, err := m.Apply(Request{Settings: Settings{Mode: "acme", HTTPSListen: "127.0.0.1:0"}}, noBind)
	if err != ErrInvalid || !HasError(cs) {
		t.Fatalf("invalid acme settings applied: %v %+v", err, cs)
	}
	if m.Current().Mode != "off" {
		t.Error("settings changed by a refused proposal")
	}
	m.Locked = "flags"
	if _, _, err := m.Apply(Request{Settings: Settings{Mode: "off", Listen: "127.0.0.1:0"}}, noBind); err != ErrLocked {
		t.Errorf("locked: %v", err)
	}
}

func TestManagerSameAddressTakeover(t *testing.T) {
	m, _ := newManager(t)
	m.Window = 300 * time.Millisecond
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if err := m.Start(Settings{Mode: "off", Listen: addr}.Normalize()); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.Apply(Request{Settings: Settings{Mode: "proxy", Listen: addr, PublicURL: "https://vm.example", TrustedProxies: []string{"127.0.0.1"}}}, noBind)
	if err != nil {
		t.Fatal(err)
	}
	if c, body := get(t, insecureClient, "http://"+addr+"/"); c != 200 || body != "gen=2" {
		t.Fatalf("takeover: %d %s", c, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Status("").Pending != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if c, body := get(t, insecureClient, "http://"+addr+"/"); c != 200 || body != "gen=1" {
		t.Fatalf("revert did not restore the listener: %d %s", c, body)
	}
}

func TestManagerLoadUndoesUnconfirmed(t *testing.T) {
	m, st := newManager(t)
	st.SetWebSetting("web", `{"mode":"selfsigned"}`)
	st.SetWebSetting("web.pending", `{"prev":{"mode":"off","listen":"127.0.0.1:8080"},"deadline":"2026-01-01T00:00:00Z"}`)
	s, ok, err := m.Load()
	if err != nil || !ok || s.Mode != "off" {
		t.Fatalf("%+v %v %v", s, ok, err)
	}
	if _, has, _ := st.WebSetting("web.pending"); has {
		t.Error("pending kept")
	}
}

func TestManagerOwnUploadAndRenew(t *testing.T) {
	m, _ := newManager(t)
	if err := m.Start(Settings{Mode: "off", Listen: "127.0.0.1:0"}.Normalize()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c, k := testCert(t, []string{"localhost", "127.0.0.1"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	if _, _, err := m.Apply(Request{Settings: Settings{Mode: "own", HTTPSListen: "127.0.0.1:0", Names: []string{"localhost"}}, OwnCertPEM: c, OwnKeyPEM: k}, noBind); err != nil {
		t.Fatal(err)
	}
	st := m.Status("")
	if st.Cert == nil || st.Cert.Fingerprint == "" || !st.Settings.Own.Managed {
		t.Fatalf("%+v", st)
	}
	fi, err := os.Stat(st.Settings.Own.KeyFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", fi, err)
	}
	if _, err := m.Renew(); err == nil {
		t.Error("renew accepted in mode own")
	}
	m.Confirm(m.gen)

	// Self-signed: renew changes the fingerprint.
	if _, _, err := m.Apply(Request{Settings: Settings{Mode: "selfsigned", HTTPSListen: "127.0.0.1:0", Names: []string{"localhost"}}}, noBind); err != nil {
		t.Fatal(err)
	}
	before := m.Status("").Cert.Fingerprint
	ci, err := m.Renew()
	if err != nil || ci.Fingerprint == before {
		t.Fatalf("renew: %v %s vs %s", err, ci.Fingerprint, before)
	}
	if got := m.Status("").Cert; got.Fingerprint != ci.Fingerprint || got.NotAfter.Sub(got.NotBefore) < 824*24*time.Hour {
		t.Errorf("served cert: %+v", got)
	}
	if fi, _ := os.Stat(filepath.Join(m.DataDir, "tls-key.pem")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("self-signed key mode: %v", fi)
	}
	// P-256 key.
	if _, _, err := LoadPair(filepath.Join(m.DataDir, "tls-cert.pem"), filepath.Join(m.DataDir, "tls-key.pem")); err != nil {
		t.Error(err)
	}
}

// An own certificate needs no Names field: the check reads the names from the
// certificate itself.
func TestOwnNamesComeFromCertificate(t *testing.T) {
	now := time.Now()
	pemCert, key := testCert(t, []string{"vm.example", "10.0.0.5"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	_, leaf, err := ParseKeyPair(pemCert, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(CertNames(leaf), ","); got != "vm.example,10.0.0.5" {
		t.Errorf("CertNames = %q", got)
	}
	env := noBind
	env.OwnCertPEM, env.OwnKeyPEM = pemCert, key
	cs := Validate(Settings{Mode: "own", HTTPSListen: "127.0.0.1:8443"}.Normalize(), env)
	if HasError(cs) {
		t.Fatalf("%+v", cs)
	}
	found := false
	for _, c := range cs {
		if c.Title == "Names" && c.Status == "ok" && strings.Contains(c.Detail, "vm.example") && strings.Contains(c.Detail, "10.0.0.5") {
			found = true
		}
	}
	if !found {
		t.Errorf("the certificate's names are not reported: %+v", cs)
	}
}

func TestFirstRunDefaults(t *testing.T) {
	d := FirstRunDefaults()
	if d.Mode != ModeSelfSigned || d.HTTPSListen != ":8443" || len(d.Names) != 0 {
		t.Errorf("%+v", d)
	}
	if o := Defaults(); o.Mode != ModeOff || o.Listen != DefaultListen {
		t.Errorf("the reset default must stay loopback HTTP: %+v", o)
	}
}

// The first-run defaults have nil lists. The page calls join on them, so the
// status must send [] and never null (a white page otherwise).
func TestStatusListsAreNeverNull(t *testing.T) {
	m, _ := newManager(t)
	m.cur = FirstRunDefaults()
	b, err := json.Marshal(m.Status("").Settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("a list is null: %s", b)
	}
}

func TestListenComposeAndSplit(t *testing.T) {
	ifaces := []Iface{{Name: "eth0", Addrs: []string{"10.0.0.5", "fd00::5"}}}
	cases := []struct {
		host string
		port int
		want string
		cust bool
	}{
		{"", 8443, ":8443", false},
		{"127.0.0.1", 443, "127.0.0.1:443", false},
		{"10.0.0.5", 8443, "10.0.0.5:8443", false},
		{"10.9.9.9", 8443, "10.9.9.9:8443", true}, // an address that is gone stays visible
	}
	for _, c := range cases {
		got := ComposeListen(c.host, c.port)
		if got != c.want {
			t.Errorf("ComposeListen(%q,%d) = %q, want %q", c.host, c.port, got, c.want)
		}
		h, p, cust := SplitListen(got, ifaces)
		if h != c.host || p != c.port || cust != c.cust {
			t.Errorf("SplitListen(%q) = %q,%d,%v", got, h, p, cust)
		}
	}
	if h, p, c := SplitListen("0.0.0.0:8443", ifaces); h != "" || p != 8443 || c {
		t.Errorf("0.0.0.0 must read as all interfaces: %q %d %v", h, p, c)
	}
	if ComposeListen("fd00::5", 443) != "[fd00::5]:443" {
		t.Error("IPv6 must be bracketed")
	}
}

func TestInterfacesSkipLoopbackAndLinkLocal(t *testing.T) {
	for _, f := range Interfaces() {
		for _, a := range f.Addrs {
			ip := net.ParseIP(a)
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				t.Errorf("%s lists %s", f.Name, a)
			}
		}
		if len(f.Addrs) == 0 {
			t.Errorf("%s has no address", f.Name)
		}
	}
}

// A stored interface address that is gone must not stop the controller.
func TestStartFallsBackWhenAddressIsGone(t *testing.T) {
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1")
	m, _ := newManager(t)
	s := Settings{Mode: "selfsigned", HTTPSListen: "192.0.2.1" + port, Names: []string{"localhost"}}.Normalize()
	used, warn, err := m.StartOrFallBack(s)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if used.HTTPSListen != port || warn == "" || !strings.Contains(warn, "192.0.2.1") {
		t.Errorf("used %q, warning %q", used.HTTPSListen, warn)
	}
	// Any other failure is still a failure.
	m2, _ := newManager(t)
	busy := Settings{Mode: "off", Listen: m.Status("").Listeners[0].Addr}.Normalize()
	if _, _, err := m2.StartOrFallBack(busy); err == nil {
		t.Error("a busy port must still fail")
	}
}

// Peek (used by virticle setup beside the running controller) must not undo a
// change that waits for confirmation, which Load does.
func TestPeekLeavesAPendingChangeAlone(t *testing.T) {
	m, st := newManager(t)
	prev := FirstRunDefaults()
	next := FirstRunDefaults()
	next.HTTPSListen = ":9443"
	pb, _ := json.Marshal(pendingRecord{Prev: prev, Deadline: time.Now().Add(time.Minute)})
	nb, _ := json.Marshal(next)
	st.SetWebSetting(keyPending, string(pb))
	st.SetWebSetting(keySettings, string(nb))
	got, p, ok, err := m.Peek()
	if err != nil || !ok || got.HTTPSListen != ":9443" || p == nil || p.HTTPSListen != ":8443" {
		t.Fatalf("Peek = %+v %+v %v %v", got, p, ok, err)
	}
	if _, pending := st.m[keyPending]; !pending {
		t.Error("Peek removed the pending record")
	}
	if raw := st.m[keySettings]; !strings.Contains(raw, "9443") {
		t.Errorf("Peek changed the stored settings: %s", raw)
	}
}

func TestSecondsUntilRoundsUpAndStopsAtZero(t *testing.T) {
	if n := secondsUntil(time.Now().Add(-time.Minute)); n != 0 {
		t.Errorf("past deadline: %d", n)
	}
	if n := secondsUntil(time.Now().Add(59500 * time.Millisecond)); n != 60 {
		t.Errorf("59.5 seconds left: %d", n)
	}
}
