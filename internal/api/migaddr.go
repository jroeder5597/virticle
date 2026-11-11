package api

// The address other nodes use to reach a node, and the URIs built from it.
//
// Migration must never depend on one node resolving another's host name: the
// nodes of a cluster often have no shared DNS. So the memory stream, the NBD
// disk copy and the qemu+ssh connection all name the destination by the
// address stored for it, not by the host name its libvirtd reports.

import (
	"fmt"
	"net"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/store"
)

// splitAddr splits a node address ("host", "host:port", "[v6]:port" or a bare
// IPv6 literal) into host and port; port is empty when absent.
func splitAddr(addr string) (host, port string) {
	addr = strings.TrimSpace(addr)
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return strings.Trim(addr, "[]"), ""
}

// migrationTarget is how other nodes reach n: its migration address when one
// is set, else the address the controller uses. The port, when there is one,
// is the SSH port.
func migrationTarget(n store.Node) (host, port string) {
	if a := strings.TrimSpace(n.MigrationAddr); a != "" {
		return splitAddr(a)
	}
	return splitAddr(n.Addr)
}

// validAddrHost reports whether h is an IP address or a plain host name.
func validAddrHost(h string) bool {
	return net.ParseIP(h) != nil || hostRE.MatchString(h)
}

// bracket wraps an IPv6 literal in square brackets for use in a URI.
func bracket(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

// sshURI is libvirt's connection URI from a source node to the target.
func sshURI(user, host, port string) string {
	h := bracket(host)
	if port != "" && port != "22" {
		h += ":" + port
	}
	return "qemu+ssh://" + user + "@" + h + "/system"
}

// tcpURI is the URI of the migration data stream (VIR_MIGRATE_PARAM_URI) or of
// the NBD disk copy (VIR_MIGRATE_PARAM_DISKS_URI). libvirt picks the port from
// 49152-49215 on the destination when none is given.
func tcpURI(host string) string { return "tcp://" + bracket(host) }

// scpHost is host in the form scp and ssh accept after "user@": IPv6
// literals need brackets for scp's host:path syntax.
func scpHost(host string) string { return bracket(host) }

// migEndpoint is everything the migration needs to know about the target.
type migEndpoint struct {
	User, Host, Port string
}

func (e migEndpoint) URI() string { return sshURI(e.User, e.Host, e.Port) }

func (e migEndpoint) String() string {
	h := e.User + "@" + e.Host
	if e.Port != "" && e.Port != "22" {
		h += " (port " + e.Port + ")"
	}
	return h
}

// sshArgs are the ssh/scp options for the endpoint's port; scp uses -P.
func (e migEndpoint) sshPort(flag string) string {
	if e.Port == "" || e.Port == "22" {
		return ""
	}
	return fmt.Sprintf("%s %s ", flag, e.Port)
}

// endpointFor validates the target node's settings.
func endpointFor(n store.Node) (migEndpoint, error) {
	host, port := migrationTarget(n)
	user := n.User
	if user == "" {
		user = "root"
	}
	if !userRE.MatchString(user) || !validAddrHost(host) {
		return migEndpoint{}, fmt.Errorf("unsupported characters in user %q or address %q", user, host)
	}
	return migEndpoint{user, host, port}, nil
}

// migrateAddrParams are the migration parameters that name the destination:
// the data stream (VIR_MIGRATE_PARAM_URI) and the NBD disk copy
// (VIR_MIGRATE_PARAM_DISKS_URI). Without them libvirt falls back to the
// destination's own host name, which the source may not be able to resolve.
func migrateAddrParams(host string) []libvirt.TypedParam {
	u := tcpURI(host)
	return []libvirt.TypedParam{
		{Field: libvirt.MigrateParamURI, Value: *libvirt.NewTypedParamValueString(u)},
		{Field: libvirt.MigrateParamDisksURI, Value: *libvirt.NewTypedParamValueString(u)},
	}
}

// tlsDestination is the name QEMU verifies the destination's certificate
// against (VIR_MIGRATE_PARAM_TLS_DESTINATION). It must be one the certificate
// always carries: certNames puts the libvirt host name in the certificate
// whenever it is a valid DNS name, and the node's addresses always, so the
// host name is used when valid and the migration address otherwise.
func tlsDestination(hostname, migHost string) string {
	if hostRE.MatchString(hostname) {
		return hostname
	}
	return migHost
}
