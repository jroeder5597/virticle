package nodecmd

import (
	"strings"
)

// qcow2-bitmap-rm PATH NAME...: remove the named persistent bitmaps from a qcow2 image that no QEMU has open.
//
// A QEMU that was killed leaves its persistent dirty bitmaps marked "in use" in the image. They can no longer
// be used, and qemu-img refuses to resize the image while they are there ("Bitmap ... is inconsistent").
// qemu-img takes the image's write lock, so the call fails while a VM runs on the file: a running VM's bitmaps
// are never touched by this verb. Only names this program gives its bitmaps are accepted, so a bitmap someone
// made by hand stays.

func init() {
	Register("qcow2-bitmap-rm", verbQcowBitmapRm)
}

func bitmapNameOK(n string) bool {
	return nameOK(n) && (strings.HasPrefix(n, "virticle-") || strings.HasPrefix(n, "backup-"))
}

func verbQcowBitmapRm(c *Ctx) error {
	if err := c.Need(2, 20, "qcow2-bitmap-rm PATH NAME..."); err != nil {
		return err
	}
	if err := checkPlain(c.Args[0]); err != nil {
		return err
	}
	for _, n := range c.Args[1:] {
		if !bitmapNameOK(n) {
			return Die("unsupported bitmap name")
		}
	}
	if !c.HasTool("qemu-img") {
		return Fail("qemu-img is not installed on this node")
	}
	r, err := c.ResolveMod(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, err := r.Lstat()
	if err != nil || !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	if !IsQcow2(r.Path) {
		return Fail("%s is not a qcow2 image", c.Args[0])
	}
	for _, n := range c.Args[1:] {
		out, err := c.CombinedOutput("qemu-img", "bitmap", "--remove", "-f", "qcow2", "--", r.Path, n)
		if err != nil {
			if _, ok := err.(*Error); ok {
				return err
			}
			return Fail("qemu-img could not remove the bitmap %s: %s", n, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
