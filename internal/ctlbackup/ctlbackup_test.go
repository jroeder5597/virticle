package ctlbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"virticle/internal/store"
)

// newDataDir makes a data directory with a real database and the usual files.
func newDataDir(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateFirstUser("admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"id_ed25519":                "PRIVATE KEY",
		"id_ed25519.pub":            "ssh-ed25519 AAAA virticle-controller",
		"known_hosts":               "node1 ssh-ed25519 BBBB",
		"instance-id":               "abc123def456",
		"tls-cert.pem":              "CERT",
		"tls-key.pem":               "TLSKEY",
		"migration-ca/ca-cert.pem":  "CACERT",
		"migration-ca/ca-key.pem":   "CAKEY",
		"web-own/x1/cert.pem":       "OWNCERT",
		"acme/acme_account+key":     "ACME",
		"secrets.key":               "SETTINGSKEY",
		"metrics.db":                "MUST NOT BE SAVED",
		"images.json":               "cache",
		"virticle.db.bak-1-2020":    "old copy",
		"backups/virticle-backup-x": "an earlier backup",
		LockName:                    "",
	}
	for n, c := range files {
		p := filepath.Join(dir, filepath.FromSlash(n))
		os.MkdirAll(filepath.Dir(p), 0o700)
		mode := os.FileMode(0o600)
		if n == "id_ed25519.pub" {
			mode = 0o644
		}
		if err := os.WriteFile(p, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("/etc/passwd", filepath.Join(dir, "known_hosts.link"))
	os.Symlink("/etc/passwd", filepath.Join(dir, "acme", "linked"))
	return dir, st
}

func srcFor(dir string, st *store.Store) Source {
	return Source{DataDir: dir, Snapshot: st.SnapshotTo, App: "test", Host: "ctl1", Now: func() time.Time { return time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC) }}
}

func makeBackup(t *testing.T, dir string, st *store.Store) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := Write(&b, srcFor(dir, st)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func entryNames(t *testing.T, archive []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		out = append(out, h.Name)
	}
	return out
}

func TestBackupContentsAndManifest(t *testing.T) {
	dir, st := newDataDir(t)
	arc := makeBackup(t, dir, st)
	names := strings.Join(entryNames(t, arc), " ")
	if !strings.HasPrefix(names, "manifest.json ") {
		t.Fatalf("manifest must come first: %s", names)
	}
	for _, want := range []string{"data/virticle.db", "data/id_ed25519", "data/id_ed25519.pub", "data/known_hosts", "data/instance-id",
		"data/tls-cert.pem", "data/tls-key.pem", "data/migration-ca/ca-key.pem", "data/migration-ca/ca-cert.pem", "data/web-own/x1/cert.pem",
		"data/acme/acme_account+key", "data/secrets.key"} {
		if !strings.Contains(names+" ", want+" ") {
			t.Errorf("%s missing from %s", want, names)
		}
	}
	for _, bad := range []string{"metrics.db", "images.json", ".bak-", "backups", "lock", "link", "linked", "passwd"} {
		if strings.Contains(names, bad) {
			t.Errorf("%q must not be in the backup: %s", bad, names)
		}
	}
	dest := t.TempDir()
	m, err := Extract(bytes.NewReader(arc), dest)
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != FormatVersion || m.App != "test" || m.Host != "ctl1" || m.SchemaVersion != store.SchemaVersion || m.Created.Year() != 2026 {
		t.Errorf("manifest: %+v", m)
	}
	for _, f := range m.Files {
		b, err := os.ReadFile(filepath.Join(dest, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Size {
			t.Errorf("%s: checksum or size differs", f.Path)
		}
		fi, _ := os.Stat(filepath.Join(dest, f.Path))
		if fi.Mode().Perm() != os.FileMode(f.Mode) {
			t.Errorf("%s: mode %v, manifest %o", f.Path, fi.Mode().Perm(), f.Mode)
		}
		if strings.Contains(f.Path, "key") && f.Mode != 0o600 {
			t.Errorf("%s must be private: %o", f.Path, f.Mode)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "id_ed25519")); string(b) != "PRIVATE KEY" {
		t.Error("key contents differ")
	}
	if fi, _ := os.Stat(filepath.Join(dest, "id_ed25519.pub")); fi.Mode().Perm() != 0o644 {
		t.Errorf("public key mode %v", fi.Mode().Perm())
	}
}

func TestBackupOfLiveDatabaseIsConsistent(t *testing.T) {
	dir, st := newDataDir(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // a busy controller: tasks keep being written during the backup
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				tk, err := st.StartTask("k", "t", "n", "u")
				if err == nil {
					st.FinishTask(&tk, nil)
				}
			}
		}
	}()
	for i := 0; i < 5; i++ {
		arc := makeBackup(t, dir, st)
		dest := t.TempDir()
		if _, err := Extract(bytes.NewReader(arc), dest); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FileSchemaVersion(filepath.Join(dest, DBFile)); err != nil { // integrity_check
			t.Fatalf("backup %d: %v", i, err)
		}
		cp, err := store.Open(filepath.Join(dest, DBFile))
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := cp.UserCount(); n != 1 {
			t.Errorf("users in copy: %d", n)
		}
		tasks, _ := cp.Tasks(1000000)
		for _, tk := range tasks {
			if tk.Status != "ok" && tk.Status != "failed" && tk.Status != "running" {
				t.Errorf("odd task %+v", tk)
			}
		}
		cp.Close()
	}
	close(stop)
	wg.Wait()
}

// rawArchive builds an archive from named contents; the manifest lists those
// in files (path -> content) and entries may differ to build bad archives.
type rawEntry struct {
	name    string
	body    string
	typ     byte
	link    string
	skipMan bool // not listed in the manifest
}

func rawArchive(t *testing.T, schema int, entries []rawEntry, mutate func(*Manifest)) []byte {
	t.Helper()
	m := Manifest{Format: FormatVersion, App: "t", Created: time.Now().UTC(), Host: "h", SchemaVersion: schema}
	for _, e := range entries {
		if e.skipMan || e.typ != 0 {
			continue
		}
		p := strings.TrimPrefix(e.name, "data/")
		sum := sha256.Sum256([]byte(e.body))
		m.Files = append(m.Files, FileEntry{Path: p, Size: int64(len(e.body)), SHA256: hex.EncodeToString(sum[:]), Mode: 0o600})
	}
	if mutate != nil {
		mutate(&m)
	}
	mb, _ := json.Marshal(m)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(h *tar.Header, body string) {
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		h.Size = int64(len(body))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		h.Mode = 0o600
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte(body))
		}
	}
	put(&tar.Header{Name: manifestName}, string(mb))
	for _, e := range entries {
		put(&tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link}, e.body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// goodEntries returns entries with a real database and the key.
func goodEntries(t *testing.T, dir string, st *store.Store) []rawEntry {
	t.Helper()
	cp := filepath.Join(t.TempDir(), "db")
	if err := st.SnapshotTo(cp); err != nil {
		t.Fatal(err)
	}
	db, _ := os.ReadFile(cp)
	return []rawEntry{{name: "data/virticle.db", body: string(db)}, {name: "data/id_ed25519", body: "KEY"}}
}

func TestExtractRefusesBadArchives(t *testing.T) {
	dir, st := newDataDir(t)
	good := goodEntries(t, dir, st)
	with := func(extra ...rawEntry) []rawEntry { return append(append([]rawEntry{}, good...), extra...) }
	cases := map[string][]byte{
		"path traversal":   rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/../../etc/evil", body: "x"}), nil),
		"absolute path":    rawArchive(t, store.SchemaVersion, with(rawEntry{name: "/etc/evil", body: "x"}), nil),
		"traversal in dir": rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/acme/../../x", body: "x"}), nil),
		"symlink":          rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", typ: tar.TypeSymlink, link: "/etc/shadow"}), nil),
		"hard link":        rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", typ: tar.TypeLink, link: "data/id_ed25519"}), nil),
		"directory entry":  rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/acme/", typ: tar.TypeDir}), nil),
		"unlisted entry":   rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", body: "x", skipMan: true}), nil),
		"not allowed name": rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/metrics.db", body: "x"}), nil),
		"duplicate entry":  rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", body: "x"}, rawEntry{name: "data/known_hosts", body: "x", skipMan: true}), nil),
		"outside data/":    rawArchive(t, store.SchemaVersion, with(rawEntry{name: "known_hosts", body: "x", skipMan: true}), nil),
		"missing file": rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", body: "x"}), func(m *Manifest) {
			m.Files = append(m.Files, FileEntry{Path: "instance-id", SHA256: strings.Repeat("0", 64), Mode: 0o600})
		}),
		"wrong checksum": rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", body: "x"}), func(m *Manifest) { m.Files[len(m.Files)-1].SHA256 = strings.Repeat("a", 64) }),
		"wrong size":     rawArchive(t, store.SchemaVersion, with(rawEntry{name: "data/known_hosts", body: "x"}), func(m *Manifest) { m.Files[len(m.Files)-1].Size = 7 }),
		"manifest bad path": rawArchive(t, store.SchemaVersion, good, func(m *Manifest) {
			m.Files = append(m.Files, FileEntry{Path: "../x", SHA256: strings.Repeat("0", 64), Mode: 0o600})
		}),
		"manifest bad mode": rawArchive(t, store.SchemaVersion, good, func(m *Manifest) { m.Files[1].Mode = 0o4777 }),
		"no database":       rawArchive(t, store.SchemaVersion, good[1:], nil),
		"newer format":      rawArchive(t, store.SchemaVersion, good, func(m *Manifest) { m.Format = 99 }),
		"not gzip":          []byte("hello"),
		"gzip but not backup": func() []byte {
			var b bytes.Buffer
			g := gzip.NewWriter(&b)
			g.Write([]byte("hello"))
			g.Close()
			return b.Bytes()
		}(),
	}
	for name, arc := range cases {
		dest := t.TempDir()
		if _, err := Extract(bytes.NewReader(arc), dest); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the control: the good entries alone are accepted
	if _, err := Extract(bytes.NewReader(rawArchive(t, store.SchemaVersion, good, nil)), t.TempDir()); err != nil {
		t.Errorf("good archive refused: %v", err)
	}
	// nothing escaped
	if _, err := os.Stat("/etc/evil"); err == nil {
		t.Error("wrote outside")
	}
}

func TestTamperDetection(t *testing.T) {
	dir, st := newDataDir(t)
	arc := makeBackup(t, dir, st)
	// Rebuild the archive with one byte of the SSH key changed but the old manifest.
	gz, _ := gzip.NewReader(bytes.NewReader(arc))
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	ogz := gzip.NewWriter(&out)
	tw := tar.NewWriter(ogz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		if h.Name == "data/id_ed25519" {
			body = []byte("PRIVATE KEY!") // same listed name, changed content (size differs)
			h.Size = int64(len(body))
			if len(body) == len("PRIVATE KEY") {
				t.Fatal("test bug")
			}
		}
		tw.WriteHeader(h)
		tw.Write(body)
	}
	tw.Close()
	ogz.Close()
	if _, err := Extract(bytes.NewReader(out.Bytes()), t.TempDir()); err == nil {
		t.Error("changed key accepted")
	}
	// Same size, different byte: only the checksum can tell.
	gz, _ = gzip.NewReader(bytes.NewReader(arc))
	tr = tar.NewReader(gz)
	out.Reset()
	ogz = gzip.NewWriter(&out)
	tw = tar.NewWriter(ogz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		if h.Name == "data/id_ed25519" {
			body[0] ^= 1
		}
		tw.WriteHeader(h)
		tw.Write(body)
	}
	tw.Close()
	ogz.Close()
	_, err := Extract(bytes.NewReader(out.Bytes()), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("flipped byte: %v", err)
	}
	// Damage to the compressed stream.
	bad := append([]byte(nil), arc...)
	bad[len(bad)/2] ^= 0xff
	if _, err := Extract(bytes.NewReader(bad), t.TempDir()); err == nil {
		t.Error("damaged stream accepted")
	}
	// A truncated file.
	if _, err := Extract(bytes.NewReader(arc[:len(arc)-40]), t.TempDir()); err == nil {
		t.Error("truncated archive accepted")
	}
}

func TestEncryptionRoundTrip(t *testing.T) {
	dir, st := newDataDir(t)
	path := filepath.Join(t.TempDir(), "b.tar.gz.age")
	if _, _, err := WriteFile(path, srcFor(dir, st), "a long enough passphrase"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !IsEncrypted(raw) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("not encrypted")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	open := func(b []byte, pass string, have bool) (io.Reader, error) {
		var ask func() (string, error)
		if have {
			ask = func() (string, error) { return pass, nil }
		}
		return Open(bytes.NewReader(b), ask)
	}
	if _, err := open(raw, "", false); !errors.Is(err, ErrNeedPassphrase) {
		t.Errorf("no passphrase: %v", err)
	}
	if _, err := open(raw, "another passphrase", true); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	r, err := open(raw, "a long enough passphrase", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(r, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// a changed byte inside the ciphertext is an error, not garbage
	bad := append([]byte(nil), raw...)
	bad[len(bad)-20] ^= 1
	r, err = open(bad, "a long enough passphrase", true)
	if err == nil {
		_, err = Extract(r, t.TempDir())
	}
	if err == nil {
		t.Error("tampered ciphertext accepted")
	}
	// plain files pass through untouched, with or without a passphrase function
	plain := makeBackup(t, dir, st)
	r, err = open(plain, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(r, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if CheckPassphrase("short") == nil || CheckPassphrase("long enough pass") != nil {
		t.Error("passphrase policy")
	}
}

type fakeHost struct {
	root    bool
	exist   *Owner
	service *Owner
	active  bool
	chowned map[string]Owner
}

func (f *fakeHost) IsRoot() bool { return f.root }
func (f *fakeHost) OwnerOf(string) (Owner, bool) {
	if f.exist == nil {
		return Owner{}, false
	}
	return *f.exist, true
}
func (f *fakeHost) ServiceAccount() (Owner, bool) {
	if f.service == nil {
		return Owner{}, false
	}
	return *f.service, true
}
func (f *fakeHost) Chown(p string, o Owner) error {
	if f.chowned == nil {
		f.chowned = map[string]Owner{}
	}
	f.chowned[filepath.Base(p)] = o
	return nil
}
func (f *fakeHost) ServiceActive(string) bool { return f.active }

func TestPickOwner(t *testing.T) {
	svc, other, root := &Owner{990, 990}, &Owner{1000, 1000}, &Owner{0, 0}
	cases := []struct {
		name              string
		isRoot            bool
		existing, service *Owner
		want              *Owner
	}{
		{"not root", false, other, svc, nil},
		{"root, existing owner wins", true, other, svc, other},
		{"root, fresh machine uses the service account", true, nil, svc, svc},
		{"root, root-owned directory uses the service account", true, root, svc, svc},
		{"root, no account", true, nil, nil, nil},
	}
	for _, c := range cases {
		got := PickOwner(c.isRoot, c.existing, c.service)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
	}
}

func opts(arc []byte, dir string, h Host) RestoreOptions {
	return RestoreOptions{Archive: bytes.NewReader(arc), DataDir: dir, Host: h, SchemaMax: store.SchemaVersion,
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}
}

func TestRestoreMovesOldDirectoryAsideAndSetsOwner(t *testing.T) {
	src, st := newDataDir(t)
	arc := makeBackup(t, src, st)
	target := filepath.Join(t.TempDir(), "virticle")
	os.Mkdir(target, 0o700)
	os.WriteFile(filepath.Join(target, "old-file"), []byte("old"), 0o600)
	h := &fakeHost{root: true, exist: &Owner{111, 222}}
	res, err := Restore(opts(arc, target, h))
	if err != nil {
		t.Fatal(err)
	}
	if want := target + ".before-restore-20260102T030405Z"; res.Aside != want {
		t.Errorf("aside %q", res.Aside)
	}
	if b, _ := os.ReadFile(filepath.Join(res.Aside, "old-file")); string(b) != "old" {
		t.Error("old data lost")
	}
	if b, _ := os.ReadFile(filepath.Join(target, "id_ed25519")); string(b) != "PRIVATE KEY" {
		t.Error("key not restored")
	}
	if _, err := os.Stat(filepath.Join(target, "metrics.db")); err == nil {
		t.Error("metrics.db restored")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", fi.Mode().Perm())
	}
	if o := h.chowned["id_ed25519"]; o != (Owner{111, 222}) {
		t.Errorf("owner %+v", o)
	}
	if o := h.chowned["virticle.db"]; o != (Owner{111, 222}) {
		t.Errorf("db owner %+v", o)
	}
	left, _ := filepath.Glob(target + ".restore-tmp-*")
	if len(left) != 0 {
		t.Errorf("staging left: %v", left)
	}
	// the restored database opens and holds the user
	rs, err := store.Open(filepath.Join(target, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if n, _ := rs.UserCount(); n != 1 {
		t.Errorf("users %d", n)
	}
	// a second restore in the same second does not overwrite the first aside directory
	res2, err := Restore(opts(arc, target, &fakeHost{}))
	if err != nil || res2.Aside == res.Aside {
		t.Errorf("second aside: %v %q", err, res2.Aside)
	}
}

func TestRestoreIntoMissingOrEmptyDirectory(t *testing.T) {
	src, st := newDataDir(t)
	arc := makeBackup(t, src, st)
	parent := t.TempDir()
	for _, pre := range []bool{false, true} {
		target := filepath.Join(parent, fmt.Sprint("d", pre))
		if pre {
			os.Mkdir(target, 0o755)
		}
		res, err := Restore(opts(arc, target, &fakeHost{root: true, service: &Owner{5, 5}}))
		if err != nil || res.Aside != "" {
			t.Fatalf("%v %q", err, res.Aside)
		}
		if _, err := os.Stat(filepath.Join(target, DBFile)); err != nil {
			t.Error(err)
		}
		if res.Owner == nil || *res.Owner != (Owner{5, 5}) {
			t.Errorf("owner %v", res.Owner)
		}
	}
}

func TestRestoreRefusals(t *testing.T) {
	src, st := newDataDir(t)
	arc := makeBackup(t, src, st)
	target := filepath.Join(t.TempDir(), "virticle")
	os.Mkdir(target, 0o700)
	os.WriteFile(filepath.Join(target, "keep"), []byte("1"), 0o600)
	untouched := func() {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(b) != "1" {
			t.Fatal("existing data directory was touched")
		}
		if m, _ := filepath.Glob(target + ".*"); len(m) != 0 {
			t.Fatalf("leftovers: %v", m)
		}
	}

	// the service is running (systemd says so)
	if _, err := Restore(opts(arc, target, &fakeHost{active: true})); !errors.As(err, new(ServiceRunningError)) {
		t.Errorf("service active: %v", err)
	}
	untouched()

	// the controller holds the lock
	lk, err := LockDataDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(opts(arc, target, &fakeHost{})); !errors.As(err, new(ServiceRunningError)) {
		t.Errorf("locked: %v", err)
	}
	if _, err := LockDataDir(target); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: %v", err)
	}
	lk.Close()
	os.Remove(filepath.Join(target, LockName))
	untouched()

	// a backup from a newer schema
	newer := rawArchive(t, store.SchemaVersion+1, goodEntriesWithVersion(t, store.SchemaVersion+1), nil)
	_, err = Restore(opts(newer, target, &fakeHost{}))
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer schema: %v", err)
	}
	untouched()

	// a damaged archive
	bad := append([]byte(nil), arc...)
	bad[len(bad)/3] ^= 0xff
	if _, err := Restore(opts(bad, target, &fakeHost{})); err == nil {
		t.Error("damaged archive restored")
	}
	untouched()

	// an older schema is fine (the normal migration runs at the next start)
	older := rawArchive(t, 0, goodEntriesWithVersion(t, 0), nil)
	if _, err := Restore(opts(older, target, &fakeHost{})); err != nil {
		t.Errorf("older schema: %v", err)
	}
}

// goodEntriesWithVersion builds a database file whose PRAGMA user_version is v.
func goodEntriesWithVersion(t *testing.T, v int) []rawEntry {
	t.Helper()
	dir, st := newDataDir(t)
	cp := filepath.Join(t.TempDir(), "db")
	if err := st.SnapshotTo(cp); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// user_version lives at byte offset 60 of the header (big endian)
	b, _ := os.ReadFile(cp)
	b[60], b[61], b[62], b[63] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	_ = dir
	return []rawEntry{{name: "data/virticle.db", body: string(b)}, {name: "data/id_ed25519", body: "KEY"}}
}

func TestLocalRetentionWithFakeClock(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		n := FileName("ctl", start.AddDate(0, 0, i), false)
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
		if _, err := Prune(dir, 7); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "virticle-backup-odd.tar.gz"), nil, 0o600)
	Prune(dir, 7)
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	got := strings.Join(names, " ")
	if strings.Count(got, "virticle-backup-ctl-") != 7 || !strings.Contains(got, "20260312T033000Z") || strings.Contains(got, "20260305T033000Z") ||
		!strings.Contains(got, "notes.txt") || !strings.Contains(got, "virticle-backup-odd.tar.gz") {
		t.Errorf("kept: %s", got)
	}
}

func TestNextDaily(t *testing.T) {
	loc := time.FixedZone("x", 2*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 6, 10, h, m, 0, 0, loc) }
	if got := NextDaily(at(1, 0), 0); !got.Equal(at(3, 30)) {
		t.Errorf("%v", got)
	}
	if got := NextDaily(at(3, 30), 0); !got.Equal(at(3, 30).AddDate(0, 0, 1)) {
		t.Errorf("at the hour: %v", got)
	}
	if got := NextDaily(at(12, 0), 7*time.Minute); !got.Equal(at(3, 37).AddDate(0, 0, 1)) {
		t.Errorf("%v", got)
	}
}

func TestRecords(t *testing.T) {
	dir := t.TempDir()
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := Remember(dir, "cli", Record{Time: t1, Size: 5, OK: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	Remember(dir, "daily", Record{Time: t1.Add(time.Hour), Error: "disk full"}, nil, nil)
	k, r, ok := ReadRecords(dir).Newest()
	if !ok || k != "cli" || r.Size != 5 {
		t.Errorf("%s %+v %v", k, r, ok)
	}
	Remember(dir, "download", Record{Time: t1.Add(2 * time.Hour), OK: true}, nil, nil)
	if k, _, _ := ReadRecords(dir).Newest(); k != "download" {
		t.Error(k)
	}
	if _, _, ok := ReadRecords(t.TempDir()).Newest(); ok {
		t.Error("empty dir has a backup")
	}
}

func TestWriteFileRefusesToOverwrite(t *testing.T) {
	dir, st := newDataDir(t)
	p := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, _, err := WriteFile(p, srcFor(dir, st), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFile(p, srcFor(dir, st), ""); err == nil {
		t.Error("overwrote")
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".virticle-backup-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".backup-tmp-*")); len(m) != 0 {
		t.Errorf("work dir left: %v", m)
	}
}
