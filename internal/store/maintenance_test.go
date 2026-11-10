package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestMaintenanceFlagAndSchedulerTasks(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.AddNode(Node{Addr: "n1", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMaintenance(n.ID, true); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.Nodes()
	if len(nodes) != 1 || !nodes[0].Maintenance {
		t.Fatalf("flag not stored: %+v", nodes)
	}
	if err := s.SetMaintenance(n.ID, false); err != nil {
		t.Fatal(err)
	}
	if nodes, _ = s.Nodes(); nodes[0].Maintenance {
		t.Fatal("flag not cleared")
	}
	if err := s.SetMaintenance(999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	a, _ := s.StartTask("snapshot create", "vm1", "n1", "scheduler")
	s.FinishTask(&a, errors.New("boom"))
	s.StartTask("snapshot create", "vm2", "n1", "alice")
	got, err := s.TasksBy("scheduler", 0)
	if err != nil || len(got) != 1 || got[0].Status != "failed" || got[0].Target != "vm1" {
		t.Fatalf("scheduler tasks: %+v %v", got, err)
	}
}
