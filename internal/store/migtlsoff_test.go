package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestMigTLSOffFlag(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, _ := s.AddNode(Node{Addr: "n1", User: "root"})
	if err := s.SetMigTLSOff(n.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetNode(n.ID); !got.MigTLSOff {
		t.Fatal("choice not stored")
	}
	// an edit of the connection leaves the choice alone
	if err := s.UpdateNodeConnection(n.ID, Node{Addr: "n1b", User: "root"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetNode(n.ID); !got.MigTLSOff {
		t.Fatal("an edit cleared the choice")
	}
	if err := s.SetMigTLSOff(n.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetNode(n.ID); got.MigTLSOff {
		t.Fatal("choice not cleared")
	}
	if err := s.SetMigTLSOff(999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
