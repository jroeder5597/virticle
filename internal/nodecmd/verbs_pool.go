package nodecmd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The pool verbs: they write into the storage pools or read images there for
// an import. download, download-cancel, image-info, realpath, free-space,
// tar-list, tar-extract, import-convert-start, import-convert-status,
// import-convert-kill and sysprep, plus import-convert-run, the process that
// import-convert-start leaves running after its own SSH command has ended.
//
//	download URL DEST [sha256|sha512 HEX]    http(s) URL -> new file DEST (a .NAME.part file next to it meanwhile)
//	download-cancel DEST                     stop a running download of DEST
//	image-info PATH                          qemu-img info as JSON; the format is recognised from the content
//	realpath file|dir PATH                   canonical path of an existing file or directory inside the roots
//	free-space DIR                           free bytes of the file system of DIR
//	tar-list FILE                            the members of an archive, one line each, like tar -tv
//	tar-extract FILE WORKDIR NAME...         extract plain-named regular members into a work directory
//	import-convert-start WORKDIR SRC         qemu-img convert SRC -> WORKDIR/out.part (qcow2) in the background
//	import-convert-status WORKDIR            exit code (once finished), "---", the tail of the progress log
//	import-convert-kill WORKDIR              stop the conversion started from WORKDIR
//	sysprep DISK...                          virt-sysprep with a fixed option set on disk images

func init() {
	Register("download", verbDownload)
	Register("download-cancel", verbDownloadCancel)
	Register("image-info", verbImageInfo)
	Register("realpath", verbRealpath)
	Register("free-space", verbFreeSpace)
	Register("tar-list", verbTarList)
	Register("tar-extract", verbTarExtract)
	Register("import-convert-start", verbImportConvertStart)
	Register("import-convert-run", verbImportConvertRun)
	Register("import-convert-status", verbImportConvertStatus)
	Register("import-convert-kill", verbImportConvertKill)
	Register("sysprep", verbSysprep)
}

// poolRunDir is where the state of running downloads is kept (root-owned
// /run/virticle-node; below Config.Prefix in tests).
func (c *Ctx) poolRunDir() string { return c.hostPath("run/virticle-node") }

// procIsNamed reports whether the process pid exists and its name (comm) is name.
func procIsNamed(pid int, name string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return err == nil && strings.TrimSpace(string(b)) == name
}

// --- format sniffing ---

// sniffFormat recognises raw, qcow2, vmdk, vpc, vhdx and vdi from the first
// bytes of the open file.
func sniffFormat(f *os.File) (string, error) {
	var h [64]byte
	n, err := io.ReadFull(f, h[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	b := h[:n]
	switch {
	case bytes.HasPrefix(b, []byte{0x51, 0x46, 0x49, 0xfb}):
		return "qcow2", nil
	case bytes.HasPrefix(b, []byte("KDMV")):
		return "vmdk", nil
	case bytes.HasPrefix(b, []byte("conectix")):
		return "vpc", nil
	case bytes.HasPrefix(b, []byte("vhdxfile")):
		return "vhdx", nil
	case bytes.HasPrefix(b, []byte("<<< ")):
		if bytes.Contains(b, []byte("Disk Image")) || bytes.Contains(b, []byte("Virtual Disk")) {
			return "vdi", nil
		}
	}
	return "raw", nil
}

// le64At is the little endian 64 bit number at off; bytes beyond the end of
// the file count as zero.
func le64At(f *os.File, off int64) uint64 {
	var b [8]byte
	_, _ = f.ReadAt(b[:], off)
	return binary.LittleEndian.Uint64(b[:])
}

var vmdkExtentRE = regexp.MustCompile(`^(RW|RDONLY|NOACCESS) [0-9]+ `)

// vmdkSelfContained: a sparse VMDK whose embedded descriptor names no other
// file. (For a header with capacity 0 qemu-img opens every extent the
// descriptor names; otherwise the descriptor is not followed.) base is the
// file name of the image itself.
func vmdkSelfContained(f *os.File, base string) bool {
	var m [4]byte
	if _, err := f.ReadAt(m[:], 0); err != nil || string(m[:]) != "KDMV" {
		return false
	}
	if le64At(f, 12) != 0 {
		return true
	}
	doff, dsz := le64At(f, 28), le64At(f, 36)
	if !(doff > 0 && dsz > 0 && dsz <= 2048 && doff <= 8192) {
		return false
	}
	buf := make([]byte, dsz*512)
	n, _ := f.ReadAt(buf, int64(doff)*512)
	for _, line := range bytes.Split(buf[:n], []byte("\n")) {
		if !vmdkExtentRE.Match(line) {
			continue
		}
		name := string(line)
		if a, z := strings.IndexByte(name, '"'), strings.LastIndexByte(name, '"'); a >= 0 && z > a {
			name = name[a+1 : z]
		}
		if name != base {
			return false
		}
	}
	return true
}

// sourceImage opens an image the caller wants read: a regular file inside the
// roots whose format is recognised from the content. It refuses what would
// make qemu-img open other files (a backing chain leaving the roots, a VMDK
// naming other extents). The caller closes the Ref.
func sourceImage(c *Ctx, arg string) (*Ref, string, error) {
	r, err := c.ResolveRead(arg)
	if err != nil {
		return nil, "", err
	}
	format, err := checkSource(c, r, arg)
	if err != nil {
		r.Close()
		return nil, "", err
	}
	return r, format, nil
}

func checkSource(c *Ctx, r *Ref, arg string) (string, error) {
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return "", Fail("%s is not a regular file", arg)
	}
	f, err := r.Open(unix.O_RDONLY, 0)
	if err != nil {
		return "", Fail("cannot read %s", arg)
	}
	defer f.Close()
	format, err := sniffFormat(f)
	if err != nil {
		return "", Fail("cannot read %s", arg)
	}
	switch format {
	case "qcow2":
		if err := c.CheckChain(r.Path); err != nil {
			return "", err
		}
	case "vmdk":
		if !vmdkSelfContained(f, r.Name) {
			return "", Fail("refusing VMDK: it refers to other files; only self-contained images are accepted")
		}
	}
	return format, nil
}

func verbImageInfo(c *Ctx) error {
	if err := c.Need(1, 1, "image-info PATH"); err != nil {
		return err
	}
	r, format, err := sourceImage(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := c.CombinedOutput("qemu-img", "info", "-U", "-f", format, "--output=json", "--", r.Path)
	if _, ok := err.(*Error); ok {
		return err
	}
	c.Stdout.Write(out)
	if err != nil {
		return ExitCode(ExitStatus(err))
	}
	return nil
}

func verbRealpath(c *Ctx) error {
	if err := c.Need(2, 2, "realpath file|dir PATH"); err != nil {
		return err
	}
	kind := c.Args[0]
	if kind != "file" && kind != "dir" {
		return Die("usage: realpath file|dir PATH")
	}
	r, err := c.ResolveRead(c.Args[1])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	switch {
	case kind == "file" && (!exists || !isRegMode(st.Mode)):
		return Fail("%s is not a regular file", c.Args[1])
	case kind == "dir" && (!exists || !isDirMode(st.Mode)):
		return Fail("%s is not a directory", c.Args[1])
	}
	fmt.Fprint(c.Stdout, r.Path)
	return nil
}

func verbFreeSpace(c *Ctx) error {
	if err := c.Need(1, 1, "free-space DIR"); err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	fd, err := r.OpenDir()
	if err != nil {
		return Fail("%s is not a directory", c.Args[0])
	}
	defer unix.Close(fd)
	var sf unix.Statfs_t
	if err := unix.Fstatfs(fd, &sf); err != nil {
		return Fail("cannot read the free space of %s", c.Args[0])
	}
	fmt.Fprintln(c.Stdout, uint64(sf.Bavail)*uint64(sf.Bsize))
	return nil
}

// --- tar ---

// tarTypeChar is the first letter of a GNU tar -tv line.
func tarTypeChar(t byte) byte {
	switch t {
	case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
		return '-'
	case tar.TypeDir:
		return 'd'
	case tar.TypeSymlink:
		return 'l'
	case tar.TypeLink:
		return 'h'
	case tar.TypeChar:
		return 'c'
	case tar.TypeBlock:
		return 'b'
	case tar.TypeFifo:
		return 'p'
	}
	return '?'
}

func tarIsRegular(t byte) bool { return tarTypeChar(t) == '-' }

// tarModeString is the rwx part of a listing line, e.g. rw-r--r--.
func tarModeString(m int64) string {
	b := []byte("---------")
	for i := 0; i < 9; i++ {
		if m&(1<<uint(8-i)) != 0 {
			b[i] = "rwxrwxrwx"[i]
		}
	}
	if m&0o4000 != 0 {
		b[2] = map[bool]byte{true: 's', false: 'S'}[b[2] == 'x']
	}
	if m&0o2000 != 0 {
		b[5] = map[bool]byte{true: 's', false: 'S'}[b[5] == 'x']
	}
	if m&0o1000 != 0 {
		b[8] = map[bool]byte{true: 't', false: 'T'}[b[8] == 'x']
	}
	return string(b)
}

// tarEscape quotes a name like tar --quoting-style=escape in the C locale:
// backslash escapes for the usual control characters, octal for the rest.
func tarEscape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\':
			sb.WriteString(`\\`)
		case ch == '\a':
			sb.WriteString(`\a`)
		case ch == '\b':
			sb.WriteString(`\b`)
		case ch == '\f':
			sb.WriteString(`\f`)
		case ch == '\n':
			sb.WriteString(`\n`)
		case ch == '\r':
			sb.WriteString(`\r`)
		case ch == '\t':
			sb.WriteString(`\t`)
		case ch == '\v':
			sb.WriteString(`\v`)
		case ch < 0x20 || ch >= 0x7f:
			fmt.Fprintf(&sb, `\%03o`, ch)
		default:
			sb.WriteByte(ch)
		}
	}
	return sb.String()
}

func tarListLine(h *tar.Header) string {
	owner := h.Uname + "/" + h.Gname
	if h.Uname == "" && h.Gname == "" {
		owner = strconv.Itoa(h.Uid) + "/" + strconv.Itoa(h.Gid)
	}
	line := fmt.Sprintf("%c%s %s %9d %s %s", tarTypeChar(h.Typeflag), tarModeString(h.Mode), tarEscape(owner), h.Size,
		h.ModTime.UTC().Format("2006-01-02 15:04"), tarEscape(h.Name))
	switch h.Typeflag {
	case tar.TypeSymlink:
		line += " -> " + tarEscape(h.Linkname)
	case tar.TypeLink:
		line += " link to " + tarEscape(h.Linkname)
	}
	return line
}

const tarScanTime = 60 * time.Second

// openArchive opens the regular file r names; the file is closed by the
// returned stop function or, at the latest, after tarScanTime when timed.
func openArchive(r *Ref, arg string) (*os.File, error) {
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return nil, Fail("%s is not a regular file", arg)
	}
	f, err := r.Open(unix.O_RDONLY, 0)
	if err != nil {
		return nil, Fail("cannot read %s", arg)
	}
	return f, nil
}

func verbTarList(c *Ctx) error {
	if err := c.Need(1, 1, "tar-list FILE"); err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := openArchive(r, c.Args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	t := time.AfterFunc(tarScanTime, func() { f.Close() })
	defer t.Stop()
	w := bufio.NewWriter(c.Stdout)
	defer w.Flush()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			w.Flush()
			return ExitCode(2) // like tar: not an archive, or a damaged one
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		fmt.Fprintln(w, tarListLine(h))
	}
}

// memberNameOK: a plain file name, as the OVA import accepts them.
func memberNameOK(s string) bool {
	if s == "" || len(s) > 121 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		alnum := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !(alnum || i > 0 && (ch == '.' || ch == '_' || ch == '-')) {
			return false
		}
	}
	return true
}

// poolWorkDir resolves an existing work directory (.virticle-*) inside the
// roots. The caller closes the Ref.
func poolWorkDir(c *Ctx, arg string) (*Ref, error) {
	r, err := c.ResolveMod(arg)
	if err != nil {
		return nil, err
	}
	st, exists, _ := r.Lstat()
	if !exists || !isDirMode(st.Mode) {
		r.Close()
		return nil, Fail("%s is not a directory", arg)
	}
	if !WorkName(r.Path) {
		r.Close()
		return nil, Die("%s is not a work directory", arg)
	}
	return r, nil
}

func verbTarExtract(c *Ctx) error {
	if err := c.Need(3, 66, "tar-extract FILE WORKDIR NAME..."); err != nil {
		return err
	}
	names := c.Args[2:]
	for _, n := range names {
		if !memberNameOK(n) {
			return Die("unsupported member name")
		}
	}
	ar, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer ar.Close()
	if st, exists, _ := ar.Lstat(); !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	wd, err := poolWorkDir(c, c.Args[1])
	if err != nil {
		return err
	}
	defer wd.Close()

	// Every member of the archive must be a regular file with a plain name,
	// so that no link or later member can redirect where a name is written.
	have := map[string]bool{}
	if err := func() error {
		f, err := openArchive(ar, c.Args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		t := time.AfterFunc(tarScanTime, func() { f.Close() })
		defer t.Stop()
		tr := tar.NewReader(f)
		count := 0
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return Fail("cannot list %s", c.Args[0])
			}
			if h.Typeflag == tar.TypeXGlobalHeader {
				continue
			}
			count++
			if !tarIsRegular(h.Typeflag) {
				return Fail("refusing archive: it contains something other than regular files")
			}
			if !memberNameOK(h.Name) {
				return Fail("refusing archive: member %s is not a plain file name", tarEscape(h.Name))
			}
			if have[h.Name] {
				return Fail("refusing archive: duplicate member")
			}
			have[h.Name] = true
		}
		if count == 0 {
			return Fail("%s is empty", c.Args[0])
		}
		return nil
	}(); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, n := range names {
		if !have[n] {
			return Fail("%s: Not found in archive", n)
		}
		want[n] = true
	}

	wfd, err := wd.OpenDir()
	if err != nil {
		return Fail("%s is not a directory", c.Args[1])
	}
	defer unix.Close(wfd)
	f, err := openArchive(ar, c.Args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for len(want) > 0 {
		h, err := tr.Next()
		if err != nil {
			return Fail("cannot read %s", c.Args[0])
		}
		if !want[h.Name] {
			continue
		}
		if err := extractMember(c, wfd, h.Name, tr, h.Size); err != nil {
			return err
		}
		delete(want, h.Name)
	}
	return nil
}

// extractMember writes size bytes from src to the file name in the directory
// wfd (0600; a link in its place is not followed). A partial file is removed.
func extractMember(c *Ctx, wfd int, name string, src io.Reader, size int64) error {
	fd, err := openat(wfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return Fail("cannot create %s: %v", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	remove := func() { _ = unix.Unlinkat(wfd, name, 0) }
	cancel := c.Cleanup(remove)
	defer cancel()
	n, err := io.CopyBuffer(f, io.LimitReader(src, size), make([]byte, 1<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		remove()
		return Fail("cannot extract %s: %v", name, err)
	}
	return nil
}

// --- import conversion ---

func verbImportConvertStart(c *Ctx) error {
	if err := c.Need(2, 2, "import-convert-start WORKDIR SRC"); err != nil {
		return err
	}
	wd, err := poolWorkDir(c, c.Args[0])
	if err != nil {
		return err
	}
	defer wd.Close()
	src, _, err := sourceImage(c, c.Args[1])
	if err != nil {
		return err
	}
	defer src.Close()
	wfd, err := wd.OpenDir()
	if err != nil {
		return Fail("%s is not a directory", c.Args[0])
	}
	defer unix.Close(wfd)
	for _, n := range []string{"rc", "qpid", "prog.log", "out.part"} {
		_ = unix.Unlinkat(wfd, n, 0)
	}
	self := c.Cfg.Self
	if self == "" {
		self = NodePath
		if exe, err := os.Executable(); err == nil {
			self = exe
		}
	}
	// The conversion outlives this command (and the SSH session that ran it):
	// the node binary runs again in a session of its own, and records the
	// process id of qemu-img, its output and its exit status in WORKDIR.
	cmd := exec.Command(self, "import-convert-run", wd.Path, src.Path)
	cmd.Env = []string{"PATH=" + FixedPath, "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Fail("cannot start the conversion: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	fmt.Fprintln(c.Stdout, "started")
	return nil
}

// verbImportConvertRun is the detached half of import-convert-start: it runs
// qemu-img convert in the foreground and records the state in WORKDIR (qpid,
// prog.log, rc). It checks everything again, so calling it by hand is no
// more than a blocking import-convert-start.
func verbImportConvertRun(c *Ctx) error {
	if err := c.Need(2, 2, "import-convert-run WORKDIR SRC"); err != nil {
		return err
	}
	wd, err := poolWorkDir(c, c.Args[0])
	if err != nil {
		return err
	}
	defer wd.Close()
	wfd, err := wd.OpenDir()
	if err != nil {
		return Fail("%s is not a directory", c.Args[0])
	}
	defer unix.Close(wfd)
	writeState := func(name, text string, excl bool) {
		fl := unix.O_WRONLY | unix.O_CREAT | unix.O_TRUNC | unix.O_NOFOLLOW
		if excl {
			fl = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW
		}
		if fd, err := openat(wfd, name, fl, 0o600); err == nil {
			f := os.NewFile(uintptr(fd), name)
			_, _ = f.WriteString(text)
			f.Close()
		}
	}
	logfd, err := openat(wfd, "prog.log", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return Fail("cannot create the progress log: %v", err)
	}
	logf := os.NewFile(uintptr(logfd), "prog.log")
	defer logf.Close()
	cancelRC := c.Cleanup(func() { writeState("rc", "143\n", true) })
	defer cancelRC()
	fail := func(err error) error {
		fmt.Fprintln(logf, errMsg(err))
		writeState("rc", "1\n", true)
		return err
	}
	src, format, err := sourceImage(c, c.Args[1])
	if err != nil {
		return fail(err)
	}
	defer src.Close()
	cmd, err := c.Command("qemu-img", "convert", "-p", "-U", "-f", format, "-O", "qcow2", "--", src.Path, wd.Path+"/out.part")
	if err != nil {
		return fail(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return fail(Fail("cannot start qemu-img: %v", err))
	}
	writeState("qpid", strconv.Itoa(cmd.Process.Pid)+"\n", false)
	err = cmd.Wait()
	rc := 0
	if err != nil {
		rc = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				rc = 128 + int(ws.Signal())
			} else if ee.ExitCode() > 0 {
				rc = ee.ExitCode()
			}
		}
	}
	writeState("rc", strconv.Itoa(rc)+"\n", true)
	return nil
}

// readSmall reads at most max bytes of the file name in the directory wfd; ""
// if there is none.
func readSmall(wfd int, name string, max int) (string, bool) {
	fd, err := openat(wfd, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	b := make([]byte, max)
	n, _ := io.ReadFull(f, b)
	return string(b[:n]), true
}

func verbImportConvertStatus(c *Ctx) error {
	if err := c.Need(1, 1, "import-convert-status WORKDIR"); err != nil {
		return err
	}
	wd, err := poolWorkDir(c, c.Args[0])
	if err != nil {
		return err
	}
	defer wd.Close()
	wfd, err := wd.OpenDir()
	if err != nil {
		return Fail("%s is not a directory", c.Args[0])
	}
	defer unix.Close(wfd)
	if rc, ok := readSmall(wfd, "rc", 64); ok {
		fmt.Fprint(c.Stdout, rc)
	}
	fmt.Fprintln(c.Stdout, "---")
	fd, err := openat(wfd, "prog.log", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), "prog.log")
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		off := st.Size() - 400
		if off < 0 {
			off = 0
		}
		b, _ := io.ReadAll(io.NewSectionReader(f, off, 400))
		c.Stdout.Write(bytes.ReplaceAll(b, []byte{'\r'}, []byte{'\n'}))
	}
	return nil
}

func verbImportConvertKill(c *Ctx) error {
	if err := c.Need(1, 1, "import-convert-kill WORKDIR"); err != nil {
		return err
	}
	wd, err := poolWorkDir(c, c.Args[0])
	if err != nil {
		return err
	}
	defer wd.Close()
	wfd, err := wd.OpenDir()
	if err != nil {
		return Fail("%s is not a directory", c.Args[0])
	}
	defer unix.Close(wfd)
	s, _ := readSmall(wfd, "qpid", 20)
	s = strings.TrimSpace(s)
	if !numOK(s) {
		return nil
	}
	pid, _ := strconv.Atoi(s)
	if pid <= 1 || !procIsNamed(pid, "qemu-img") {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 20 && procIsNamed(pid, "qemu-img"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// --- downloads ---

// urlOK: an http or https URL of plain characters, no quotes or spaces.
func urlOK(u string) bool {
	rest := ""
	switch {
	case strings.HasPrefix(u, "http://"):
		rest = u[len("http://"):]
	case strings.HasPrefix(u, "https://"):
		rest = u[len("https://"):]
	}
	if rest == "" || len(u) > 2000 {
		return false
	}
	for i := 0; i < len(u); i++ {
		ch := u[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.IndexByte("._~:/?#@!$&()*+,;=%-", ch) >= 0 {
			continue
		}
		return false
	}
	return true
}

func isAlnumByte(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
}

// dlState are the files that describe a running download of dest (canonical).
func (c *Ctx) dlState(dest string) (pidFile, cancelFile string) {
	sum := sha256.Sum256([]byte(dest))
	base := filepath.Join(c.poolRunDir(), "dl."+hex.EncodeToString(sum[:])[:32])
	return base + ".pid", base + ".cancel"
}

const (
	dlConnectTimeout = 20 * time.Second
	dlMaxRedirects   = 10
	dlCancelPoll     = 250 * time.Millisecond
)

// dlClient is the HTTP client of a download: system root certificates with
// verification on, no proxy (the old curl call ran with a clean environment
// and -q), http and https only also across redirects, at most 10 redirects,
// 20 seconds to connect, the body stored as the server sends it.
func dlClient() (*http.Client, func()) {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: dlConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: dlConnectTimeout,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to an unsupported protocol")
			}
			if len(via) > dlMaxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}, tr.CloseIdleConnections
}

func verbDownload(c *Ctx) error {
	if len(c.Args) != 2 && len(c.Args) != 4 {
		return Die("usage: download URL DEST [sha256|sha512 HEX]")
	}
	url := c.Args[0]
	if !urlOK(url) {
		return Die("unsupported URL: only plain http:// and https:// addresses")
	}
	var hsh hash.Hash
	want := ""
	if len(c.Args) == 4 {
		want = c.Args[3]
		var n int
		switch c.Args[2] {
		case "sha256":
			hsh, n = sha256.New(), 64
		case "sha512":
			hsh, n = sha512.New(), 128
		default:
			return Die("unsupported digest")
		}
		if len(want) != n {
			return Die("unsupported digest")
		}
		for i := 0; i < len(want); i++ {
			if !(want[i] >= '0' && want[i] <= '9' || want[i] >= 'a' && want[i] <= 'f') {
				return Die("unsupported digest")
			}
		}
	}
	r, err := c.ResolveNew(c.Args[1])
	if err != nil {
		return err
	}
	defer r.Close()
	if r.Name == "" || !isAlnumByte(r.Name[0]) {
		return Die("unsupported file name")
	}
	part := "." + r.Name + ".part"
	if _, exists, _ := (&Ref{dirfd: r.dirfd, Name: part}).Lstat(); exists {
		return Fail("a file named %s already exists in the pool directory", part)
	}
	run := c.poolRunDir()
	if err := os.MkdirAll(run, 0o700); err != nil {
		return Fail("cannot create %s", run)
	}
	pidFile, cancelFile := c.dlState(r.Path)
	_ = os.Remove(cancelFile)

	fd, err := openat(r.dirfd, part, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return Fail("a file named %s already exists in the pool directory", part)
	}
	f := os.NewFile(uintptr(fd), part)
	dirfd := r.dirfd
	cleanup := func() {
		_ = unix.Unlinkat(dirfd, part, 0)
		_ = os.Remove(pidFile)
		_ = os.Remove(cancelFile)
	}
	unreg := c.Cleanup(cleanup)
	defer func() { f.Close(); cleanup(); unreg() }()
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return Fail("cannot write %s", pidFile)
	}

	ctx, stop := context.WithCancel(c.Context)
	defer stop()
	var cancelled atomic.Bool
	go func() {
		t := time.NewTicker(dlCancelPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := os.Lstat(cancelFile); err == nil {
					cancelled.Store(true)
					stop()
					return
				}
			}
		}
	}()
	failed := func(err error) error {
		if cancelled.Load() {
			return Fail("download cancelled")
		}
		return Fail("download failed: %v", err)
	}

	client, closeIdle := dlClient()
	defer closeIdle()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Die("unsupported URL")
	}
	req.Header.Set("User-Agent", "virticle-node")
	resp, err := client.Do(req)
	if err != nil {
		return failed(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Fail("download failed: HTTP status %d", resp.StatusCode)
	}
	var dst io.Writer = f
	if hsh != nil {
		dst = io.MultiWriter(f, hsh)
	}
	if _, err := io.CopyBuffer(dst, resp.Body, make([]byte, 1<<20)); err != nil {
		return failed(err)
	}
	if cancelled.Load() {
		return Fail("download cancelled")
	}
	if hsh != nil {
		if got := hex.EncodeToString(hsh.Sum(nil)); got != want {
			return Fail("checksum mismatch: got %s", got)
		}
	}
	if err := f.Sync(); err != nil {
		return Fail("download failed: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return Fail("download failed: %v", err)
	}
	if err := unix.Linkat(dirfd, part, dirfd, r.Name, 0); err != nil {
		return Fail("%s appeared meanwhile; nothing was overwritten", r.Path)
	}
	fmt.Fprintf(c.Stdout, "@@done %d\n", st.Size)
	return nil
}

func verbDownloadCancel(c *Ctx) error {
	if err := c.Need(1, 1, "download-cancel DEST"); err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	none := Fail("no download of %s is running", c.Args[0])
	pidFile, cancelFile := c.dlState(r.Path)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return none
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 20 || !numOK(s) {
		return none
	}
	pid, _ := strconv.Atoi(s)
	self, _ := os.ReadFile("/proc/self/comm")
	if pid <= 1 || len(bytes.TrimSpace(self)) == 0 || !procIsNamed(pid, strings.TrimSpace(string(self))) {
		return none
	}
	// The download looks for this file a few times a second and stops.
	if err := os.WriteFile(cancelFile, nil, 0o600); err != nil {
		return Fail("cannot stop the download of %s", c.Args[0])
	}
	return nil
}

// --- clone preparation ---

// SysprepFixups are the first-boot fixes virt-sysprep runs in the guest. The
// text is part of the node binary so the caller cannot supply commands; a
// test in internal/api keeps it equal to the controller's copy
// (linuxFirstBootFixups in internal/api/agent.go).
const SysprepFixups = `# cloud-init pins its netplan config to the NIC's MAC address, which a clone
# does not have; without a seed cloud-init stays disabled and the clone would
# have no network. A seeded boot rewrites this file.
if [ -f /etc/netplan/50-cloud-init.yaml ]; then
  printf 'network:\n  version: 2\n  ethernets:\n    all-en:\n      match:\n        name: "en*"\n      dhcp4: true\n      dhcp6: true\n' > /etc/netplan/50-cloud-init.yaml
fi
rm -f /etc/systemd/network/10-cloud-init-*.network
for u in ssh sshd; do
  if [ -e /lib/systemd/system/$u.service ] || [ -e /usr/lib/systemd/system/$u.service ]; then
    mkdir -p /etc/systemd/system/$u.service.d
    printf '[Service]\nExecStartPre=\nExecStartPre=-/usr/bin/ssh-keygen -A\nExecStartPre=/usr/sbin/sshd -t\n' > /etc/systemd/system/$u.service.d/10-regenerate-hostkeys.conf
    break
  fi
done`

func verbSysprep(c *Ctx) error {
	if err := c.Need(1, 8, "sysprep DISK..."); err != nil {
		return err
	}
	var args []string
	args = append(args, "--run-command", SysprepFixups)
	for _, a := range c.Args {
		r, err := c.ResolveMod(a)
		if err != nil {
			return err
		}
		format, err := func() (string, error) {
			defer r.Close()
			st, exists, _ := r.Lstat()
			if !exists || !isRegMode(st.Mode) {
				return "", Fail("%s is not a regular file", a)
			}
			f, err := r.Open(unix.O_RDONLY, 0)
			if err != nil {
				return "", Fail("cannot read %s", a)
			}
			defer f.Close()
			format, err := sniffFormat(f)
			if err != nil {
				return "", Fail("cannot read %s", a)
			}
			switch format {
			case "qcow2":
				if err := c.CheckChain(r.Path); err != nil {
					return "", err
				}
			case "raw":
			default:
				return "", Fail("%s is neither qcow2 nor raw", a)
			}
			return format, nil
		}()
		if err != nil {
			return err
		}
		args = append(args, "--format", format, "-a", r.Path)
	}
	cmd, err := c.Command("virt-sysprep", args...)
	if err != nil {
		return err
	}
	cmd.Env = append(cmd.Env, "LIBGUESTFS_BACKEND=direct")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	c.Stdout.Write(out.Bytes())
	if err != nil {
		return ExitCode(ExitStatus(err))
	}
	return nil
}
