// Package tlscert creates and reuses the controller's self-signed certificate.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	validity  = 825 * 24 * time.Hour
	renewLeft = 30 * 24 * time.Hour
)

// Names are what the certificate must cover.
type Names struct {
	Hosts []string // DNS names; "localhost" is always added
	IPs   []net.IP // addresses the certificate must cover
	// ExtraIPs are added when a certificate is generated but not required of
	// an existing one (the interface addresses of a wildcard listener).
	ExtraIPs []net.IP
}

// Ensure returns the paths of a usable certificate and key in dir, creating
// them when missing, unreadable, close to expiry or lacking a required name.
func Ensure(dir string, n Names, now time.Time) (certFile, keyFile string, generated bool, err error) {
	certFile, keyFile = filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil && covers(c, n, now) {
		return certFile, keyFile, false, nil
	}
	if err := generate(certFile, keyFile, n, now); err != nil {
		return "", "", false, err
	}
	return certFile, keyFile, true, nil
}

// Generate creates a new certificate and key in dir whether or not one exists
// (the "renew" button), and returns their paths.
func Generate(dir string, n Names, now time.Time) (certFile, keyFile string, err error) {
	certFile, keyFile = filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	return certFile, keyFile, generate(certFile, keyFile, n, now)
}

func covers(c tls.Certificate, n Names, now time.Time) bool {
	if len(c.Certificate) == 0 {
		return false
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || now.Add(renewLeft).After(x.NotAfter) || now.Before(x.NotBefore) {
		return false
	}
	for _, h := range append([]string{"localhost"}, n.Hosts...) {
		if x.VerifyHostname(h) != nil {
			return false
		}
	}
	for _, ip := range n.IPs {
		if x.VerifyHostname(ip.String()) != nil {
			return false
		}
	}
	return true
}

func generate(certFile, keyFile string, n Names, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "virticle", Organization: []string{"virticle (self-signed)"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dedupe(append([]string{"localhost"}, n.Hosts...)),
	}
	seen := map[string]bool{}
	for _, ip := range append(append(append([]net.IP{}, n.IPs...), n.ExtraIPs...), net.ParseIP("127.0.0.1"), net.IPv6loopback) {
		if !seen[ip.String()] {
			seen[ip.String()] = true
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	// Key first: a certificate without its key is useless, never the reverse.
	if err := writeAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return writeAtomic(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
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
		if v != "" && !seen[strings.ToLower(v)] {
			seen[strings.ToLower(v)] = true
			out = append(out, v)
		}
	}
	return out
}

// Fingerprint returns the SHA-256 of the first certificate in a PEM file, as
// colon-separated hex (what browsers and `openssl x509 -fingerprint` show).
func Fingerprint(certFile string) (string, error) {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return "", fmt.Errorf("%s: no PEM certificate found", certFile)
	}
	sum := sha256.Sum256(blk.Bytes)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, 32)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":"), nil
}

// IsLoopback reports whether a listen address binds only to the loopback
// interface. An empty host (":8080") or a wildcard is not loopback.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NamesFor works out the certificate names for a listen address.
func NamesFor(addr string) Names {
	var n Names
	if h, err := os.Hostname(); err == nil && h != "" {
		n.Hosts = append(n.Hosts, h)
	}
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	switch {
	case ip != nil && !ip.IsUnspecified():
		n.IPs = append(n.IPs, ip)
	case ip != nil || host == "":
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					n.ExtraIPs = append(n.ExtraIPs, ipn.IP)
				}
			}
		}
	case host != "":
		n.Hosts = append(n.Hosts, host)
	}
	return n
}
