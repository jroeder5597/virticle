package ctlbackup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"virticle/internal/store"
)

// LockName is the file the running controller holds an exclusive lock on, so
// that a second controller or a restore on the same data directory is refused.
const LockName = ".virticle.lock"

// ErrLocked means a controller is running on the data directory.
var ErrLocked = errors.New("a Virticle controller is running on this data directory")

// LockDataDir takes the controller's lock on dir. Keep the returned file open
// for as long as the controller runs; the lock goes away with the process.
func LockDataDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

// lockHeld reports whether a controller holds the lock. A missing lock file
// means none does (and is not created: a root restore must not leave a file
// the service account cannot open).
func lockHeld(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil
}

// Owner is a user and group id.
type Owner struct{ UID, GID int }

// Host is what restore needs to know about the machine; tests replace it.
type Host interface {
	IsRoot() bool
	// OwnerOf returns the owner of a path, ok false when it does not exist.
	OwnerOf(path string) (Owner, bool)
	// ServiceAccount returns the account the packaged service runs as.
	ServiceAccount() (Owner, bool)
	Chown(path string, o Owner) error
	// ServiceActive reports whether the systemd unit for dataDir is running.
	ServiceActive(dataDir string) bool
}

// PickOwner is the rule for who owns the restored files. Not root: whoever
// runs the restore (nothing to change). Root: the owner of the existing data
// directory if that is not root (it is what the service ran as), else the
// service account if there is one, else root.
func PickOwner(isRoot bool, existing *Owner, service *Owner) *Owner {
	if !isRoot {
		return nil
	}
	if existing != nil && existing.UID != 0 {
		return existing
	}
	if service != nil {
		return service
	}
	return nil
}

// SystemHost is the real machine.
type SystemHost struct{}

func (SystemHost) IsRoot() bool { return os.Geteuid() == 0 }

func (SystemHost) OwnerOf(p string) (Owner, bool) {
	fi, err := os.Stat(p)
	if err != nil {
		return Owner{}, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return Owner{int(st.Uid), int(st.Gid)}, true
	}
	return Owner{}, false
}

func (SystemHost) ServiceAccount() (Owner, bool) {
	u, err := user.Lookup("virticle")
	if err != nil {
		return Owner{}, false
	}
	uid, e1 := strconv.Atoi(u.Uid)
	gid, e2 := strconv.Atoi(u.Gid)
	return Owner{uid, gid}, e1 == nil && e2 == nil
}

func (SystemHost) Chown(p string, o Owner) error { return os.Lchown(p, o.UID, o.GID) }

// systemDataDir is where the packages keep the data; the unit "virticle" is
// only asked about that directory.
const systemDataDir = "/var/lib/virticle"

func (SystemHost) ServiceActive(dataDir string) bool {
	if filepath.Clean(dataDir) != systemDataDir {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", "virticle").Run() == nil
}

// RestoreOptions say what to restore and where.
type RestoreOptions struct {
	Archive io.Reader // the plain (decrypted) archive
	DataDir string
	Host    Host
	Now     func() time.Time
	// SchemaMax is the newest database schema this program knows.
	SchemaMax int
}

// RestoreResult is what happened.
type RestoreResult struct {
	Manifest Manifest
	Aside    string // where the previous data directory went; empty when there was none
	Owner    *Owner // nil: files belong to the user who ran the restore
}

// ServiceRunningError tells how to stop the service.
type ServiceRunningError struct{ Dir string }

func (e ServiceRunningError) Error() string {
	return "the Virticle controller is running on " + e.Dir + "; stop it first (sudo systemctl stop virticle), then run the restore again"
}

// Restore replaces the data directory with the backup's files. Nothing in the
// real data directory is touched until the whole archive has been read and
// every checksum has matched: the files are written to a directory next to it
// first. An existing, non-empty data directory is moved aside, never
// overwritten.
func Restore(o RestoreOptions) (RestoreResult, error) {
	var res RestoreResult
	if o.Now == nil {
		o.Now = time.Now
	}
	dir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return res, err
	}
	exists := false
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return res, fmt.Errorf("%s exists and is not a directory", dir)
		}
		exists = true
		if held, err := lockHeld(dir); err != nil {
			return res, fmt.Errorf("could not tell whether the controller is running: %w", err)
		} else if held {
			return res, ServiceRunningError{dir}
		}
	}
	if o.Host.ServiceActive(dir) {
		return res, ServiceRunningError{dir}
	}
	parent := filepath.Dir(dir)
	if _, err := os.Stat(parent); err != nil {
		return res, fmt.Errorf("the directory %s does not exist", parent)
	}

	stage, err := os.MkdirTemp(parent, filepath.Base(dir)+".restore-tmp-")
	if err != nil {
		return res, fmt.Errorf("cannot write next to %s: %w", dir, err)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			os.RemoveAll(stage)
		}
	}()

	if res.Manifest, err = Extract(o.Archive, stage); err != nil {
		return res, err
	}
	m := res.Manifest
	ver, err := store.FileSchemaVersion(filepath.Join(stage, DBFile))
	if err != nil {
		return res, err
	}
	if ver != m.SchemaVersion {
		return res, errors.New("the database in the backup does not match its manifest")
	}
	if ver > o.SchemaMax {
		return res, fmt.Errorf("this backup was made by a newer Virticle (database schema %d; this program knows up to %d). Install the newer version first", ver, o.SchemaMax)
	}

	var existing, service *Owner
	if exists {
		if ow, ok := o.Host.OwnerOf(dir); ok {
			existing = &ow
		}
	}
	if ow, ok := o.Host.ServiceAccount(); ok {
		service = &ow
	}
	res.Owner = PickOwner(o.Host.IsRoot(), existing, service)
	if res.Owner != nil {
		err := filepath.WalkDir(stage, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return o.Host.Chown(p, *res.Owner)
		})
		if err != nil {
			return res, fmt.Errorf("setting the owner of the restored files: %w", err)
		}
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		return res, err
	}

	// Checked: from here on the real directory is touched.
	if exists {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return res, err
		}
		onlyLock := len(entries) == 0 || (len(entries) == 1 && entries[0].Name() == LockName)
		if onlyLock {
			if err := os.RemoveAll(dir); err != nil {
				return res, err
			}
		} else {
			res.Aside = o.asidePath(dir)
			if err := os.Rename(dir, res.Aside); err != nil {
				return res, fmt.Errorf("moving %s aside: %w (a directory that is a mount point cannot be moved; empty it by hand first)", dir, err)
			}
		}
	}
	if err := os.Rename(stage, dir); err != nil {
		if res.Aside != "" {
			if err2 := os.Rename(res.Aside, dir); err2 != nil {
				keepStage = true
				return res, fmt.Errorf("putting the restored files in place failed (%v); your previous data is in %s and the restored files in %s", err, res.Aside, stage)
			}
			res.Aside = ""
		}
		return res, fmt.Errorf("putting the restored files in place: %w", err)
	}
	return res, nil
}

func (o RestoreOptions) asidePath(dir string) string {
	base := dir + ".before-restore-" + o.Now().UTC().Format("20060102T150405Z")
	p := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
}
