package webaccess

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// ACMEStatus is what the page shows about certificate issuance.
type ACMEStatus struct {
	Domains     []string   `json:"domains"`
	Directory   string     `json:"directory"`
	LastIssued  *time.Time `json:"lastIssued,omitempty"`
	Expires     *time.Time `json:"expires,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`
	Requesting  bool       `json:"requesting"`
}

// acmeState wraps autocert and remembers how issuance is going: autocert
// itself exposes neither the last error nor the time of the last issuance.
type acmeState struct {
	mgr     *autocert.Manager
	domains []string
	dir     string

	mu         sync.Mutex
	issued     time.Time
	expires    time.Time
	lastErr    string
	lastErrAt  time.Time
	requesting bool
}

func newACME(s Settings, dataDir string) *acmeState {
	a := &acmeState{domains: s.ACME.Domains, dir: s.ACME.Directory}
	cache := &obsCache{Cache: autocert.DirCache(filepath.Join(dataDir, "acme")), st: a}
	a.mgr = &autocert.Manager{
		Prompt:     func(string) bool { return s.ACME.AcceptTerms },
		HostPolicy: autocert.HostWhitelist(s.ACME.Domains...),
		Cache:      cache,
		Email:      s.ACME.Email,
	}
	if s.ACME.Directory != "" && s.ACME.Directory != acme.LetsEncryptURL {
		a.mgr.Client = &acme.Client{DirectoryURL: s.ACME.Directory}
	}
	a.loadExisting()
	return a
}

// obsCache sees every certificate autocert stores.
type obsCache struct {
	autocert.Cache
	st *acmeState
}

func (c *obsCache) Put(ctx context.Context, key string, data []byte) error {
	if err := c.Cache.Put(ctx, key, data); err != nil {
		return err
	}
	c.st.stored(key, data, true)
	return nil
}

// stored notes a certificate in the cache. Keys of tokens, accounts and
// account keys contain "+"; certificates are stored under the domain.
func (a *acmeState) stored(key string, data []byte, fresh bool) {
	if strings.Contains(key, "+") || !a.has(key) {
		return
	}
	var exp time.Time
	for rest := data; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type == "CERTIFICATE" {
			if c, err := x509.ParseCertificate(b.Bytes); err == nil {
				exp = c.NotAfter
				break
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !exp.IsZero() && (a.expires.IsZero() || exp.Before(a.expires)) {
		a.expires = exp
	}
	if fresh {
		a.issued, a.lastErr = time.Now(), ""
	}
}

func (a *acmeState) has(name string) bool {
	for _, d := range a.domains {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

func (a *acmeState) loadExisting() {
	for _, d := range a.domains {
		if b, err := a.mgr.Cache.Get(context.Background(), d); err == nil {
			a.stored(d, b, false)
		}
	}
}

// getCertificate is autocert's, recording failures for the names we serve.
func (a *acmeState) getCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c, err := a.mgr.GetCertificate(h)
	if err != nil && a.has(h.ServerName) {
		a.fail(err)
	}
	return c, err
}

func (a *acmeState) fail(err error) {
	a.mu.Lock()
	a.lastErr, a.lastErrAt = err.Error(), time.Now()
	a.mu.Unlock()
}

// Request asks for the certificates now, as a first visitor would, and
// returns when every listed name has one or failed.
func (a *acmeState) Request() {
	a.mu.Lock()
	if a.requesting {
		a.mu.Unlock()
		return
	}
	a.requesting = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.requesting = false; a.mu.Unlock() }()
	for _, d := range a.domains {
		a.getCertificate(&tls.ClientHelloInfo{ServerName: d, SupportedProtos: []string{"h2", "http/1.1"}, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}, SupportedCurves: []tls.CurveID{tls.CurveP256}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
	}
}

func (a *acmeState) status() ACMEStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := ACMEStatus{Domains: a.domains, Directory: a.dir, LastError: a.lastErr, Requesting: a.requesting}
	if !a.issued.IsZero() {
		t := a.issued
		st.LastIssued = &t
	}
	if !a.expires.IsZero() {
		t := a.expires
		st.Expires = &t
	}
	if !a.lastErrAt.IsZero() && a.lastErr != "" {
		t := a.lastErrAt
		st.LastErrorAt = &t
	}
	return st
}

// tlsConfig is the server side: TLS-ALPN-01 challenges are answered on the
// same port.
func (a *acmeState) tlsConfig() *tls.Config {
	c := a.mgr.TLSConfig()
	c.GetCertificate = a.getCertificate
	c.MinVersion = tls.VersionTLS12
	return c
}

// httpHandler answers HTTP-01 challenges and passes everything else on.
func (a *acmeState) httpHandler(fallback http.Handler) http.Handler {
	return a.mgr.HTTPHandler(fallback)
}
