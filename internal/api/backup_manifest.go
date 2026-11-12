package api

// Pure logic of VM backups: the manifest, names, the libvirt backup XML,
// scanning a pool for backups, retention and vetting an uploaded archive.
//
// A backup is a self-contained directory "<vm>-<UTC timestamp>.vmbackup" in
// a directory-backed storage pool. The manifest is written last, so a
// directory without one is a partial backup that is never listed.

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"virticle/internal/cluster"
)

const (
	backupFormat      = 1
	backupSuffix      = ".vmbackup"
	backupAppVersion  = "virticle dev"
	backupTimeLayout  = "20060102T150405Z"
	backupMaxManifest = 64 << 10
	backupMaxXML      = 1 << 20
	backupMaxDisks    = 32
	backupMaxScan     = 500 // backups listed per pool
	backupNVRAMFile   = "nvram.fd"
)

var (
	backupDirRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}\.vmbackup$`)
	targetDevRE = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)
	sha256RE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const (
	noteSnapshots = "Snapshots are not included: a backup holds the current state of the VM only."
	noteTPM       = "TPM state is not included: a restored VM starts with an empty TPM (BitLocker keys or measured-boot secrets sealed to it will not unseal)."
)

type backupDiskRec struct {
	Target      string `json:"target"`
	File        string `json:"file"`
	Format      string `json:"format"`
	VirtualSize uint64 `json:"virtualSize"`
	Bytes       uint64 `json:"bytes"`
	SHA256      string `json:"sha256,omitempty"`
}

type backupManifest struct {
	Format     int             `json:"format"`
	VM         string          `json:"vm"`
	UUID       string          `json:"uuid"`
	Created    string          `json:"created"` // RFC 3339, UTC
	SourceNode string          `json:"sourceNode"`
	AppVersion string          `json:"appVersion"`
	Live       bool            `json:"live"` // taken while the VM was running
	Disks      []backupDiskRec `json:"disks"`
	NVRAM      string          `json:"nvram,omitempty"` // file name of the saved UEFI variables
	TPM        bool            `json:"tpm"`             // the VM had a TPM (its state is not saved)
	Notes      []string        `json:"notes"`
}

func (m *backupManifest) createdTime() time.Time {
	t, _ := time.Parse(time.RFC3339, m.Created)
	return t
}

// backupDirName is the directory name for a backup of vm taken at t.
func backupDirName(vm string, t time.Time) string {
	return vm + "-" + t.UTC().Format(backupTimeLayout) + backupSuffix
}

// parseBackupManifest reads and vets a manifest. Everything in it ends up in
// file names and commands, so only plain names are accepted.
func parseBackupManifest(raw []byte) (*backupManifest, error) {
	if len(raw) > backupMaxManifest {
		return nil, fmt.Errorf("manifest.json is larger than %d KiB", backupMaxManifest>>10)
	}
	var m backupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json is not valid: %w", err)
	}
	switch {
	case m.Format != backupFormat:
		return nil, fmt.Errorf("unsupported backup format %d (this version reads format %d)", m.Format, backupFormat)
	case !vmNameRE.MatchString(m.VM):
		return nil, errors.New("manifest: the VM name is not valid")
	case len(m.Disks) == 0 || len(m.Disks) > backupMaxDisks:
		return nil, errors.New("manifest: a backup needs between 1 and 32 disks")
	}
	if _, err := cluster.ParseUUID(m.UUID); err != nil {
		return nil, errors.New("manifest: the VM UUID is not valid")
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		return nil, errors.New("manifest: the creation time is not valid")
	}
	seenFile, seenDev := map[string]bool{}, map[string]bool{}
	for _, d := range m.Disks {
		switch {
		case !targetDevRE.MatchString(d.Target) || seenDev[d.Target]:
			return nil, fmt.Errorf("manifest: bad or duplicate disk target %q", d.Target)
		case !plainNameRE.MatchString(d.File) || seenFile[d.File] || reservedBackupFile(d.File):
			return nil, fmt.Errorf("manifest: bad or duplicate file name %q", d.File)
		case d.Format != "qcow2":
			return nil, fmt.Errorf("manifest: disk %s has format %q, only qcow2 is supported", d.Target, d.Format)
		case d.SHA256 != "" && !sha256RE.MatchString(d.SHA256):
			return nil, fmt.Errorf("manifest: disk %s has a malformed checksum", d.Target)
		}
		seenFile[d.File], seenDev[d.Target] = true, true
	}
	if m.NVRAM != "" && m.NVRAM != backupNVRAMFile {
		return nil, fmt.Errorf("manifest: bad NVRAM file name %q", m.NVRAM)
	}
	return &m, nil
}

func reservedBackupFile(n string) bool {
	return n == "manifest.json" || n == "domain.xml" || n == backupNVRAMFile
}

// files lists every file a backup directory consists of.
func (m *backupManifest) files() []string {
	out := []string{"manifest.json", "domain.xml"}
	if m.NVRAM != "" {
		out = append(out, m.NVRAM)
	}
	for _, d := range m.Disks {
		out = append(out, d.File)
	}
	return out
}

// --- libvirt backup XML ---

// backupXML is the <domainbackup> document for a push-mode full backup of the
// given disks into dir. Other disks (read-only, CD-ROMs) are left out
// explicitly. No checkpoint is created, so no dirty bitmaps stay behind.
func backupXML(dir string, disks []migDisk, files map[string]string, skip []string) string {
	var b strings.Builder
	b.WriteString("<domainbackup mode='push'><disks>")
	for _, d := range disks {
		fmt.Fprintf(&b, "<disk name='%s' backup='yes' type='file'><target file='%s'/><driver type='qcow2'/></disk>",
			esc(d.Target), esc(dir+"/"+files[d.Target]))
	}
	for _, t := range skip {
		fmt.Fprintf(&b, "<disk name='%s' backup='no'/>", esc(t))
	}
	b.WriteString("</disks></domainbackup>")
	return b.String()
}

var (
	reBackingOpen  = regexp.MustCompile(`<backingStore\b[^>]*>`)
	reBackingClose = regexp.MustCompile(`</backingStore>`)
)

// stripBackingStore removes <backingStore> elements, which describe the
// backing chain of the disks as they were on the source. A restored disk is a
// single flat image; leaving the element in would make libvirt open a
// backing file that does not exist. Nested chains are removed whole.
func stripBackingStore(x string) string {
	var out strings.Builder
	depth := 0
	for len(x) > 0 {
		o, c := reBackingOpen.FindStringIndex(x), reBackingClose.FindStringIndex(x)
		next := -1
		var open bool
		switch {
		case o != nil && (c == nil || o[0] < c[0]):
			next, open = o[0], true
		case c != nil:
			next = c[0]
		}
		if next < 0 {
			if depth == 0 {
				out.WriteString(x)
			}
			break
		}
		if depth == 0 {
			out.WriteString(x[:next])
		}
		if open {
			end := o[1]
			if !strings.HasSuffix(x[o[0]:end], "/>") {
				depth++
			}
			x = x[end:]
		} else {
			if depth > 0 {
				depth--
			}
			x = x[c[1]:]
		}
		if depth == 0 {
			// Drop the indentation and newline left behind by a removed element.
			x = strings.TrimLeft(x, " \t")
			x = strings.TrimPrefix(x, "\n")
			s := out.String()
			out.Reset()
			out.WriteString(strings.TrimRight(s, " \t"))
		}
	}
	return out.String()
}

// --- scanning pools for backups ---

// scanItem is one backup found in a pool: its directory, size on disk and the
// manifest text.
type scanItem struct {
	Dir      string
	Size     uint64
	Manifest []byte
}

func newScanMarker() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "@@@" + hex.EncodeToString(b)
}

// parseScan splits the output of the node command's backup-scan verb: per
// backup "<marker> <dir>", the size in bytes, the manifest, a newline. Directories outside poolDir or with
// unexpected names are dropped.
func parseScan(out, marker, poolDir string) []scanItem {
	var items []scanItem
	for _, sec := range strings.Split(out, marker+" ")[1:] {
		head, rest, _ := strings.Cut(sec, "\n")
		sz, body, _ := strings.Cut(rest, "\n")
		dir := strings.TrimSpace(head)
		size, _ := strconv.ParseUint(strings.TrimSpace(sz), 10, 64)
		if path.Clean(dir) != dir || path.Dir(dir) != strings.TrimRight(poolDir, "/") || !backupDirRE.MatchString(path.Base(dir)) {
			continue
		}
		items = append(items, scanItem{Dir: dir, Size: size, Manifest: []byte(strings.TrimSpace(body))})
	}
	return items
}

// --- retention ---

type retained struct {
	Key     string // what identifies the backup to the caller
	UUID    string
	Created time.Time
}

// pruneList returns the backups of vm uuid beyond the newest keep, oldest
// last. keep < 1 prunes nothing.
func pruneList(all []retained, uuid string, keep int) []retained {
	if keep < 1 {
		return nil
	}
	var mine []retained
	for _, b := range all {
		if b.UUID == uuid {
			mine = append(mine, b)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Created.After(mine[j].Created) })
	if len(mine) <= keep {
		return nil
	}
	return mine[keep:]
}

// --- vetting an uploaded archive ---

// vetTar copies a tar stream from in to out, refusing anything but a small
// set of regular files with plain names. The archive is rewritten, not
// passed through: headers are rebuilt from the vetted fields, so extensions
// (links, long names, sparse maps) never reach the node's tar. budget is the
// most data accepted (free space on the node minus a margin).
func vetTar(in io.Reader, out io.Writer, budget int64) ([]string, error) {
	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	var names []string
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, badRequest("refusing archive: not a readable tar file: " + err.Error())
		}
		if h.Typeflag != tar.TypeReg {
			return nil, badRequest("refusing archive: " + quoteTag(h.Name) + " is not a regular file (directories, links and devices are not allowed)")
		}
		if !plainNameRE.MatchString(h.Name) {
			return nil, badRequest("refusing archive: member " + quoteTag(h.Name) + " is not a plain file name (no slashes, no ..)")
		}
		if seen[h.Name] {
			return nil, badRequest("refusing archive: duplicate member " + h.Name)
		}
		seen[h.Name] = true
		if len(names) >= backupMaxDisks+3 {
			return nil, badRequest("refusing archive: too many members")
		}
		if h.Size < 0 {
			return nil, badRequest("refusing archive: bad size for " + h.Name)
		}
		if total += h.Size; total > budget {
			return nil, badRequest(fmt.Sprintf("refusing archive: its contents (%d MiB so far) do not fit the free space of the pool", total>>20))
		}
		names = append(names, h.Name)
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: h.Name, Size: h.Size, Mode: 0o600, ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, err
		}
	}
	if len(names) == 0 {
		return nil, badRequest("the archive is empty")
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return names, nil
}
