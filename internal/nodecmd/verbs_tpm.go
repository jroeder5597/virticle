package nodecmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"

	"golang.org/x/sys/unix"
)

// The state of a VM's emulated TPM (swtpm) is one small file, tpm2-00.permall, in the directory libvirt keeps for the VM:
//
//	<TPMDir>/<VM uuid>/tpm2/tpm2-00.permall    (about 6 KiB; owned by the swtpm account, mode 0600)
//
// Replication copies it with every run, so a VM that fails over keeps its TPM (keys sealed to it, BitLocker).
//
//	tpm-save UUID DST    copy the state of the VM to DST, a new file in a work directory (prints NONE and writes nothing when the VM has no state yet)
//	tpm-place SRC UUID   make SRC (a file in a work directory) the state of the VM on this node: the directory is made as
//	                     libvirt makes it, an existing state is replaced in one step. Run it only for a VM that is not running.
//
// Only a VM's own state path is ever opened: the UUID is checked to the letter, the file must be a plain file (never a
// link) of at most 1 MiB, and nothing else below TPMDir can be named.

func init() {
	Register("tpm-save", verbTPMSave)
	Register("tpm-place", verbTPMPlace)
}

const (
	tpmStateName = "tpm2-00.permall"
	tpmStateMax  = 1 << 20
)

var tpmUUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (c *Ctx) tpmDir() string {
	if c.Cfg.TPMDir != "" {
		return c.Cfg.TPMDir
	}
	return "/var/lib/libvirt/swtpm"
}

func verbTPMSave(c *Ctx) error {
	if err := c.Need(2, 2, "tpm-save UUID DST"); err != nil {
		return err
	}
	uuid := c.Args[0]
	if !tpmUUIDRE.MatchString(uuid) {
		return Die("unsupported UUID")
	}
	if !PlainPath(c.Args[1]) {
		return Die("unsupported path")
	}
	src := filepath.Join(c.tpmDir(), uuid, "tpm2", tpmStateName)
	fd, err := unix.Open(src, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		// A VM that never ran has no state yet; its TPM will start new, which is what the copy would have been.
		fmt.Fprintln(c.Stdout, "NONE")
		return nil
	}
	if err != nil {
		return Fail("cannot read the state of the emulated TPM of this VM: %v", err)
	}
	in := os.NewFile(uintptr(fd), src)
	defer in.Close()
	st, err := in.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > tpmStateMax {
		return Fail("the TPM state file is not a plain file of at most 1 MiB")
	}
	dst, err := c.ResolveNew(c.Args[1])
	if err != nil {
		return err
	}
	defer dst.Close()
	return c.CreateNew(dst, 0o600, func(f *os.File) error {
		if _, err := io.Copy(f, io.LimitReader(in, tpmStateMax)); err != nil {
			return Fail("cannot copy the TPM state: %v", err)
		}
		return nil
	})
}

func verbTPMPlace(c *Ctx) error {
	if err := c.Need(2, 2, "tpm-place SRC UUID"); err != nil {
		return err
	}
	uuid := c.Args[1]
	if !tpmUUIDRE.MatchString(uuid) {
		return Die("unsupported UUID")
	}
	src, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	sst, exists, _ := src.Lstat()
	if !exists || !isRegMode(sst.Mode) || sst.Size > tpmStateMax {
		return Fail("%s is not a plain file of at most 1 MiB", c.Args[0])
	}
	in, err := src.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot read %s: %v", c.Args[0], err)
	}
	defer in.Close()
	data, err := io.ReadAll(io.LimitReader(in, tpmStateMax+1))
	if err != nil || len(data) == 0 || len(data) > tpmStateMax {
		return Fail("%s is not usable as TPM state", c.Args[0])
	}

	uid, gid := -1, -1
	if !c.Cfg.Unprivileged {
		u, err := user.Lookup("tss")
		if err != nil {
			return Fail("the swtpm account tss does not exist on this node: install swtpm")
		}
		uid, _ = strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
	}
	base := c.tpmDir()
	if st, err := os.Lstat(base); err != nil || !st.IsDir() {
		return Fail("%s does not exist: libvirt's swtpm support is not installed", base)
	}
	vmDir := filepath.Join(base, uuid)
	dir := filepath.Join(vmDir, "tpm2")
	for _, d := range []string{vmDir, dir} {
		if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
			return Fail("cannot create %s: %v", d, err)
		}
		if st, err := os.Lstat(d); err != nil || !st.IsDir() {
			return Fail("%s is not a directory", d)
		}
		if uid >= 0 {
			if err := os.Chown(d, uid, gid); err != nil {
				return Fail("cannot hand %s to the swtpm account: %v", d, err)
			}
		}
	}
	part := filepath.Join(dir, tpmStateName+".part")
	_ = os.Remove(part)
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Fail("cannot write %s: %v", part, err)
	}
	cancel := c.Cleanup(func() { _ = os.Remove(part) })
	defer cancel()
	defer os.Remove(part)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return Fail("cannot write %s: %v", part, err)
	}
	if uid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			f.Close()
			return Fail("cannot hand the TPM state to the swtpm account: %v", err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return Fail("cannot write %s: %v", part, err)
	}
	if err := f.Close(); err != nil {
		return Fail("cannot write %s: %v", part, err)
	}
	if err := os.Rename(part, filepath.Join(dir, tpmStateName)); err != nil {
		return Fail("cannot put the TPM state in place: %v", err)
	}
	return nil
}
