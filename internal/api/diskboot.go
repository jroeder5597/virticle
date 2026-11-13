package api

import (
	"net/http"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// Firmware of an existing disk. The node command reads the start of the image through qemu-img and reports the
// partition table; only what is certain restricts the choice:
//
//   - a partition table without an EFI System Partition cannot start with UEFI: BIOS only;
//   - an EFI System Partition without MBR boot code and without a BIOS boot partition cannot start with BIOS:
//     UEFI only;
//   - everything else (both present, an empty disk, unreadable or unknown) is no restriction.
const (
	reasonBIOSOnly = "This disk has no EFI system partition, so it can only start with BIOS."
	reasonUEFIOnly = "This disk has an EFI system partition and no BIOS boot code, so it can only start with UEFI."
)

type diskFirmware struct {
	Only   string `json:"only,omitempty"` // "bios" or "uefi"; empty means no restriction
	Reason string `json:"reason,omitempty"`
}

func firmwareOfDisk(d hostconn.DiskBoot) diskFirmware {
	if d.Table != "mbr" && d.Table != "gpt" {
		return diskFirmware{}
	}
	switch {
	case !d.ESP:
		return diskFirmware{Only: "bios", Reason: reasonBIOSOnly}
	case !d.BootCode && !d.BiosBoot:
		return diskFirmware{Only: "uefi", Reason: reasonUEFIOnly}
	}
	return diskFirmware{}
}

// detectDiskFirmware looks at a volume; any failure means no restriction.
func detectDiskFirmware(h *hostconn.Host, pool libvirt.StoragePool, volName string) diskFirmware {
	vol, err := h.StorageVolLookupByName(pool, volName)
	if err != nil {
		return diskFirmware{}
	}
	p, err := h.StorageVolGetPath(vol)
	if err != nil {
		return diskFirmware{}
	}
	d, err := h.DiskBoot(p)
	if err != nil {
		return diskFirmware{}
	}
	return firmwareOfDisk(d)
}

// applyDiskFirmware steers the firmware of a new VM by what its disk can start with: an unspecified firmware (none
// given, or the server default) follows the disk, an explicit contrary choice is refused. Secure Boot, which needs
// UEFI, is left to checkFirmware.
func (q *createVMRequest) applyDiskFirmware(f diskFirmware) error {
	if f.Only == "" {
		return nil
	}
	if q.Firmware != "" && !q.fwDefaulted && q.Firmware != f.Only {
		return badRequest(f.Reason)
	}
	q.Firmware, q.fwDefaulted = f.Only, false
	return nil
}

func (s *Server) volumeFirmware(w http.ResponseWriter, r *http.Request) {
	h, pool, vol, err := s.volume(r)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, detectDiskFirmware(h, pool, vol.Name))
}
