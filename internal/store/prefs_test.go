package store

import (
	"path/filepath"
	"testing"
)

func TestUserPrefs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateUser("alice", "correct horse battery", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateUser("bob", "correct horse battery", RoleOperator)
	if m, err := s.UserPrefs(a.ID); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	if err := s.SetUserPrefs(a.ID, map[string]string{"tasksDock": "closed", "consoleClick": "inline"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserPrefs(a.ID, map[string]string{"tasksDock": "open"}); err != nil { // partial: replaces one, keeps the other
		t.Fatal(err)
	}
	m, _ := s.UserPrefs(a.ID)
	if len(m) != 2 || m["tasksDock"] != "open" || m["consoleClick"] != "inline" {
		t.Errorf("alice %v", m)
	}
	if m, _ := s.UserPrefs(b.ID); len(m) != 0 {
		t.Errorf("bob must not see alice's: %v", m)
	}
	s.Close()
	s, err = Open(p) // persists, and the table is created on an existing database
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, _ := s.UserPrefs(a.ID); m["tasksDock"] != "open" {
		t.Errorf("after reopen %v", m)
	}
	if err := s.DeleteUser(a.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.UserPrefs(a.ID); len(m) != 0 {
		t.Errorf("deleted user's prefs remain: %v", m)
	}
}
