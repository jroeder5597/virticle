package store

import (
	"path/filepath"
	"testing"
)

func TestRollingRunsKeepStateAndFindTheNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LatestRoll(); ok {
		t.Fatal("no run yet")
	}
	a, err := s.CreateRoll(100, "running", `{"n":1}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateRoll(200, "running", `{"n":2}`)
	if err := s.SaveRoll(b, 250, "failed", `{"n":3}`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// The state survives a restart of the controller: Open does not touch it.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, ok, err := s.LatestRoll()
	if err != nil || !ok || r.ID != b || r.Status != "failed" || r.State != `{"n":3}` || r.Finished != 250 || r.Started != 200 {
		t.Fatalf("%+v %v %v", r, ok, err)
	}
	s.PruneRolls(1)
	if r, _, _ := s.LatestRoll(); r.ID != b {
		t.Fatal("pruned the newest")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM rolling_runs`).Scan(&n)
	if n != 1 || a == b {
		t.Fatalf("%d rows", n)
	}
}
