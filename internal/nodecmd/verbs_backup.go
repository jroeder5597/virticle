package nodecmd

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The backup verbs: df, files-size, backup-scan, tar-pack, tar-unpack,
// convert-start, convert-poll, convert-kill, convert-finish, and convert-run,
// the detached half of convert-start (it takes the same arguments and checks
// them again, so running it by hand is the same as convert-start in the
// foreground).

func init() {
	Register("df", verbDF)
	Register("files-size", verbFilesSize)
	Register("backup-scan", verbBackupScan)
	Register("tar-pack", verbTarPack)
	Register("tar-unpack", verbTarUnpack)
	Register("convert-start", verbConvertStart)
	Register("convert-run", verbConvertRun)
	Register("convert-poll", verbConvertPoll)
	Register("convert-kill", verbConvertKill)
	Register("convert-finish", verbConvertFinish)
}

var (
	plainNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	markerRE    = regexp.MustCompile(`^@@@[0-9a-f]{16}$`)
	scanNameRE  = regexp.MustCompile(`^[A-Za-z0-9._+=,@-]+$`)
)

// nameOK: a plain file name (no slash, no leading dot or dash, at most 120).
func nameOK(s string) bool {
	return plainNameRE.MatchString(s) && s[0] != '-' && s[0] != '.' && len(s) <= 120
}

func checkNames(names []string) error {
	for _, n := range names {
		if !nameOK(n) {
			return Die("unsupported file name")
		}
	}
	return nil
}

// openDirArg resolves arg (links followed inside the roots) and opens it as a
// directory. The caller closes both.
func openDirArg(c *Ctx, arg string) (*Ref, int, error) {
	r, err := c.ResolveRead(arg)
	if err != nil {
		return nil, -1, err
	}
	fd, err := r.OpenDir()
	if err != nil {
		r.Close()
		return nil, -1, Fail("%s is not a directory", arg)
	}
	return r, fd, nil
}

// workDirArg resolves arg as an existing work directory (.virticle-*) that is
// not a link, and opens it. The caller closes both.
func workDirArg(c *Ctx, arg string) (*Ref, int, error) {
	r, err := c.ResolveMod(arg)
	if err != nil {
		return nil, -1, err
	}
	st, exists, _ := r.Lstat()
	if !exists || !isDirMode(st.Mode) {
		r.Close()
		return nil, -1, Die("%s is not a directory", arg)
	}
	if !WorkName(r.Path) {
		r.Close()
		return nil, -1, Die("%s is not a work directory", arg)
	}
	fd, err := r.OpenDir()
	if err != nil {
		r.Close()
		return nil, -1, Die("%s is not a directory", arg)
	}
	return r, fd, nil
}

// regularAt reports the size of name below dirfd when it is a regular file
// and not a link.
func regularAt(dirfd int, name string) (int64, bool) {
	st, err := fstatat(dirfd, name)
	if err != nil || !isRegMode(st.Mode) {
		return 0, false
	}
	return st.Size, true
}

func verbDF(c *Ctx) error {
	if err := c.Need(1, 1, "df DIR"); err != nil {
		return err
	}
	r, fd, err := openDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	defer unix.Close(fd)
	var sf unix.Statfs_t
	if err := unix.Fstatfs(fd, &sf); err != nil {
		return Fail("cannot get the free space of %s: %v", c.Args[0], err)
	}
	fmt.Fprintln(c.Stdout, uint64(sf.Bavail)*uint64(sf.Bsize))
	return nil
}

// files-size DIR NAME...: per name the size of the regular file DIR/NAME (not
// a link), or MISSING.
func verbFilesSize(c *Ctx) error {
	if err := c.Need(2, 41, "files-size DIR NAME..."); err != nil {
		return err
	}
	r, fd, err := openDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	defer unix.Close(fd)
	if err := checkNames(c.Args[1:]); err != nil {
		return err
	}
	for _, n := range c.Args[1:] {
		if size, ok := regularAt(fd, n); ok {
			fmt.Fprintln(c.Stdout, size)
		} else {
			fmt.Fprintln(c.Stdout, "MISSING")
		}
	}
	return nil
}

const (
	scanMaxBackups  = 500
	scanMaxManifest = 64 * 1024
	// find -size -64k: under 64 blocks of 1 KiB, rounded up.
	scanManifestLimit = 63 * 1024
)

// backup-scan DIR MARKER: per backup (DIR/*.vmbackup/manifest.json) a section:
// "MARKER DIR/NAME", the size of the backup directory in bytes, the manifest
// (at most 64 KiB), a newline. At most 500 backups.
func verbBackupScan(c *Ctx) error {
	if err := c.Need(2, 2, "backup-scan DIR MARKER"); err != nil {
		return err
	}
	if !markerRE.MatchString(c.Args[1]) {
		return Die("unsupported marker")
	}
	r, fd, err := openDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	defer unix.Close(fd)
	names, _ := listDir(fd)
	sort.Strings(names)
	count := 0
	for _, name := range names {
		if count >= scanMaxBackups {
			break
		}
		if !strings.HasSuffix(name, ".vmbackup") || !scanNameRE.MatchString(name) {
			continue
		}
		st, err := fstatat(fd, name)
		if err != nil || !isDirMode(st.Mode) {
			continue
		}
		bfd, err := openat(fd, name, oDir, 0)
		if err != nil {
			continue
		}
		if manifest, ok := readManifest(bfd); ok {
			count++
			fmt.Fprintf(c.Stdout, "%s %s/%s\n%d\n", c.Args[1], c.Args[0], name, duBytes(bfd, map[[2]uint64]bool{}, 0))
			c.Stdout.Write(manifest)
			fmt.Fprintln(c.Stdout)
		}
		unix.Close(bfd)
	}
	return nil
}

// listDir lists the names in the directory fd refers to (on a descriptor of
// its own, fd stays usable).
func listDir(fd int) ([]string, error) {
	nfd, err := openat(fd, ".", oDir, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(nfd), "dir")
	defer f.Close()
	return f.Readdirnames(-1)
}

func readManifest(bfd int) ([]byte, bool) {
	size, ok := regularAt(bfd, "manifest.json")
	if !ok || size > scanManifestLimit {
		return nil, false
	}
	mfd, err := openat(bfd, "manifest.json", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(mfd), "manifest.json")
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, scanMaxManifest))
	return b, err == nil
}

// duBytes is du -sb of the open directory: the sizes of everything in it, the
// directories themselves and the directory itself included, hard links once.
func duBytes(fd int, seen map[[2]uint64]bool, depth int) int64 {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return 0
	}
	total := st.Size
	if depth > 16 {
		return total
	}
	names, _ := listDir(fd)
	for _, n := range names {
		cst, err := fstatat(fd, n)
		if err != nil {
			continue
		}
		if isDirMode(cst.Mode) {
			if sub, err := openat(fd, n, oDir, 0); err == nil {
				total += duBytes(sub, seen, depth+1)
				unix.Close(sub)
			}
			continue
		}
		if cst.Nlink > 1 {
			key := [2]uint64{uint64(cst.Dev), cst.Ino}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		total += cst.Size
	}
	return total
}

// tar-pack DIR NAME...: a tar of the named files of a backup directory (one
// with a manifest.json) on stdout.
func verbTarPack(c *Ctx) error {
	if err := c.Need(2, 41, "tar-pack DIR NAME..."); err != nil {
		return err
	}
	r, fd, err := openDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	defer unix.Close(fd)
	if _, ok := regularAt(fd, "manifest.json"); !ok {
		return Die("%s is not a backup", c.Args[0])
	}
	names := c.Args[1:]
	for _, n := range names {
		if !nameOK(n) {
			return Die("unsupported file name")
		}
		if _, ok := regularAt(fd, n); !ok {
			return Fail("%s is not a regular file", n)
		}
	}
	tw := tar.NewWriter(c.Stdout)
	for _, n := range names {
		if err := packOne(tw, fd, n); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return Fail("cannot write the archive: %v", err)
	}
	return nil
}

func packOne(tw *tar.Writer, dirfd int, name string) error {
	ffd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return Fail("%s is not a regular file", name)
	}
	f := os.NewFile(uintptr(ffd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(ffd, &st); err != nil || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", name)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg, Name: name, Size: st.Size, Mode: int64(st.Mode & 0o777),
		Uid: int(st.Uid), Gid: int(st.Gid), ModTime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec), Format: tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return Fail("cannot write the archive: %v", err)
	}
	if n, err := io.Copy(tw, io.LimitReader(f, st.Size)); err != nil || n != st.Size {
		return Fail("cannot pack %s", name)
	}
	return nil
}

// emptyDir removes everything in the directory dirfd refers to.
func emptyDir(dirfd int) {
	var dst unix.Stat_t
	if unix.Fstat(dirfd, &dst) != nil {
		return
	}
	names, _ := listDir(dirfd)
	for _, n := range names {
		st, err := fstatat(dirfd, n)
		if err != nil {
			continue
		}
		if isDirMode(st.Mode) {
			_ = removeTree(dirfd, n, uint64(dst.Dev))
		} else {
			_ = unix.Unlinkat(dirfd, n, 0)
		}
	}
}

// tar-unpack DIR: unpack a tar from stdin into the empty work directory DIR.
// Members with a slash in the name are skipped; anything else that is not a
// plain regular file (links, devices, directories) makes the whole unpack fail
// and empties the directory again, as does a broken archive or a signal.
func verbTarUnpack(c *Ctx) error {
	if err := c.Need(1, 1, "tar-unpack DIR"); err != nil {
		return err
	}
	r, fd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	defer unix.Close(fd) // runs after cancel below
	if names, _ := listDir(fd); len(names) != 0 {
		return Fail("%s is not empty", c.Args[0])
	}
	cancel := c.Cleanup(func() { emptyDir(fd) })
	defer cancel()
	if err := unpackAll(c.Stdin, fd); err != nil {
		emptyDir(fd)
		return err
	}
	return nil
}

// zeroTail counts the zero bytes at the very end of what has been read, to
// tell a finished archive (it ends in at least two zero blocks) from one that
// was cut off at a block boundary, which archive/tar reports as a normal end.
type zeroTail struct {
	r io.Reader
	n int
}

func (z *zeroTail) Read(p []byte) (int, error) {
	k, err := z.r.Read(p)
	for _, b := range p[:k] {
		if b == 0 {
			z.n++
		} else {
			z.n = 0
		}
	}
	return k, err
}

func unpackAll(r io.Reader, dirfd int) error {
	in := &zeroTail{r: r}
	tr := tar.NewReader(in)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Fail("tar failed: %v", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader || strings.Contains(h.Name, "/") {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Name == "" || h.Name == "." || h.Name == ".." || strings.ContainsRune(h.Name, 0) {
			return Fail("the archive holds something other than plain files")
		}
		ffd, err := openat(dirfd, h.Name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return Fail("tar failed: cannot create %s: %v", h.Name, err)
		}
		f := os.NewFile(uintptr(ffd), h.Name)
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return Fail("tar failed: %v", err)
		}
	}
	// Let the sender finish its write.
	_, _ = io.Copy(io.Discard, in)
	if in.n < 1024 {
		return Fail("tar failed: unexpected end of the archive")
	}
	return nil
}

var convertFormats = map[string]bool{"raw": true, "qcow2": true, "vpc": true, "vhdx": true, "vdi": true}

// convertPrep checks the arguments of convert-start and convert-run. It
// returns the work directory (its Ref and open descriptor) and the canonical
// source path.
func convertPrep(c *Ctx) (*Ref, int, string, error) {
	if !convertFormats[c.Args[2]] {
		return nil, -1, "", Die("unsupported format")
	}
	w, wfd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return nil, -1, "", err
	}
	fail := func(err error) (*Ref, int, string, error) {
		unix.Close(wfd)
		w.Close()
		return nil, -1, "", err
	}
	s, err := c.ResolveRead(c.Args[1])
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	if st, ok, _ := s.Lstat(); !ok || !isRegMode(st.Mode) {
		return fail(Fail("%s is not a regular file", c.Args[1]))
	}
	if c.Args[2] == "qcow2" {
		if err := c.CheckChain(s.Path); err != nil {
			return fail(err)
		}
		out, err := c.CombinedOutput("qemu-img", "info", "-U", "-f", "qcow2", "--output=json", "--", s.Path)
		if _, ok := err.(*Error); ok {
			return fail(err)
		}
		if strings.Contains(string(out), `"data-file"`) {
			return fail(Die("%s uses an external data file", c.Args[1]))
		}
	}
	return w, wfd, s.Path, nil
}

func removeFiles(dirfd int, names ...string) {
	for _, n := range names {
		_ = unix.Unlinkat(dirfd, n, 0)
	}
}

func writeSmall(dirfd int, name string, flags int, content string) error {
	fd, err := openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|flags, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// convert-start WORKDIR SRC FORMAT: start qemu-img convert of SRC to
// WORKDIR/out.part, detached from this process and from the SSH session. The
// state lives in WORKDIR: qpid (pid of qemu-img), rc (its exit code) and
// prog.log. The detached part is this program again, run as convert-run.
func verbConvertStart(c *Ctx) error {
	if err := c.Need(3, 3, "convert-start WORKDIR SRC FORMAT"); err != nil {
		return err
	}
	w, wfd, _, err := convertPrep(c)
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	if !c.HasTool("qemu-img") {
		return Fail("qemu-img is not installed on this node")
	}
	removeFiles(wfd, "rc", "qpid", "prog.log", "out.part")
	self := c.Cfg.Self
	if self == "" {
		self = NodePath
		if exe, err := os.Executable(); err == nil {
			self = exe
		}
	}
	cmd := exec.Command(self, "convert-run", c.Args[0], c.Args[1], c.Args[2])
	cmd.Env = []string{"PATH=" + FixedPath, "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Fail("cannot start the conversion: %v", err)
	}
	_ = cmd.Process.Release()
	// Report a conversion that did not come up instead of "started".
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, hasPid := regularAt(wfd, "qpid")
		_, hasRC := regularAt(wfd, "rc")
		if hasPid || hasRC {
			fmt.Fprintln(c.Stdout, "started")
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return Fail("the conversion did not start")
}

// convert-run WORKDIR SRC FORMAT: the detached part of convert-start. Runs
// qemu-img in the foreground, records its pid (qpid) and exit code (rc) and
// leaves no out.part behind when it failed or was stopped.
func verbConvertRun(c *Ctx) error {
	if err := c.Need(3, 3, "convert-run WORKDIR SRC FORMAT"); err != nil {
		return err
	}
	w, wfd, src, err := convertPrep(c)
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	// qpid is created first and exclusively: one conversion per work directory.
	if err := writeSmall(wfd, "qpid", unix.O_EXCL, ""); err != nil {
		return Fail("a conversion was started in %s already", c.Args[0])
	}
	logfd, err := openat(wfd, "prog.log", unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return Fail("cannot write the progress log: %v", err)
	}
	logf := os.NewFile(uintptr(logfd), "prog.log")
	defer logf.Close()
	var once sync.Once
	finish := func(rc int) {
		once.Do(func() {
			if rc != 0 {
				removeFiles(wfd, "out.part")
			}
			_ = writeSmall(wfd, "rc", unix.O_TRUNC, strconv.Itoa(rc)+"\n")
		})
	}
	// A signal to this process: qemu-img is killed through the context, the
	// part file goes, and rc says the conversion did not finish.
	cancel := c.Cleanup(func() { finish(143) })
	defer cancel()
	cmd, err := c.Command("qemu-img", "convert", "-p", "-U", "-f", c.Args[2], "-O", "qcow2", "--", src, w.Path+"/out.part")
	if err != nil {
		finish(127)
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		finish(127)
		return Fail("cannot start qemu-img: %v", err)
	}
	_ = writeSmall(wfd, "qpid", unix.O_TRUNC, strconv.Itoa(cmd.Process.Pid)+"\n")
	err = cmd.Wait()
	rc := 0
	if err != nil {
		rc = ExitStatus(err)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				rc = 128 + int(ws.Signal())
			}
		}
	}
	finish(rc)
	return nil
}

// convert-poll WORKDIR: the exit code once finished, "---", the end of the
// progress log (carriage returns as newlines).
func verbConvertPoll(c *Ctx) error {
	if err := c.Need(1, 1, "convert-poll WORKDIR"); err != nil {
		return err
	}
	w, wfd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	if b, err := readTail(wfd, "rc", 64); err == nil {
		c.Stdout.Write(b)
	}
	fmt.Fprintln(c.Stdout, "---")
	if b, err := readTail(wfd, "prog.log", 400); err == nil {
		c.Stdout.Write([]byte(strings.ReplaceAll(string(b), "\r", "\n")))
	}
	return nil
}

// readTail is the last n bytes of the regular file name below dirfd.
func readTail(dirfd int, name string, n int64) ([]byte, error) {
	fd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	return io.ReadAll(io.NewSectionReader(f, off, n))
}

// convert-kill WORKDIR: stop the conversion recorded in WORKDIR, if it still
// runs (and the pid still belongs to qemu-img). Succeeds for any work
// directory.
func verbConvertKill(c *Ctx) error {
	if err := c.Need(1, 1, "convert-kill WORKDIR"); err != nil {
		return err
	}
	w, wfd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	b, err := readTail(wfd, "qpid", 64)
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(b))
	pid, err := strconv.Atoi(s)
	if s == "" || len(s) > 15 || err != nil || pid <= 1 || strings.Trim(s, "0123456789") != "" {
		return nil
	}
	comm, err := os.ReadFile(fmt.Sprintf("%s/%d/comm", c.Cfg.proc(), pid))
	if err == nil && strings.TrimSpace(string(comm)) == "qemu-img" {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		time.Sleep(time.Second)
	}
	return nil
}

// convert-finish WORKDIR DEST: move the finished WORKDIR/out.part to the new
// file DEST (never replaced) and drop the state files.
func verbConvertFinish(c *Ctx) error {
	if err := c.Need(2, 2, "convert-finish WORKDIR DEST"); err != nil {
		return err
	}
	w, wfd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	if _, ok := regularAt(wfd, "out.part"); !ok {
		return Fail("there is no converted image")
	}
	dst, err := c.ResolveNew(c.Args[1])
	if err != nil {
		return err
	}
	defer dst.Close()
	err = unix.Renameat2(wfd, "out.part", dst.DirFD(), dst.Name, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
	case errors.Is(err, unix.EEXIST):
		return Fail("a file with this name appeared in the pool directory")
	case errors.Is(err, unix.EXDEV):
		if err := finishAcross(c, wfd, dst); err != nil {
			return err
		}
	case errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP):
		// The file system has no RENAME_NOREPLACE (NFS): a hard link is exclusive too, so link, then remove the
		// work file (the same as the move verb does).
		if lerr := unix.Linkat(wfd, "out.part", dst.DirFD(), dst.Name, 0); lerr != nil {
			switch {
			case errors.Is(lerr, unix.EEXIST):
				return Fail("a file with this name appeared in the pool directory")
			case errors.Is(lerr, unix.EXDEV):
				if err := finishAcross(c, wfd, dst); err != nil {
					return err
				}
			default:
				return Fail("cannot move the result into place: %v", lerr)
			}
		} else {
			_ = unix.Unlinkat(wfd, "out.part", 0)
		}
	default:
		return Fail("cannot move the result into place: %v", err)
	}
	removeFiles(wfd, "rc", "qpid", "prog.log")
	return nil
}

// finishAcross is the move to another file system: copy, then remove.
func finishAcross(c *Ctx, wfd int, dst *Ref) error {
	ffd, err := openat(wfd, "out.part", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Fail("cannot move the result into place: %v", err)
	}
	in := os.NewFile(uintptr(ffd), "out.part")
	defer in.Close()
	var st unix.Stat_t
	if err := unix.Fstat(ffd, &st); err != nil {
		return Fail("cannot move the result into place: %v", err)
	}
	err = c.CreateNew(dst, st.Mode&0o7777, func(f *os.File) error {
		if err := CopySparse(f, in, st.Size); err != nil {
			return Fail("cannot move the result into place: %v", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = unix.Unlinkat(wfd, "out.part", 0)
	return nil
}
