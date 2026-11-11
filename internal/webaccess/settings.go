// Package webaccess decides how the controller's web UI is served: plain HTTP,
// a self-signed or own certificate, ACME, or plain HTTP behind a reverse proxy.
// It holds the settings model and its validation, the listeners, and the
// configuration snippets for common proxies. It does not know the API.
package webaccess

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Modes.
const (
	ModeOff        = "off"
	ModeSelfSigned = "selfsigned"
	ModeOwn        = "own"
	ModeACME       = "acme"
	ModeProxy      = "proxy"
)

// Let's Encrypt directory URLs.
const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// DefaultListen is where the controller listens when nothing else is set, and
// where "virticle web reset" puts it back.
const DefaultListen = "127.0.0.1:8080"

// Settings is the whole web access configuration. It is stored as JSON.
type Settings struct {
	Mode string `json:"mode"`
	// Listen is the plain HTTP address (modes off and proxy).
	Listen string `json:"listen"`
	// HTTPSListen is the TLS address (modes selfsigned, own, acme).
	HTTPSListen string `json:"httpsListen"`
	// HTTPRedirect, when set, is an extra plain HTTP address that only
	// redirects to HTTPS (and answers ACME HTTP-01 in mode acme).
	HTTPRedirect string `json:"httpRedirect"`
	// PublicURL is the address users type, e.g. https://virticle.example.org.
	PublicURL string `json:"publicUrl"`
	// Names are the host names and IP addresses a self-signed certificate
	// covers, or that an own certificate must cover.
	Names []string `json:"names"`
	Own   Own      `json:"own"`
	ACME  ACME     `json:"acme"`
	// TrustedProxies are the addresses or CIDRs whose X-Forwarded-* headers
	// are believed. Only used with mode proxy.
	TrustedProxies []string `json:"trustedProxies"`
	HSTS           bool     `json:"hsts"`
}

// Own locates the certificate of mode own. Managed files were uploaded
// through the UI and live in the data directory.
type Own struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	Managed  bool   `json:"managed"`
}

// ACME is the configuration of mode acme.
type ACME struct {
	Domains     []string `json:"domains"`
	Email       string   `json:"email"`
	Directory   string   `json:"directory"`
	AcceptTerms bool     `json:"acceptTerms"`
}

// Defaults is what a controller without stored settings does.
func Defaults() Settings {
	return Settings{Mode: ModeOff, Listen: DefaultListen, HTTPSListen: ":8443", ACME: ACME{Directory: LetsEncrypt}, TrustedProxies: []string{"127.0.0.1", "::1"}}
}

// IsTLS reports whether the mode makes the controller terminate TLS itself.
func (s Settings) IsTLS() bool {
	return s.Mode == ModeSelfSigned || s.Mode == ModeOwn || s.Mode == ModeACME
}

// Trusted returns the trusted proxy list when it is in force (mode proxy).
func (s Settings) Trusted() []netip.Prefix {
	if s.Mode != ModeProxy {
		return nil
	}
	p, _ := ParseProxies(s.TrustedProxies)
	return p
}

// PublicHost is host[:port] of the public URL, or "".
func (s Settings) PublicHost() string {
	u, err := url.Parse(s.PublicURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// Normalize trims and de-duplicates the lists and fills in empty addresses
// with their defaults. It never fails; Check reports what is wrong.
func (s Settings) Normalize() Settings {
	d := Defaults()
	s.Mode = strings.ToLower(strings.TrimSpace(s.Mode))
	s.Listen = strings.TrimSpace(s.Listen)
	s.HTTPSListen = strings.TrimSpace(s.HTTPSListen)
	s.HTTPRedirect = strings.TrimSpace(s.HTTPRedirect)
	s.PublicURL = strings.TrimRight(strings.TrimSpace(s.PublicURL), "/")
	if s.Listen == "" {
		s.Listen = d.Listen
	}
	if s.HTTPSListen == "" {
		s.HTTPSListen = d.HTTPSListen
	}
	s.Names = cleanList(s.Names)
	s.TrustedProxies = cleanList(s.TrustedProxies)
	s.ACME.Domains = cleanList(s.ACME.Domains)
	s.ACME.Email = strings.TrimSpace(s.ACME.Email)
	s.ACME.Directory = strings.TrimSpace(s.ACME.Directory)
	if s.ACME.Directory == "" {
		s.ACME.Directory = d.ACME.Directory
	}
	s.Own.CertFile = strings.TrimSpace(s.Own.CertFile)
	s.Own.KeyFile = strings.TrimSpace(s.Own.KeyFile)
	return s
}

func cleanList(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
			k := strings.ToLower(f)
			if !seen[k] {
				seen[k] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// ParseProxies reads plain IPs and CIDRs.
func ParseProxies(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range list {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an IP address nor a CIDR range", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

var dnsName = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// ValidName reports whether s is a host name or an IP address.
func ValidName(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	return len(s) <= 253 && dnsName.MatchString(s)
}

// ParsePublicURL accepts scheme://host[:port] with no path.
func ParsePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("must look like https://virticle.example.org")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("must not contain a path, query or user name")
	}
	if !ValidName(u.Hostname()) {
		return nil, fmt.Errorf("%q is not a valid host name", u.Hostname())
	}
	return u, nil
}

// SplitAddr validates host:port and returns the port.
func SplitAddr(addr string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("%q is not host:port (for example 127.0.0.1:8080 or :8443)", addr)
	}
	n, err := net.LookupPort("tcp", p)
	if err != nil || n < 0 || n > 65535 {
		return "", 0, fmt.Errorf("%q has an invalid port", addr)
	}
	return h, n, nil
}

// IsWildcard reports whether the address accepts connections on every interface.
func IsWildcard(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return h == "" || (ip != nil && ip.IsUnspecified())
}

// SuggestedNames are the names a self-signed certificate should cover by
// default: this machine's host name and its global interface addresses.
func SuggestedNames() []string {
	var out []string
	if h, err := os.Hostname(); err == nil && h != "" {
		out = append(out, h)
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && !n.IP.IsMulticast() {
			out = append(out, n.IP.String())
		}
	}
	return cleanList(out)
}
