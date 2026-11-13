package store

import (
	"database/sql"
	"fmt"
	"os"
)

// SnapshotTo writes a consistent copy of the live database to dst with
// VACUUM INTO. Writes that happen meanwhile wait; the copy shows the database
// as it was when the copy began. dst must not exist.
func (s *Store) SnapshotTo(dst string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, dst)
	return err
}

// SnapshotFile does the same for a database file that another process (the
// running controller) may have open. It never creates a database and never
// changes the source.
func SnapshotFile(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	db, err := openRaw(src)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	return nil
}

// FileSchemaVersion reads the schema version of a database file and checks
// that it is a sound SQLite database.
func FileSchemaVersion(path string) (int, error) {
	db, err := openRaw(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		return 0, fmt.Errorf("the database is not readable: %w", err)
	}
	if check != "ok" {
		return 0, fmt.Errorf("the database is damaged: %s", check)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

func openRaw(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
