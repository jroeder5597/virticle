package cluster

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// Step statuses of a connection test.
const (
	StepOK      = "ok"
	StepWarning = "warning"
	StepError   = "error"
	StepInfo    = "info"
	StepSkipped = "skipped" // an earlier step failed, so this one did not run
)

// TestStep is one check of a connection test.
type TestStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// TestResult is what a trial connection to a node's settings found. Steps
// lists every check in the order it runs; a failed step is followed by
// "skipped" steps for what depends on it. The other fields are the earlier
// flat result and stay for compatibility.
type TestResult struct {
	Hostname       string     `json:"hostname"`
	LibvirtVersion string     `json:"libvirtVersion"`
	HostUUID       string     `json:"hostUuid"`
	Privilege      string     `json:"privilege"` // detected: root | sudo | none
	PrivilegeNote  string     `json:"privilegeNote"`
	ResolvedIPs    []string   `json:"resolvedIps,omitempty"`
	OK             bool       `json:"ok"`
	Steps          []TestStep `json:"steps"`
	// DomCaps is the node's KVM domain capabilities document, for the CPU
	// baseline check; empty when it could not be read.
	DomCaps string `json:"-"`
}

// testPlan lists the step ids and titles in the order they run.
func testPlan(cfg store.Node) [][2]string {
	p := [][2]string{{"address", "Address resolves"}}
	if cfg.Jump != "" {
		p = append(p, [2]string{"jump", "Jump host"})
	}
	return append(p,
		[2]string{"reach", "Host reachable"},
		[2]string{"hostkey", "SSH host key"},
		[2]string{"ssh", "SSH sign-in as " + cfg.User},
		[2]string{"libvirt", "libvirt reachable"},
		[2]string{"privilege", "Root access"},
		[2]string{"migration", "Migration address"},
	)
}

// dialStage says which step a Dial error belongs to.
func dialStage(err error) string {
	m := err.Error()
	switch {
	case strings.HasPrefix(m, "jump host "):
		return "jump"
	case strings.Contains(m, "key mismatch"):
		return "hostkey"
	case strings.Contains(m, "unable to authenticate"), strings.Contains(m, "no supported methods"):
		return "ssh"
	case strings.HasPrefix(m, "libvirt on "):
		return "libvirt"
	}
	return "reach"
}

// stepper collects steps in plan order.
type stepper struct {
	plan  [][2]string
	steps []TestStep
}

func (s *stepper) add(id, status, detail string) {
	for _, p := range s.plan {
		if p[0] == id {
			s.steps = append(s.steps, TestStep{ID: id, Title: p[1], Status: status, Detail: detail})
			return
		}
	}
}

// fail records a failure at id and marks every planned step after it as not run.
func (s *stepper) fail(id, detail string) {
	s.add(id, StepError, detail)
	seen := false
	for _, p := range s.plan {
		if seen {
			s.add(p[0], StepSkipped, "")
		}
		if p[0] == id {
			seen = true
		}
	}
}

func (s *stepper) failed() bool {
	for _, st := range s.steps {
		if st.Status == StepError {
			return true
		}
	}
	return false
}

// dialFailed records the steps that passed before a dial error at stage,
// then the failure itself.
func (s *stepper) dialFailed(stage string, err error, keyKnown bool) {
	passed := map[string]string{
		"jump":    "reached",
		"reach":   "TCP connection and SSH handshake succeeded",
		"hostkey": "matches the recorded key",
		"ssh":     "key accepted",
	}
	if !keyKnown {
		passed["hostkey"] = "new host, trusted on first use"
	}
	for _, p := range s.plan[1:] {
		if p[0] == stage || stage == "jump" {
			break
		}
		s.add(p[0], StepOK, passed[p[0]])
	}
	msg := err.Error()
	switch stage {
	case "hostkey":
		msg = "changed: the host presents a different key than the one on record; forget the host key if the node was reinstalled"
	case "jump":
		msg = strings.TrimPrefix(msg, "jump host ")
	case "ssh":
		msg = "login refused (" + afterSSH(msg) + ")"
	}
	s.fail(stage, msg)
}

func afterSSH(m string) string {
	if i := strings.Index(m, "ssh: "); i >= 0 {
		return m[i+len("ssh: "):]
	}
	return m
}

// TestConnect opens a real connection with the given settings (SSH, the
// libvirt session, a version call and the privilege probe) and closes it
// again. Host keys are checked against a throw-away copy of known_hosts, so a
// trial of an address that is never saved leaves no trust entry behind.
// The result carries its step list even when the connection fails; the error
// is then the first failure.
func (m *Manager) TestConnect(cfg store.Node, timeout time.Duration) (res TestResult, err error) {
	st := &stepper{plan: testPlan(cfg)}
	defer func() { res.Steps, res.OK = st.steps, !st.failed() }()
	t, err := m.Target(cfg)
	if err != nil {
		return res, err
	}
	res.ResolvedIPs = ResolveAddr(cfg.Addr)
	switch {
	case len(res.ResolvedIPs) > 0:
		d := strings.Join(res.ResolvedIPs, ", ")
		if cfg.Jump != "" && net.ParseIP(strings.Split(cfg.Addr, ":")[0]) == nil {
			d += " (the jump host resolves it when connecting)"
		}
		st.add("address", StepOK, d)
	case cfg.Jump != "":
		st.add("address", StepInfo, "not resolvable from here; the jump host resolves it")
	default:
		err = fmt.Errorf("%s does not resolve", cfg.Addr)
		st.fail("address", err.Error())
		return res, err
	}
	dir, err := os.MkdirTemp("", "virticle-khtest")
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
	known := hostconn.HostKeyKnown(kh, cfg.Addr)
	h, err := hostconn.DialTimeout(t, kh, timeout)
	if err != nil {
		st.dialFailed(dialStage(err), err, known)
		return res, err
	}
	defer h.Close()
	if cfg.Jump != "" {
		st.add("jump", StepOK, "reached")
	}
	st.add("reach", StepOK, "TCP connection and SSH handshake succeeded")
	if known {
		st.add("hostkey", StepOK, "matches the recorded key")
	} else {
		st.add("hostkey", StepInfo, "new host, trusted on first use")
	}
	st.add("ssh", StepOK, "key accepted")
	ver, err := h.ConnectGetLibVersion()
	if err != nil {
		err = fmt.Errorf("libvirt on %s: %w", cfg.Addr, err)
		st.fail("libvirt", err.Error())
		return res, err
	}
	res.LibvirtVersion = strconv.FormatUint(ver/1000000, 10) + "." + strconv.FormatUint(ver/1000%1000, 10) + "." + strconv.FormatUint(ver%1000, 10)
	if name, err := h.ConnectGetHostname(); err == nil {
		res.Hostname = name
	}
	if res.HostUUID, err = h.HostUUID(); err != nil {
		res.HostUUID = ""
	}
	d := "libvirt " + res.LibvirtVersion
	if res.Hostname != "" {
		d += " on " + res.Hostname
	}
	st.add("libvirt", StepOK, d)
	p, note, err := h.DetectPrivilege()
	if err != nil {
		err = fmt.Errorf("checking the login user on %s: %w", cfg.Addr, err)
		st.fail("privilege", err.Error())
		return res, err
	}
	res.Privilege, res.PrivilegeNote = string(p), note
	if p == hostconn.PrivNone {
		st.add("privilege", StepWarning, note+"; features that need root are off")
	} else {
		st.add("privilege", StepOK, string(p)+": "+note)
	}
	status, detail := migrationStep(h, cfg)
	st.add("migration", status, detail)
	if dc, err := h.ConnectGetDomainCapabilities(nil, libvirt.OptString{"x86_64"}, libvirt.OptString{"q35"}, libvirt.OptString{"kvm"}, 0); err == nil {
		res.DomCaps = dc
	}
	return res, nil
}

// migrationStep checks that the address other nodes use resolves on the
// node itself.
func migrationStep(h *hostconn.Host, cfg store.Node) (string, string) {
	addr, src := cfg.MigrationAddr, "set explicitly"
	if addr == "" {
		addr, src = cfg.Addr, "the node address"
	}
	host := addr
	if hp, _, err := net.SplitHostPort(addr); err == nil {
		host = hp
	}
	if net.ParseIP(host) != nil {
		return StepOK, host + " (" + src + ")"
	}
	out, err := h.Run("getent ahosts " + hostconn.Quote(host))
	f := strings.Fields(out)
	if err != nil || len(f) == 0 {
		return StepWarning, host + " (" + src + ") does not resolve on the node; other nodes may not reach it by name"
	}
	return StepOK, host + " resolves to " + f[0] + " on the node (" + src + ")"
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
