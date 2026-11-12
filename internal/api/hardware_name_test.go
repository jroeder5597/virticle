package api

import "testing"

func TestFreeVolumeName(t *testing.T) {
	taken := map[string]bool{"web-disk1.qcow2": true, "web-disk2.qcow2": true}
	if got := freeVolumeName("web-disk", "qcow2", func(n string) bool { return taken[n] }); got != "web-disk3.qcow2" {
		t.Errorf("got %s", got)
	}
	if got := freeVolumeName("db-disk", "raw", func(string) bool { return false }); got != "db-disk1.raw" {
		t.Errorf("got %s", got)
	}
}
