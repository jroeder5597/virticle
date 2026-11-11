package cluster

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"virt-proj/internal/hostconn"
	"virt-proj/internal/store"
)

// TestResult is what a trial connection to a node's settings found.
type TestResult struct {
	Hostname       string   `json:"hostname"`
	LibvirtVersion string   `json:"libvirtVersion"`
	HostUUID       string   `json:"hostUuid"`
	Privilege      string   `json:"privilege"` // detected: root | sudo | none
	PrivilegeNote  string   `json:"privilegeNote"`
	ResolvedIPs    []string `json:"resolvedIps,omitempty"`
}

// TestConnect opens a real connection with the given settings (SSH, the
// libvirt session, a version call and the privilege probe) and closes it
// again. Host keys are checked against a throw-away copy of known_hosts, so a
// trial of an address that is never saved leaves no trust entry behind.
func (m *Manager) TestConnect(cfg store.Node, timeout time.Duration) (TestResult, error) {
	var res TestResult
	t, err := m.Target(cfg)
	if err != nil {
		return res, err
	}
	dir, err := os.MkdirTemp("", "vp-khtest")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	kh := filepath.Join(dir, "known_hosts")
	if b, err := os.ReadFile(m.knownHosts); err == nil {
		if err := os.WriteFile(kh, b, 0o600); err != nil {
			return res, err
		}
	}
	h, err := hostconn.DialTimeout(t, kh, timeout)
	if err != nil {
		return res, err
	}
	defer h.Close()
	ver, err := h.ConnectGetLibVersion()
	if err != nil {
		return res, fmt.Errorf("libvirt on %s: %w", cfg.Addr, err)
	}
	res.LibvirtVersion = strconv.FormatUint(ver/1000000, 10) + "." + strconv.FormatUint(ver/1000%1000, 10) + "." + strconv.FormatUint(ver%1000, 10)
	if name, err := h.ConnectGetHostname(); err == nil {
		res.Hostname = name
	}
	if res.HostUUID, err = h.HostUUID(); err != nil {
		res.HostUUID = ""
	}
	p, note, err := h.DetectPrivilege()
	if err != nil {
		return res, fmt.Errorf("checking the login user on %s: %w", cfg.Addr, err)
	}
	res.Privilege, res.PrivilegeNote = string(p), note
	res.ResolvedIPs = ResolveAddr(cfg.Addr)
	return res, nil
}

// ForgetHostKey removes the recorded SSH host key of addr; the next
// connection trusts the key the host then presents.
func (m *Manager) ForgetHostKey(addr string) (int, error) {
	return hostconn.ForgetHostKey(m.knownHosts, addr)
}

// ResolveAddr returns the IP addresses an address leads to, as seen from the
// controller: the literal itself for an IP, else the DNS answer. It is
// informational only; a node behind a jump host is resolved by the jump host.
func ResolveAddr(addr string) []string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return nil
	}
	return ips
}
