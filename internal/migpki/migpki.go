// Package migpki is the small certificate authority behind encrypted
// migration streams. The controller keeps one CA; every node gets one
// certificate (usable as TLS server and client certificate) signed by it, which
// libvirt hands to QEMU for the memory stream and the NBD disk mirror.
//
// ECDSA P-256 with SHA-256 is accepted by GnuTLS (Debian, RHEL family) and by
// QEMU's TLS credentials; for ECDSA keys QEMU requires only the
// digitalSignature key usage.
package migpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CAValidity   = 10 * 365 * 24 * time.Hour
	NodeValidity = 2 * 365 * 24 * time.Hour
	// RenewLeft is how long before expiry a node certificate counts as expiring.
	RenewLeft = 30 * 24 * time.Hour

	caCertFile = "ca-cert.pem"
	caKeyFile  = "ca-key.pem"
)

// CA is the controller's migration certificate authority.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     *ecdsa.PrivateKey
}

// ErrNoCA is returned by LoadCA when the controller has not created a CA yet.
var ErrNoCA = errors.New("the controller has no migration CA yet")

// LoadCA reads the CA from dir without creating it.
func LoadCA(dir string) (*CA, error) {
	certPEM, cerr := os.ReadFile(filepath.Join(dir, caCertFile))
	keyPEM, kerr := os.ReadFile(filepath.Join(dir, caKeyFile))
	if errors.Is(cerr, os.ErrNotExist) && errors.Is(kerr, os.ErrNotExist) {
		return nil, ErrNoCA
	}
	if cerr != nil {
		return nil, cerr
	}
	if kerr != nil {
		return nil, kerr
	}
	return parseCA(certPEM, keyPEM)
}

// LoadOrCreateCA reads the CA from dir or creates it (key mode 0600).
func LoadOrCreateCA(dir string, now time.Time) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	certPEM, cerr := os.ReadFile(certPath)
	keyPEM, kerr := os.ReadFile(keyPath)
	if cerr == nil && kerr == nil {
		return parseCA(certPEM, keyPEM)
	}
	if cerr != nil && !errors.Is(cerr, os.ErrNotExist) {
		return nil, cerr
	}
	if kerr != nil && !errors.Is(kerr, os.ErrNotExist) {
		return nil, kerr
	}
	if cerr == nil || kerr == nil {
		return nil, fmt.Errorf("%s has only one of %s and %s; refusing to create a new CA over it", dir, caCertFile, caKeyFile)
	}
	ca, keyOut, err := newCA(now)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Key first: a CA certificate without its key is useless, never the reverse.
	if err := writeAtomic(keyPath, keyOut, 0o600); err != nil {
		return nil, err
	}
	if err := writeAtomic(certPath, ca.CertPEM, 0o644); err != nil {
		return nil, err
	}
	return ca, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	crt, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, errors.New("CA key: no PEM data")
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CA key: %w", err)
	}
	pub, ok := crt.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("CA key does not belong to the CA certificate")
	}
	return &CA{Cert: crt, CertPEM: certPEM, key: key}, nil
}

func newCA(now time.Time) (*CA, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "virticle migration CA", Organization: []string{"virticle"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	crt, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return &CA{Cert: crt, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: key},
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// Fingerprint is the SHA-256 of the CA certificate, as lowercase hex.
func (ca *CA) Fingerprint() string { return Fingerprint(ca.Cert) }

// Fingerprint is the SHA-256 of a certificate's DER encoding, lowercase hex.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// Names are what a node certificate must cover.
type Names struct {
	Hosts []string // DNS names; the first is used as the common name
	IPs   []net.IP
}

// Issue creates a certificate and key for one node. The same certificate
// serves as QEMU's server certificate (incoming migration) and client
// certificate (outgoing migration), hence both extended key usages.
func (ca *CA) Issue(n Names, now time.Time) (certPEM, keyPEM []byte, err error) {
	hosts := dedupe(n.Hosts)
	if len(hosts) == 0 {
		return nil, nil, errors.New("a node certificate needs at least one host name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(NodeValidity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{"virticle migration"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     hosts,
	}
	seen := map[string]bool{}
	for _, ip := range n.IPs {
		if ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// Info is a certificate as shown to the user.
type Info struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	DNSNames    []string  `json:"dnsNames"`
	IPs         []string  `json:"ips"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
}

// ParseCert reads the first certificate of a PEM block.
func ParseCert(pemBytes []byte) (*x509.Certificate, Info, error) {
	c, err := parseCertPEM(pemBytes)
	if err != nil {
		return nil, Info{}, err
	}
	in := Info{Subject: c.Subject.String(), Issuer: c.Issuer.String(), DNSNames: c.DNSNames, IPs: []string{}, NotBefore: c.NotBefore, NotAfter: c.NotAfter, Fingerprint: Fingerprint(c)}
	if in.DNSNames == nil {
		in.DNSNames = []string{}
	}
	for _, ip := range c.IPAddresses {
		in.IPs = append(in.IPs, ip.String())
	}
	return c, in, nil
}

func parseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// Verify checks that c was issued by this CA, may act as TLS server and
// client, and covers host. Validity in time is judged separately so that an
// expired certificate is reported as expired, not as untrusted: the chain is
// checked at the nearest moment inside c's own validity.
func (ca *CA) Verify(c *x509.Certificate, host string, now time.Time) error {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	at := now
	if at.Before(c.NotBefore) {
		at = c.NotBefore
	}
	if at.After(c.NotAfter) {
		at = c.NotAfter
	}
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{u}}); err != nil {
			return err
		}
	}
	if host != "" {
		if err := c.VerifyHostname(host); err != nil {
			return err
		}
	}
	return nil
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		v = strings.TrimSpace(v)
		if v != "" && !seen[strings.ToLower(v)] {
			seen[strings.ToLower(v)] = true
			out = append(out, v)
		}
	}
	return out
}
