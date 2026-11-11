package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCloudProfiles(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.SaveCloudProfile(CloudProfile{Name: "a", User: "u", PwHash: "h", SSHKeys: []string{"k1"}, Default: true})
	if err != nil || a.ID == 0 {
		t.Fatal(a, err)
	}
	if _, err := s.SaveCloudProfile(CloudProfile{Name: "a"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate name: %v", err)
	}
	b, _ := s.SaveCloudProfile(CloudProfile{Name: "b", Default: true})
	ps, _ := s.CloudProfiles()
	if len(ps) != 2 || ps[0].Default || !ps[1].Default {
		t.Errorf("default must move to b: %+v", ps)
	}
	got, _ := s.CloudProfile(a.ID)
	if got.PwHash != "h" || len(got.SSHKeys) != 1 || got.Packages == nil {
		t.Errorf("roundtrip: %+v", got)
	}
	b.Name = "a"
	if _, err := s.SaveCloudProfile(b); !errors.Is(err, ErrNameTaken) {
		t.Errorf("rename onto existing: %v", err)
	}
	if _, err := s.SaveCloudProfile(CloudProfile{ID: 999, Name: "z"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	if err := s.DeleteCloudProfile(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCloudProfile(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}
