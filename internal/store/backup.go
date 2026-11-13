package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the database schema version this build writes, kept in
// PRAGMA user_version. Raise it whenever a change to the schema or a
// migration is added: a database found at a lower version is copied first
// (see backupBeforeMigration). Databases from before the version was recorded
// read as 0.
//
// History (one linear sequence; every migration is idempotent and runs on
// every start, so the number only decides whether a copy is made first):
//
//	1  user roles and the auth tables
//	2  node connection settings (privilege, migration address, host uuid)
//	3  migration_limit column on nodes (migration network and automatic limit)
//	4  notify_destinations table (alert notifications)
const SchemaVersion = 4

// keepBackups is how many pre-migration copies stay in the data directory.
const keepBackups = 3

// backupBeforeMigration copies the database to <file>.bak-<oldversion>-<time>
// when it holds data and its schema version is below SchemaVersion, and
// returns the copy's path ("" when no copy was needed). The copy is made with
// VACUUM INTO, so it is one consistent file even when the write-ahead log has
// unmerged pages. It runs before any schema statement.
func backupBeforeMigration(db *sql.DB, path string, now time.Time) (string, error) {
	var old, tables int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&old); err != nil {
		return "", err
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		return "", err
	}
	if tables == 0 {
		return "", nil // new database: nothing to lose
	}
	if old > SchemaVersion {
		log.Printf("WARNING: database %s has schema version %d, newer than this build (%d); it was written by a newer virticle", path, old, SchemaVersion)
		return "", nil
	}
	if old == SchemaVersion {
		return "", nil
	}
	dst := fmt.Sprintf("%s.bak-%d-%s", path, old, now.UTC().Format("20060102T150405Z"))
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return "", fmt.Errorf("backing up the database before the schema upgrade: %w", err)
	}
	log.Printf("database schema %d -> %d: copied %s to %s", old, SchemaVersion, filepath.Base(path), dst)
	pruneBackups(path)
	return dst, nil
}

// pruneBackups removes all but the newest keepBackups copies.
func pruneBackups(path string) {
	matches, _ := filepath.Glob(path + ".bak-*")
	type bak struct {
		name string
		mod  time.Time
	}
	var all []bak
	for _, m := range matches {
		if !strings.HasPrefix(m, path+".bak-") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
			all = append(all, bak{m, fi.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].mod.Equal(all[j].mod) {
			return all[i].mod.After(all[j].mod)
		}
		return all[i].name > all[j].name
	})
	for i := keepBackups; i < len(all); i++ {
		if err := os.Remove(all[i].name); err != nil {
			log.Printf("could not remove old database copy %s: %v", all[i].name, err)
		}
	}
}

func setSchemaVersion(db *sql.DB) error {
	var cur int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	if cur >= SchemaVersion {
		return nil
	}
	_, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion))
	return err
}
