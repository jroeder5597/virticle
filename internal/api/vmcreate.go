package api

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/cloudinit"
	"virt-proj/internal/cluster"
)

type createVMRequest struct {
	Name      string `json:"name"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memoryMiB"`
	Firmware  string `json:"firmware"` // bios | uefi
	// Disk: a new volume "<name>.qcow2" in Pool. With BaseVolume set it
	// starts as a copy of that image (a cloud image), grown to DiskGiB.
	Pool       string `json:"pool"`
	DiskGiB    int    `json:"diskGiB"`
	BaseVolume string `json:"baseVolume"`
	// ISO is the path of an installer image to attach as a CD-ROM.
	ISO     string `json:"iso"`
	Network string `json:"network"`
	Start   bool   `json:"start"`
	// CloudInit, when set, attaches a NoCloud seed as a read-only SATA CD-ROM
	// (volume "<name>-seed.iso" in Pool).
	CloudInit *cloudInitRequest `json:"cloudInit"`

	seedPath string // set while creating, once the seed volume exists
}

type cloudInitRequest struct {
	Hostname    string   `json:"hostname"`
	User        string   `json:"user"`
	Password    string   `json:"password"`
	SSHKeys     []string `json:"sshKeys"`
	Packages    []string `json:"packages"`
	RawUserData string   `json:"rawUserData"`
}

// seedVolumeName is also how vmDelete recognises the seed of a VM.
func seedVolumeName(vm string) string { return vm + "-seed.iso" }

var vmNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (q createVMRequest) domainXML(diskPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<domain type='kvm'>\n  <name>%s</name>\n  <memory unit='MiB'>%d</memory>\n  <vcpu>%d</vcpu>\n", esc(q.Name), q.MemoryMiB, q.VCPUs)
	fw := ""
	if q.Firmware == "uefi" {
		fw = " firmware='efi'"
	}
	fmt.Fprintf(&b, "  <os%s>\n    <type arch='x86_64' machine='q35'>hvm</type>\n    <boot dev='hd'/>\n", fw)
	if q.ISO != "" {
		b.WriteString("    <boot dev='cdrom'/>\n")
	}
	b.WriteString("  </os>\n  <features><acpi/><apic/></features>\n  <cpu mode='host-model'/>\n  <clock offset='utc'/>\n  <devices>\n")
	fmt.Fprintf(&b, "    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2' discard='unmap'/>\n      <source file='%s'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>\n", esc(diskPath))
	if q.ISO != "" {
		fmt.Fprintf(&b, "    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='%s'/>\n      <target dev='sda' bus='sata'/>\n      <readonly/>\n    </disk>\n", esc(q.ISO))
	}
	if q.seedPath != "" {
		fmt.Fprintf(&b, "    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='%s'/>\n      <target dev='sdb' bus='sata'/>\n      <readonly/>\n    </disk>\n", esc(q.seedPath))
	}
	if q.Network != "" {
		fmt.Fprintf(&b, "    <interface type='network'>\n      <source network='%s'/>\n      <model type='virtio'/>\n    </interface>\n", esc(q.Network))
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
	case q.BaseVolume == "" && q.DiskGiB < 1:
		err = badRequest("disk size is required")
	}
	var seed []byte
	if ci := q.CloudInit; ci != nil && err == nil {
		cfg := cloudinit.Config{Hostname: ci.Hostname, User: ci.User, Password: ci.Password, SSHKeys: ci.SSHKeys, Packages: ci.Packages, RawUserData: ci.RawUserData}
		if cfg.Hostname == "" && cfg.RawUserData == "" {
			cfg.Hostname = q.Name
		}
		if seed, err = cfg.Seed(); err != nil {
			err = badRequest("cloud-init: " + err.Error())
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
	pool, err := h.StoragePoolLookupByName(q.Pool)
	if err != nil {
		fail(w, err)
		return
	}

	var uuid string
	err = s.task(r, "create", q.Name, h.Name, func(func(float64)) error {
		volName := q.Name + ".qcow2"
		size := uint64(q.DiskGiB) << 30
		var vol libvirt.StorageVol
		if q.BaseVolume != "" {
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
			h.StorageVolDelete(vol, 0)
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
