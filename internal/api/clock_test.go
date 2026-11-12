package api

import (
	"strings"
	"testing"

	"virticle/internal/domxml"
)

func TestCreateSerialAndClock(t *testing.T) {
	base := createVMRequest{Name: "x", VCPUs: 1, MemoryMiB: 256, Pool: "default"}
	lin := build(t, base)
	for _, want := range []string{"<serial type='pty'/>", "<console type='pty'/>", "<timer name='rtc' tickpolicy='catchup'/>", "<timer name='pit' tickpolicy='delay'/>", "<timer name='hpet' present='no'/>", "<timer name='kvmclock' present='yes'/>"} {
		if !strings.Contains(lin, want) {
			t.Errorf("linux default lacks %s", want)
		}
	}
	if strings.Contains(lin, "hypervclock") {
		t.Error("a Linux guest must not get hypervclock")
	}
	off := false
	base.Serial = &off
	if x := build(t, base); strings.Contains(x, "<serial") || strings.Contains(x, "<console") {
		t.Errorf("serial=false still wrote a serial device:\n%s", x)
	}
	win := build(t, createVMRequest{Name: "w", VCPUs: 1, MemoryMiB: 256, Pool: "default", OSType: "windows"})
	if !strings.Contains(win, "hypervclock") || !strings.Contains(win, "<hyperv mode='custom'>") || !strings.Contains(win, "clock offset='localtime'") {
		t.Errorf("windows keeps its Hyper-V clock and enlightenments:\n%s", win)
	}
	if strings.Contains(win, "kvmclock") {
		t.Error("windows does not get kvmclock")
	}
}

func TestSerialPending(t *testing.T) {
	none := "<domain><devices></devices></domain>"
	pty := "<domain><devices><serial type='pty'/></devices></domain>"
	live := func(x string) []domxml.Serial { s, _ := domxml.Serials(x); return s }
	if got := serialPending(live(none), pty); got != "add" {
		t.Errorf("add: %q", got)
	}
	if got := serialPending(live(pty), none); got != "remove" {
		t.Errorf("remove: %q", got)
	}
	if got := serialPending(live(pty), pty); got != "" {
		t.Errorf("same: %q", got)
	}
}

func TestClockSyncMetadata(t *testing.T) {
	meta, err := metaSetChild("", "clock", "<clock sync='off'/>")
	if err != nil || !strings.Contains(meta, "<clock sync='off'/>") {
		t.Fatalf("%q %v", meta, err)
	}
	// other features' children survive
	meta, err = metaSetChild("<meta><tags><tag>a</tag></tags></meta>", "clock", "<clock sync='off'/>")
	if err != nil || !strings.Contains(meta, "<tag>a</tag>") || !strings.Contains(meta, "sync='off'") {
		t.Fatalf("%q %v", meta, err)
	}
	meta, err = metaSetChild(meta, "clock", "")
	if err != nil || strings.Contains(meta, "clock") || !strings.Contains(meta, "<tag>a</tag>") {
		t.Fatalf("%q %v", meta, err)
	}
}
