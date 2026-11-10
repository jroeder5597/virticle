package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/coder/websocket"
)

// Reverse proxy support. By default the server believes only what it sees on
// the connection: the peer address, r.Host and r.TLS. When the direct peer is
// inside one of the -trusted-proxy ranges, the X-Forwarded-* headers it sends
// are used instead for the client address, the scheme and the host. Requests
// from any other peer have those headers ignored, so a client cannot choose
// its own rate-limit key or audit address. The Forwarded header (RFC 7239) is
// not read.

// ParseTrustedProxies reads a comma-separated list of CIDRs and plain IPs.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("-trusted-proxy: %q is neither an IP address nor a CIDR range", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// reqView is what the server believes about a request's origin.
type reqView struct {
	IP        string // client address
	Host      string // host the browser used (Origin must match it)
	HTTPS     bool   // the browser talks HTTPS
	Forwarded bool   // taken from a trusted proxy's headers
}

type viewKey struct{}

// viewOf returns the view computed by wrap; for a request that did not pass
// through it, the direct connection's own view.
func viewOf(r *http.Request) reqView {
	if v, ok := r.Context().Value(viewKey{}).(reqView); ok {
		return v
	}
	return directView(r)
}

func directView(r *http.Request) reqView {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return reqView{IP: ip, Host: r.Host, HTTPS: r.TLS != nil}
}

func inRanges(ranges []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ranges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Server) computeView(r *http.Request) reqView {
	v := directView(r)
	if len(s.TrustedProxies) == 0 {
		return v
	}
	peer, err := netip.ParseAddr(v.IP)
	if err != nil || !inRanges(s.TrustedProxies, peer) {
		return v
	}
	v.Forwarded = true
	v.IP = forwardedClient(s.TrustedProxies, r.Header.Values("X-Forwarded-For"), peer.Unmap())
	switch strings.ToLower(lastToken(r.Header.Values("X-Forwarded-Proto"))) {
	case "https":
		v.HTTPS = true
	case "http":
		v.HTTPS = false
	}
	if h := lastToken(r.Header.Values("X-Forwarded-Host")); h != "" && hostRe.MatchString(h) {
		v.Host = h
	}
	return v
}

// forwardedClient walks X-Forwarded-For from the right, skipping the
// addresses of trusted proxies, and returns the first other address: that is
// the last hop a trusted proxy saw, and everything left of it is claimed by
// the client. An unparsable entry ends the walk at the address to its right.
func forwardedClient(trusted []netip.Prefix, xff []string, peer netip.Addr) string {
	var hops []string
	for _, h := range xff {
		for _, p := range strings.Split(h, ",") {
			if p = strings.TrimSpace(p); p != "" {
				hops = append(hops, p)
			}
		}
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		client = a.Unmap()
		if !inRanges(trusted, client) {
			break
		}
	}
	return client.String()
}

// lastToken is the right-most comma-separated value over all header lines,
// i.e. the one added by the nearest proxy.
func lastToken(vals []string) string {
	last := ""
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				last = p
			}
		}
	}
	return last
}

func withView(r *http.Request, v reqView) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), viewKey{}, v))
}

// wsOptions are the WebSocket accept options for this request. The library
// compares Origin with r.Host; behind a proxy that rewrites Host, the host
// the browser used (from X-Forwarded-Host) is allowed as well.
func wsOptions(r *http.Request, subprotocols ...string) *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{Subprotocols: subprotocols}
	if v := viewOf(r); v.Forwarded && !strings.EqualFold(v.Host, r.Host) {
		opts.OriginPatterns = []string{globEscape(v.Host)}
	}
	return opts
}

// globEscape makes a host usable as a path.Match pattern ("[::1]:80").
func globEscape(h string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `*`, `\*`, `?`, `\?`).Replace(h)
}
