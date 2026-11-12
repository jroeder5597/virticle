package hostconn

// Typed operations for backups (see virticle-helper.sh, "BACKUP VERBS"): free
// space, file sizes, the backup scan, tar pack and unpack, and qemu-img
// convert with progress. As in nodeops.go, root and broad sudo run the shell
// the code used before; a scoped login runs the helper verb.

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
	out, err := h.run("df -P -B1 -- "+Quote(dir)+" | awk 'NR==2{print $4}'", "df", dir)
	if err != nil {
		return 0, fmt.Errorf("%s", firstLineOf(out, err))
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// FilesSize returns the size in bytes of each named regular file of dir (not
// a symbolic link), or "MISSING". Names must be plain file names.
func (h *Host) FilesSize(dir string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	script := "cd " + Quote(dir) + " || exit 1; for f in " + quoteAll(names) +
		`; do if [ -f "$f" ] && [ ! -L "$f" ]; then stat -c %s -- "$f"; else echo MISSING; fi; done`
	out, err := h.run(script, "files-size", append([]string{dir}, names...)...)
	if err != nil {
		return nil, fmt.Errorf("%s", firstLineOf(out, err))
	}
	sizes := strings.Fields(out)
	if len(sizes) != len(names) {
		return nil, errors.New("unexpected answer while checking the files")
	}
	return sizes, nil
}

// ScanBackups lists the backups in a pool directory. rootCmd is the shell
// command that does it for root and sudo logins; the scoped helper has the
// same scan built in (marker: "@@@" and 16 lower-case hex digits). The output
// is returned even when find reports unreadable entries.
func (h *Host) ScanBackups(rootCmd, dir, marker string) string {
	out, _ := h.run(rootCmd, "backup-scan", dir, marker)
	return out
}

// TarPack writes a tar of the named files of the backup directory dir to out.
func (h *Host) TarPack(ctx context.Context, dir string, names []string, out io.Writer) error {
	cmd := "cd " + Quote(dir) + " && tar -cf - --format=posix -- " + quoteAll(names)
	return h.pipe(ctx, cmd, "tar-pack", append([]string{dir}, names...), nil, out)
}

// TarUnpack unpacks a tar stream into the empty work directory dir.
func (h *Host) TarUnpack(ctx context.Context, dir string, in io.Reader) error {
	cmd := "umask 077; cd " + Quote(dir) + " && tar --no-same-owner --no-same-permissions -xf -"
	return h.pipe(ctx, cmd, "tar-unpack", []string{dir}, in, nil)
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
	w := func(n string) string { return work + "/" + n }
	if h.helperMode() {
		out, err := h.RunHelper("convert-start", work, src, srcFmt)
		if err != nil || out != "started" {
			return fmt.Errorf("cannot start qemu-img: %s", firstLineOf(out, err))
		}
	} else {
		for _, f := range []string{"rc", "qpid", "prog.log", "out.part"} {
			h.RunRoot("rm -f -- " + Quote(w(f)))
		}
		inner := fmt.Sprintf("qemu-img convert -p -U -f %s -O qcow2 -- %s %s >%s 2>&1 & p=$!; echo $p >%s; wait $p; echo $? >%s",
			Quote(srcFmt), Quote(src), Quote(w("out.part")), Quote(w("prog.log")), Quote(w("qpid")), Quote(w("rc")))
		if out, err := h.RunRoot("umask 077; nohup sh -c " + Quote(inner) + " >/dev/null 2>&1 </dev/null & echo started"); err != nil || out != "started" {
			return fmt.Errorf("cannot start qemu-img: %s %v", out, err)
		}
	}
	deadline := time.Now().Add(6 * time.Hour)
	misses := 0
	for {
		time.Sleep(2 * time.Second)
		if stop != nil && stop() {
			return errors.New("conversion cancelled")
		}
		out, err := h.run("cat -- "+Quote(w("rc"))+" 2>/dev/null; echo ---; tail -c 400 -- "+Quote(w("prog.log"))+" 2>/dev/null | tr '\\r' '\\n'", "convert-poll", work)
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
	if h.helperMode() {
		if out, err := h.RunHelper("convert-finish", work, dest); err != nil {
			return fmt.Errorf("cannot move the result into the pool: %s", firstLineOf(out, err))
		}
		return nil
	}
	// mv -n plus the existence test keeps a race from overwriting anything.
	out, err := h.RunRoot("if [ -e " + Quote(dest) + " ]; then echo 'a file with this name appeared in the pool directory'; exit 1; fi; mv -n -- " + Quote(w("out.part")) + " " + Quote(dest))
	if err != nil {
		return fmt.Errorf("cannot move the result into the pool: %s", out)
	}
	h.RunRoot("rm -f -- " + Quote(w("rc")) + " " + Quote(w("qpid")) + " " + Quote(w("prog.log")))
	return nil
}

// RemoveWork stops a conversion still running from the work directory (by the
// PID it recorded, after checking that the process really is qemu-img) and
// deletes the directory with everything in it.
func (h *Host) RemoveWork(dir string) error {
	if h.helperMode() {
		// Cancelling and the failing task both remove the directory: one of
		// them finds it gone, which is not an error.
		gone := func() bool {
			ex, err := h.Exists(dir)
			return err == nil && !ex[dir]
		}
		if gone() {
			return nil
		}
		if out, err := h.RunHelper("convert-kill", dir); err != nil && !gone() {
			return fmt.Errorf("%s", firstLineOf(out, err))
		}
		if err := h.RemoveTree(dir); err != nil && !gone() {
			return err
		}
		return nil
	}
	q := Quote(dir)
	_, err := h.RunRoot("p=$(cat " + Quote(dir+"/qpid") + " 2>/dev/null); if [ -n \"$p\" ] && [ \"$(cat /proc/$p/comm 2>/dev/null)\" = qemu-img ]; then kill $p; sleep 1; fi; rm -rf -- " + q)
	return err
}
