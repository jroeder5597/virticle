package hostconn

// Typed operations that write into storage pools: downloads, image import
// (conversion, OVA extraction) and clone preparation. Like nodeops.go, each one
// runs a verb of the node command, which checks every path itself and has no
// way to take a command from the caller.

import (
	"errors"
	"fmt"
	"path"
	"regexp"
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

var helperPrefixRE = regexp.MustCompile(`^virticle-(node|helper): `)

func helperMessage(out string, err error) string {
	return helperPrefixRE.ReplaceAllString(firstLineOf(out, err), "")
}

var mismatchRE = regexp.MustCompile(`checksum mismatch: got ([0-9a-f]+)`)

// Download has the node fetch URL into Dest. It blocks until the file is in
// place or the download failed; the partial file is removed on every failure.
// The bytes never pass through the controller. See DownloadCancel.
func (h *Host) Download(o DownloadOpts) error {
	args := []string{o.URL, o.Dest}
	if o.Algo != "" {
		args = append(args, o.Algo, o.Digest)
	}
	out, err := h.RunNode("download", args...)
	if err == nil {
		return nil
	}
	if o.Cancelled != nil && o.Cancelled() {
		return ErrCancelled
	}
	if m := mismatchRE.FindStringSubmatch(out); m != nil {
		return &DigestMismatch{Got: m[1]}
	}
	return fmt.Errorf("download failed: %s", helperMessage(out, err))
}

func msgOrDefault(out string, err error, def string) string {
	if err == nil {
		return def
	}
	return helperMessage(out, err)
}

// DownloadCancel stops the download of dest that is running on the node. It
// returns an error when none is.
func (h *Host) DownloadCancel(dest string) error {
	out, err := h.RunNode("download-cancel", dest)
	if err != nil {
		return errors.New(helperMessage(out, err))
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
// named is reported, never opened (an image that refers to
// other files outside the pools is refused). The output of a failing run is
// returned with the error.
func (h *Host) ImageInfo(file string) (string, error) {
	return h.run("image-info", file)
}

// ResolveFile returns the real path of a regular file (symbolic links
// resolved); the file must lie inside a storage pool directory.
func (h *Host) ResolveFile(file string) (string, error) {
	out, err := h.run("realpath", "file", file)
	if err != nil || out == "" {
		return "", errors.New(msgOrDefault(out, err, "no such file"))
	}
	return out, nil
}

// ResolveDir is ResolveFile for a directory.
func (h *Host) ResolveDir(dir string) (string, error) {
	out, err := h.run("realpath", "dir", dir)
	if err != nil || out == "" {
		return "", errors.New(msgOrDefault(out, err, "no such directory"))
	}
	return out, nil
}

// TarList lists a tar archive (tar -tv, quoting style escape). An error means
// the file is not a readable archive.
func (h *Host) TarList(file string) (string, error) {
	return h.run("tar-list", file)
}

// TarExtract extracts the named members (plain file names) of an archive into
// a work directory. The whole archive must consist of regular files with
// plain names.
func (h *Host) TarExtract(file, workDir string, names ...string) (string, error) {
	return h.run("tar-extract", append([]string{file, workDir}, names...)...)
}

// ConvertStart starts, detached from the SSH session, a conversion of src to
// qcow2 into workDir/out.part. Its state is read with ConvertStatus. srcFmt
// is the format ImageInfo reported; the node command recognises it again itself.
func (h *Host) ConvertStart(workDir, src, srcFmt string) error {
	out, err := h.run("import-convert-start", workDir, src)
	if err != nil || out != "started" {
		return fmt.Errorf("cannot start qemu-img: %s", helperMessage(out, err))
	}
	return nil
}

// ConvertStatus returns "<exit code, once finished>\n---\n<tail of the progress log>".
func (h *Host) ConvertStatus(workDir string) (string, error) {
	return h.run("import-convert-status", workDir)
}

// ConvertKill stops the conversion started from workDir, if one still runs,
// and deletes the work directory.
func (h *Host) ConvertKill(workDir string) error {
	if out, err := h.RunNode("import-convert-kill", workDir); err != nil {
		return errors.New(helperMessage(out, err))
	}
	return h.RemoveTree(workDir)
}

// Sysprep prepares disk images for cloning with virt-sysprep. The node command
// uses its own first-boot script and a fixed option set, and only accepts
// qcow2 or raw images inside the pool directories.
func (h *Host) Sysprep(disks ...string) (string, error) {
	return h.run("sysprep", disks...)
}

// HasTool reports whether a program is on the node's PATH (no privilege needed).
func (h *Host) HasTool(name string) bool {
	_, err := h.Run("command -v " + Quote(name))
	return err == nil
}
