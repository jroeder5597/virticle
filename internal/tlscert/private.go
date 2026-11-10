package tlscert

import (
	"net"
	"net/netip"
	"strings"
)

// IsPrivate reports whether a listen address is a loopback, private (RFC 1918,
// unique-local) or link-local IP address. A wildcard address, a public one and
// a host name other than "localhost" are not.
func IsPrivate(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}
