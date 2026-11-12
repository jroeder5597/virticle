package api

// GET /api/local-node: what the controller can tell, without root, about the
// machine it runs on, so that the first-run panel can offer "Set up this
// machine as a node". Nothing here changes anything.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type localNodeInfo struct {
	Capable      bool   `json:"capable"`
	KVMDevice    bool   `json:"kvmDevice"`
	SSHReachable bool   `json:"sshReachable"`
	Address      string `json:"address"`
	Hostname     string `json:"hostname"`
	Registered   bool   `json:"registered"`
	Reason       string `json:"reason,omitempty"`
}

// localProbe holds everything the detection reads from the machine, so that
// tests can replace it.
type localProbe struct {
	Root      string                 // "/" or a fake root with proc/cpuinfo, .dockerenv, ...
	Env       func(string) string    // os.Getenv
	Hostname  func() (string, error) // os.Hostname
	SSHOpen   func() bool            // TCP connect to 127.0.0.1:22
	RouteAddr func() string          // source address of the default route, "" if unknown
	IfAddrs   func() []net.IP        // addresses of the interfaces
}

func realLocalProbe() localProbe {
	return localProbe{
		Root:     "/",
		Env:      os.Getenv,
		Hostname: os.Hostname,
		SSHOpen: func() bool {
			c, err := net.DialTimeout("tcp", "127.0.0.1:22", time.Second)
			if err != nil {
				return false
			}
			c.Close()
			return true
		},
		RouteAddr: func() string {
			// A UDP connect sends nothing; it only makes the kernel pick the route.
			c, err := net.Dial("udp4", "192.0.2.1:9")
			if err != nil {
				return ""
			}
			defer c.Close()
			if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP != nil {
				return a.IP.String()
			}
			return ""
		},
		IfAddrs: func() []net.IP {
			var out []net.IP
			addrs, _ := net.InterfaceAddrs()
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					out = append(out, ipn.IP)
				}
			}
			return out
		},
	}
}

func (p localProbe) isContainer() bool {
	for _, f := range []string{".dockerenv", "run/.containerenv"} {
		if _, err := os.Stat(filepath.Join(p.Root, f)); err == nil {
			return true
		}
	}
	return p.Env("container") != ""
}

func (p localProbe) hasVirtCPU() bool {
	b, err := os.ReadFile(filepath.Join(p.Root, "proc/cpuinfo"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "flags") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == "vmx" || f == "svm" {
				return true
			}
		}
	}
	return false
}

func isUsableIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4.IsGlobalUnicast() && !ip4.IsLinkLocalUnicast()
}

// address is the one to store for this machine: the source address of the
// default route, else the first usable IPv4 interface address, else loopback.
func (p localProbe) address() string {
	if a := net.ParseIP(p.RouteAddr()); a != nil && isUsableIPv4(a) {
		return a.String()
	}
	for _, ip := range p.IfAddrs() {
		if isUsableIPv4(ip) {
			return ip.String()
		}
	}
	return "127.0.0.1"
}

// isThisMachine says whether a stored node address points here.
func (p localProbe) isThisMachine(addr, hostname string) bool {
	host, _ := splitAddr(addr)
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	if host == "localhost" || (hostname != "" && host == strings.ToLower(hostname)) {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, m := range p.IfAddrs() {
		if m.Equal(ip) {
			return true
		}
	}
	return false
}

func (p localProbe) info(nodeAddrs []string) localNodeInfo {
	var r localNodeInfo
	r.Hostname, _ = p.Hostname()
	if _, err := os.Stat(filepath.Join(p.Root, "dev/kvm")); err == nil {
		r.KVMDevice = true
	}
	switch {
	case p.isContainer():
		r.Reason = "This is a container; add another machine as a node."
	case !p.hasVirtCPU():
		r.Reason = "This machine's CPU has no hardware virtualisation."
	default:
		r.Capable = true
	}
	r.Address = p.address()
	for _, a := range nodeAddrs {
		if p.isThisMachine(a, r.Hostname) {
			r.Registered = true
		}
	}
	if r.Capable {
		r.SSHReachable = p.SSHOpen()
	}
	return r
}

func (s *Server) localNode(w http.ResponseWriter, r *http.Request) {
	var addrs []string
	for _, n := range s.Nodes.Status() {
		addrs = append(addrs, n.Addr)
	}
	writeJSON(w, realLocalProbe().info(addrs))
}
