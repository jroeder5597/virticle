package ctlbackup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const stampLayout = "20060102T150405Z"

var hostClean = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// FileName is the default name of a backup file.
func FileName(host string, t time.Time, encrypted bool) string {
	host = strings.Trim(hostClean.ReplaceAllString(host, "-"), "-.")
	if host == "" {
		host = "controller"
	}
	n := "virticle-backup-" + host + "-" + t.UTC().Format(stampLayout) + ".tar.gz"
	if encrypted {
		n += ".age"
	}
	return n
}

// WriteFile makes a backup file at path. The file is created with mode 0600,
// refuses to replace an existing file, and appears under its name only when
// it is complete. pass, when not empty, encrypts it. It returns the manifest
// and the size of the file.
func WriteFile(path string, src Source, pass string) (Manifest, int64, error) {
	var m Manifest
	dir := filepath.Dir(path)
	if _, err := os.Lstat(path); err == nil {
		return m, 0, fmt.Errorf("%s already exists; nothing was overwritten", path)
	}
	tmp, err := os.CreateTemp(dir, ".virticle-backup-*.tmp") // 0600
	if err != nil {
		return m, 0, err
	}
	defer os.Remove(tmp.Name())
	var w io.Writer = tmp
	var enc io.WriteCloser
	if pass != "" {
		if enc, err = Encrypt(tmp, pass); err != nil {
			tmp.Close()
			return m, 0, err
		}
		w = enc
	}
	m, err = Write(w, src)
	if err == nil && enc != nil {
		err = enc.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return m, 0, err
	}
	fi, err := os.Stat(tmp.Name())
	if err != nil {
		return m, 0, err
	}
	// Link then remove: a second backup in the same second cannot replace this one.
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return m, 0, fmt.Errorf("%s already exists; nothing was overwritten", path)
		}
		if err := os.Rename(tmp.Name(), path); err != nil { // file systems without hard links
			return m, 0, err
		}
	}
	return m, fi.Size(), nil
}

// Prune keeps the newest keep backups made by WriteFile in dir (by the time in
// their names) and removes the rest. Other files are left alone.
func Prune(dir string, keep int) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type bk struct{ name, stamp string }
	var all []bk
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(n, "virticle-backup-") || !strings.HasSuffix(n, ".tar.gz") {
			continue
		}
		stem := strings.TrimSuffix(n, ".tar.gz")
		if len(stem) < len(stampLayout) {
			continue
		}
		stamp := stem[len(stem)-len(stampLayout):]
		if _, err := time.Parse(stampLayout, stamp); err != nil {
			continue
		}
		all = append(all, bk{n, stamp})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].stamp != all[j].stamp {
			return all[i].stamp > all[j].stamp
		}
		return all[i].name > all[j].name
	})
	var gone []string
	for i := keep; i < len(all); i++ {
		if err := os.Remove(filepath.Join(dir, all[i].name)); err != nil {
			return gone, err
		}
		gone = append(gone, all[i].name)
	}
	return gone, nil
}

// DailyHour and DailyMinute are the local time of the daily backup.
const (
	DailyHour   = 3
	DailyMinute = 30
)

// NextDaily is the first 03:30 local time after now, plus jitter (so that
// several controllers do not all start at the same second).
func NextDaily(now time.Time, jitter time.Duration) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), DailyHour, DailyMinute, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, DailyHour, DailyMinute, 0, 0, now.Location())
	}
	return t.Add(jitter)
}

// Record is what is remembered about one backup.
type Record struct {
	Time      time.Time `json:"time"`
	Size      int64     `json:"size,omitempty"`
	Encrypted bool      `json:"encrypted,omitempty"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

// Records are the last backup of each kind: "cli", "download" and "daily".
type Records map[string]Record

const recordsName = "last-backup.json"

// ReadRecords reads the remembered backups of dir; a missing file is none.
func ReadRecords(dir string) Records {
	r := Records{}
	if b, err := os.ReadFile(filepath.Join(dir, recordsName)); err == nil {
		json.Unmarshal(b, &r)
	}
	if r == nil {
		r = Records{}
	}
	return r
}

// Remember stores the last backup of one kind. The file is replaced by
// renaming, so a service account can replace a file that root wrote.
func Remember(dir, kind string, rec Record, owner *Owner, h Host) error {
	r := ReadRecords(dir)
	r[kind] = rec
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".last-backup-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if owner != nil && h != nil {
		if err := h.Chown(tmp.Name(), *owner); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, recordsName))
}

// Newest returns the most recent successful record and its kind.
func (r Records) Newest() (string, Record, bool) {
	var kind string
	var best Record
	found := false
	for k, v := range r {
		if v.OK && (!found || v.Time.After(best.Time)) {
			kind, best, found = k, v, true
		}
	}
	return kind, best, found
}
