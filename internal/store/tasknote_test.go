package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// A task that ended well keeps its one-clause note; it is never mistaken for an error.
func TestTaskNote(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ok, _ := s.StartTask("backup", "vm", "n1", "admin")
	s.FinishTaskNote(&ok, nil, "guest file systems frozen")
	bad, _ := s.StartTask("backup", "vm", "n1", "admin")
	s.FinishTaskNote(&bad, errors.New("boom"), "ignored")
	plain, _ := s.StartTask("backup", "vm", "n1", "admin")
	s.FinishTask(&plain, nil)

	list, err := s.Tasks(10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]Task{}
	for _, x := range list {
		got[x.ID] = x
	}
	if g := got[ok.ID]; g.Status != "ok" || g.Note != "guest file systems frozen" || g.Error != "" {
		t.Errorf("ok task: %+v", g)
	}
	if g := got[bad.ID]; g.Status != "failed" || g.Error != "boom" || g.Note != "" {
		t.Errorf("failed task: %+v", g)
	}
	if g := got[plain.ID]; g.Status != "ok" || g.Note != "" || g.Error != "" {
		t.Errorf("plain task: %+v", g)
	}
}
