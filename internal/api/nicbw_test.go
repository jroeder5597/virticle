package api

import "testing"

func TestMbitToKBps(t *testing.T) {
	for mbit, want := range map[int]uint64{1: 125, 8: 1000, 50: 6250, 100: 12500, 1000: 125000, 100000: 12500000} {
		if got := mbitToKBps(mbit); got != want {
			t.Errorf("%d Mbit/s = %d kB/s, want %d", mbit, got, want)
		}
	}
	if kbpsToMbit(6250) != 50 || kbpsToMbit(62) != 0.496 {
		t.Errorf("kbpsToMbit wrong: %v %v", kbpsToMbit(6250), kbpsToMbit(62))
	}
}

func TestLimitBurst(t *testing.T) {
	for kbps, want := range map[uint64]uint64{125: 128, 6250: 1250, 12500: 2500, 12500000: 65536} {
		if got := limitBurstKB(kbps); got != want {
			t.Errorf("burst for %d = %d, want %d", kbps, got, want)
		}
	}
}

func TestNicLimitCheck(t *testing.T) {
	for _, l := range []nicLimit{{0, 0}, {1, 1}, {50, 0}, {100000, 100000}} {
		if err := l.check(); err != nil {
			t.Errorf("%+v refused: %v", l, err)
		}
	}
	for _, l := range []nicLimit{{-1, 0}, {0, -5}, {100001, 0}} {
		if l.check() == nil {
			t.Errorf("%+v accepted", l)
		}
	}
}

func TestNicLimitXML(t *testing.T) {
	if got := (nicLimit{}).xml(); got != "" {
		t.Errorf("no limit gave %q", got)
	}
	got := nicLimit{Down: 50, Up: 10}.xml()
	want := "<bandwidth><inbound average='6250' burst='1250'/><outbound average='1250' burst='250'/></bandwidth>"
	if got != want {
		t.Errorf("got %s", got)
	}
}
