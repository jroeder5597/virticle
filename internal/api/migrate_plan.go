package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Pure helpers for migration: parsing what a domain and its snapshots
// reference, and quoting values for the shell commands run on the nodes.

// shq quotes s for a POSIX shell as one word.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Paths and names that end up in commands on the nodes (scp, rm, ssh) are
// restricted to a conservative alphabet on top of being quoted, because scp
// may hand a remote path to a shell on the other side.
var (
	safePathRE = regexp.MustCompile(`^/[A-Za-z0-9._+=,@/-]+$`)
	hostRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	userRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
)

// migDisk is one disk device of a domain as far as migration cares.
type migDisk struct {
	Target    string
	Device    string // disk | cdrom | floppy | lun
	Type      string // file | block | network | volume
	Format    string
	Path      string // source file
	ReadOnly  bool
	Shareable bool
}

// migDomain is the read-only view of a domain definition used to plan a
// migration. It is never written back.
type migDomain struct {
	Name  string
	UUID  string
	Disks []migDisk
	Ifs   []migIf
	// Counts of devices that tie a VM to its host.
	Hostdevs int
	TPM      bool
	// TPMState2 says the TPM is an emulated one of version 2.0 (swtpm): its state is one file that replication copies.
	TPMState2 bool
	NVRAM     string
	// NVRAMFormat is "qcow2" or "raw" (libvirt: raw unless the definition says so).
	NVRAMFormat string
	// NVRAMTemplate is the firmware package file the store was made from.
	NVRAMTemplate string
	CPUMode       string
	CPUXML        string // the <cpu> element verbatim
	Arch          string
	Machine       string // machine type, e.g. pc-q35-10.0
	Emulator      string // QEMU binary path
}

type migIf struct {
	Type, Network, Bridge, Dev string
}

var (
	cpuFullRE = regexp.MustCompile(`(?s)<cpu[ >].*?</cpu>`)
	cpuSelfRE = regexp.MustCompile(`<cpu[^>]*/>`)
)

func parseMigDomain(raw string) (migDomain, error) {
	var x struct {
		Name string `xml:"name"`
		UUID string `xml:"uuid"`
		OS   struct {
			Type struct {
				Arch    string `xml:"arch,attr"`
				Machine string `xml:"machine,attr"`
			} `xml:"type"`
			NVRAM struct {
				Format   string `xml:"format,attr"`
				Template string `xml:"template,attr"`
				Path     string `xml:",chardata"`
			} `xml:"nvram"`
		} `xml:"os"`
		CPU struct {
			Mode string `xml:"mode,attr"`
		} `xml:"cpu"`
		Devices struct {
			Emulator string `xml:"emulator"`
			Disks    []struct {
				Type   string `xml:"type,attr"`
				Device string `xml:"device,attr"`
				Driver struct {
					Type string `xml:"type,attr"`
				} `xml:"driver"`
				Source struct {
					File string `xml:"file,attr"`
				} `xml:"source"`
				Target struct {
					Dev string `xml:"dev,attr"`
				} `xml:"target"`
				ReadOnly  *struct{} `xml:"readonly"`
				Shareable *struct{} `xml:"shareable"`
			} `xml:"disk"`
			Interfaces []struct {
				Type   string `xml:"type,attr"`
				Source struct {
					Network string `xml:"network,attr"`
					Bridge  string `xml:"bridge,attr"`
					Dev     string `xml:"dev,attr"`
				} `xml:"source"`
			} `xml:"interface"`
			Hostdevs []struct{} `xml:"hostdev"`
			TPM      *struct {
				Backend struct {
					Type    string `xml:"type,attr"`
					Version string `xml:"version,attr"`
				} `xml:"backend"`
			} `xml:"tpm"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return migDomain{}, err
	}
	d := migDomain{
		Name: x.Name, UUID: x.UUID, Hostdevs: len(x.Devices.Hostdevs), TPM: x.Devices.TPM != nil,
		NVRAM: strings.TrimSpace(x.OS.NVRAM.Path), NVRAMTemplate: rawNvramTemplate(raw, nvramEl{Template: x.OS.NVRAM.Template, Path: strings.TrimSpace(x.OS.NVRAM.Path)}), NVRAMFormat: map[bool]string{true: "qcow2", false: "raw"}[x.OS.NVRAM.Format == "qcow2"], CPUMode: x.CPU.Mode, Arch: x.OS.Type.Arch, Machine: x.OS.Type.Machine, Emulator: strings.TrimSpace(x.Devices.Emulator),
	}
	if t := x.Devices.TPM; t != nil && t.Backend.Type == "emulator" && (t.Backend.Version == "2.0" || t.Backend.Version == "") {
		d.TPMState2 = true
	}
	if m := cpuFullRE.FindString(raw); m != "" {
		d.CPUXML = m
	} else if m := cpuSelfRE.FindString(raw); m != "" {
		d.CPUXML = m
	}
	for _, k := range x.Devices.Disks {
		d.Disks = append(d.Disks, migDisk{
			Target: k.Target.Dev, Device: k.Device, Type: k.Type, Format: k.Driver.Type, Path: k.Source.File,
			ReadOnly: k.ReadOnly != nil, Shareable: k.Shareable != nil,
		})
	}
	for _, i := range x.Devices.Interfaces {
		d.Ifs = append(d.Ifs, migIf{Type: i.Type, Network: i.Source.Network, Bridge: i.Source.Bridge, Dev: i.Source.Dev})
		if i.Type == "hostdev" {
			d.Hostdevs++
		}
	}
	return d, nil
}

// imgInfo is one image of a backing chain as reported by qemu-img.
type imgInfo struct {
	Path        string
	Format      string
	VirtualSize uint64
}

// parseChain reads `qemu-img info --backing-chain --output=json`, which lists
// the active image first and its backing files after it.
func parseChain(out string) ([]imgInfo, error) {
	var raw []struct {
		Filename    string `json:"filename"`
		Format      string `json:"format"`
		VirtualSize uint64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("cannot read qemu-img output: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("qemu-img reported no image")
	}
	chain := make([]imgInfo, len(raw))
	for i, r := range raw {
		chain[i] = imgInfo{Path: r.Filename, Format: r.Format, VirtualSize: r.VirtualSize}
	}
	return chain, nil
}

// snapFiles are the files a snapshot's XML refers to.
type snapFiles struct {
	Memory string   // external memory state, if any
	Disks  []string // overlay files created by this snapshot
	Domain []string // disk files of the domain as it was when the snapshot was taken
	// Internal is set when disk or memory state lives inside the qcow2 image
	// itself, where a live disk mirror does not carry it.
	Internal bool
}

func parseSnapshotFiles(raw string) (snapFiles, error) {
	var x struct {
		Memory struct {
			Snapshot string `xml:"snapshot,attr"`
			File     string `xml:"file,attr"`
		} `xml:"memory"`
		Disks struct {
			Disk []struct {
				Snapshot string `xml:"snapshot,attr"`
				Source   struct {
					File string `xml:"file,attr"`
				} `xml:"source"`
			} `xml:"disk"`
		} `xml:"disks"`
		Domain struct {
			Devices struct {
				Disk []struct {
					Device string `xml:"device,attr"`
					Source struct {
						File string `xml:"file,attr"`
					} `xml:"source"`
					ReadOnly *struct{} `xml:"readonly"`
				} `xml:"disk"`
			} `xml:"devices"`
		} `xml:"domain"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return snapFiles{}, err
	}
	var f snapFiles
	if x.Memory.Snapshot == "external" {
		f.Memory = x.Memory.File
	}
	f.Internal = x.Memory.Snapshot == "internal"
	for _, d := range x.Disks.Disk {
		if d.Snapshot == "internal" {
			f.Internal = true
		}
		if d.Snapshot == "external" && d.Source.File != "" {
			f.Disks = append(f.Disks, d.Source.File)
		}
	}
	for _, d := range x.Domain.Devices.Disk {
		if d.Source.File != "" && (d.Device == "" || d.Device == "disk") && d.ReadOnly == nil {
			f.Domain = append(f.Domain, d.Source.File)
		}
	}
	return f, nil
}

// migFile is a file that must exist on the target for the VM to work there.
type migFile struct {
	Path string
	// Owned files belong to this VM: they are removed from the source after
	// a successful move. Others (read-only media, shared base images) stay.
	Owned bool
	// Active files are the writable top images that libvirt mirrors while the
	// VM runs; they are created empty on the target instead of copied.
	Active bool
	Size   uint64 // bytes allocated on the source
	Mode   string // permission bits on the source, octal; the copy gets the same
	// Skip marks a file that is not ours and already exists on the target
	// (a shared base image); it is assumed identical and not copied.
	Skip bool
	// Shared marks a file that lives on shared storage (an NFS pool both nodes mount from the same server and
	// export, at the same path). Both nodes see this one file, so it is never copied, never mirrored and never
	// deleted from the source. Skip is set as well.
	Shared bool
	// SharedPool is the name of that pool, for messages.
	SharedPool string
}

// fileSet collects files in insertion order without duplicates; a file seen
// again keeps its flags and gains Owned if any sighting was owned.
type fileSet struct {
	list []*migFile
	idx  map[string]*migFile
}

func (s *fileSet) add(p string, owned, active bool) {
	if p == "" {
		return
	}
	if s.idx == nil {
		s.idx = map[string]*migFile{}
	}
	if f, ok := s.idx[p]; ok {
		f.Owned = f.Owned || owned
		f.Active = f.Active || active
		return
	}
	f := &migFile{Path: p, Owned: owned, Active: active}
	s.idx[p] = f
	s.list = append(s.list, f)
}

func (s *fileSet) get(p string) *migFile { return s.idx[p] }

func (s *fileSet) remove(p string) {
	delete(s.idx, p)
	for i, f := range s.list {
		if f.Path == p {
			s.list = append(s.list[:i], s.list[i+1:]...)
			return
		}
	}
}

func dirOf(p string) string { return path.Dir(p) }
