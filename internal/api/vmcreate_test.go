package api

import (
	"strings"
	"testing"
)

// The firmware of a Linux guest comes from one constant when the request has none.
func TestDefaultLinuxFirmware(t *testing.T) {
	q := createVMRequest{}
	if err := q.normalize(); err != nil {
		t.Fatal(err)
	}
	if q.Firmware != defaultLinuxFirmware || !q.fwDefaulted {
		t.Errorf("absent firmware = %q (defaulted %v), want %q", q.Firmware, q.fwDefaulted, defaultLinuxFirmware)
	}
	// What the caller names is kept and is not "defaulted".
	for _, fw := range []string{"bios", "uefi"} {
		q := createVMRequest{Firmware: fw}
		if err := q.normalize(); err != nil || q.Firmware != fw || q.fwDefaulted {
			t.Errorf("explicit %s: %q %v %v", fw, q.Firmware, q.fwDefaulted, err)
		}
	}
	// Windows is not covered by the Linux constant.
	w := createVMRequest{OSType: "windows"}
	if err := w.normalize(); err != nil || w.Firmware != "" {
		t.Errorf("windows firmware %q, %v", w.Firmware, err)
	}
}

// A default of UEFI never fails on a node without UEFI; an explicit request still does.
func TestDefaultFirmwareFallsBack(t *testing.T) {
	noUEFI := fwCaps{}
	q := createVMRequest{Firmware: "uefi", fwDefaulted: true}
	q.fallBackToBIOS(noUEFI)
	if q.Firmware != "bios" {
		t.Errorf("defaulted uefi on a node without UEFI = %q", q.Firmware)
	}
	e := createVMRequest{Firmware: "uefi"}
	e.fallBackToBIOS(noUEFI)
	if err := e.checkFirmware(noUEFI); err == nil {
		t.Error("explicit uefi on a node without UEFI must be refused")
	}
	ok := createVMRequest{Firmware: "uefi", fwDefaulted: true}
	ok.fallBackToBIOS(fwCaps{UEFI: true})
	if ok.Firmware != "uefi" {
		t.Errorf("uefi node lost the default: %q", ok.Firmware)
	}
}

// The serial console is added unless the request says serial:false.
func TestSerialAlwaysAddedByDefault(t *testing.T) {
	x := build(t, createVMRequest{Name: "s", VCPUs: 1, MemoryMiB: 128})
	if !strings.Contains(x, "<serial type='pty'/>") || !strings.Contains(x, "<console type='pty'/>") {
		t.Errorf("no serial console by default:\n%s", x)
	}
	off := false
	x = build(t, createVMRequest{Name: "s", VCPUs: 1, MemoryMiB: 128, Serial: &off})
	if strings.Contains(x, "<serial") {
		t.Errorf("serial:false still adds one:\n%s", x)
	}
}
