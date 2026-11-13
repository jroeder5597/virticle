package nodecmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Node to node transfer with the peer account's own key.
//
// The peer account is the service account when the program was started through
// sudo (SUDO_USER), and root when it was started directly by root. Its ~/.ssh
// holds the peer key, the peer known_hosts file and authorized_keys. For the
// service account the files are read and edited by this program started again
// as that account (the svc-* verbs), so root never follows a link planted
// there; for root the same functions run in this process.

func init() {
	Register("peer-key", verbPeerKey)
	Register("peer-allow", verbPeerAllow)
	Register("peer-disallow", verbPeerDisallow)
	Register("peer-hostkey-set", verbPeerHostkeySet)
	Register("peer-hostkey-unset", verbPeerHostkeyUnset)
	Register("peer-list", verbPeerList)
	Register("peer-send", verbPeerSend)
	Register("peer-ping", verbPeerPing)
	Register("peer-recv", verbPeerRecv)
	Register("svc-allow", verbSvcAllow)
	Register("svc-hosts", verbSvcHosts)
	Register("svc-list", verbSvcList)
	Register("svc-peerkey", verbSvcPeerKey)
}

const (
	peerKeyName   = "virticle_peer_ed25519"
	peerHostsName = "virticle_peer_known_hosts"
	// PeerMark is in every authorized_keys line of a peer key.
	PeerMark = "virticle-node peer-recv"
	// oldPeerMark is in the peer key lines of the shell script that came
	// before the node command.
	oldPeerMark = "virticle-helper peer-recv"
)

// PeerLine is the authorized_keys line for a peer key: the key can run
// peer-recv and nothing else. For root the command is the program itself, for
// the service account it goes through sudo.
func PeerLine(blob string, root bool) string {
	cmd := "sudo -n " + NodePath + " peer-recv"
	if root {
		cmd = NodePath + " peer-recv"
	}
	return `restrict,command="` + cmd + `" ssh-ed25519 ` + blob + " virticle-peer"
}

func numOK(s string) bool {
	return s != "" && len(s) <= 15 && strings.Trim(s, "0123456789") == ""
}

func hostOK(s string) bool {
	if s == "" || s[0] == '-' || len(s) > 253 {
		return false
	}
	return strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789.:_-") == ""
}

func portOK(s string) bool {
	if !numOK(s) {
		return false
	}
	n, _ := strconv.Atoi(s)
	return n >= 1 && n <= 65535
}

var peerUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func peerArgs(host, port, user string) error {
	if !hostOK(host) || !portOK(port) || !peerUserRE.MatchString(user) {
		return Die("unsupported host, port or user")
	}
	return nil
}

var blobRE = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)

// readLine reads one line (without the newline) from r; ok is false at EOF
// before any byte. A last line without newline counts.
func readLine(r *bufio.Reader) (string, bool) {
	l, err := r.ReadString('\n')
	if err != nil && l == "" {
		return "", false
	}
	return strings.TrimSuffix(l, "\n"), true
}

// readKey reads one "ssh-ed25519 BLOB [comment]" line from stdin.
func readKey(c *Ctx) (string, error) {
	line, _ := readLine(bufio.NewReader(io.LimitReader(c.Stdin, 4096)))
	if !strings.HasPrefix(line, "ssh-ed25519 ") {
		return "", Die("expected an ssh-ed25519 public key")
	}
	blob, _, _ := strings.Cut(strings.TrimPrefix(line, "ssh-ed25519 "), " ")
	if !blobRE.MatchString(blob) || len(blob) < 60 || len(blob) > 100 {
		return "", Die("unsupported key")
	}
	return blob, nil
}

// --- the account ---

// DeriveAccount decides which account the peer verbs act for: the sudo caller
// when there is one, root when started directly. lookup resolves a name.
func DeriveAccount(sudoUser string, euid int, lookup func(name string) (*Account, error)) (*Account, error) {
	if sudoUser == "" || sudoUser == "root" {
		if euid != 0 {
			return nil, Die("this runs through sudo for the service account, or as root")
		}
		a, err := lookup("root")
		if err != nil || a.UID != 0 {
			return nil, Die("cannot find the root account")
		}
		return a, nil
	}
	if strings.Trim(sudoUser, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		return nil, Die("unsupported account")
	}
	a, err := lookup(sudoUser)
	if err != nil || a.UID == 0 {
		return nil, Die("unsupported account")
	}
	if !PlainPath(a.Home) {
		return nil, Die("unsupported home directory")
	}
	return a, nil
}

func (c *Ctx) lookupAccount(name string) (*Account, error) {
	if cmd, err := c.Command("getent", "passwd", name); err == nil {
		if out, err := cmd.Output(); err == nil {
			f := strings.Split(strings.TrimSpace(string(out)), ":")
			if len(f) >= 6 && f[0] == name {
				uid, e1 := strconv.ParseUint(f[2], 10, 32)
				gid, e2 := strconv.ParseUint(f[3], 10, 32)
				if e1 == nil && e2 == nil {
					return &Account{Name: name, Home: f[5], UID: uint32(uid), GID: uint32(gid)}, nil
				}
			}
		}
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	return &Account{Name: name, Home: u.HomeDir, UID: uint32(uid), GID: uint32(gid)}, nil
}

func (c *Ctx) account() (*Account, error) {
	if c.Cfg.Account != nil {
		return c.Cfg.Account, nil
	}
	return DeriveAccount(os.Getenv("SUDO_USER"), os.Geteuid(), c.lookupAccount)
}

// svcRun runs the svc verb for account a with the given arguments: in this
// process for root (and in tests), else as the account itself.
func (c *Ctx) svcRun(a *Account, stdout io.Writer, verb string, args ...string) error {
	if a.UID == 0 || c.Cfg.Unprivileged {
		sc := c.sub(args, c.Stdin, stdout)
		switch verb {
		case "svc-allow":
			return svcAllow(sc, args[0], args[1], args[2], args[3])
		case "svc-hosts":
			return svcHosts(sc, args[0], args[1], args[2:])
		case "svc-list":
			return svcList(sc, args[0], args[1])
		case "svc-peerkey":
			return svcPeerKey(sc, args[0])
		}
		return Die("unsupported verb")
	}
	self := c.Cfg.Self
	if self == "" {
		self = NodePath
		if exe, err := os.Executable(); err == nil {
			self = exe
		}
	}
	cmd := exec.CommandContext(c.Context, self, append([]string{verb}, args...)...)
	cmd.Env = []string{"PATH=" + FixedPath, "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.Stdout = stdout
	var eb bytes.Buffer
	cmd.Stderr = &eb
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: []uint32{}}}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(eb.String()), "virticle-node: "))
		if msg == "" {
			msg = err.Error()
		}
		return Fail("%s", msg)
	}
	return nil
}

func svcDirAny(c *Ctx, home string) (int, error) {
	if !PlainPath(home) {
		return -1, Die("unsupported home directory")
	}
	fd, err := unix.Open(home+"/.ssh", oDir, 0)
	if err != nil {
		return -1, Fail("no usable %s/.ssh", home)
	}
	if !c.Cfg.Unprivileged {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || int(st.Uid) != os.Geteuid() {
			unix.Close(fd)
			return -1, Die("%s/.ssh is not yours", home)
		}
	}
	return fd, nil
}

// svcVerbGuard: the svc-* verbs are for the service account itself. Root never
// runs them from the command line, so a service account cannot use sudo to make
// root edit the files of another account.
func svcVerbGuard(c *Ctx) error {
	if os.Geteuid() == 0 && !c.Cfg.Unprivileged {
		return Die("this is for the service account itself")
	}
	return nil
}

const maxSSHFile = 4 << 20

// readSSHFile returns the content of name in dirfd; nil, false when absent.
// A symbolic link is refused.
func readSSHFile(dirfd int, name, display string) ([]byte, bool, error) {
	st, err := fstatat(dirfd, name)
	if err != nil {
		if isNotExist(err) {
			return nil, false, nil
		}
		return nil, false, Fail("cannot read %s", display)
	}
	if isLinkMode(st.Mode) {
		return nil, false, Fail("%s is a symbolic link", display)
	}
	if !isRegMode(st.Mode) {
		return nil, false, nil
	}
	fd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, Fail("cannot read %s", display)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSSHFile))
	if err != nil {
		return nil, false, Fail("cannot read %s", display)
	}
	return b, true, nil
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// replaceSSHFile writes content to a temporary file next to name and renames
// it over name. unchanged compares with the old content.
func (c *Ctx) replaceSSHFile(dirfd int, tmpPrefix, name, display string, content, old []byte, hadOld bool) error {
	if hadOld && bytes.Equal(content, old) {
		return nil
	}
	var tmp string
	var fd int
	var err error
	for i := 0; i < 100; i++ {
		tmp = tmpPrefix + randName(6)
		fd, err = openat(dirfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if err == nil || !errIs(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return Fail("cannot create a temporary file")
	}
	remove := func() { _ = unix.Unlinkat(dirfd, tmp, 0) }
	cancel := c.Cleanup(remove)
	defer cancel()
	f := os.NewFile(uintptr(fd), tmp)
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		remove()
		return Fail("cannot write %s", display)
	}
	if err := unix.Renameat(dirfd, tmp, dirfd, name); err != nil {
		remove()
		return Fail("cannot replace %s", display)
	}
	return nil
}

// svcAllow: authorized_keys without the peer lines that carry blob, plus line
// when it is not empty.
func svcAllow(c *Ctx, home, blob, line, mark string) error {
	dirfd, err := svcDirAny(c, home)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	display := home + "/.ssh/authorized_keys"
	old, had, err := readSSHFile(dirfd, "authorized_keys", display)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	for _, l := range splitLines(old) {
		if strings.Contains(l, blob) && strings.Contains(l, mark) {
			continue
		}
		// A line written for the old shell script names a program that is
		// gone. sshd takes the first line that matches a key, so such a line
		// would shadow the new one for the same peer.
		if strings.Contains(l, oldPeerMark) {
			continue
		}
		out.WriteString(l + "\n")
	}
	if line != "" {
		out.WriteString(line + "\n")
	}
	return c.replaceSSHFile(dirfd, ".authorized_keys.", "authorized_keys", display, out.Bytes(), old, had)
}

// svcHosts: the peer known_hosts without name, plus a line per key line.
func svcHosts(c *Ctx, home, name string, keys []string) error {
	dirfd, err := svcDirAny(c, home)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	display := home + "/.ssh/" + peerHostsName
	old, had, err := readSSHFile(dirfd, peerHostsName, display)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	for _, l := range splitLines(old) {
		if f := strings.Fields(l); len(f) > 0 && f[0] == name {
			continue
		}
		out.WriteString(l + "\n")
	}
	for _, k := range keys {
		out.WriteString(name + " " + k + "\n")
	}
	return c.replaceSSHFile(dirfd, ".peer_hosts.", peerHostsName, display, out.Bytes(), old, had)
}

var allowLineRE = regexp.MustCompile(` ssh-ed25519 ([A-Za-z0-9+/=]*) virticle-peer$`)

// svcList prints "allow BLOB" for the peer keys and "host A B" for the hosts.
func svcList(c *Ctx, home, mark string) error {
	dirfd, err := svcDirAny(c, home)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	ak, _, err := readSSHFile(dirfd, "authorized_keys", home+"/.ssh/authorized_keys")
	if err != nil {
		return err
	}
	for _, l := range splitLines(ak) {
		if strings.Contains(l, mark) {
			if m := allowLineRE.FindStringSubmatch(l); m != nil {
				fmt.Fprintln(c.Stdout, "allow "+m[1])
			}
		}
	}
	kh, _, err := readSSHFile(dirfd, peerHostsName, home+"/.ssh/"+peerHostsName)
	if err != nil {
		return err
	}
	for _, l := range splitLines(kh) {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		b := ""
		if len(f) > 1 {
			b = f[1]
		}
		fmt.Fprintf(c.Stdout, "host %s %s\n", f[0], b)
	}
	return nil
}

func verbSvcAllow(c *Ctx) error {
	if err := c.Need(4, 4, "svc-allow HOME BLOB LINE MARK"); err != nil {
		return err
	}
	if err := svcVerbGuard(c); err != nil {
		return err
	}
	return svcAllow(c, c.Args[0], c.Args[1], c.Args[2], c.Args[3])
}

func verbSvcHosts(c *Ctx) error {
	if err := c.Need(2, -1, "svc-hosts HOME NAME [KEYLINE]..."); err != nil {
		return err
	}
	if err := svcVerbGuard(c); err != nil {
		return err
	}
	return svcHosts(c, c.Args[0], c.Args[1], c.Args[2:])
}

func verbSvcList(c *Ctx) error {
	if err := c.Need(2, 2, "svc-list HOME MARK"); err != nil {
		return err
	}
	if err := svcVerbGuard(c); err != nil {
		return err
	}
	return svcList(c, c.Args[0], c.Args[1])
}

// verbSvcPeerKey prints the first 400 bytes of the peer public key file.
func verbSvcPeerKey(c *Ctx) error {
	if err := c.Need(1, 1, "svc-peerkey HOME"); err != nil {
		return err
	}
	if err := svcVerbGuard(c); err != nil {
		return err
	}
	return svcPeerKey(c, c.Args[0])
}

func svcPeerKey(c *Ctx, home string) error {
	dirfd, err := svcDirAny(c, home)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	fd, err := openat(dirfd, peerKeyName+".pub", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Fail("no peer key")
	}
	f := os.NewFile(uintptr(fd), "key")
	defer f.Close()
	io.Copy(c.Stdout, io.LimitReader(f, 400))
	return nil
}

// --- the verbs ---

func verbPeerKey(c *Ctx) error {
	if err := c.Need(0, 0, "peer-key"); err != nil {
		return err
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := c.svcRun(a, &b, "svc-peerkey", a.Home); err != nil {
		return Fail("no peer key (run virticle node prepare again)")
	}
	first, _, _ := strings.Cut(b.String(), "\n")
	if !strings.HasPrefix(first, "ssh-ed25519 ") {
		return Fail("the peer key file is not an ssh-ed25519 key")
	}
	f := strings.Fields(first)
	if len(f) < 2 {
		return Fail("the peer key file is not an ssh-ed25519 key")
	}
	fmt.Fprintln(c.Stdout, f[0]+" "+f[1])
	return nil
}

func verbPeerAllow(c *Ctx) error {
	if err := c.Need(0, 0, "peer-allow (public key on stdin)"); err != nil {
		return err
	}
	blob, err := readKey(c)
	if err != nil {
		return err
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	if err := c.svcRun(a, io.Discard, "svc-allow", a.Home, blob, PeerLine(blob, a.UID == 0), PeerMark); err != nil {
		return Fail("cannot update authorized_keys: %s", errMsg(err))
	}
	return nil
}

// verbPeerDisallow removes only lines that carry the peer-recv restriction, so
// the controller's own login key cannot be removed through it.
func verbPeerDisallow(c *Ctx) error {
	if err := c.Need(0, 0, "peer-disallow (public key on stdin)"); err != nil {
		return err
	}
	blob, err := readKey(c)
	if err != nil {
		return err
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	if err := c.svcRun(a, io.Discard, "svc-allow", a.Home, blob, "", PeerMark); err != nil {
		return Fail("cannot update authorized_keys: %s", errMsg(err))
	}
	return nil
}

var hostKeyTypes = []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa"}

func knownHostsName(host, port string) string {
	if port == "22" {
		return host
	}
	return "[" + host + "]:" + port
}

func verbPeerHostkeySet(c *Ctx) error {
	if err := c.Need(2, 2, "peer-hostkey-set HOST PORT (key lines on stdin)"); err != nil {
		return err
	}
	host, port := c.Args[0], c.Args[1]
	if !hostOK(host) || !portOK(port) {
		return Die("unsupported host or port")
	}
	var keys []string
	rd := bufio.NewReader(io.LimitReader(c.Stdin, 64<<10))
	for {
		l, ok := readLine(rd)
		if !ok {
			break
		}
		typ, rest, _ := strings.Cut(l, " ")
		known := false
		for _, t := range hostKeyTypes {
			known = known || typ == t
		}
		if !known || !strings.Contains(l, " ") {
			return Die("expected host key lines")
		}
		b, _, _ := strings.Cut(rest, " ")
		if !blobRE.MatchString(b) {
			return Die("unsupported host key")
		}
		if len(b) > 1000 {
			return Die("unsupported host key")
		}
		keys = append(keys, typ+" "+b)
		if len(keys) > 4 {
			return Die("too many keys")
		}
	}
	if len(keys) == 0 {
		return Die("no host key given")
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	args := append([]string{a.Home, knownHostsName(host, port)}, keys...)
	if err := c.svcRun(a, io.Discard, "svc-hosts", args...); err != nil {
		return Fail("cannot update the peer known_hosts: %s", errMsg(err))
	}
	return nil
}

func verbPeerHostkeyUnset(c *Ctx) error {
	if err := c.Need(2, 2, "peer-hostkey-unset HOST PORT"); err != nil {
		return err
	}
	host, port := c.Args[0], c.Args[1]
	if !hostOK(host) || !portOK(port) {
		return Die("unsupported host or port")
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	if err := c.svcRun(a, io.Discard, "svc-hosts", a.Home, knownHostsName(host, port)); err != nil {
		return Fail("cannot update the peer known_hosts: %s", errMsg(err))
	}
	return nil
}

func verbPeerList(c *Ctx) error {
	if err := c.Need(0, 0, "peer-list"); err != nil {
		return err
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	return c.svcRun(a, c.Stdout, "svc-list", a.Home, PeerMark)
}

// peerSSH runs ssh to the peer as the account, with its own key and the host
// keys recorded by peer-hostkey-set. Checking is strict: an unknown or changed
// host key is an error, never accepted. The result is the combined output.
func (c *Ctx) peerSSH(a *Account, host, port, user string, stdin io.Reader) ([]byte, error) {
	cmd, err := c.Command("ssh",
		"-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-i", a.Home+"/.ssh/"+peerKeyName,
		"-o", "UserKnownHostsFile="+a.Home+"/.ssh/"+peerHostsName, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes",
		"-o", "PasswordAuthentication=no", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no",
		"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=8", "-o", "LogLevel=ERROR",
		"-p", port, "--", user+"@"+host, "peer-recv")
	if err != nil {
		return nil, err
	}
	if a.UID != 0 && !c.Cfg.Unprivileged {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: []uint32{}}}
	}
	cmd.Stdin = stdin
	return cmd.CombinedOutput()
}

// firstLinesJoined is the first n lines joined with spaces (head | tr).
func firstLinesJoined(b []byte, n int) string {
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(strings.Split(strings.Join(lines, ""), "\n"), " ")
}

func firstLine(b []byte) string {
	l, _, _ := strings.Cut(strings.TrimRight(string(b), "\n"), "\n")
	return l
}

func verbPeerPing(c *Ctx) error {
	if err := c.Need(3, 3, "peer-ping HOST PORT USER"); err != nil {
		return err
	}
	if err := peerArgs(c.Args[0], c.Args[1], c.Args[2]); err != nil {
		return err
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	out, err := c.peerSSH(a, c.Args[0], c.Args[1], c.Args[2], strings.NewReader("virticle-peer 1 ping\n"))
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return err
		}
		return Fail("%s", firstLinesJoined(out, 3))
	}
	r := strings.TrimRight(string(out), "\n")
	if !strings.HasPrefix(r, "@@pong ") {
		return Fail("unexpected answer: %s", firstLine(out))
	}
	fmt.Fprintln(c.Stdout, r)
	return nil
}

type countReader struct {
	r io.Reader
	n atomic.Int64
}

func (cr *countReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n.Add(int64(n))
	return n, err
}

func verbPeerSend(c *Ctx) error {
	if err := c.Need(6, 7, "peer-send HOST PORT USER SRC DST MODE [BYTES_PER_SECOND]"); err != nil {
		return err
	}
	host, port, user, src, dst, mode := c.Args[0], c.Args[1], c.Args[2], c.Args[3], c.Args[4], c.Args[5]
	var rate uint64 // bytes per second, 0: not limited
	if len(c.Args) == 7 {
		if !numOK(c.Args[6]) {
			return Die("unsupported rate")
		}
		rate, _ = strconv.ParseUint(c.Args[6], 10, 64)
	}
	if err := peerArgs(host, port, user); err != nil {
		return err
	}
	if !PlainPath(dst) {
		return Die("unsupported destination path")
	}
	if len(mode) != 3 || strings.Trim(mode, "01234567") != "" {
		return Die("unsupported mode")
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	r, err := c.ResolveRead(src)
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", src)
	}
	f, err := r.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot open %s", src)
	}
	defer f.Close()
	size := st.Size
	body := &countReader{r: NewRateReader(context.Background(), io.LimitReader(f, size), rate)}
	stdin := io.MultiReader(strings.NewReader(fmt.Sprintf("virticle-peer 1 recv %s %d %s\n", mode, size, dst)), body)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fmt.Fprintf(c.Stdout, "@@progress %d %d\n", body.n.Load(), size)
			}
		}
	}()
	out, err := c.peerSSH(a, host, port, user, stdin)
	close(stop)
	<-stopped
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return err
		}
		return Fail("sending to %s@%s failed: %s", user, host, firstLinesJoined(out, 3))
	}
	if strings.TrimRight(string(out), "\n") != fmt.Sprintf("@@received %d", size) {
		return Fail("the peer did not confirm the file: %s", firstLine(out))
	}
	fmt.Fprintf(c.Stdout, "@@done %d\n", size)
	return nil
}

// verbPeerRecv is the forced command of a peer key. Stdin: one header line,
// then the data. A path is checked like for every other verb, and an existing
// file is never replaced.
func verbPeerRecv(c *Ctx) error {
	if err := c.Need(0, 0, "peer-recv"); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(c.Stdin, 64<<10)
	var hdr []byte
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return Die("no header")
		}
		if b == '\n' {
			break
		}
		hdr = append(hdr, b)
		if len(hdr) > 400 {
			return Die("header too long")
		}
	}
	for _, b := range hdr {
		if !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || strings.IndexByte("._+=,@/ -", b) >= 0) {
			return Die("unsupported header")
		}
	}
	f := strings.Fields(string(hdr))
	if len(f) < 3 || f[0] != "virticle-peer" || f[1] != "1" {
		return Die("unsupported header")
	}
	switch f[2] {
	case "ping":
		if len(f) != 3 {
			return Die("unsupported header")
		}
		fmt.Fprintf(c.Stdout, "@@pong %s\n", Version)
		return nil
	case "recv":
		if len(f) != 6 {
			return Die("unsupported header")
		}
		mode, sizeArg, dst := f[3], f[4], f[5]
		if len(mode) != 3 || strings.Trim(mode, "01234567") != "" {
			return Die("unsupported mode")
		}
		if !numOK(sizeArg) {
			return Die("unsupported size")
		}
		size, _ := strconv.ParseInt(sizeArg, 10, 64)
		m, _ := strconv.ParseUint(mode, 8, 32)
		r, err := c.ResolveNew(dst)
		if err != nil {
			return err
		}
		defer r.Close()
		err = c.CreateNew(r, uint32(m), func(out *os.File) error {
			n, err := io.CopyN(out, rd, size)
			if err != nil || n != size {
				return Fail("the file arrived incomplete")
			}
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(c.Stdout, "@@received %d\n", size)
		return nil
	}
	return Die("unsupported header")
}
