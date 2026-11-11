package hostconn

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Conn is a plain SSH connection to a host, without the libvirt session on
// top. "virticle node prepare" uses it with the administrator's credential
// before libvirt (or the service account) exists on the host. The credential
// lives in the Target and in memory only.
type Conn struct {
	Name string
	ssh  *ssh.Client
	jump *ssh.Client
}

// DialConn opens the SSH connection (through t.Jump when set).
func DialConn(t Target, knownHosts string) (*Conn, error) {
	var jump *ssh.Client
	if t.Jump != nil {
		j, err := dialSSH(*t.Jump, knownHosts, nil)
		if err != nil {
			return nil, fmt.Errorf("jump host %s: %w", t.Jump.Addr, err)
		}
		jump = j
	}
	c, err := dialSSH(t, knownHosts, jump)
	if err != nil {
		if jump != nil {
			jump.Close()
		}
		return nil, fmt.Errorf("ssh %s: %w", t.Addr, err)
	}
	return &Conn{Name: t.Addr, ssh: c, jump: jump}, nil
}

// Close ends the connection.
func (c *Conn) Close() {
	c.ssh.Close()
	if c.jump != nil {
		c.jump.Close()
	}
}

// Run executes a shell command and returns its combined output, trimmed.
func (c *Conn) Run(cmd string) (string, error) { return c.RunInput(cmd, "") }

// RunInput is Run with stdin fed to the command. A secret passed this way
// never appears on a command line.
func (c *Conn) RunInput(cmd, stdin string) (string, error) {
	s, err := c.ssh.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	if stdin != "" {
		s.Stdin = strings.NewReader(stdin)
	}
	out, err := s.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// agentSigners asks the SSH agent named by SSH_AUTH_SOCK for its keys.
func agentSigners() ([]ssh.Signer, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK is not set")
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, err
	}
	// The connection stays open for the signing that follows; the process
	// (the CLI) is short-lived.
	return agent.NewClient(conn).Signers()
}
