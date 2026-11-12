package nodecmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The file verbs: roots, stat, remove, rmtree, rmdir, move, copy, read, write,
// mkworkdir, chmod-work, qemu-img-info, sha256, tools-missing.

func init() {
	Register("roots", verbRoots)
	Register("stat", verbStat)
	Register("remove", verbRemove)
	Register("rmtree", verbRmtree)
	Register("rmdir", verbRmdir)
	Register("move", verbMove)
	Register("copy", verbCopy)
	Register("read", verbRead)
	Register("write", verbWrite)
	Register("mkworkdir", verbMkworkdir)
	Register("chmod-work", verbChmodWork)
	Register("qemu-img-info", verbQemuImgInfo)
	Register("sha256", verbSHA256)
	Register("tools-missing", verbToolsMissing)
}

// checkPlain refuses every path in args that is not plain, before any of them
// is acted on.
func checkPlain(args ...string) error {
	for _, a := range args {
		if !PlainPath(a) {
			return Die("unsupported path")
		}
	}
	return nil
}

// WorkName reports whether the last component of p is a work directory name
// (.virticle-*).
func WorkName(p string) bool {
	i := strings.LastIndexByte(p, '/')
	return strings.HasPrefix(p[i+1:], ".virticle-")
}

func verbRoots(c *Ctx) error {
	if err := c.Need(0, 0, "roots"); err != nil {
		return err
	}
	for _, r := range c.Roots() {
		fmt.Fprintln(c.Stdout, r)
	}
	return nil
}

func verbStat(c *Ctx) error {
	if err := c.Need(1, 200, "stat PATH..."); err != nil {
		return err
	}
	if err := checkPlain(c.Args...); err != nil {
		return err
	}
	for _, a := range c.Args {
		r, err := c.ResolveRead(a)
		if err != nil {
			return err
		}
		st, exists, err := r.Lstat()
		r.Close()
		if err != nil {
			return Fail("cannot stat %s", a)
		}
		if !exists {
			fmt.Fprintln(c.Stdout, "MISSING")
			continue
		}
		fmt.Fprintf(c.Stdout, "%s|%d|%d|%o|%d\n", kindName(st), st.Blocks, 512, st.Mode&0o7777, st.Size)
	}
	return nil
}

func verbRemove(c *Ctx) error {
	if err := c.Need(1, 200, "remove PATH..."); err != nil {
		return err
	}
	if err := checkPlain(c.Args...); err != nil {
		return err
	}
	for _, a := range c.Args {
		r, err := c.ResolveMod(a)
		if err != nil {
			return err
		}
		st, exists, err := r.Lstat()
		if err == nil && exists {
			if isDirMode(st.Mode) {
				r.Close()
				return Die("%s is a directory", a)
			}
			err = r.Unlink()
			if isNotExist(err) {
				err = nil
			}
		}
		r.Close()
		if err != nil {
			return Fail("cannot remove %s", a)
		}
	}
	return nil
}

func verbRmtree(c *Ctx) error {
	if err := c.Need(1, 1, "rmtree DIR"); err != nil {
		return err
	}
	r, err := c.ResolveMod(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isDirMode(st.Mode) {
		return Die("%s is not a directory", c.Args[0])
	}
	if !WorkName(r.Path) {
		fd, err := r.OpenDir()
		if err != nil {
			return Fail("cannot remove %s", c.Args[0])
		}
		mst, merr := fstatat(fd, "manifest.json")
		unix.Close(fd)
		if merr != nil || !isRegMode(mst.Mode) {
			return Die("%s is neither a work directory nor a backup", c.Args[0])
		}
	}
	if err := removeTree(r.DirFD(), r.Name, uint64(st.Dev)); err != nil {
		return Fail("cannot remove %s: %v", c.Args[0], err)
	}
	return nil
}

func verbRmdir(c *Ctx) error {
	if err := c.Need(1, 1, "rmdir DIR"); err != nil {
		return err
	}
	r, err := c.ResolveMod(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isDirMode(st.Mode) {
		return Die("%s is not a directory", c.Args[0])
	}
	if err := unix.Unlinkat(r.DirFD(), r.Name, unix.AT_REMOVEDIR); err != nil {
		return Fail("%s is not empty", c.Args[0])
	}
	return nil
}

func verbMove(c *Ctx) error {
	if err := c.Need(2, 2, "move SRC DST"); err != nil {
		return err
	}
	src, err := c.ResolveMod(c.Args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	sst, exists, _ := src.Lstat()
	if !exists {
		return Fail("%s does not exist", c.Args[0])
	}
	dst, err := c.ResolveNew(c.Args[1])
	if err != nil {
		return err
	}
	defer dst.Close()
	err = unix.Renameat2(src.DirFD(), src.Name, dst.DirFD(), dst.Name, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errIs(err, unix.EEXIST):
		return Fail("%s appeared meanwhile; nothing was overwritten", c.Args[1])
	case errIs(err, unix.EINVAL) || errIs(err, unix.ENOSYS) || errIs(err, unix.ENOTSUP):
		// The file system has no RENAME_NOREPLACE (NFS): a hard link is
		// exclusive too, so link then unlink for a file.
		if isRegMode(sst.Mode) {
			if lerr := unix.Linkat(src.DirFD(), src.Name, dst.DirFD(), dst.Name, 0); lerr != nil {
				if errIs(lerr, unix.EEXIST) {
					return Fail("%s appeared meanwhile; nothing was overwritten", c.Args[1])
				}
				return Fail("cannot move %s: %v", c.Args[0], lerr)
			}
			return unlinkFinal(src, c.Args[0])
		}
		// A directory: the existence check above is all there is.
		if rerr := unix.Renameat(src.DirFD(), src.Name, dst.DirFD(), dst.Name); rerr != nil {
			return Fail("cannot move %s: %v", c.Args[0], rerr)
		}
		return nil
	case errIs(err, unix.EXDEV) && isRegMode(sst.Mode):
		return moveAcross(c, src, dst, sst)
	}
	return Fail("cannot move %s: %v", c.Args[0], err)
}

func unlinkFinal(r *Ref, arg string) error {
	if err := r.Unlink(); err != nil {
		return Fail("cannot move %s: %v", arg, err)
	}
	return nil
}

// moveAcross is a move between file systems: copy, then remove the source.
func moveAcross(c *Ctx, src, dst *Ref, sst unix.Stat_t) error {
	in, err := src.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot move %s: %v", c.Args[0], err)
	}
	defer in.Close()
	err = c.CreateNew(dst, sst.Mode&0o7777, func(f *os.File) error {
		if err := CopySparse(f, in, sst.Size); err != nil {
			return Fail("cannot move %s: %v", c.Args[0], err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return unlinkFinal(src, c.Args[0])
}

func verbCopy(c *Ctx) error {
	if err := c.Need(2, 2, "copy SRC DST"); err != nil {
		return err
	}
	src, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	sst, exists, _ := src.Lstat()
	if !exists || !isRegMode(sst.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	dst, err := c.ResolveNew(c.Args[1])
	if err != nil {
		return err
	}
	defer dst.Close()
	in, err := src.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot copy %s: %v", c.Args[0], err)
	}
	defer in.Close()
	return c.CreateNew(dst, sst.Mode&0o7777, func(f *os.File) error {
		if err := CopySparse(f, in, sst.Size); err != nil {
			return Fail("cannot copy %s: %v", c.Args[0], err)
		}
		return nil
	})
}

// LimitArg parses a byte limit argument: 1 to 64 MiB.
func LimitArg(s string) (int64, error) {
	if s == "" || len(s) > 15 || strings.Trim(s, "0123456789") != "" {
		return 0, Die("unsupported limit")
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	if n < 1 || n > 64<<20 {
		return 0, Die("unsupported limit")
	}
	return n, nil
}

func verbRead(c *Ctx) error {
	if err := c.Need(2, 2, "read PATH MAXBYTES"); err != nil {
		return err
	}
	max, err := LimitArg(c.Args[1])
	if err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	f, err := r.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot read %s", c.Args[0])
	}
	defer f.Close()
	// One byte more than asked: the caller sees that the file is larger.
	if _, err := io.Copy(c.Stdout, io.LimitReader(f, max+1)); err != nil {
		return Fail("cannot read %s", c.Args[0])
	}
	return nil
}

func verbWrite(c *Ctx) error {
	if err := c.Need(2, 3, "write PATH MAXBYTES [MODE]"); err != nil {
		return err
	}
	max, err := LimitArg(c.Args[1])
	if err != nil {
		return err
	}
	mode := uint32(0o600)
	if len(c.Args) == 3 {
		switch c.Args[2] {
		case "600":
		case "644":
			mode = 0o644
		default:
			return Die("unsupported mode")
		}
	}
	r, err := c.ResolveNew(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	return c.CreateNew(r, mode, func(f *os.File) error {
		n, err := io.Copy(f, io.LimitReader(c.Stdin, max+1))
		if err != nil {
			return Fail("cannot write %s", c.Args[0])
		}
		if n > max {
			return Fail("input larger than %d bytes", max)
		}
		return nil
	})
}

var tagRE = regexp.MustCompile(`^[a-z]{1,12}$`)

func verbMkworkdir(c *Ctx) error {
	if err := c.Need(2, 2, "mkworkdir DIR TAG"); err != nil {
		return err
	}
	if !tagRE.MatchString(c.Args[1]) {
		return Die("unsupported tag")
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
	for i := 0; i < 100; i++ {
		name := ".virticle-" + c.Args[1] + "." + randName(6)
		err := unix.Mkdirat(fd, name, 0o700)
		if err == nil {
			fmt.Fprintf(c.Stdout, "%s/%s\n", c.Args[0], name)
			return nil
		}
		if !errIs(err, unix.EEXIST) {
			return Fail("cannot create a work directory in %s", c.Args[0])
		}
	}
	return Fail("cannot create a work directory in %s", c.Args[0])
}

func verbChmodWork(c *Ctx) error {
	if err := c.Need(2, 2, "chmod-work MODE DIR"); err != nil {
		return err
	}
	var mode uint32
	switch c.Args[0] {
	case "700":
		mode = 0o700
	case "711":
		mode = 0o711
	default:
		return Die("unsupported mode")
	}
	r, err := c.ResolveMod(c.Args[1])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isDirMode(st.Mode) {
		return Die("%s is not a directory", c.Args[1])
	}
	if !WorkName(r.Path) {
		return Die("%s is not a work directory", c.Args[1])
	}
	fd, err := r.OpenDir()
	if err != nil {
		return Fail("chmod failed")
	}
	defer unix.Close(fd)
	if err := unix.Fchmod(fd, mode); err != nil {
		return Fail("chmod failed")
	}
	return nil
}

func verbSHA256(c *Ctx) error {
	if err := c.Need(1, 1, "sha256 PATH"); err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	f, err := r.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot read %s", c.Args[0])
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return Fail("cannot read %s", c.Args[0])
	}
	fmt.Fprintln(c.Stdout, hex.EncodeToString(h.Sum(nil)))
	return nil
}

func verbQemuImgInfo(c *Ctx) error {
	if err := c.Need(1, 3, "qemu-img-info PATH [chain] [FORMAT]"); err != nil {
		return err
	}
	var chain bool
	format := ""
	for _, a := range c.Args[1:] {
		switch a {
		case "chain":
			chain = true
		case "raw", "qcow2", "vpc", "vhdx", "vdi":
			format = a
		default:
			return Die("unsupported argument")
		}
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	if format == "" {
		format = "raw"
		if IsQcow2(r.Path) {
			format = "qcow2"
		}
	}
	if format == "qcow2" {
		if err := c.CheckChain(r.Path); err != nil {
			return err
		}
	}
	args := []string{"info", "-U", "-f", format}
	if chain {
		args = append(args, "--backing-chain")
	}
	args = append(args, "--output=json", "--", r.Path)
	out, err := c.CombinedOutput("qemu-img", args...)
	if _, ok := err.(*Error); ok {
		return err
	}
	c.Stdout.Write(out)
	if err != nil {
		return ExitCode(ExitStatus(err))
	}
	return nil
}

// builtinTools are the tools the old shell script needed that this program does
// itself, so they are never missing.
var builtinTools = map[string]bool{"tar": true, "sha256sum": true}

func verbToolsMissing(c *Ctx) error {
	if err := c.Need(1, 8, "tools-missing NAME..."); err != nil {
		return err
	}
	for _, t := range c.Args {
		switch t {
		case "qemu-img", "ssh", "scp", "tar", "sha256sum":
		default:
			return Die("unsupported tool name")
		}
	}
	for _, t := range c.Args {
		if !builtinTools[t] && !c.HasTool(t) {
			fmt.Fprintln(c.Stdout, t)
		}
	}
	return nil
}
