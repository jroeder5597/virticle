package nodecmd

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// nvram-new TEMPLATE DST: make a qcow2 UEFI variable store from the firmware
// package's raw VARS template. libvirt cannot convert the template itself,
// and only a qcow2 variable store lets a VM take internal snapshots.
//
// TEMPLATE must be a regular file of at most 8 MiB below /usr/share (after
// resolving links: Rocky links /usr/share/OVMF/* to /usr/share/edk2/ovmf/*).
// DST must be a new name ending in .qcow2 directly inside the NVRAM directory.
func init() { Register("nvram-new", verbNvramNew) }

const nvramTemplateMax = 8 << 20

func verbNvramNew(c *Ctx) error {
	if err := c.Need(2, 2, "nvram-new TEMPLATE DST"); err != nil {
		return err
	}
	tpl, dstArg := c.Args[0], c.Args[1]
	if !PlainPath(tpl) || !PlainPath(dstArg) {
		return Die("unsupported path")
	}
	share := c.Cfg.ShareDir
	if share == "" {
		share = "/usr/share"
	}
	full, err := filepath.EvalSymlinks(tpl)
	if err != nil {
		return Fail("the firmware template %s does not exist", tpl)
	}
	if !strings.HasPrefix(full, share+"/") {
		return Die("%s is not a firmware template below %s", tpl, share)
	}
	if path.Dir(path.Clean(dstArg)) != path.Clean(c.Cfg.NVRAMDir) || !strings.HasSuffix(dstArg, ".qcow2") {
		return Die("%s is not a .qcow2 file inside the NVRAM directory", dstArg)
	}
	in, err := os.Open(full)
	if err != nil {
		return Fail("cannot read %s: %v", tpl, err)
	}
	st, err := in.Stat()
	in.Close()
	if err != nil || !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > nvramTemplateMax {
		return Fail("%s is not a usable firmware template", tpl)
	}
	if !c.HasTool("qemu-img") {
		return Fail("qemu-img is not installed on this node")
	}
	dst, err := c.ResolveNew(dstArg)
	if err != nil {
		return err
	}
	defer dst.Close()
	return c.CreateNew(dst, 0o600, func(f *os.File) error {
		cmd, err := c.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", "--", full, "/proc/self/fd/3")
		if err != nil {
			return err
		}
		cmd.ExtraFiles = []*os.File{f}
		if out, err := cmd.CombinedOutput(); err != nil {
			return Fail("qemu-img convert failed: %s", strings.TrimSpace(string(out)))
		}
		// The store belongs to the emulator account that owns the NVRAM
		// directory (qemu on Rocky and Fedora, libvirt-qemu on Debian). Left
		// to root, libvirt cannot see the file as that account, takes it for
		// missing and fails the start with "conversion of the nvram template
		// to another target format is not supported".
		if !c.Cfg.Unprivileged {
			if st, err := os.Stat(c.Cfg.NVRAMDir); err == nil {
				if sys, ok := st.Sys().(*syscall.Stat_t); ok {
					if err := f.Chown(int(sys.Uid), int(sys.Gid)); err != nil {
						return Fail("cannot hand the variable store to its owner: %v", err)
					}
				}
			}
		}
		return nil
	})
}
