package hostconn

import "testing"

func TestAvailableFromStats(t *testing.T) {
	// the lab node: 170 MiB free, 4 MiB buffers, 1.8 GiB of page cache
	got := availableFromStats(map[string]uint64{"total": 2708608, "free": 175828, "buffers": 4148, "cached": 1818924})
	if want := uint64(175828+4148+1818924) * 1024; got != want {
		t.Errorf("available %d, want %d", got, want)
	}
	if availableFromStats(map[string]uint64{"total": 100}) != 0 {
		t.Error("statistics without a free figure must fall back to libvirt's free memory")
	}
}
