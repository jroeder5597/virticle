package api

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/store"
)

type createVMRequest struct {
	Name      string `json:"name"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memoryMiB"`
	Firmware  string `json:"firmware"` // bios | uefi
	// SecureBoot needs UEFI; TPM adds an emulated TPM 2.0 (swtpm).
	SecureBoot bool `json:"secureBoot"`
	TPM        bool `json:"tpm"`
	// OSType is a hint ("linux", the default, or "windows") that picks
	// devices: Windows gets Hyper-V enlightenments, a local-time clock, and
	// SATA disk and e1000e NIC unless DiskBus / NICModel say otherwise
	// (virtio needs the virtio-win driver during Windows setup).
	OSType   string `json:"osType"`
	DiskBus  string `json:"diskBus"`  // virtio | sata
	NICModel string `json:"nicModel"` // virtio | e1000e
	// Disk: a new volume "<name>.qcow2" in Pool. With BaseVolume set it
	// starts as a copy of that image (a cloud image), grown to DiskGiB.
	Pool       string `json:"pool"`
	DiskGiB    int    `json:"diskGiB"`
	BaseVolume string `json:"baseVolume"`
	// ExistingVolume uses that volume of Pool as the VM's disk as it is: no
	// copy, no resize (an imported disk, say). DiskGiB is ignored.
	ExistingVolume string `json:"existingVolume"`
	// ISO is the path of an installer image to attach as a CD-ROM.
	ISO     string `json:"iso"`
	Network string `json:"network"`
	Start   bool   `json:"start"`
	// CloudInit, when set, attaches a NoCloud seed as a read-only SATA CD-ROM
	// (volume "<name>-seed.iso" in Pool).
	CloudInit *cloudInitRequest `json:"cloudInit"`

	seedPath   string // set while creating, once the seed volume exists
	diskFormat string // driver type of the disk; qcow2 unless an existing volume says otherwise
	tpmModel   string
}

type cloudInitRequest struct {
	Hostname    string   `json:"hostname"`
	User        string   `json:"user"`
	Password    string   `json:"password"`
	SSHKeys     []string `json:"sshKeys"`
	Packages    []string `json:"packages"`
	RawUserData string   `json:"rawUserData"`
	// Profile is a saved cloud-init profile that the fields above override
	// or extend; see mergeProfile.
	Profile int64 `json:"profile"`
}

// seedVolumeName is also how vmDelete recognises the seed of a VM.
func seedVolumeName(vm string) string { return vm + "-seed.iso" }

var vmNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// normalize fills in the defaults the OS hint implies.
func (q *createVMRequest) normalize() error {
	if q.OSType == "" {
		q.OSType = "linux"
	}
	if q.OSType != "linux" && q.OSType != "windows" {
		return badRequest("osType must be linux or windows")
	}
	win := q.OSType == "windows"
	if q.DiskBus == "" {
		q.DiskBus = map[bool]string{true: "sata", false: "virtio"}[win]
	}
	if q.NICModel == "" {
		q.NICModel = map[bool]string{true: "e1000e", false: "virtio"}[win]
	}
	if q.DiskBus != "virtio" && q.DiskBus != "sata" {
		return badRequest("diskBus must be virtio or sata")
	}
	if q.NICModel != "virtio" && q.NICModel != "e1000e" {
		return badRequest("nicModel must be virtio or e1000e")
	}
	if q.diskFormat == "" {
		q.diskFormat = "qcow2"
	}
	return nil
}

func (q createVMRequest) domainXML(diskPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<domain type='kvm'>\n  <name>%s</name>\n  <memory unit='MiB'>%d</memory>\n  <vcpu>%d</vcpu>\n", esc(q.Name), q.MemoryMiB, q.VCPUs)
	// UEFI uses libvirt's firmware auto-selection: it picks the OVMF build
	// matching these features and creates the NVRAM file at first start.
	osAttr, fwFeatures := "", ""
	if q.Firmware == "uefi" {
		osAttr = " firmware='efi'"
		sb := map[bool]string{true: "yes", false: "no"}[q.SecureBoot]
		fwFeatures = fmt.Sprintf("    <firmware>\n      <feature enabled='%s' name='enrolled-keys'/>\n      <feature enabled='%s' name='secure-boot'/>\n    </firmware>\n", sb, sb)
	}
	fmt.Fprintf(&b, "  <os%s>\n    <type arch='x86_64' machine='q35'>hvm</type>\n%s    <boot dev='hd'/>\n", osAttr, fwFeatures)
	if q.ISO != "" {
		b.WriteString("    <boot dev='cdrom'/>\n")
	}
	win := q.OSType == "windows"
	features := "<acpi/><apic/>"
	if q.SecureBoot {
		features += "<smm state='on'/>" // required by the Secure Boot firmware
	}
	if win {
		features += "<hyperv mode='custom'><relaxed state='on'/><vapic state='on'/><spinlocks state='on' retries='8191'/><vpindex state='on'/><runtime state='on'/><synic state='on'/><stimer state='on'/><frequencies state='on'/></hyperv>"
	}
	clock := "<clock offset='utc'/>"
	if win {
		clock = "<clock offset='localtime'><timer name='rtc' tickpolicy='catchup'/><timer name='pit' tickpolicy='delay'/><timer name='hpet' present='no'/><timer name='hypervclock' present='yes'/></clock>"
	}
	fmt.Fprintf(&b, "  </os>\n  <features>%s</features>\n  <cpu mode='host-model'/>\n  %s\n  <devices>\n", features, clock)
	// SATA CD-ROMs take sda.. unless the disk is on SATA too.
	diskDev, bus, cd1, cd2 := "vda", q.DiskBus, "sda", "sdb"
	if bus == "sata" {
		diskDev, cd1, cd2 = "sda", "sdb", "sdc"
	} else {
		bus = "virtio"
	}
	format := q.diskFormat
	if format == "" {
		format = "qcow2"
	}
	fmt.Fprintf(&b, "    <disk type='file' device='disk'>\n      <driver name='qemu' type='%s' discard='unmap'/>\n      <source file='%s'/>\n      <target dev='%s' bus='%s'/>\n    </disk>\n", esc(format), esc(diskPath), diskDev, bus)
	if q.ISO != "" {
		fmt.Fprintf(&b, "    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='%s'/>\n      <target dev='%s' bus='sata'/>\n      <readonly/>\n    </disk>\n", esc(q.ISO), cd1)
	}
	if q.seedPath != "" {
		fmt.Fprintf(&b, "    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='%s'/>\n      <target dev='%s' bus='sata'/>\n      <readonly/>\n    </disk>\n", esc(q.seedPath), cd2)
	}
	if q.Network != "" {
		model := q.NICModel
		if model == "" {
			model = "virtio"
		}
		fmt.Fprintf(&b, "    <interface type='network'>\n      <source network='%s'/>\n      <model type='%s'/>\n    </interface>\n", esc(q.Network), esc(model))
	}
	if q.TPM {
		model := q.tpmModel
		if model == "" {
			model = "tpm-crb"
		}
		b.WriteString("    " + tpmXML(model) + "\n")
	}
	b.WriteString(`    <serial type='pty'/>
    <console type='pty'/>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <input type='tablet' bus='usb'/>
    <graphics type='vnc' autoport='yes' listen='127.0.0.1'/>
    <video><model type='virtio'/></video>
    <rng model='virtio'><backend model='random'>/dev/urandom</backend></rng>
  </devices>
</domain>`)
	return b.String()
}

// createVM makes the disk, defines the domain and optionally starts it. The
// XML is only generated here, at creation; later edits modify the existing
// definition in place.
func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var q createVMRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	switch {
	case !vmNameRE.MatchString(q.Name):
		err = badRequest("name must start with a letter or digit and contain only letters, digits, dot, dash and underscore")
	case q.VCPUs < 1 || q.MemoryMiB < 64:
		err = badRequest("at least 1 vCPU and 64 MiB of memory are required")
	case q.Pool == "":
		err = badRequest("a storage pool is required")
	case q.BaseVolume == "" && q.ExistingVolume == "" && q.DiskGiB < 1:
		err = badRequest("disk size is required")
	case q.BaseVolume != "" && q.ExistingVolume != "":
		err = badRequest("choose either a base image to copy or an existing volume to use, not both")
	}
	if err == nil {
		err = q.normalize()
	}
	var seed []byte
	if ci := q.CloudInit; ci != nil && err == nil {
		var prof *store.CloudProfile
		if prof, err = s.resolveProfile(ci.Profile); err == nil {
			cfg := mergeProfile(prof, ciInput{ci.Hostname, ci.User, ci.Password, ci.SSHKeys, ci.Packages, ci.RawUserData})
			if cfg.Hostname == "" && cfg.RawUserData == "" {
				cfg.Hostname = q.Name
			}
			if seed, err = cfg.Seed(); err != nil {
				err = badRequest("cloud-init: " + err.Error())
			}
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := h.DomainLookupByName(q.Name); err == nil {
		fail(w, badRequest("a VM named "+q.Name+" already exists on this node"))
		return
	}
	if q.Firmware == "uefi" || q.TPM || q.SecureBoot {
		caps, err := nodeFwCaps(h)
		if err == nil {
			err = q.checkFirmware(caps)
		}
		if err != nil {
			fail(w, err)
			return
		}
		q.tpmModel = caps.tpmModel()
	}
	pool, err := h.StoragePoolLookupByName(q.Pool)
	if err != nil {
		fail(w, err)
		return
	}
	var existing libvirt.StorageVol
	if q.ExistingVolume != "" {
		if existing, err = existingDisk(h, pool, &q); err != nil {
			fail(w, err)
			return
		}
	}

	var uuid string
	err = s.task(r, "create", q.Name, h.Name, func(func(float64)) error {
		volName := q.Name + ".qcow2"
		size := uint64(q.DiskGiB) << 30
		var vol libvirt.StorageVol
		if q.ExistingVolume != "" {
			vol = existing
		} else if q.BaseVolume != "" {
			base, err := h.StorageVolLookupByName(pool, q.BaseVolume)
			if err != nil {
				return err
			}
			_, baseSize, _, err := h.StorageVolGetInfo(base)
			if err != nil {
				return err
			}
			if vol, err = h.StorageVolCreateXMLFrom(pool, volumeXML(volName, "qcow2", baseSize), base, 0); err != nil {
				return err
			}
			if size > baseSize {
				if err := h.StorageVolResize(vol, size, 0); err != nil {
					h.StorageVolDelete(vol, 0)
					return err
				}
			}
		} else if vol, err = h.StorageVolCreateXML(pool, volumeXML(volName, "qcow2", size), 0); err != nil {
			return err
		}
		var seedVol *libvirt.StorageVol
		rollback := func() {
			if q.ExistingVolume == "" { // an existing volume is never ours to delete
				h.StorageVolDelete(vol, 0)
			}
			if seedVol != nil {
				h.StorageVolDelete(*seedVol, 0)
			}
		}
		path, err := h.StorageVolGetPath(vol)
		if err != nil {
			rollback()
			return err
		}
		if seed != nil {
			sv, err := h.StorageVolCreateXML(pool, volumeXML(seedVolumeName(q.Name), "raw", uint64(len(seed))), 0)
			if err != nil {
				rollback()
				return fmt.Errorf("cloud-init seed: %w", err)
			}
			seedVol = &sv
			if err := h.StorageVolUpload(sv, bytes.NewReader(seed), 0, 0, 0); err != nil {
				rollback()
				return fmt.Errorf("cloud-init seed upload: %w", err)
			}
			if q.seedPath, err = h.StorageVolGetPath(sv); err != nil {
				rollback()
				return err
			}
		}
		dom, err := h.DomainDefineXMLFlags(q.domainXML(path), libvirt.DomainDefineValidate)
		if err != nil {
			rollback()
			return err
		}
		uuid = cluster.FormatUUID(dom.UUID)
		if q.Start {
			return h.DomainCreate(dom)
		}
		return nil
	})
	if err != nil && uuid == "" {
		fail(w, err)
		return
	}
	// A VM that was defined but failed to start is still reported, with the
	// start error, so the UI can take the user to it.
	resp := map[string]string{"uuid": uuid}
	if err != nil {
		resp["startError"] = err.Error()
	}
	writeJSON(w, resp)
}
