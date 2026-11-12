package hostconn

// Typed operations on a node's files. Feature code calls these and never
// branches on how the login obtains root:
//   - root and broad sudo run the plain shell commands the code used before;
//   - the scoped helper account runs the matching helper verb, which checks
//     every path itself (see virticle-helper.sh: only files inside the
//     storage pool directories and the UEFI NVRAM directory).
// The two paths take the same arguments and return the same results. A path
// that is not plain (letters, digits and . _ + = , @ / -) is refused in both.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrNotHelper is returned by operations that exist for helper-mode nodes only.
var ErrNotHelper = errors.New("this node is not in helper mode")

func (h *Host) helperMode() bool { return h.priv == PrivHelper }

// run executes rootCmd as root, or the helper verb for a scoped login.
func (h *Host) run(rootCmd, sub string, args ...string) (string, error) {
	if h.helperMode() {
		return h.RunHelper(sub, args...)
	}
	return h.RunRoot(rootCmd)
}

// pipe is run for commands whose output must arrive unchanged (not trimmed)
// or that read stdin.
func (h *Host) pipe(ctx context.Context, rootCmd, sub string, args []string, stdin io.Reader, stdout io.Writer) error {
	if h.helperMode() {
		return h.Pipe(ctx, HelperCmd(sub, args...), stdin, stdout)
	}
	return h.PipeRoot(ctx, rootCmd, stdin, stdout)
}

func quoteAll(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// FileStat describes one path on a node.
type FileStat struct {
	Path      string
	Exists    bool
	Kind      string // file | dir | other
	Blocks    uint64 // 512-byte-or-BlockSize units, as stat reports
	BlockSize uint64
	Mode      string // octal permission bits, e.g. 644
	Size      uint64 // apparent size
}

// Allocated is the space the file occupies on disk.
func (f FileStat) Allocated() uint64 { return f.Blocks * f.BlockSize }

// Stat inspects paths (symbolic links are followed). A path that does not
// exist is reported with Exists false; a path the node refuses (outside the
// pools in helper mode) is an error.
func (h *Host) Stat(paths ...string) ([]FileStat, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := h.run(`for f in `+quoteAll(paths)+`; do stat -L -c '%F|%b|%B|%a|%s' -- "$f" 2>/dev/null || echo MISSING; done`, "stat", paths...)
	if err != nil {
		return nil, fmt.Errorf("%s", firstLineOf(out, err))
	}
	return parseStat(paths, out)
}

func firstLineOf(out string, err error) string {
	l, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if l == "" {
		return err.Error()
	}
	return l
}

func parseStat(paths []string, out string) ([]FileStat, error) {
	lines := strings.Split(out, "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("unexpected output while inspecting files: %s", out)
	}
	res := make([]FileStat, len(paths))
	for i, l := range lines {
		res[i].Path = paths[i]
		l = strings.TrimSpace(l)
		if l == "MISSING" {
			continue
		}
		f := strings.Split(l, "|")
		if len(f) != 5 {
			return nil, fmt.Errorf("unexpected output while inspecting %s: %s", paths[i], l)
		}
		b, e1 := strconv.ParseUint(f[1], 10, 64)
		bs, e2 := strconv.ParseUint(f[2], 10, 64)
		sz, e3 := strconv.ParseUint(f[4], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			return nil, fmt.Errorf("unexpected output while inspecting %s: %s", paths[i], l)
		}
		kind := "other"
		switch {
		case strings.HasPrefix(f[0], "regular"):
			kind = "file"
		case f[0] == "directory":
			kind = "dir"
		}
		res[i] = FileStat{Path: paths[i], Exists: true, Kind: kind, Blocks: b, BlockSize: bs, Mode: f[3], Size: sz}
	}
	return res, nil
}

// Exists reports which of paths exist.
func (h *Host) Exists(paths ...string) (map[string]bool, error) {
	st, err := h.Stat(paths...)
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, s := range st {
		m[s.Path] = s.Exists
	}
	return m, nil
}

// Remove deletes files (not directories). A file that does not exist is not an error.
func (h *Host) Remove(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	out, err := h.run("rm -f -- "+quoteAll(paths), "remove", paths...)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// RemoveTree deletes a work directory (a name starting ".virticle-") or a
// directory that holds a manifest.json (a backup), with everything in it.
func (h *Host) RemoveTree(dir string) error {
	q := Quote(dir)
	out, err := h.run(`[ ! -L `+q+` ] && [ -d `+q+` ] && { [ -f `+q+`/manifest.json ] || case `+q+` in */.virticle-*) true ;; *) false ;; esac; } && rm -rf --one-file-system -- `+q, "rmtree", dir)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// Rmdir removes an empty directory.
func (h *Host) Rmdir(dir string) error {
	out, err := h.run("rmdir -- "+Quote(dir), "rmdir", dir)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// Move renames a file or directory; dst must not exist and is never replaced.
func (h *Host) Move(src, dst string) error {
	out, err := h.run("test ! -e "+Quote(dst)+" && mv -T -n -- "+Quote(src)+" "+Quote(dst)+" && test ! -e "+Quote(src), "move", src, dst)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// Copy copies a regular file on the node; dst must not exist and is never replaced.
func (h *Host) Copy(src, dst string) error {
	out, err := h.run("test ! -e "+Quote(dst)+" && umask 077 && cp --preserve=mode -- "+Quote(src)+" "+Quote(dst), "copy", src, dst)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// ReadLimited returns the content of a node file, exactly. A file larger than
// max bytes is refused.
func (h *Host) ReadLimited(path string, max int) ([]byte, error) {
	if max < 1 || max > 64<<20 {
		return nil, fmt.Errorf("unsupported limit %d", max)
	}
	var buf bytes.Buffer
	n := strconv.Itoa(max + 1)
	if err := h.pipe(context.Background(), "head -c "+n+" -- "+Quote(path), "read", []string{path, strconv.Itoa(max)}, nil, &buf); err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if buf.Len() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return buf.Bytes(), nil
}

// WriteFile creates a new file with data (at most max bytes, mode 600 or
// 644). An existing file is never replaced.
func (h *Host) WriteFile(path string, data []byte, max int, mode string) error {
	if mode == "" {
		mode = "600"
	}
	if mode != "600" && mode != "644" {
		return fmt.Errorf("unsupported mode %s", mode)
	}
	if max < 1 || max > 64<<20 || len(data) > max {
		return fmt.Errorf("%s: the content (%d bytes) exceeds the limit", path, len(data))
	}
	q := Quote(path)
	root := "set -C; umask 077; cat > " + q + " && chmod " + mode + " " + q
	return h.pipe(context.Background(), root, "write", []string{path, strconv.Itoa(max), mode}, bytes.NewReader(data), nil)
}

// MkWorkDir creates a private work directory DIR/.virticle-TAG.XXXXXX (TAG:
// lower-case letters) and returns its path.
func (h *Host) MkWorkDir(dir, tag string) (string, error) {
	prefix := strings.TrimSuffix(dir, "/") + "/.virticle-" + tag + "."
	out, err := h.run("umask 077 && mktemp -d -- "+Quote(prefix+"XXXXXX"), "mkworkdir", dir, tag)
	if err != nil || !strings.HasPrefix(out, prefix) || len(out) != len(prefix)+6 || strings.ContainsAny(out, "\n\r\x00") {
		return "", fmt.Errorf("cannot create a work directory in %s on %s: %s", dir, h.Name, firstLineOf(out, errors.New("unexpected answer")))
	}
	return out, nil
}

// ChmodWork sets the mode of a work directory: "700" or "711" (QEMU's user
// must be able to enter a directory it writes into).
func (h *Host) ChmodWork(dir, mode string) error {
	if mode != "700" && mode != "711" {
		return fmt.Errorf("unsupported mode %s", mode)
	}
	out, err := h.run("chmod "+mode+" -- "+Quote(dir), "chmod-work", mode, dir)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// QemuInfo runs qemu-img info as JSON (a shared lock is not taken). chain asks
// for the whole backing chain. The output of a failing run is returned with the error.
func (h *Host) QemuInfo(path string, chain bool) (string, error) {
	flag, args := "", []string{path}
	if chain {
		flag, args = "--backing-chain ", []string{path, "chain"}
	}
	return h.run("qemu-img info -U "+flag+"--output=json -- "+Quote(path)+" 2>&1", "qemu-img-info", args...)
}

// SHA256 returns the SHA-256 of a node file as 64 hex digits.
func (h *Host) SHA256(path string) (string, error) {
	out, err := h.run("sha256sum -- "+Quote(path), "sha256", path)
	f := strings.Fields(out)
	if err != nil || len(f) < 1 || len(f[0]) != 64 || strings.Trim(f[0], "0123456789abcdef") != "" {
		return "", fmt.Errorf("sha256 of %s: %s", path, firstLineOf(out, errors.New("no checksum")))
	}
	return f[0], nil
}

// MissingTools returns those of names (qemu-img, ssh, scp, tar, sha256sum)
// that are not installed on the node.
func (h *Host) MissingTools(names ...string) ([]string, error) {
	out, err := h.run(`for t in `+quoteAll(names)+`; do command -v "$t" >/dev/null 2>&1 || echo "$t"; done`, "tools-missing", names...)
	if err != nil {
		return nil, fmt.Errorf("%s", firstLineOf(out, err))
	}
	return strings.Fields(out), nil
}

// AllowedRoots lists the directories the helper confines file operations to
// (helper mode only): the target directories of the node's dir, fs and netfs
// storage pools and the UEFI NVRAM directory.
func (h *Host) AllowedRoots() ([]string, error) {
	if !h.helperMode() {
		return nil, ErrNotHelper
	}
	out, err := h.RunHelper("roots")
	if err != nil {
		return nil, fmt.Errorf("%s", firstLineOf(out, err))
	}
	return strings.Fields(out), nil
}
