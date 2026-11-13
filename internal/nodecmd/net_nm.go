package nodecmd

import (
	"strconv"
	"strings"
)

// The NetworkManager backend. It never edits the administrator's profile of
// the uplink except for turning its autoconnect off (the old value is kept
// and put back on rollback and delete). It adds two profiles of its own:
//
//	virticle-BRIDGE        type bridge, interface BRIDGE, a copy of the ipv4/ipv6 settings of the uplink's profile
//	virticle-port-UPLINK   type ethernet on UPLINK, a port of the bridge
//
// The profile names are the marker NetworkManager itself shows; the managed
// bridge list of the node command is the authoritative one.
//
// No NetworkManager checkpoint is used: the rollback is the node command's
// own timer, the same for every configuration system.

type nmStack struct{}

// nmCopyKeys are the ipv4 and ipv6 properties of the uplink's profile that
// the bridge's profile takes over. Anything not listed keeps NetworkManager's default.
var nmCopyKeys = map[string]bool{
	"ipv4.method": true, "ipv4.addresses": true, "ipv4.gateway": true, "ipv4.dns": true, "ipv4.dns-search": true,
	"ipv4.dns-priority": true, "ipv4.routes": true, "ipv4.route-metric": true, "ipv4.route-table": true,
	"ipv4.ignore-auto-dns": true, "ipv4.ignore-auto-routes": true, "ipv4.never-default": true, "ipv4.may-fail": true,
	"ipv4.dhcp-client-id": true, "ipv4.dhcp-hostname": true, "ipv4.dhcp-send-hostname": true, "ipv4.dhcp-timeout": true,
	"ipv4.dhcp-iaid": true,
	"ipv6.method":    true, "ipv6.addresses": true, "ipv6.gateway": true, "ipv6.dns": true, "ipv6.dns-search": true,
	"ipv6.dns-priority": true, "ipv6.routes": true, "ipv6.route-metric": true, "ipv6.route-table": true,
	"ipv6.ignore-auto-dns": true, "ipv6.ignore-auto-routes": true, "ipv6.never-default": true, "ipv6.may-fail": true,
	"ipv6.addr-gen-mode": true, "ipv6.ip6-privacy": true, "ipv6.dhcp-iaid": true, "ipv6.dhcp-duid": true, "ipv6.ra-timeout": true,
}

// nmTerse parses `nmcli -t` field output: KEY:VALUE per line, ':' and '\' escaped by a backslash in values.
func nmTerse(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if v[i] == '\\' && i+1 < len(v) {
				i++
			}
			b.WriteByte(v[i])
		}
		m[k] = b.String()
	}
	return m
}

func nmVersionAtLeast(out string, major, minor int) bool {
	f := strings.Fields(out)
	if len(f) == 0 {
		return false
	}
	p := strings.Split(strings.TrimRight(f[len(f)-1], ".\n"), ".")
	if len(p) < 2 {
		return false
	}
	a, e1 := strconv.Atoi(p[0])
	b, e2 := strconv.Atoi(p[1])
	return e1 == nil && e2 == nil && (a > major || (a == major && b >= minor))
}
