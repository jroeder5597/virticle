package hostconn

// Typed operations for replicas (see internal/nodecmd/verbs_repl.go): all of
// them work on plain file names inside a work directory.

import (
	"fmt"
	"strconv"
	"strings"
)

// ReplOverlay creates the empty qcow2 overlay name on the image backing.
func (h *Host) ReplOverlay(work, name, backing string) error {
	if out, err := h.RunNode("repl-overlay", work, name, backing); err != nil {
		return fmt.Errorf("cannot create %s: %s", name, firstLineOf(out, err))
	}
	return nil
}

// ReplRebase points the image name at the file backing without touching data.
func (h *Host) ReplRebase(work, name, backing string) error {
	if out, err := h.RunNode("repl-rebase", work, name, backing); err != nil {
		return fmt.Errorf("cannot rebase %s: %s", name, firstLineOf(out, err))
	}
	return nil
}

// ReplCommit writes the image name (and the overlays between it and base)
// into base, or into its backing file when base is empty. The images stay
// valid if the call is cut short; call it again.
func (h *Host) ReplCommit(work, name, base string) error {
	args := []string{work, name}
	if base != "" {
		args = append(args, base)
	}
	if out, err := h.RunNode("repl-commit", args...); err != nil {
		return fmt.Errorf("cannot commit %s: %s", name, firstLineOf(out, err))
	}
	return nil
}

// QcowBitmapRemove removes the named persistent bitmaps from a qcow2 image that no QEMU has open.
func (h *Host) QcowBitmapRemove(path string, names ...string) error {
	args := append([]string{path}, names...)
	if out, err := h.RunNode("qcow2-bitmap-rm", args...); err != nil {
		return fmt.Errorf("cannot remove the bitmaps of %s: %s", path, firstLineOf(out, err))
	}
	return nil
}

// FileStamp is the modification time (ns) and size of a file.
type FileStamp struct {
	Exists bool
	Mtime  int64
	Size   int64
}

// Stamps returns the modification time and size of each file.
func (h *Host) Stamps(paths ...string) ([]FileStamp, error) {
	out, err := h.RunNode("mtime", paths...)
	if err != nil {
		return nil, nodeErr(out, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("unexpected answer while checking the files")
	}
	res := make([]FileStamp, len(paths))
	for i, l := range lines {
		if l == "MISSING" {
			continue
		}
		a, b, ok := strings.Cut(l, "|")
		m, e1 := strconv.ParseInt(a, 10, 64)
		sz, e2 := strconv.ParseInt(b, 10, 64)
		if !ok || e1 != nil || e2 != nil {
			return nil, fmt.Errorf("unexpected answer while checking the files")
		}
		res[i] = FileStamp{Exists: true, Mtime: m, Size: sz}
	}
	return res, nil
}

// ReplList returns the regular files of a replica directory with their sizes.
func (h *Host) ReplList(work string) (map[string]uint64, error) {
	out, err := h.RunNode("repl-ls", work)
	if err != nil {
		return nil, nodeErr(out, err)
	}
	res := map[string]uint64{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		n, sz, ok := strings.Cut(l, "|")
		v, err := strconv.ParseUint(sz, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("unexpected answer while listing %s", work)
		}
		res[n] = v
	}
	return res, nil
}

// TPMSave copies the emulated TPM's state of the VM (swtpm, one small file) into dst, a new file in a work directory.
// saved is false when the VM has no state yet (it never ran): nothing was written.
func (h *Host) TPMSave(uuid, dst string) (saved bool, err error) {
	out, err := h.RunNode("tpm-save", uuid, dst)
	if err != nil {
		return false, nodeErr(out, err)
	}
	return strings.TrimSpace(out) != "NONE", nil
}

// TPMPlace makes src (a file in a work directory) the state of the VM's emulated TPM on this node.
func (h *Host) TPMPlace(src, uuid string) error {
	out, err := h.RunNode("tpm-place", src, uuid)
	if err != nil {
		return nodeErr(out, err)
	}
	return nil
}

// WorkList returns the replication work directories directly in a pool directory, with their modification time (unix seconds).
func (h *Host) WorkList(dir string) (map[string]int64, error) {
	out, err := h.RunNode("work-ls", dir)
	if err != nil {
		return nil, nodeErr(out, err)
	}
	res := map[string]int64{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		i := strings.LastIndexByte(l, '|')
		if i <= 0 {
			continue
		}
		n, err := strconv.ParseInt(l[i+1:], 10, 64)
		if err != nil {
			continue
		}
		res[l[:i]] = n
	}
	return res, nil
}
