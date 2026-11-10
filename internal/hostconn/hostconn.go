// Package hostconn connects to a libvirt host the agentless way: one SSH
// connection per host, with the libvirt RPC protocol spoken over a channel
// forwarded to the host's libvirt UNIX socket. The same SSH connection is
// reused for host-level commands that libvirt has no API for.
package hostconn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Sockets tried in order: the monolithic/proxy socket first, then the modular
// QEMU daemon's own socket (hosts running virtqemud without virtproxyd).
var libvirtSockets = []string{
	"/run/libvirt/libvirt-sock",
	"/run/libvirt/virtqemud-sock",
}

// Target describes how to reach one host over SSH.
type Target struct {
	Addr     string // host or host:port
	User     string
	KeyFile  string // private key path; optional if Password is set
	Password string // optional
	Jump     *Target
}

// Host is a live connection to one libvirt host.
type Host struct {
	Name string
	*libvirt.Libvirt
	ssh  *ssh.Client
	jump *ssh.Client
}

// Dial opens the SSH connection and the libvirt session on top of it.
// knownHosts is a known_hosts file used trust-on-first-use.
func Dial(t Target, knownHosts string) (*Host, error) {
	var jump *ssh.Client
	if t.Jump != nil {
		j, err := dialSSH(*t.Jump, knownHosts, nil)
		if err != nil {
			return nil, fmt.Errorf("jump host %s: %w", t.Jump.Addr, err)
		}
		jump = j
	}
	client, err := dialSSH(t, knownHosts, jump)
	if err != nil {
		if jump != nil {
			jump.Close()
		}
		return nil, fmt.Errorf("ssh %s: %w", t.Addr, err)
	}
	h := &Host{Name: t.Addr, ssh: client, jump: jump}
	h.Libvirt = libvirt.NewWithDialer(sockDialer{client})
	if err := h.Libvirt.Connect(); err != nil {
		h.Close()
		return nil, fmt.Errorf("libvirt on %s: %w", t.Addr, err)
	}
	return h, nil
}

// Close tears down the libvirt session and the SSH connection(s).
func (h *Host) Close() {
	if h.Libvirt != nil {
		h.Libvirt.Disconnect()
	}
	h.ssh.Close()
	if h.jump != nil {
		h.jump.Close()
	}
}

// DialNet opens a connection from the host itself, for example to a VM's VNC
// server listening on the host's loopback ("tcp", "127.0.0.1:5900") or on a
// UNIX socket ("unix", path).
func (h *Host) DialNet(network, addr string) (net.Conn, error) {
	return h.ssh.Dial(network, addr)
}

// Run executes a shell command on the host and returns its combined output.
func (h *Host) Run(cmd string) (string, error) {
	s, err := h.ssh.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	// CombinedOutput serialises the two copy goroutines; sharing a plain
	// bytes.Buffer between Stdout and Stderr is a data race.
	out, err := s.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// Stream runs a long-lived command on the host and calls line for every line
// it prints, until the command ends or ctx is cancelled. A terminal is
// requested so that closing the session hangs up the remote process instead
// of leaving it behind.
func (h *Host) Stream(ctx context.Context, cmd string, line func(string)) error {
	s, err := h.ssh.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.RequestPty("dumb", 24, 4096, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		return err
	}
	out, err := s.StdoutPipe()
	if err != nil {
		return err
	}
	if err := s.Start(cmd); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line(strings.TrimRight(sc.Text(), "\r"))
	}
	if ctx.Err() != nil {
		return nil
	}
	return s.Wait()
}

type sockDialer struct{ c *ssh.Client }

func (d sockDialer) Dial() (net.Conn, error) {
	var errs []error
	for _, p := range libvirtSockets {
		conn, err := d.c.Dial("unix", p)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p, err))
	}
	return nil, errors.Join(errs...)
}

func withPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, "22")
	}
	return addr
}

func dialSSH(t Target, knownHosts string, via *ssh.Client) (*ssh.Client, error) {
	var auth []ssh.AuthMethod
	if t.KeyFile != "" {
		pem, err := os.ReadFile(t.KeyFile)
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		auth = append(auth, ssh.Password(t.Password))
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: tofu(knownHosts),
		Timeout:         15 * time.Second,
	}
	addr := withPort(t.Addr)
	// Ask the server for the key type we already have on record; otherwise it
	// may present a different type and look like a changed key.
	cfg.HostKeyAlgorithms = knownKeyTypes(knownHosts, addr)
	if via == nil {
		return ssh.Dial("tcp", addr, cfg)
	}
	conn, err := via.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// knownKeyTypes returns the host key algorithms recorded for addr, or nil
// (meaning the library defaults) when the host is not yet known.
func knownKeyTypes(path, addr string) []string {
	check, err := knownhosts.New(path)
	if err != nil {
		return nil
	}
	tcp, _ := net.ResolveTCPAddr("tcp", addr)
	if tcp == nil {
		tcp = &net.TCPAddr{}
	}
	var ke *knownhosts.KeyError
	if !errors.As(check(addr, tcp, probeKey{}), &ke) {
		return nil
	}
	var types []string
	for _, k := range ke.Want {
		t := k.Key.Type()
		if t == ssh.KeyAlgoRSA {
			types = append(types, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		}
		types = append(types, t)
	}
	return types
}

// probeKey is a key that matches nothing, used to list a host's known keys.
type probeKey struct{}

func (probeKey) Type() string                        { return "probe" }
func (probeKey) Marshal() []byte                     { return nil }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe key") }

var khMu sync.Mutex

// tofu accepts and records a host key the first time a host is seen and
// rejects any later change of that key.
func tofu(path string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		khMu.Lock()
		defer khMu.Unlock()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		check, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = check(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			_, werr := fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
			return werr
		}
		return err
	}
}
