package nodecmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Migration certificate verbs (migtls-*) and gpu-users. The certificate
// directory is the one qemu.conf names, below /etc, reached through no
// symbolic link: every step opens the next component with O_NOFOLLOW.

func init() {
	Register("migtls-state", verbMigtlsState)
	Register("migtls-mkdir", verbMigtlsMkdir)
	Register("migtls-put", verbMigtlsPut)
	Register("migtls-discard", verbMigtlsDiscard)
	Register("migtls-commit", verbMigtlsCommit)
	Register("migtls-remove", verbMigtlsRemove)
	Register("migtls-rmdir", verbMigtlsRmdir)
	Register("gpu-users", verbGPUUsers)
}

const (
	legacyMarker = ".virt-proj-migration.json"
	backupSuffix = ".virticle-orig"
	tmpSuffix    = ".virticle-tmp"
)

// migFiles are the files of the migration setup. No other name is touched.
var migFiles = []string{"server-key.pem", "client-key.pem", "ca-cert.pem", "server-cert.pem", "client-cert.pem", ".virticle-migration.json"}

func knownName(n string, legacy bool) bool {
	for _, f := range migFiles {
		if n == f {
			return true
		}
	}
	return legacy && n == legacyMarker
}

// confValue is the last value of key in qemu.conf, quoted or not.
func (c *Ctx) confValue(key string) string {
	b, err := os.ReadFile(c.Cfg.Prefix + "/etc/libvirt/qemu.conf")
	if err != nil {
		return ""
	}
	quoted := regexp.MustCompile(`^[ \t]*` + key + `[ \t]*=[ \t]*"([^"]*)".*`)
	bare := regexp.MustCompile(`^[ \t]*` + key + `[ \t]*=[ \t]*([^"\s#][^\s#]*).*`)
	val := ""
	for _, l := range strings.Split(string(b), "\n") {
		if m := quoted.FindStringSubmatch(l); m != nil {
			val = m[1]
		} else if m := bare.FindStringSubmatch(l); m != nil {
			val = m[1]
		}
	}
	return val
}

// CertDir is the directory libvirt hands QEMU for migration.
func (c *Ctx) CertDir() string {
	d := c.confValue("migrate_tls_x509_cert_dir")
	if d == "" {
		d = c.confValue("default_tls_x509_cert_dir")
	}
	if d == "" {
		d = "/etc/pki/qemu"
	}
	return d
}

func belowEtc(p string) bool { return strings.HasPrefix(p, "/etc/") && len(p) > 5 }

// needCertDir: dir must be the configured certificate directory, below /etc.
func (c *Ctx) needCertDir(dir string) error {
	if !PlainPath(dir) {
		return Die("unsupported directory")
	}
	if !belowEtc(dir) {
		return Die("the directory must be below /etc")
	}
	if dir != c.CertDir() {
		return Die("%s is not the certificate directory named in qemu.conf", dir)
	}
	return nil
}

// walk opens the directories of path (absolute, below Cfg.Prefix) one by one
// without following symbolic links. depth is the number of components that
// exist; fd is the last one opened (the whole path when depth == the number of
// components). With mkdir the missing ones are created (mode 0755). A
// component that is a symbolic link or not a directory is refused.
func (c *Ctx) walk(path string, mkdir bool) (fd, depth, total int, err error) {
	root := c.Cfg.Prefix
	if root == "" {
		root = "/"
	}
	fd, err = unix.Open(root, oDir&^unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, 0, 0, Fail("cannot open %s", root)
	}
	comps := strings.Split(strings.TrimPrefix(path, "/"), "/")
	total = len(comps)
	for i, comp := range comps {
		nfd, err := openat(fd, comp, oDir, 0)
		if err != nil && isNotExist(err) && mkdir {
			if merr := unix.Mkdirat(fd, comp, 0o755); merr != nil && !errIs(merr, unix.EEXIST) {
				unix.Close(fd)
				return -1, i, total, Fail("cannot create %s", path)
			}
			nfd, err = openat(fd, comp, oDir, 0)
			if err == nil {
				_ = unix.Fchmod(nfd, 0o755)
			}
		}
		if err != nil {
			if isNotExist(err) {
				return fd, i, total, nil
			}
			unix.Close(fd)
			return -1, i, total, Die("%s is, or lies below, a symbolic link", path)
		}
		unix.Close(fd)
		fd = nfd
	}
	return fd, total, total, nil
}

// needDirExists opens the certificate directory, which must exist, be owned by
// root and not be writable by anybody else.
func (c *Ctx) needDirExists(dir string) (int, error) {
	fd, depth, total, err := c.walk(dir, false)
	if err != nil {
		return -1, err
	}
	if depth != total {
		unix.Close(fd)
		return -1, Die("%s does not exist", dir)
	}
	if !c.Cfg.Unprivileged {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil || st.Uid != 0 {
			unix.Close(fd)
			return -1, Die("%s is not owned by root", dir)
		}
		if st.Mode&0o022 != 0 {
			unix.Close(fd)
			return -1, Die("%s is writable by others than root", dir)
		}
	}
	return fd, nil
}

func verbMigtlsState(c *Ctx) error {
	if err := c.Need(1, 1, "migtls-state DIR"); err != nil {
		return err
	}
	dir := c.Args[0]
	if err := c.needCertDir(dir); err != nil {
		return err
	}
	fd, depth, total, err := c.walk(dir, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	comps := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	for i := range comps {
		ok := 0
		if i < depth {
			ok = 1
		}
		fmt.Fprintf(c.Stdout, "@@dir /%s %d\n", strings.Join(comps[:i+1], "/"), ok)
	}
	full := depth == total
	// Symbolic links in the directory are followed only when only root can
	// have put them there.
	follow := false
	if full {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && (c.Cfg.Unprivileged || (st.Uid == 0 && st.Mode&0o022 == 0)) {
			follow = true
		}
	}
	open := func(name string) *os.File {
		if !full {
			return nil
		}
		flags := unix.O_RDONLY
		if !follow {
			flags |= unix.O_NOFOLLOW
		}
		ffd, err := openat(fd, name, flags, 0)
		if err != nil {
			return nil
		}
		return os.NewFile(uintptr(ffd), name)
	}
	names := append(append([]string{}, migFiles...), legacyMarker)
	for _, name := range names {
		f := open(name)
		if f == nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(int(f.Fd()), &st) == nil && isRegMode(st.Mode) {
			h := sha256.New()
			io.Copy(h, f)
			fmt.Fprintf(c.Stdout, "@@file %s %s %s:%s %o\n", name, hex.EncodeToString(h.Sum(nil)), userName(st.Uid), groupName(st.Gid), st.Mode&0o7777)
		}
		f.Close()
	}
	for _, p := range [][2]string{{"ca-cert.pem", "ca"}, {"server-cert.pem", "server"}, {"client-cert.pem", "client"}, {".virticle-migration.json", "marker"}, {legacyMarker, "oldmarker"}} {
		fmt.Fprintf(c.Stdout, "@@content %s\n", p[1])
		if f := open(p[0]); f != nil {
			b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
			f.Close()
			c.Stdout.Write(b)
			if len(b) > 0 && b[len(b)-1] != '\n' {
				fmt.Fprintln(c.Stdout)
			}
		}
	}
	fmt.Fprintln(c.Stdout, "@@content ls")
	if full {
		if dfd, err := unix.Dup(fd); err == nil {
			d := os.NewFile(uintptr(dfd), dir)
			if ents, err := d.Readdirnames(-1); err == nil {
				sort.Strings(ents)
				for _, e := range ents {
					fmt.Fprintln(c.Stdout, e)
				}
			}
			d.Close()
		}
	}
	fmt.Fprintln(c.Stdout, "@@end")
	return nil
}

func userName(uid uint32) string {
	if u, err := user.LookupId(strconv.Itoa(int(uid))); err == nil {
		return u.Username
	}
	return "UNKNOWN"
}

func groupName(gid uint32) string {
	if g, err := user.LookupGroupId(strconv.Itoa(int(gid))); err == nil {
		return g.Name
	}
	return "UNKNOWN"
}

func verbMigtlsMkdir(c *Ctx) error {
	if err := c.Need(1, 1, "migtls-mkdir DIR"); err != nil {
		return err
	}
	if err := c.needCertDir(c.Args[0]); err != nil {
		return err
	}
	fd, _, _, err := c.walk(c.Args[0], true)
	if err != nil {
		if e, ok := err.(*Error); ok && e.Code == 1 {
			return Die("cannot create %s", c.Args[0])
		}
		return err
	}
	unix.Close(fd)
	return nil
}

var ownerRE = regexp.MustCompile(`^(0|[0-9]+|[a-z_][a-z0-9_-]*):0$`)

func lookupUID(name string) (uint32, bool) {
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(n), true
	}
	if u, err := user.Lookup(name); err == nil {
		n, err := strconv.ParseUint(u.Uid, 10, 32)
		return uint32(n), err == nil
	}
	return 0, false
}

func verbMigtlsPut(c *Ctx) error {
	if err := c.Need(4, 4, "migtls-put DIR NAME OWNER MODE"); err != nil {
		return err
	}
	dir, name, owner, modeArg := c.Args[0], c.Args[1], c.Args[2], c.Args[3]
	if err := c.needCertDir(dir); err != nil {
		return err
	}
	fd, err := c.needDirExists(dir)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if !knownName(name, false) {
		return Die("unsupported file name")
	}
	var mode uint32
	if strings.HasSuffix(name, "-key.pem") {
		if modeArg != "0400" {
			return Die("private keys must have mode 0400")
		}
		mode = 0o400
	} else {
		if modeArg != "0644" {
			return Die("certificates must have mode 0644")
		}
		mode = 0o644
	}
	if !ownerRE.MatchString(owner) {
		return Die("unsupported owner")
	}
	uid, ok := lookupUID(strings.TrimSuffix(owner, ":0"))
	if !ok && !c.Cfg.Unprivileged {
		return Die("unsupported owner")
	}
	tmp := name + tmpSuffix
	if st, err := fstatat(fd, tmp); err == nil && isLinkMode(st.Mode) {
		return Die("%s/%s is a symbolic link", dir, tmp)
	}
	_ = unix.Unlinkat(fd, tmp, 0)
	tfd, err := openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return Die("cannot write %s/%s", dir, tmp)
	}
	f := os.NewFile(uintptr(tfd), tmp)
	remove := func() { _ = unix.Unlinkat(fd, tmp, 0) }
	cancel := c.Cleanup(remove)
	defer cancel()
	n, err := io.Copy(f, io.LimitReader(c.Stdin, 65537))
	if err != nil {
		f.Close()
		remove()
		return Die("cannot write %s/%s", dir, tmp)
	}
	if n > 65536 {
		f.Close()
		remove()
		return Die("input too large")
	}
	if !c.Cfg.Unprivileged {
		if err := f.Chown(int(uid), 0); err != nil {
			f.Close()
			remove()
			return Die("chown failed")
		}
	}
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		f.Close()
		remove()
		return Die("chmod failed")
	}
	if err := f.Close(); err != nil {
		remove()
		return Die("cannot write %s/%s", dir, tmp)
	}
	return nil
}

func verbMigtlsDiscard(c *Ctx) error {
	if err := c.Need(1, 1, "migtls-discard DIR"); err != nil {
		return err
	}
	if err := c.needCertDir(c.Args[0]); err != nil {
		return err
	}
	fd, depth, total, err := c.walk(c.Args[0], false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if depth == total {
		for _, f := range migFiles {
			_ = unix.Unlinkat(fd, f+tmpSuffix, 0)
		}
	}
	return nil
}

func verbMigtlsCommit(c *Ctx) error {
	if err := c.Need(1, -1, "migtls-commit DIR [backup:NAME]... [rm-legacy] [restorecon]"); err != nil {
		return err
	}
	dir := c.Args[0]
	if err := c.needCertDir(dir); err != nil {
		return err
	}
	fd, err := c.needDirExists(dir)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var backups []string
	legacy, restore := false, false
	for _, a := range c.Args[1:] {
		switch {
		case strings.HasPrefix(a, "backup:"):
			n := strings.TrimPrefix(a, "backup:")
			if !knownName(n, true) {
				return Die("unsupported file name")
			}
			backups = append(backups, n)
		case a == "rm-legacy":
			legacy = true
		case a == "restorecon":
			restore = true
		default:
			return Die("unsupported argument")
		}
	}
	for _, f := range migFiles {
		st, err := fstatat(fd, f+tmpSuffix)
		if err != nil || !isRegMode(st.Mode) {
			return Die("%s%s is missing", f, tmpSuffix)
		}
	}
	for _, n := range backups {
		if _, err := fstatat(fd, n+backupSuffix); err == nil {
			return Die("%s%s already exists", n, backupSuffix)
		}
	}
	for _, n := range backups {
		if _, err := fstatat(fd, n); err == nil {
			if err := unix.Renameat(fd, n, fd, n+backupSuffix); err != nil {
				return Die("cannot back up %s", n)
			}
		}
	}
	for _, f := range migFiles {
		if err := unix.Renameat(fd, f+tmpSuffix, fd, f); err != nil {
			return Die("cannot move %s into place", f)
		}
	}
	if legacy {
		_ = unix.Unlinkat(fd, legacyMarker, 0)
	}
	if restore && !c.Cfg.Unprivileged && c.HasTool("restorecon") {
		if cmd, err := c.Command("restorecon", "-R", dir); err == nil {
			_ = cmd.Run()
		}
	}
	return nil
}

func verbMigtlsRemove(c *Ctx) error {
	if err := c.Need(1, -1, "migtls-remove DIR (rm:NAME|restore:NAME)..."); err != nil {
		return err
	}
	dir := c.Args[0]
	if err := c.needCertDir(dir); err != nil {
		return err
	}
	fd, depth, total, err := c.walk(dir, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	for _, a := range c.Args[1:] {
		if !strings.HasPrefix(a, "rm:") && !strings.HasPrefix(a, "restore:") {
			return Die("unsupported argument")
		}
		_, n, _ := strings.Cut(a, ":")
		if !knownName(n, true) {
			return Die("unsupported file name")
		}
	}
	for _, a := range c.Args[1:] {
		op, n, _ := strings.Cut(a, ":")
		switch op {
		case "rm":
			if depth == total {
				if err := unix.Unlinkat(fd, n, 0); err != nil && !isNotExist(err) {
					return Fail("cannot remove %s: %v", n, err)
				}
			}
		case "restore":
			if depth != total {
				return Die("%s%s does not exist", n, backupSuffix)
			}
			if _, err := fstatat(fd, n+backupSuffix); err != nil {
				return Die("%s%s does not exist", n, backupSuffix)
			}
			if err := unix.Renameat(fd, n+backupSuffix, fd, n); err != nil {
				return Die("cannot restore %s", n)
			}
		}
	}
	return nil
}

// verbMigtlsRmdir removes TARGET (empty) when it is DIR or one of its parents
// below /etc; the setup creates exactly those.
func verbMigtlsRmdir(c *Ctx) error {
	if err := c.Need(2, 2, "migtls-rmdir DIR TARGET"); err != nil {
		return err
	}
	dir, target := c.Args[0], c.Args[1]
	if err := c.needCertDir(dir); err != nil {
		return err
	}
	if !PlainPath(target) {
		return Die("unsupported directory")
	}
	if !belowEtc(target) {
		return Die("the directory must be below /etc")
	}
	if !strings.HasPrefix(dir+"/", target+"/") {
		return Die("%s is not the certificate directory or one of its parents", target)
	}
	par, base := splitPath(target)
	fd, depth, total, err := c.walk(par, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if depth != total {
		return Die("cannot remove %s: it does not exist", target)
	}
	if err := unix.Unlinkat(fd, base, unix.AT_REMOVEDIR); err != nil {
		if errIs(err, unix.ENOTEMPTY) || errIs(err, unix.EEXIST) {
			return Die("%s is not empty", target)
		}
		return Die("cannot remove %s: %v", target, err)
	}
	return nil
}

var pciRE = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// verbGPUUsers prints the names of the processes that hold a graphics card
// open, then "ok".
func verbGPUUsers(c *Ctx) error {
	if err := c.Need(1, 1, "gpu-users PCIADDR"); err != nil {
		return err
	}
	a := c.Args[0]
	if !pciRE.MatchString(a) {
		return Die("unsupported PCI address")
	}
	dev := c.Cfg.sys() + "/bus/pci/devices/" + a
	exact := map[string]bool{}
	for _, pat := range []string{"card*", "renderD*"} {
		m, _ := globNames(dev+"/drm", pat)
		for _, n := range m {
			exact["/dev/dri/"+n] = true
		}
	}
	nvidia := false
	if l, err := os.Readlink(dev + "/driver"); err == nil && l[strings.LastIndexByte(l, '/')+1:] == "nvidia" {
		nvidia = true
	}
	if len(exact) > 0 || nvidia {
		ents, _ := os.ReadDir(c.Cfg.proc())
		var pids []string
		for _, e := range ents {
			if n := e.Name(); n != "" && n[0] >= '0' && n[0] <= '9' {
				pids = append(pids, n)
			}
		}
		sort.Strings(pids)
		for _, pid := range pids {
			fds, err := os.ReadDir(c.Cfg.proc() + "/" + pid + "/fd")
			if err != nil {
				continue
			}
			hit := false
			for _, fe := range fds {
				t, err := os.Readlink(c.Cfg.proc() + "/" + pid + "/fd/" + fe.Name())
				if err != nil {
					continue
				}
				if exact[t] || (nvidia && strings.Contains(t, "/dev/nvidia")) {
					hit = true
					break
				}
			}
			if hit {
				if b, err := os.ReadFile(c.Cfg.proc() + "/" + pid + "/comm"); err == nil {
					out := bufio.NewReader(strings.NewReader(string(b)))
					l, _ := out.ReadString('\n')
					fmt.Fprintln(c.Stdout, strings.TrimSuffix(l, "\n"))
				}
			}
		}
	}
	fmt.Fprintln(c.Stdout, "ok")
	return nil
}

// globNames lists the names in dir that match pattern (path.Match syntax).
func globNames(dir, pattern string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if ok, _ := pathMatch(pattern, e.Name()); ok {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
