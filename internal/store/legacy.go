package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DBName is the database file in the data directory.
	DBName = "virticle.db"
	// legacyDBName is what versions before the rename called it.
	legacyDBName = "virt-proj.db"
)

// DBPath returns the database path inside dir. When only the database of the
// old name exists it is renamed (with its -wal and -shm companions) first, so
// an upgrade never starts on an empty database beside the old one. If both
// exist the new one is used and a warning is logged through logf; if the
// rename fails an error is returned and nothing is started on a fresh file.
func DBPath(dir string, logf func(format string, args ...any)) (string, error) {
	cur, old := filepath.Join(dir, DBName), filepath.Join(dir, legacyDBName)
	if exists(cur) {
		if exists(old) {
			logf("WARNING: both %s and %s exist in %s; using %s and leaving the old file alone", DBName, legacyDBName, dir, DBName)
		}
		return cur, nil
	}
	if !exists(old) {
		return cur, nil
	}
	// The write-ahead log and the shared-memory index go first and the main
	// file last: a run interrupted in between is picked up again by the next
	// start, which still sees the old main file and finishes the job.
	for _, sfx := range []string{"-wal", "-shm", ""} {
		if err := os.Rename(old+sfx, cur+sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("renaming %s to %s: %w", legacyDBName+sfx, DBName+sfx, err)
		}
	}
	logf("renamed the database %s to %s", legacyDBName, DBName)
	return cur, nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
