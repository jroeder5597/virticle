// Package ctlbackup backs up and restores the controller itself: its
// database, its SSH key, the migration certificate authority, the web
// certificates and the other small files that make this controller the one
// the nodes trust. VMs, storage and networks are never part of it; libvirt on
// the nodes holds them.
//
// A backup is one .tar.gz file (optionally wrapped in age encryption, see
// crypt.go):
//
//	manifest.json        first entry; format, versions, every file with its SHA-256
//	data/<path>          one entry per file, <path> relative to the data directory
//
// Only regular files are stored. Restore checks every entry against the
// manifest before anything in the real data directory is touched.
package ctlbackup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"virticle/internal/store"
)

// FormatVersion is the layout of the archive. A restore refuses a higher one.
const FormatVersion = 1

// DBFile is the database's name in the data directory and in the archive.
const DBFile = "virticle.db"

const (
	manifestName = "manifest.json"
	dataPrefix   = "data/"
	maxManifest  = 4 << 20
	// maxTotal bounds what a restore will write, whatever the manifest claims.
	maxTotal = 8 << 30
)

// FileEntry is one file of the backup.
type FileEntry struct {
	Path   string `json:"path"` // relative to the data directory, slash separated
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"` // permission bits only
}

// Manifest describes a backup.
type Manifest struct {
	Format        int         `json:"format"`
	App           string      `json:"app"` // version of the program that made it
	Created       time.Time   `json:"created"`
	Host          string      `json:"host"`
	SchemaVersion int         `json:"schemaVersion"` // of the database in the backup
	Files         []FileEntry `json:"files"`
}

// topFiles and topDirs are what a backup takes from the data directory. The
// list is explicit on purpose: metrics.db (rebuilt by itself), the images
// catalogue cache, the pre-upgrade copies, earlier backups and the lock file
// stay out. secret.key (the one key that encrypts every stored secret) is
// named explicitly: without it the sealed values in the database are lost.
var (
	topFiles = []string{"id_ed25519", "id_ed25519.pub", "known_hosts", "instance-id", "tls-cert.pem", "tls-key.pem", "secret.key"}
	topDirs  = []string{"migration-ca", "web-own", "acme"}
)

// Allowed reports whether a path (relative, slash separated) may be part of a
// backup. Backup uses it to choose files and restore to refuse any other.
func Allowed(p string) bool {
	if p == DBFile {
		return true
	}
	if !fs.ValidPath(p) || p == "." {
		return false
	}
	first, rest, nested := strings.Cut(p, "/")
	if !nested {
		for _, f := range topFiles {
			if p == f {
				return true
			}
		}
		return false
	}
	for _, d := range topDirs {
		if first == d && rest != "" {
			return true
		}
	}
	return false
}

// Source says what to back up.
type Source struct {
	DataDir string
	// Snapshot writes a consistent copy of the database to the given path,
	// which does not exist yet.
	Snapshot func(dst string) error
	App      string
	Host     string
	Now      func() time.Time
}

type item struct {
	entry FileEntry
	data  []byte // contents, for files read into memory
	file  string // contents on disk, for the database copy
}

// Write makes the backup archive and returns its manifest. The archive is
// written to w in one pass.
func Write(w io.Writer, src Source) (Manifest, error) {
	var m Manifest
	if src.Now == nil {
		src.Now = time.Now
	}
	tmp, err := os.MkdirTemp(src.DataDir, ".backup-tmp-")
	if err != nil {
		return m, fmt.Errorf("making a work directory in %s: %w", src.DataDir, err)
	}
	defer os.RemoveAll(tmp)

	// The database first: a consistent copy, never the live file.
	dbCopy := filepath.Join(tmp, DBFile)
	if err := src.Snapshot(dbCopy); err != nil {
		return m, fmt.Errorf("copying the database: %w", err)
	}
	dbItem, err := fileItem(dbCopy, DBFile, 0o600)
	if err != nil {
		return m, err
	}
	items := []item{dbItem}
	more, err := collect(src.DataDir)
	if err != nil {
		return m, err
	}
	items = append(items, more...)
	sort.SliceStable(items[1:], func(i, j int) bool { return items[1+i].entry.Path < items[1+j].entry.Path })

	m = Manifest{Format: FormatVersion, App: src.App, Created: src.Now().UTC().Truncate(time.Second), Host: src.Host}
	if m.SchemaVersion, err = schemaOf(dbCopy); err != nil {
		return m, err
	}
	for _, it := range items {
		m.Files = append(m.Files, it.entry)
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	hdr := func(name string, size int64, mode int64) *tar.Header {
		return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: mode, ModTime: m.Created, Format: tar.FormatPAX}
	}
	if err := tw.WriteHeader(hdr(manifestName, int64(len(mb)), 0o600)); err != nil {
		return m, err
	}
	if _, err := tw.Write(mb); err != nil {
		return m, err
	}
	for _, it := range items {
		if err := tw.WriteHeader(hdr(dataPrefix+it.entry.Path, it.entry.Size, 0o600)); err != nil {
			return m, err
		}
		if it.file != "" {
			f, err := os.Open(it.file)
			if err != nil {
				return m, err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return m, err
			}
			continue
		}
		if _, err := tw.Write(it.data); err != nil {
			return m, err
		}
	}
	if err := tw.Close(); err != nil {
		return m, err
	}
	return m, gz.Close()
}

func schemaOf(dbFile string) (int, error) { return store.FileSchemaVersion(dbFile) }

// publicFile is true for files that hold no secret.
func publicFile(rel string) bool {
	return strings.HasSuffix(rel, ".pub") || strings.HasSuffix(rel, "cert.pem") || rel == "known_hosts" || rel == "instance-id"
}

// fileItem hashes a file on disk that nothing else changes.
func fileItem(p, rel string, mode uint32) (item, error) {
	f, err := os.Open(p)
	if err != nil {
		return item{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return item{}, err
	}
	return item{file: p, entry: FileEntry{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Mode: mode}}, nil
}

// collect reads the small files. They are read into memory once, so the size
// and checksum in the manifest are those of the bytes that are archived even
// if a certificate is renewed meanwhile.
func collect(dir string) ([]item, error) {
	var out []item
	add := func(rel string) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil // symlinks and special files are never followed
		}
		if fi.Size() > 64<<20 {
			return fmt.Errorf("%s is unexpectedly large (%d bytes)", rel, fi.Size())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		mode := uint32(0o600)
		if fi.Mode().Perm()&0o004 != 0 && publicFile(rel) {
			mode = 0o644 // public files stay readable, as they were
		}
		out = append(out, item{data: b, entry: FileEntry{Path: rel, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), Mode: mode}})
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if Allowed(e.Name()) && e.Name() != DBFile {
			if err := add(e.Name()); err != nil {
				return nil, err
			}
		}
	}
	for _, d := range topDirs {
		root := filepath.Join(dir, d)
		if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
			continue
		}
		err := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			return add(filepath.ToSlash(rel))
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Extract reads a backup archive from r and writes its files below dest
// (which must exist and be empty). Every entry is checked against the manifest
// while it is read; any problem is an error and the caller removes dest.
func Extract(r io.Reader, dest string) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, errors.New("this is not a Virticle backup (not a gzip archive)")
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != manifestName || h.Typeflag != tar.TypeReg {
		return m, errors.New("this is not a Virticle backup (no manifest.json at the start)")
	}
	if h.Size > maxManifest {
		return m, errors.New("the manifest is too large")
	}
	mb, err := io.ReadAll(io.LimitReader(tr, maxManifest))
	if err != nil {
		return m, fmt.Errorf("reading the manifest: %w", err)
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		return m, fmt.Errorf("the manifest is damaged: %w", err)
	}
	if err := m.validate(); err != nil {
		return m, err
	}
	want := map[string]FileEntry{}
	var total int64
	for _, f := range m.Files {
		want[f.Path] = f
		total += f.Size
	}
	if total > maxTotal {
		return m, errors.New("the manifest lists more data than a controller backup can hold")
	}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, fmt.Errorf("reading the archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return m, fmt.Errorf("the archive holds an entry that is not a plain file (%q); a Virticle backup never does", h.Name)
		}
		rel, ok := strings.CutPrefix(h.Name, dataPrefix)
		if !ok || !Allowed(rel) {
			return m, fmt.Errorf("the archive holds an entry that does not belong in a backup (%q)", h.Name)
		}
		f, listed := want[rel]
		if !listed {
			return m, fmt.Errorf("the archive holds %q, which the manifest does not list", rel)
		}
		if seen[rel] {
			return m, fmt.Errorf("the archive holds %q twice", rel)
		}
		seen[rel] = true
		if h.Size != f.Size {
			return m, fmt.Errorf("%s has a different size than the manifest says", rel)
		}
		if err := extractFile(tr, dest, f); err != nil {
			return m, err
		}
	}
	for _, f := range m.Files {
		if !seen[f.Path] {
			return m, fmt.Errorf("%s is listed in the manifest but missing from the archive", f.Path)
		}
	}
	return m, nil
}

func extractFile(r io.Reader, dest string, f FileEntry) error {
	p := filepath.Join(dest, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(f.Mode))
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", f.Path, err)
	}
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s does not match its checksum: the backup is damaged or was changed", f.Path)
	}
	return os.Chmod(p, os.FileMode(f.Mode))
}

func (m *Manifest) validate() error {
	if m.Format < 1 {
		return errors.New("this is not a Virticle backup (the manifest has no format version)")
	}
	if m.Format > FormatVersion {
		return fmt.Errorf("this backup has format %d; this program reads up to format %d. Use a newer virticle to restore it", m.Format, FormatVersion)
	}
	seen := map[string]bool{}
	hasDB, hasKey := false, false
	for _, f := range m.Files {
		if !Allowed(f.Path) || path.Clean(f.Path) != f.Path {
			return fmt.Errorf("the manifest lists a path that does not belong in a backup (%q)", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("the manifest lists %q twice", f.Path)
		}
		seen[f.Path] = true
		if f.Size < 0 || len(f.SHA256) != sha256.Size*2 {
			return fmt.Errorf("the manifest entry for %q is not valid", f.Path)
		}
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			return fmt.Errorf("the manifest entry for %q is not valid", f.Path)
		}
		if f.Mode&^0o644 != 0 || f.Mode&0o400 == 0 {
			return fmt.Errorf("the manifest gives %q an unsafe mode", f.Path)
		}
		hasDB = hasDB || f.Path == DBFile
		hasKey = hasKey || f.Path == "id_ed25519"
	}
	if !hasDB || !hasKey {
		return errors.New("the backup has no database or no SSH key; it is not a complete controller backup")
	}
	return nil
}
