package webaccess

import (
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Iface is a network interface of the controller machine that can be chosen
// as the address of the web UI.
type Iface struct {
	Name string `json:"name"`
	// Addrs are its global addresses, IPv4 first. The first is the one bound.
	Addrs []string `json:"addrs"`
}

// Interfaces lists the interfaces that are up and have a usable address:
// loopback and link-local addresses are left out.
func Interfaces() []Iface {
	ifs, err := net.Interfaces()
	if err != nil {
		return []Iface{}
	}
	out := []Iface{}
	for _, in := range ifs {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := in.Addrs()
		var list []net.IP
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && !n.IP.IsMulticast() && !n.IP.IsUnspecified() {
				list = append(list, n.IP)
			}
		}
		if len(list) == 0 {
			continue
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].To4() != nil && list[j].To4() == nil })
		f := Iface{Name: in.Name}
		for _, ip := range list {
			f.Addrs = append(f.Addrs, ip.String())
		}
		out = append(out, f)
	}
	return out
}

// ComposeListen builds the listen address from the host part the interface
// choice stands for ("" is every interface) and a port.
func ComposeListen(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// SplitListen is the reverse: the host part and the port of a listen address.
// custom is true when the host is neither every interface, this machine only,
// nor an address of one of ifaces; the page then offers it as an extra entry
// so that a stored value is never lost.
func SplitListen(addr string, ifaces []Iface) (host string, port int, custom bool) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0, true
	}
	port, _ = strconv.Atoi(p)
	if IsWildcard(addr) {
		return "", port, false
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() && ip.To4() != nil && h == "127.0.0.1" {
		return h, port, false
	}
	for _, f := range ifaces {
		if len(f.Addrs) > 0 && strings.EqualFold(f.Addrs[0], h) {
			return h, port, false
		}
	}
	return h, port, true
}

// CanBindLowPorts reports whether this process may listen on ports below 1024:
// it holds CAP_NET_BIND_SERVICE (the packaged unit grants it).
func CanBindLowPorts() bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			v, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
			return err == nil && v&(1<<10) != 0
		}
	}
	return false
}
