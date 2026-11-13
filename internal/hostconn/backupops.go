package hostconn

// Typed operations for backups: free space, file sizes, the backup scan, tar
// pack and unpack, and qemu-img convert with progress. As in nodeops.go, each
// one runs a verb of the node command.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FreeBytes is the free space of the file system holding dir.
func (h *Host) FreeBytes(dir string) (int64, error) {
	out, err := h.run("df", dir)
	if err != nil {
		return 0, nodeErr(out, err)
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// FilesSize returns the size in bytes of each named regular file of dir (not
// a symbolic link), or "MISSING". Names must be plain file names.
func (h *Host) FilesSize(dir string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out, err := h.run("files-size", append([]string{dir}, names...)...)
	if err != nil {
		return nil, nodeErr(out, err)
	}
	sizes := strings.Fields(out)
	if len(sizes) != len(names) {
		return nil, errors.New("unexpected answer while checking the files")
	}
	return sizes, nil
}

// ScanBackups lists the backups in a pool directory (marker: "@@@" and 16
// lower-case hex digits). The output is returned even when the scan reports
// unreadable entries.
func (h *Host) ScanBackups(dir, marker string) string {
	out, _ := h.run("backup-scan", dir, marker)
	return out
}

// TarPack writes a tar of the named files of the backup directory dir to out.
func (h *Host) TarPack(ctx context.Context, dir string, names []string, out io.Writer) error {
	return h.pipe(ctx, "tar-pack", append([]string{dir}, names...), nil, out)
}

// TarUnpack unpacks a tar stream into the empty work directory dir.
func (h *Host) TarUnpack(ctx context.Context, dir string, in io.Reader) error {
	return h.pipe(ctx, "tar-unpack", []string{dir}, in, nil)
}

var convertProgressRE = regexp.MustCompile(`\((\d+(?:\.\d+)?)/100%\)`)

func convertProgress(log string) (float64, bool) {
	all := convertProgressRE.FindAllStringSubmatch(log, -1)
	if len(all) == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(all[len(all)-1][1], 64)
	return f / 100, err == nil
}

// Convert runs qemu-img convert from src (format srcFmt) into a new qcow2
// file dest, using the work directory work for its bookkeeping files. The
// conversion is detached from the SSH session and polled, so no long-lived
// channel is needed. stop, when set, is polled; once it returns true Convert
// gives up (the caller then calls RemoveWork, which stops qemu-img).
// progress receives 0..1.
func (h *Host) Convert(work, src, srcFmt, dest string, stop func() bool, progress func(float64)) error {
	return h.ConvertChain(work, src, srcFmt, nil, dest, stop, progress)
}

// ConvertChain is Convert for a VM's backups: src is the full backup and
// overlays the incremental backups on top of it, oldest first (qcow2 images
// that hold only the blocks that changed). dest receives the VM's disk as it
// was at the last of them.
func (h *Host) ConvertChain(work, src, srcFmt string, overlays []string, dest string, stop func() bool, progress func(float64)) error {
	args := append([]string{work, src, srcFmt}, overlays...)
	if out, err := h.RunNode("convert-start", args...); err != nil || out != "started" {
		return fmt.Errorf("cannot start qemu-img: %s", firstLineOf(out, err))
	}
	deadline := time.Now().Add(6 * time.Hour)
	misses := 0
	for {
		time.Sleep(2 * time.Second)
		if stop != nil && stop() {
			return errors.New("conversion cancelled")
		}
		out, err := h.run("convert-poll", work)
		if err != nil {
			if misses++; misses >= 15 {
				return fmt.Errorf("lost contact with the node; qemu-img may still be running (work directory %s, nothing was deleted)", work)
			}
			continue
		}
		misses = 0
		rc, log, _ := strings.Cut(out, "---")
		if p, ok := convertProgress(log); ok && progress != nil {
			progress(p)
		}
		if rc = strings.TrimSpace(rc); rc != "" {
			if rc != "0" {
				lines := strings.Split(strings.TrimSpace(log), "\n")
				return fmt.Errorf("qemu-img convert failed (exit %s): %s", rc, strings.TrimSpace(lines[len(lines)-1]))
			}
			break
		}
		if time.Now().After(deadline) {
			return errors.New("conversion did not finish within 6 hours; it was stopped")
		}
	}
	if out, err := h.RunNode("convert-finish", work, dest); err != nil {
		return fmt.Errorf("cannot move the result into the pool: %s", firstLineOf(out, err))
	}
	return nil
}

// DetachBacking removes the backing file reference from the qcow2 image
// name in the work directory work.
func (h *Host) DetachBacking(work, name string) error {
	if out, err := h.RunNode("backup-detach", work, name); err != nil {
		return fmt.Errorf("cannot detach %s: %s", name, firstLineOf(out, err))
	}
	return nil
}

// RemoveWork stops a conversion still running from the work directory (by the
// PID it recorded, after checking that the process really is qemu-img) and
// deletes the directory with everything in it.
func (h *Host) RemoveWork(dir string) error {
	// Cancelling and the failing task both remove the directory: one of
	// them finds it gone, which is not an error.
	gone := func() bool {
		ex, err := h.Exists(dir)
		return err == nil && !ex[dir]
	}
	if gone() {
		return nil
	}
	if out, err := h.RunNode("convert-kill", dir); err != nil && !gone() {
		return nodeErr(out, err)
	}
	if err := h.RemoveTree(dir); err != nil && !gone() {
		return err
	}
	return nil
}
