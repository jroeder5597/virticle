package webaccess

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// CertInfo is what the page shows about a certificate.
type CertInfo struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Names       []string  `json:"names"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"` // SHA-256, colon separated
	SelfSigned  bool      `json:"selfSigned"`
}

// Describe summarises a leaf certificate.
func Describe(c *x509.Certificate) CertInfo {
	ci := CertInfo{
		Subject: c.Subject.CommonName, Issuer: c.Issuer.CommonName,
		NotBefore: c.NotBefore, NotAfter: c.NotAfter,
		Fingerprint: fingerprint(c.Raw),
		SelfSigned:  c.Subject.String() == c.Issuer.String(),
		Names:       append([]string{}, c.DNSNames...),
	}
	for _, ip := range c.IPAddresses {
		ci.Names = append(ci.Names, ip.String())
	}
	if ci.Subject == "" && len(c.Subject.Organization) > 0 {
		ci.Subject = c.Subject.Organization[0]
	}
	return ci
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, 32)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// ParseKeyPair checks that certPEM (leaf first, then the chain) and keyPEM
// belong together and returns the usable pair and its leaf.
func ParseKeyPair(certPEM, keyPEM []byte) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate and key: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate: %v", err)
	}
	pair.Leaf = leaf
	return pair, leaf, nil
}

// LoadPair reads a certificate chain and key from files.
func LoadPair(certFile, keyFile string) (tls.Certificate, *x509.Certificate, error) {
	c, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	k, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return ParseKeyPair(c, k)
}

// FileCert serves a certificate from files and picks up a changed file: it
// looks at the modification times at most every few seconds per handshake, and
// Reload (SIGHUP) forces it. A file that no longer parses keeps the old
// certificate in use and the error is kept for the status page.
type FileCert struct {
	CertFile, KeyFile string
	Logf              func(string, ...any)

	mu      sync.Mutex
	cur     *tls.Certificate
	leaf    *x509.Certificate
	stamp   [2]time.Time
	checked time.Time
	lastErr error
}

// NewFileCert loads the pair once; it fails if that does not work.
func NewFileCert(certFile, keyFile string) (*FileCert, error) {
	f := &FileCert{CertFile: certFile, KeyFile: keyFile}
	if err := f.Reload(); err != nil {
		return nil, err
	}
	return f, nil
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// Reload re-reads both files now.
func (f *FileCert) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reload()
}

func (f *FileCert) reload() error {
	pair, leaf, err := LoadPair(f.CertFile, f.KeyFile)
	f.checked = time.Now()
	if err != nil {
		f.lastErr = err
		return err
	}
	f.cur, f.leaf, f.lastErr = &pair, leaf, nil
	f.stamp = [2]time.Time{mtime(f.CertFile), mtime(f.KeyFile)}
	return nil
}

// Get is a tls.Config.GetCertificate.
func (f *FileCert) Get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.checked) > 5*time.Second {
		f.checked = time.Now()
		if c, k := mtime(f.CertFile), mtime(f.KeyFile); !c.Equal(f.stamp[0]) || !k.Equal(f.stamp[1]) {
			if err := f.reload(); err != nil {
				if f.Logf != nil {
					f.Logf("web: certificate file changed but cannot be used, keeping the old one: %v", err)
				}
				f.stamp = [2]time.Time{c, k} // do not retry until it changes again
			} else if f.Logf != nil {
				f.Logf("web: reloaded certificate %s", f.CertFile)
			}
		}
	}
	return f.cur, nil
}

// Leaf returns the certificate in use and the last reload error.
func (f *FileCert) Leaf() (*x509.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaf, f.lastErr
}
