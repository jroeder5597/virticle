package hostconn

// Typed operations that write into storage pools: downloads, image import
// (conversion, OVA extraction) and clone preparation. Like nodeops.go, root and
// broad sudo run the shell the code used before; the scoped helper account
// runs the matching helper verb, which checks every path itself and has no
// way to take a command from the caller.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ErrCancelled is returned by Download when its Cancelled callback said so.
var ErrCancelled = errors.New("cancelled")

// DigestMismatch is returned by Download when the file's digest differs.
type DigestMismatch struct{ Got string }

func (e *DigestMismatch) Error() string { return "checksum mismatch: the downloaded file has " + e.Got }

// DownloadPart is the name a download of dest is written under until it is
// complete. A dot file: libvirt's directory pools do not list it, so a
// half-written image never shows up as a volume.
func DownloadPart(dest string) string {
	return path.Dir(dest) + "/." + path.Base(dest) + ".part"
}

func downloadPidFile(dest string) string {
	sum := sha256.Sum256([]byte(dest))
	return "/tmp/virticle-dl-" + hex.EncodeToString(sum[:8]) + ".pid"
}

// DownloadOpts describes one download.
type DownloadOpts struct {
	URL  string // http or https
	Dest string // a new file; must not exist
	// Algo (sha256 or sha512) and Digest (lower-case hex) are optional: when
	// given, the file is moved into place only if its digest matches.
	Algo, Digest string
	// Cancelled, when set, is asked between steps and after a failure.
	Cancelled func() bool
}

const curlDownloadOpts = "curl -q -fsSL --proto '=http,https' --proto-redir '=http,https' --max-redirs 10 --connect-timeout 20"

var helperPrefixRE = regexp.MustCompile(`^virticle-helper: `)

func helperMessage(out string, err error) string {
	return helperPrefixRE.ReplaceAllString(firstLineOf(out, err), "")
}

var mismatchRE = regexp.MustCompile(`checksum mismatch: got ([0-9a-f]+)`)

// Download has the node fetch URL into Dest. It blocks until the file is in
// place or the download failed; the partial file is removed on every failure.
// The bytes never pass through the controller. See DownloadCancel.
func (h *Host) Download(o DownloadOpts) error {
	cancelled := func() bool { return o.Cancelled != nil && o.Cancelled() }
	if h.helperMode() {
		args := []string{o.URL, o.Dest}
		if o.Algo != "" {
			args = append(args, o.Algo, o.Digest)
		}
		out, err := h.RunHelper("download", args...)
		if err == nil {
			return nil
		}
		if cancelled() {
			return ErrCancelled
		}
		if m := mismatchRE.FindStringSubmatch(out); m != nil {
			return &DigestMismatch{Got: m[1]}
		}
		return fmt.Errorf("download failed: %s", helperMessage(out, err))
	}

	part, pf := DownloadPart(o.Dest), downloadPidFile(o.Dest)
	defer func() {
		h.RunRoot("rm -f -- " + Quote(part) + " " + Quote(pf))
	}()
	if out, err := h.RunRoot(fmt.Sprintf("if [ -e %[1]s ] || [ -e %[2]s ]; then echo 'a file named %[3]s (or .%[3]s.part) already exists in the pool directory'; exit 1; fi", Quote(o.Dest), Quote(part), path.Base(o.Dest))); err != nil {
		return errors.New(orStr(out, err))
	}
	if cancelled() {
		return ErrCancelled
	}
	// -f: an HTTP error status is a failure, not a saved error page.
	script := fmt.Sprintf("umask 077; %s -o %s -- %s & echo $! > %s; wait $!", curlDownloadOpts, Quote(part), Quote(o.URL), Quote(pf))
	if out, err := h.RunRoot(script); err != nil {
		if cancelled() {
			return ErrCancelled
		}
		return fmt.Errorf("download failed: %s", orStr(out, err))
	}
	if cancelled() {
		return ErrCancelled
	}
	if o.Algo != "" {
		sum := o.Algo + "sum"
		out, err := h.RunRoot(fmt.Sprintf("%s -- %s & echo $! > %s; wait $!", sum, Quote(part), Quote(pf)))
		if err != nil {
			if cancelled() {
				return ErrCancelled
			}
			return fmt.Errorf("computing the %s on the node failed: %s", o.Algo, orStr(out, err))
		}
		got, _, _ := strings.Cut(strings.TrimSpace(out), " ")
		if !strings.EqualFold(got, o.Digest) {
			return &DigestMismatch{Got: got}
		}
		if cancelled() {
			return ErrCancelled
		}
	}
	// mv -n never overwrites; if the name appeared meanwhile it exits 0 and
	// leaves the source behind, which is what the check after it detects.
	if out, err := h.RunRoot(fmt.Sprintf("mv -n -- %[1]s %[2]s && if [ -e %[1]s ]; then echo 'a file with this name appeared in the pool directory; nothing was overwritten'; exit 1; fi", Quote(part), Quote(o.Dest))); err != nil {
		return fmt.Errorf("could not move the image into place: %s", orStr(out, err))
	}
	return nil
}

func msgOrDefault(out string, err error, def string) string {
	if err == nil {
		return def
	}
	return helperMessage(out, err)
}

func orStr(out string, err error) string {
	if out != "" {
		return out
	}
	return err.Error()
}

// DownloadCancel stops the download of dest that is running on the node. It
// returns an error when none is.
func (h *Host) DownloadCancel(dest string) error {
	if h.helperMode() {
		out, err := h.RunHelper("download-cancel", dest)
		if err != nil {
			return errors.New(helperMessage(out, err))
		}
		return nil
	}
	out, err := h.RunRoot("kill $(cat -- " + Quote(downloadPidFile(dest)) + ") 2>/dev/null")
	if err != nil {
		return errors.New(orStr(out, err))
	}
	return nil
}

// FetchText downloads a small text (a checksum file, at most 2 MB) on the
// node. It needs no privilege.
func (h *Host) FetchText(url string) (string, error) {
	return h.Run(fmt.Sprintf("t=$(mktemp) || exit 1; if %s --max-time 60 -o \"$t\" -- %s; then head -c 2000000 \"$t\"; rc=0; else rc=$?; fi; rm -f \"$t\"; exit $rc", curlDownloadOpts, Quote(url)))
}

// ImageInfo runs qemu-img info as JSON on an image that is to be imported.
// The format is recognised from the content; a backing file or extent that is
// named is reported, never opened (and in helper mode an image that refers to
// other files outside the pools is refused). The output of a failing run is
// returned with the error.
func (h *Host) ImageInfo(file string) (string, error) {
	return h.run("qemu-img info -U --output=json -- "+Quote(file)+" 2>&1", "image-info", file)
}

// ResolveFile returns the real path of a regular file (symbolic links
// resolved); in helper mode the file must lie inside a storage pool directory.
func (h *Host) ResolveFile(file string) (string, error) {
	out, err := h.run("p=$(realpath -e -- "+Quote(file)+") && [ -f \"$p\" ] && printf '%s' \"$p\"", "realpath", "file", file)
	if err != nil || out == "" {
		return "", errors.New(msgOrDefault(out, err, "no such file"))
	}
	return out, nil
}

// ResolveDir is ResolveFile for a directory.
func (h *Host) ResolveDir(dir string) (string, error) {
	out, err := h.run("realpath -e -- "+Quote(dir), "realpath", "dir", dir)
	if err != nil || out == "" {
		return "", errors.New(msgOrDefault(out, err, "no such directory"))
	}
	return out, nil
}

// FreeBytes is the free space of the file system holding dir.
func (h *Host) FreeBytes(dir string) (int64, error) {
	out, err := h.run("df -P -B1 -- "+Quote(dir)+" | awk 'NR==2{print $4}'", "free-space", dir)
	if err != nil {
		return 0, errors.New(helperMessage(out, err))
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// TarList lists a tar archive (tar -tv, quoting style escape). An error means
// the file is not a readable archive.
func (h *Host) TarList(file string) (string, error) {
	return h.run("LC_ALL=C TZ=UTC timeout 60 tar --quoting-style=escape -tvf "+Quote(file)+" 2>/dev/null", "tar-list", file)
}

// TarExtract extracts the named members (plain file names) of an archive into
// a work directory. In helper mode the whole archive must consist of regular
// files with plain names.
func (h *Host) TarExtract(file, workDir string, names ...string) (string, error) {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = Quote(n)
	}
	return h.run("tar --no-same-owner --no-same-permissions -xf "+Quote(file)+" -C "+Quote(workDir)+" -- "+strings.Join(quoted, " ")+" 2>&1",
		"tar-extract", append([]string{file, workDir}, names...)...)
}

// ConvertStart starts, detached from the SSH session, a conversion of src to
// qcow2 into workDir/out.part. Its state is read with ConvertStatus. srcFmt
// is the format ImageInfo reported; the helper recognises it again itself.
func (h *Host) ConvertStart(workDir, src, srcFmt string) error {
	files := []string{"rc", "qpid", "prog.log", "out.part"}
	q := make([]string, len(files))
	for i, f := range files {
		q[i] = Quote(workDir + "/" + f)
	}
	inner := fmt.Sprintf("qemu-img convert -p -U -f %s -O qcow2 -- %s %s >%s 2>&1 & p=$!; echo $p >%s; wait $p; echo $? >%s",
		Quote(srcFmt), Quote(src), q[3], q[2], q[1], q[0])
	out, err := h.run("rm -f -- "+strings.Join(q, " ")+"; umask 077; nohup sh -c "+Quote(inner)+" >/dev/null 2>&1 </dev/null & echo started",
		"convert-start", workDir, src)
	if err != nil || out != "started" {
		return fmt.Errorf("cannot start qemu-img: %s", helperMessage(out, err))
	}
	return nil
}

// ConvertStatus returns "<exit code, once finished>\n---\n<tail of the progress log>".
func (h *Host) ConvertStatus(workDir string) (string, error) {
	rc, log := Quote(workDir+"/rc"), Quote(workDir+"/prog.log")
	return h.run("cat -- "+rc+" 2>/dev/null; echo ---; tail -c 400 -- "+log+" 2>/dev/null | tr '\\r' '\\n'", "convert-status", workDir)
}

// ConvertKill stops the conversion started from workDir, if one still runs,
// and deletes the work directory.
func (h *Host) ConvertKill(workDir string) error {
	if h.helperMode() {
		if out, err := h.RunHelper("convert-kill", workDir); err != nil {
			return errors.New(helperMessage(out, err))
		}
		return h.RemoveTree(workDir)
	}
	_, err := h.RunRoot("p=$(cat " + Quote(workDir+"/qpid") + " 2>/dev/null); if [ -n \"$p\" ] && [ \"$(cat /proc/$p/comm 2>/dev/null)\" = qemu-img ]; then kill $p; sleep 1; fi; rm -rf -- " + Quote(workDir))
	return err
}

// Sysprep prepares disk images for cloning with virt-sysprep. Root and sudo
// run it with the given first-boot script; the helper uses its own copy of
// that script (the argument is ignored there), a fixed option set, and only
// accepts qcow2 or raw images inside the pool directories.
func (h *Host) Sysprep(fixups string, disks ...string) (string, error) {
	cmd := "LIBGUESTFS_BACKEND=direct virt-sysprep --run-command " + Quote(fixups)
	for _, d := range disks {
		cmd += " -a " + Quote(d)
	}
	return h.run(cmd+" 2>&1", "sysprep", disks...)
}

// HasTool reports whether a program is on the node's PATH (no privilege needed).
func (h *Host) HasTool(name string) bool {
	_, err := h.Run("command -v " + Quote(name))
	return err == nil
}
