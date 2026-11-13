package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func baks(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "t.db.bak-*"))
	return m
}

func TestFreshDatabaseIsNotBackedUp(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	var v int
	st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	st.Close()
	if v != SchemaVersion {
		t.Fatalf("user_version %d, want %d", v, SchemaVersion)
	}
	if b := baks(t, dir); len(b) != 0 {
		t.Fatalf("fresh database was backed up: %v", b)
	}
	// Reopening at the current version makes no copy either.
	st, err = Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if b := baks(t, dir); len(b) != 0 {
		t.Fatalf("same-version open was backed up: %v", b)
	}
}

func TestOldDatabaseIsCopiedBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	// A database from before versions were recorded: user_version 0, old users table.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, pwhash TEXT NOT NULL, created INTEGER NOT NULL);
		INSERT INTO users(username, pwhash, created) VALUES('alice','x',1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	b := baks(t, dir)
	if len(b) != 1 {
		t.Fatalf("want one copy, got %v", b)
	}
	if want := "t.db.bak-0-"; filepath.Base(b[0])[:len(want)] != want {
		t.Fatalf("copy name %s", b[0])
	}
	// The copy is the pre-migration database: it has no role column.
	cp, err := sql.Open("sqlite", "file:"+b[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var n int
	if err := cp.QueryRow(`SELECT count(*) FROM pragma_table_info('users') WHERE name='role'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("copy was taken after migration (role columns %d, err %v)", n, err)
	}
	var u string
	if err := cp.QueryRow(`SELECT username FROM users`).Scan(&u); err != nil || u != "alice" {
		t.Fatalf("copy lost data: %q %v", u, err)
	}
}

func TestOnlyNewestThreeCopiesKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		p := path + ".bak-0-" + time.Unix(int64(i), 0).UTC().Format("20060102T150405Z")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	pruneBackups(path)
	b := baks(t, dir)
	if len(b) != keepBackups {
		t.Fatalf("kept %d: %v", len(b), b)
	}
	for _, old := range []int64{0, 1} {
		if _, err := os.Stat(path + ".bak-0-" + time.Unix(old, 0).UTC().Format("20060102T150405Z")); err == nil {
			t.Fatalf("oldest copy %d still there", old)
		}
	}
}

// A feature build that ran its own "version 3" left databases that say 3 but
// lack the tables and columns of the other migrations. Every migration runs on
// every start, so opening such a database fills the gaps; a database at
// version 2 goes through the whole sequence in one open.
func TestLinearSchemaSequenceFillsGaps(t *testing.T) {
	for _, from := range []int{2, 3, 4} {
		dir := t.TempDir()
		path := filepath.Join(dir, "t.db")
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		raw, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		// Take away what versions above `from` added, as a database from another build would lack it.
		stmts := []string{`DROP TABLE notify_destinations`, `DROP TABLE recovery_codes`,
			`ALTER TABLE users DROP COLUMN totp_enabled`, `ALTER TABLE nodes DROP COLUMN migration_limit`}
		if from >= 3 {
			stmts = stmts[:3]
		}
		if from >= 4 {
			stmts = stmts[1:3]
		}
		for _, q := range stmts {
			if _, err := raw.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, from)); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		st, err = Open(path)
		if err != nil {
			t.Fatalf("from %d: %v", from, err)
		}
		for _, q := range []string{
			`SELECT count(*) FROM notify_destinations`, `SELECT count(*) FROM recovery_codes`,
			`SELECT totp_enabled FROM users LIMIT 1`, `SELECT migration_limit FROM nodes LIMIT 1`} {
			rows, err := st.db.Query(q)
			if err != nil {
				t.Errorf("from %d: %s: %v", from, q, err)
				continue
			}
			rows.Close()
		}
		var v int
		st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
		if v != SchemaVersion {
			t.Errorf("from %d: version %d, want %d", from, v, SchemaVersion)
		}
		st.Close()
		if len(baks(t, dir)) != 1 {
			t.Errorf("from %d: want one pre-upgrade copy, got %v", from, baks(t, dir))
		}
	}
}
