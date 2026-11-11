package store

import (
	"path/filepath"
	"testing"
)

func TestWebSettings(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.WebSetting("web"); err != nil || ok {
		t.Fatalf("empty: %v %v", ok, err)
	}
	if err := s.SetWebSetting("web", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWebSetting("web", `{"a":2}`); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.WebSetting("web"); !ok || v != `{"a":2}` {
		t.Fatalf("got %q %v", v, ok)
	}
	if err := s.DeleteWebSetting("web"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.WebSetting("web"); ok {
		t.Fatal("still there")
	}
}
