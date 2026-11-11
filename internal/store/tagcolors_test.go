package store

import (
	"path/filepath"
	"testing"
)

func TestTagColors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.TagColors(); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	if err := s.SetTagColor("prod", "#ff0000"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTagColor("prod", "#00ff00"); err != nil { // replaces
		t.Fatal(err)
	}
	s.SetTagColor("blue team", "#0000ff")
	m, _ := s.TagColors()
	if len(m) != 2 || m["prod"] != "#00ff00" || m["blue team"] != "#0000ff" {
		t.Errorf("got %v", m)
	}
	s.Close()
	// persists across a reopen (and the table is created on an existing database)
	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.DeleteTagColor("prod"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTagColor("prod"); err != nil {
		t.Errorf("deleting a missing colour: %v", err)
	}
	m, _ = s.TagColors()
	if len(m) != 1 || m["blue team"] != "#0000ff" {
		t.Errorf("after delete: %v", m)
	}
}
