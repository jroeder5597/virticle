package nodecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"virticle/internal/nodeproto"
)

const (
	// Version is the protocol number `version` prints. The controller compares
	// it with the number it was built with (internal/hostconn).
	Version = nodeproto.Version
	// NodePath is where the binary is installed on a node.
	NodePath = nodeproto.Path
	// FixedPath is the PATH for every tool the binary starts.
	FixedPath = "/usr/sbin:/usr/bin:/sbin:/bin"
)

// Error is the error type of a verb. Code is the exit status: 2 for arguments
// the verb refuses (Die), 1 for an operation that failed (Fail). A message is
// printed to stderr with the prefix "virticle-node: "; an empty message prints
// nothing.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Die is a refusal of the arguments or the paths (exit status 2).
func Die(format string, a ...any) error { return &Error{2, fmt.Sprintf(format, a...)} }

// Fail is an operation that did not work (exit status 1).
func Fail(format string, a ...any) error { return &Error{1, fmt.Sprintf(format, a...)} }

// ExitCode ends the verb with status n and prints nothing (for a tool whose
// own output and status are passed on).
func ExitCode(n int) error { return &Error{n, ""} }

// Account is the account whose home holds the peer key, the peer known_hosts
// file and authorized_keys: the service account when started through sudo,
// root when started directly.
type Account struct {
	Name     string
	Home     string
	UID, GID uint32
}

// Config is everything a verb takes from the machine. The real binary uses
// DefaultConfig and nothing can change it from outside (no flag, no
// environment variable): the program runs as root. Tests build their own.
type Config struct {
	// Pools returns the raw target directories of the libvirt storage pools of
	// type dir, fs and netfs. The roots logic filters them. Nil means none.
	Pools func(c *Ctx) []string
	// NVRAMDir is the UEFI NVRAM directory, an allowed root of its own.
	NVRAMDir string
	// ShareDir is where firmware templates live (default /usr/share).
	ShareDir string
	// Prefix is put in front of the /etc paths of the migration certificate
	// verbs and of qemu.conf. Empty in production.
	Prefix string
	// Tools maps a tool name to an absolute path ("" means not installed). A
	// tool that is not listed is looked up in FixedPath.
	Tools map[string]string
	// Unprivileged is for tests that do not run as root: no chown, no owner
	// checks, service-account verbs run in this process.
	Unprivileged bool
	// Account is the peer account. Nil: derived from SUDO_USER, or root.
	Account *Account
	// SysDir and ProcDir default to /sys and /proc.
	SysDir, ProcDir string
	// Self is the path to run for the svc-* verbs (default: this executable).
	Self string
}

// DefaultConfig is the configuration of the installed binary.
func DefaultConfig() *Config {
	return &Config{Pools: virshPools, NVRAMDir: "/var/lib/libvirt/qemu/nvram"}
}

func (cfg *Config) sys() string {
	if cfg.SysDir != "" {
		return cfg.SysDir
	}
	return "/sys"
}

func (cfg *Config) proc() string {
	if cfg.ProcDir != "" {
		return cfg.ProcDir
	}
	return "/proc"
}

type cleanupEntry struct {
	id int
	fn func()
}

// shared is the state of one run, shared by a Ctx and its copies.
type shared struct {
	mu        sync.Mutex
	next      int
	cleanups  []cleanupEntry
	roots     []string
	rootsDone bool
}

// Ctx is what a verb receives.
type Ctx struct {
	Args           []string // the arguments after the verb name
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Cfg            *Config
	// Context is cancelled when the process gets SIGINT, SIGTERM, SIGHUP or
	// SIGPIPE. Tools started with Command are killed then.
	Context context.Context
	sh      *shared
}

// Need checks the number of arguments (max < 0: no upper limit).
func (c *Ctx) Need(min, max int, usage string) error {
	if len(c.Args) < min || (max >= 0 && len(c.Args) > max) {
		return Die("usage: %s", usage)
	}
	return nil
}

// Cleanup registers fn to run when the process is signalled while the verb is
// still working. The returned function unregisters it (call it when the thing
// fn removes is meant to stay, or was removed already). fn must be safe to run
// more than once and concurrently with the verb. Every file a verb creates and
// might leave behind needs one; CreateNew does this itself.
func (c *Ctx) Cleanup(fn func()) (cancel func()) {
	c.sh.mu.Lock()
	defer c.sh.mu.Unlock()
	c.sh.next++
	id := c.sh.next
	c.sh.cleanups = append(c.sh.cleanups, cleanupEntry{id, fn})
	return func() {
		c.sh.mu.Lock()
		defer c.sh.mu.Unlock()
		for i, e := range c.sh.cleanups {
			if e.id == id {
				c.sh.cleanups = append(c.sh.cleanups[:i], c.sh.cleanups[i+1:]...)
				return
			}
		}
	}
}

func (s *shared) runCleanups() {
	s.mu.Lock()
	list := s.cleanups
	s.cleanups = nil
	s.mu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		list[i].fn()
	}
}

// Tool returns the absolute path of an external tool.
func (c *Ctx) Tool(name string) (string, error) {
	if p, ok := c.Cfg.Tools[name]; ok {
		if p == "" {
			return "", Fail("%s is not installed on this node", name)
		}
		return p, nil
	}
	if name == "" || strings.ContainsAny(name, "/\x00") {
		return "", Die("unsupported tool name")
	}
	for _, d := range filepath.SplitList(FixedPath) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", Fail("%s is not installed on this node", name)
}

// HasTool reports whether Tool would find name.
func (c *Ctx) HasTool(name string) bool {
	_, err := c.Tool(name)
	return err == nil
}

// Command prepares a tool to run: absolute path, argv slice, never a shell,
// PATH=FixedPath, LC_ALL=C, killed (SIGTERM) when the run is cancelled. Set
// Stdin, Stdout and Stderr on the result as needed.
func (c *Ctx) Command(name string, args ...string) (*exec.Cmd, error) {
	p, err := c.Tool(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(c.Context, p, args...)
	cmd.Env = []string{"PATH=" + FixedPath, "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// Output runs a tool and returns its standard output.
func (c *Ctx) Output(name string, args ...string) ([]byte, error) {
	cmd, err := c.Command(name, args...)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

// CombinedOutput runs a tool and returns standard output and error together.
func (c *Ctx) CombinedOutput(name string, args ...string) ([]byte, error) {
	cmd, err := c.Command(name, args...)
	if err != nil {
		return nil, err
	}
	return cmd.CombinedOutput()
}

// ExitStatus is the exit status in the error of a finished command (1 when
// it is not an exit error).
func ExitStatus(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ee.ExitCode()
	}
	return 1
}

// sub is a context for another verb run in this process.
func (c *Ctx) sub(args []string, stdin io.Reader, stdout io.Writer) *Ctx {
	d := *c
	d.Args, d.Stdin, d.Stdout = args, stdin, stdout
	return &d
}
