package nodecmd

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Path confinement. The rules are the ones the old shell script had:
//
//   - a path is absolute, made of the characters A-Za-z0-9._+=,@/-, has no
//     empty, "." or ".." component, no trailing slash and at most 200 bytes
//     (PlainPath);
//   - it must lie inside an allowed root: the target directory of a libvirt
//     storage pool of type dir, fs or netfs (minus the denied system
//     directories), or the UEFI NVRAM directory (Roots);
//   - ResolveRead follows symbolic links, but the place they lead to must be
//     inside a root; ResolveNew and ResolveMod canonicalise the parent
//     directory the same way;
//   - ResolveNew never names something that exists, ResolveMod refuses a final
//     symbolic link and a pool directory itself.
//
// The check is not a check-then-use. A Ref holds an open descriptor of the
// parent directory, taken with O_NOFOLLOW and compared with the place the
// kernel reports for it (/proc/self/fd), so it is exactly the directory that
// was found inside the roots. The verbs then work relative to that descriptor
// with O_NOFOLLOW, never by path again.

var plainRE = regexp.MustCompile(`^[A-Za-z0-9._+=,@/-]+$`)

// MaxPathLen is the longest path a verb accepts.
const MaxPathLen = 200

// PlainPath reports whether p is an acceptable path (see above).
func PlainPath(p string) bool {
	if len(p) > MaxPathLen || p == "/" || !strings.HasPrefix(p, "/") || !plainRE.MatchString(p) {
		return false
	}
	for _, comp := range strings.Split(p[1:], "/") {
		if comp == "" || comp == "." || comp == ".." {
			return false
		}
	}
	return true
}

// deniedTrees are system directories that are never a root, with everything
// below them; deniedExact are never a root themselves (a directory above a
// denied place must not be one either, see DeniedRoot).
var deniedTrees = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr",
	"/var/spool", "/var/log", "/var/lib/dpkg", "/var/lib/rpm", "/var/lib/sudo", "/var/lib/virticle",
	"/var/lib/libvirt/qemu", "/var/lib/libvirt/secrets", "/var/lib/libvirt/swtpm", "/var/lib/libvirt/network",
	"/var/lib/libvirt/dnsmasq", "/var/lib/libvirt/lockd", "/var/lib/libvirt/sanlock",
}

var deniedExact = []string{"/", "/home", "/var", "/var/lib", "/var/lib/libvirt"}

// DeniedRoot reports whether the canonical directory p may not be an allowed
// root. A storage pool may point anywhere, and a member of the libvirt group
// can define one, so these places are excluded.
func DeniedRoot(p string) bool {
	for _, d := range deniedExact {
		if p == d {
			return true
		}
	}
	for _, d := range deniedTrees {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// Roots returns the canonical allowed roots, in the order found: pool
// directories first, then the NVRAM directory. It is computed once per run.
func (c *Ctx) Roots() []string {
	c.sh.mu.Lock()
	if c.sh.rootsDone {
		defer c.sh.mu.Unlock()
		return c.sh.roots
	}
	c.sh.mu.Unlock()
	var roots []string
	add := func(d string, checkDenied bool) {
		if !PlainPath(d) {
			return
		}
		canon, err := filepath.EvalSymlinks(d)
		if err != nil {
			return
		}
		if fi, err := os.Stat(canon); err != nil || !fi.IsDir() {
			return
		}
		if checkDenied && DeniedRoot(canon) {
			return
		}
		for _, r := range roots {
			if r == canon {
				return
			}
		}
		roots = append(roots, canon)
	}
	if c.Cfg.Pools != nil {
		for _, d := range c.Cfg.Pools(c) {
			add(d, true)
		}
	}
	if c.Cfg.NVRAMDir != "" {
		add(c.Cfg.NVRAMDir, false)
	}
	c.sh.mu.Lock()
	c.sh.roots, c.sh.rootsDone = roots, true
	c.sh.mu.Unlock()
	return roots
}

// InRoots reports whether the canonical path p is a root or lies below one.
func (c *Ctx) InRoots(p string) bool {
	for _, r := range c.Roots() {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// RootOrAbove reports whether the canonical path p is a root or a directory
// above one.
func (c *Ctx) RootOrAbove(p string) bool {
	for _, r := range c.Roots() {
		if r == p || strings.HasPrefix(r, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

var poolPathRE = regexp.MustCompile(`(?m)^[ \t]*<path>(/[^<]*)</path>`)

// virshPools asks libvirt for the target directories of the pools of type
// dir, fs and netfs.
func virshPools(c *Ctx) []string {
	if !c.HasTool("virsh") {
		return nil
	}
	out, err := c.Output("virsh", "-q", "--connect", "qemu:///system", "pool-list", "--all", "--name")
	if err != nil {
		return nil
	}
	var dirs []string
	for _, name := range strings.Split(string(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		x, err := c.Output("virsh", "--connect", "qemu:///system", "pool-dumpxml", "--pool", name)
		if err != nil {
			continue
		}
		s := string(x)
		if !strings.HasPrefix(s, "<pool type='dir'") && !strings.HasPrefix(s, "<pool type='fs'") && !strings.HasPrefix(s, "<pool type='netfs'") {
			continue
		}
		if m := poolPathRE.FindStringSubmatch(s); m != nil {
			dirs = append(dirs, m[1])
		}
	}
	return dirs
}

// Ref is a confined name: an open descriptor of its parent directory plus the
// last path component. Close it when done.
type Ref struct {
	Arg   string // the path as the caller gave it, for messages
	Path  string // canonical absolute path (the name may not exist)
	Dir   string // canonical parent directory
	Name  string // last component
	dirfd int
}

// Close releases the parent directory descriptor.
func (r *Ref) Close() {
	if r.dirfd >= 0 {
		unix.Close(r.dirfd)
		r.dirfd = -1
	}
}

// DirFD is the descriptor of the parent directory, for *at system calls.
func (r *Ref) DirFD() int { return r.dirfd }

// Lstat is the stat of the name without following a final symbolic link.
// exists is false when there is no such name.
func (r *Ref) Lstat() (st unix.Stat_t, exists bool, err error) {
	st, err = fstatat(r.dirfd, r.Name)
	if err != nil {
		if isNotExist(err) {
			return st, false, nil
		}
		return st, false, err
	}
	return st, true, nil
}

// Open opens the name (O_NOFOLLOW is added) for reading or whatever flags say.
func (r *Ref) Open(flags int, perm uint32) (*os.File, error) {
	fd, err := openat(r.dirfd, r.Name, flags|unix.O_NOFOLLOW, perm)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), r.Path), nil
}

// OpenDir opens the name as a directory and returns its descriptor.
func (r *Ref) OpenDir() (int, error) { return openat(r.dirfd, r.Name, oDir, 0) }

// Unlink removes the name (not a directory).
func (r *Ref) Unlink() error { return unix.Unlinkat(r.dirfd, r.Name, 0) }

// openVerifiedDir opens the canonical directory canon and makes sure it is
// still that place.
func openVerifiedDir(canon string) (int, error) {
	fd, err := unix.Open(canon, oDir, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return -1, Die("%s is not a directory", canon)
		}
		return -1, Die("cannot open %s: %v", canon, err)
	}
	got, err := fdPath(fd)
	if err != nil || got != canon {
		unix.Close(fd)
		return -1, Die("%s changed while it was being checked", canon)
	}
	return fd, nil
}

func splitPath(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	return p[:i], p[i+1:]
}

// parentRef canonicalises the parent directory of arg, requires it to be
// inside the roots and opens it.
func (c *Ctx) parentRef(arg string) (*Ref, error) {
	if !PlainPath(arg) {
		return nil, Die("unsupported path")
	}
	par, name := splitPath(arg)
	if par == "" {
		return nil, Die("unsupported path")
	}
	canon, err := filepath.EvalSymlinks(par)
	if err != nil {
		return nil, Die("the directory of %s does not exist", arg)
	}
	if !c.InRoots(canon) {
		return nil, Die("%s is not inside a storage pool directory", arg)
	}
	fd, err := openVerifiedDir(canon)
	if err != nil {
		return nil, Die("the directory of %s is not usable: %s", arg, errMsg(err))
	}
	return &Ref{Arg: arg, Path: canon + "/" + name, Dir: canon, Name: name, dirfd: fd}, nil
}

func errMsg(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return err.Error()
}

// ResolveRead is for a path that is read: it may exist or not. An existing
// path is resolved through symbolic links and must end up inside the roots;
// a missing one must have its parent directory inside the roots.
func (c *Ctx) ResolveRead(arg string) (*Ref, error) {
	if !PlainPath(arg) {
		return nil, Die("unsupported path")
	}
	if _, err := os.Lstat(arg); err == nil {
		full, err := filepath.EvalSymlinks(arg)
		if err != nil {
			return nil, Die("cannot resolve %s", arg)
		}
		if !c.InRoots(full) {
			return nil, Die("%s is not inside a storage pool directory", arg)
		}
		dir, name := path.Dir(full), path.Base(full)
		fd, err := openVerifiedDir(dir)
		if err != nil {
			return nil, Die("cannot resolve %s: %s", arg, errMsg(err))
		}
		return &Ref{Arg: arg, Path: full, Dir: dir, Name: name, dirfd: fd}, nil
	}
	return c.parentRef(arg)
}

// ResolveNew is for a name that is about to be created: its parent must be
// inside the roots, and nothing may exist under that name (not even a
// symbolic link). Creating must still use an exclusive call (CreateNew,
// Mkdirat, Linkat), the check here only gives the early, readable error.
func (c *Ctx) ResolveNew(arg string) (*Ref, error) {
	r, err := c.parentRef(arg)
	if err != nil {
		return nil, err
	}
	if _, exists, err := r.Lstat(); err != nil || exists {
		r.Close()
		if err != nil {
			return nil, Fail("cannot look at %s: %v", arg, err)
		}
		return nil, Fail("%s already exists", arg)
	}
	return r, nil
}

// ResolveMod is for a name that is changed or removed: its parent must be
// inside the roots, the name must not be a symbolic link and not be a pool
// directory or a directory above one. The name may be missing.
func (c *Ctx) ResolveMod(arg string) (*Ref, error) {
	r, err := c.parentRef(arg)
	if err != nil {
		return nil, err
	}
	st, exists, err := r.Lstat()
	if err != nil {
		r.Close()
		return nil, Fail("cannot look at %s: %v", arg, err)
	}
	if exists && isLinkMode(st.Mode) {
		r.Close()
		return nil, Die("%s is a symbolic link", arg)
	}
	if c.RootOrAbove(r.Path) {
		r.Close()
		return nil, Die("%s is a storage pool directory", arg)
	}
	return r, nil
}

// PartName is the name of the hidden part file CreateNew uses for r.
func (r *Ref) PartName() string { return r.Name + ".virticle-part." + strconv.Itoa(os.Getpid()) }

// CreateNew makes r's name appear with the content fill writes, atomically and
// without replacing anything: fill writes into a part file next to it (mode
// 0600, created exclusively), the part file gets mode, and is then hard linked
// to the final name, which fails when the name has appeared meanwhile. The
// part file is removed in every case, also when the process is killed. Errors
// from fill are returned as they are.
func (c *Ctx) CreateNew(r *Ref, mode uint32, fill func(f *os.File) error) error {
	part := r.PartName()
	fd, err := openat(r.dirfd, part, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return Fail("cannot write %s: %v", r.Arg, err)
	}
	f := os.NewFile(uintptr(fd), part)
	dirfd := r.dirfd
	remove := func() { _ = unix.Unlinkat(dirfd, part, 0) }
	cancel := c.Cleanup(remove)
	defer cancel()
	defer remove()
	if err := fill(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		f.Close()
		return Fail("chmod failed: %v", err)
	}
	if err := f.Close(); err != nil {
		return Fail("cannot write %s: %v", r.Arg, err)
	}
	if err := unix.Linkat(dirfd, part, dirfd, r.Name, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return Fail("%s appeared meanwhile; nothing was overwritten", r.Arg)
		}
		return Fail("cannot create %s: %v", r.Arg, err)
	}
	return nil
}

// CopySparse copies size bytes of src to dst, skipping the holes of src so a
// sparse disk image stays sparse, and sets the length of dst.
func CopySparse(dst, src *os.File, size int64) error {
	buf := make([]byte, 1<<20)
	var off int64
	for off < size {
		data, err := src.Seek(off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
				return copyPlain(dst, src, size, buf)
			}
			return err
		}
		hole, err := src.Seek(data, unix.SEEK_HOLE)
		if err != nil || hole > size {
			hole = size
		}
		if _, err := io.CopyBuffer(io.NewOffsetWriter(dst, data), io.NewSectionReader(src, data, hole-data), buf); err != nil {
			return err
		}
		off = hole
	}
	return dst.Truncate(size)
}

func copyPlain(dst, src *os.File, size int64, buf []byte) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.CopyBuffer(io.NewOffsetWriter(dst, 0), io.LimitReader(src, size), buf); err != nil {
		return err
	}
	return dst.Truncate(size)
}

// qcow2 header: magic, backing file offset (u64 at 8) and size (u32 at 16).
const qcow2Magic = 0x514649fb

// HeaderBacking returns the backing file name a qcow2 file records ("" for
// other files or none). It fails for a header that cannot be followed.
func HeaderBacking(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h [20]byte
	n, _ := io.ReadFull(f, h[:])
	if n < 4 || binary.BigEndian.Uint32(h[:4]) != qcow2Magic {
		return "", nil
	}
	if n < 20 {
		return "", nil
	}
	off := binary.BigEndian.Uint64(h[8:16])
	ln := binary.BigEndian.Uint32(h[16:20])
	if off>>63 != 0 {
		return "", errors.New("bad backing file offset")
	}
	if ln == 0 || off == 0 {
		return "", nil
	}
	if ln > 1023 || off > 1048576 {
		return "", errors.New("backing file name out of range")
	}
	b := make([]byte, ln)
	m, _ := f.ReadAt(b, int64(off))
	return string(b[:m]), nil
}

// IsQcow2 reports whether the file starts with the qcow2 magic.
func IsQcow2(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	var h [4]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return false
	}
	return binary.BigEndian.Uint32(h[:]) == qcow2Magic
}

// CheckChain checks that every backing file of the qcow2 image file (a
// canonical path) lies inside the roots, so qemu-img cannot be made to open
// anything else. A chain of more than 32 files is refused.
func (c *Ctx) CheckChain(file string) error {
	cur := file
	for n := 1; ; n++ {
		bn, err := HeaderBacking(cur)
		if err != nil {
			return Die("cannot follow the backing chain of %s", file)
		}
		if bn == "" {
			return nil
		}
		bp := bn
		if !strings.HasPrefix(bn, "/") {
			bp = path.Dir(cur) + "/" + bn
		}
		if !PlainPath(bp) {
			return Die("the backing file of %s has an unsupported name", cur)
		}
		if fi, err := os.Stat(bp); err != nil || !fi.Mode().IsRegular() {
			return Fail("the backing file %s of %s does not exist", bp, cur)
		}
		canon, err := filepath.EvalSymlinks(bp)
		if err != nil {
			return Fail("cannot resolve the backing file %s", bp)
		}
		if !c.InRoots(canon) {
			return Die("the backing file %s of %s is outside the storage pool directories", bp, cur)
		}
		cur = canon
		if n > 32 {
			return Die("backing chain too long")
		}
	}
}
