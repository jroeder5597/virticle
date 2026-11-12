package nodecmd

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// Low-level helpers on file descriptors. Everything the verbs do to a file is
// done relative to a directory descriptor with O_NOFOLLOW, so a path component
// swapped for a symbolic link after the check cannot redirect the operation.

const oDir = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// fdPath is where the kernel says the open descriptor lives now.
func fdPath(fd int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
}

func openat(dirfd int, name string, flags int, mode uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags|unix.O_CLOEXEC, mode)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

// fstatat is the stat of name below dirfd; a final symbolic link is not followed.
func fstatat(dirfd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

func errIs(err, target error) bool { return errors.Is(err, target) }

func pathMatch(pattern, name string) (bool, error) { return path.Match(pattern, name) }

func isNotExist(err error) bool { return errors.Is(err, unix.ENOENT) }

func isDirMode(m uint32) bool  { return m&unix.S_IFMT == unix.S_IFDIR }
func isRegMode(m uint32) bool  { return m&unix.S_IFMT == unix.S_IFREG }
func isLinkMode(m uint32) bool { return m&unix.S_IFMT == unix.S_IFLNK }

// kindName is the word GNU stat -c %F prints.
func kindName(st unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if st.Size == 0 {
			return "regular empty file"
		}
		return "regular file"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFLNK:
		return "symbolic link"
	case unix.S_IFBLK:
		return "block special file"
	case unix.S_IFCHR:
		return "character special file"
	case unix.S_IFIFO:
		return "fifo"
	case unix.S_IFSOCK:
		return "socket"
	}
	return "weird file"
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randName returns n random letters and digits.
func randName(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

// removeTree removes the directory name below dirfd with everything in it. It
// does not follow symbolic links and does not leave the file system dev.
func removeTree(dirfd int, name string, dev uint64) error {
	fd, err := openat(dirfd, name, oDir, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return err
	}
	if uint64(st.Dev) != dev {
		f.Close()
		return fmt.Errorf("%s is on another file system", name)
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		f.Close()
		return err
	}
	for _, n := range names {
		cst, err := fstatat(fd, n)
		if err != nil {
			f.Close()
			return err
		}
		if isDirMode(cst.Mode) {
			err = removeTree(fd, n, dev)
		} else {
			err = unix.Unlinkat(fd, n, 0)
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	f.Close()
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}
