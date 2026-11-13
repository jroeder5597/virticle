package api

import (
	"testing"

	"virticle/internal/hostconn"
)

func TestFirmwareOfDisk(t *testing.T) {
	rows := []struct {
		name string
		d    hostconn.DiskBoot
		only string
	}{
		{"MBR with boot code, no ESP", hostconn.DiskBoot{Table: "mbr", BootCode: true}, "bios"},
		{"GPT without ESP", hostconn.DiskBoot{Table: "gpt", BootCode: true}, "bios"},
		{"GPT with ESP only", hostconn.DiskBoot{Table: "gpt", ESP: true}, "uefi"},
		{"MBR with an EFI partition and no boot code", hostconn.DiskBoot{Table: "mbr", ESP: true}, "uefi"},
		{"cloud image: ESP and BIOS boot partition", hostconn.DiskBoot{Table: "gpt", ESP: true, BiosBoot: true, BootCode: true}, ""},
		{"ESP and boot code", hostconn.DiskBoot{Table: "gpt", ESP: true, BootCode: true}, ""},
		{"ESP and BIOS boot partition without boot code", hostconn.DiskBoot{Table: "gpt", ESP: true, BiosBoot: true}, ""},
		{"empty disk", hostconn.DiskBoot{Table: "none"}, ""},
		{"unknown table", hostconn.DiskBoot{Table: "unknown"}, ""},
		{"nothing reported", hostconn.DiskBoot{}, ""},
	}
	for _, r := range rows {
		got := firmwareOfDisk(r.d)
		if got.Only != r.only || (r.only != "") != (got.Reason != "") {
			t.Errorf("%s: %+v", r.name, got)
		}
	}
}

func TestApplyDiskFirmware(t *testing.T) {
	bios, uefi, free := diskFirmware{Only: "bios", Reason: reasonBIOSOnly}, diskFirmware{Only: "uefi", Reason: reasonUEFIOnly}, diskFirmware{}
	// The server default follows the disk.
	q := createVMRequest{Firmware: "uefi", fwDefaulted: true}
	if err := q.applyDiskFirmware(bios); err != nil || q.Firmware != "bios" {
		t.Errorf("default, BIOS-only disk: %+v %v", q, err)
	}
	// A request that names no firmware (Windows) follows a UEFI-only disk.
	q = createVMRequest{OSType: "windows"}
	if err := q.applyDiskFirmware(uefi); err != nil || q.Firmware != "uefi" {
		t.Errorf("unnamed, UEFI-only disk: %+v %v", q, err)
	}
	// An explicit contrary choice is refused with the sentence the UI shows.
	q = createVMRequest{Firmware: "uefi"}
	if err := q.applyDiskFirmware(bios); err == nil || err.Error() != reasonBIOSOnly {
		t.Errorf("explicit UEFI on a BIOS-only disk: %v", err)
	}
	q = createVMRequest{Firmware: "bios"}
	if err := q.applyDiskFirmware(uefi); err == nil || err.Error() != reasonUEFIOnly {
		t.Errorf("explicit BIOS on a UEFI-only disk: %v", err)
	}
	// An explicit matching choice and no restriction change nothing.
	q = createVMRequest{Firmware: "bios"}
	if err := q.applyDiskFirmware(bios); err != nil || q.Firmware != "bios" {
		t.Errorf("%+v %v", q, err)
	}
	q = createVMRequest{Firmware: "bios"}
	if err := q.applyDiskFirmware(free); err != nil || q.Firmware != "bios" {
		t.Errorf("%+v %v", q, err)
	}
}
