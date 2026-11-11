package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// A database made before the connection settings existed opens, keeps its
// nodes and gives them the defaults.
func TestNodeConnectionMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, addr TEXT UNIQUE NOT NULL, user TEXT NOT NULL, jump TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL)`,
		`INSERT INTO nodes(addr, user, jump, created) VALUES('10.0.0.5','alice','j@h',1)`,
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
	if err != nil || len(nodes) != 1 {
		t.Fatalf("%v %+v", err, nodes)
	}
	n := nodes[0]
	if n.Addr != "10.0.0.5" || n.User != "alice" || n.Jump != "j@h" || n.Privilege != "" || n.MigrationAddr != "" || n.HostUUID != "" || n.Maintenance {
		t.Errorf("unexpected defaults: %+v", n)
	}
	s.Close()
	s, err = Open(path) // opening twice must not fail on existing columns
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateNodeConnection(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.AddNode(Node{Addr: "a", User: "root"})
	b, _ := s.AddNode(Node{Addr: "b", User: "root"})
	if err := s.SetMaintenance(a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeConnection(a.ID, Node{Addr: "a2", User: "bob", Jump: "x@y", Privilege: "sudo", MigrationAddr: "10.1.1.1", HostUUID: "u"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNode(a.ID)
	if err != nil || got.Addr != "a2" || got.User != "bob" || got.Privilege != "sudo" || got.MigrationAddr != "10.1.1.1" || !got.Maintenance || got.ID != a.ID {
		t.Errorf("%+v %v", got, err)
	}
	if err := s.UpdateNodeConnection(a.ID, Node{Addr: "b", User: "root"}); err == nil {
		t.Error("duplicate address accepted")
	}
	if err := s.UpdateNodeConnection(999, Node{Addr: "z", User: "root"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	_ = b
}
