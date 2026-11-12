package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A node stored with the retired broad-sudo privilege is set to "detect"
// when the database is opened; the other values are read as they are.
func TestRetiredSudoPrivilegeIsReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sudo.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, addr TEXT UNIQUE NOT NULL, user TEXT NOT NULL, jump TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, maintenance INTEGER NOT NULL DEFAULT 0, privilege TEXT NOT NULL DEFAULT '', migration_addr TEXT NOT NULL DEFAULT '', host_uuid TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO nodes(addr, user, privilege, created) VALUES('a','admin','sudo',1),('b','virticle','helper',1),('c','root','root',1),('d','x','none',1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	nodes, err := s.Nodes()
	if err != nil || len(nodes) != 4 {
		t.Fatalf("%v %+v", err, nodes)
	}
	want := map[string]string{"a": "", "b": "helper", "c": "root", "d": "none"}
	for _, n := range nodes {
		if n.Privilege != want[n.Addr] {
			t.Errorf("node %s: privilege %q, want %q", n.Addr, n.Privilege, want[n.Addr])
		}
	}
}
