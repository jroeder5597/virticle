package api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cloudinit"
	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

type cloneRequest struct {
	Name   string `json:"name"`
	Linked bool   `json:"linked"`
	Start  bool   `json:"start"`
	// Sysprep gives the clone a new identity (machine-id, SSH host keys, ...).
	Sysprep bool `json:"sysprep"`
	// Guest overrides the guest family guess: "linux" or "windows".
	Guest     string          `json:"guest"`
	Customize *cloneCustomize `json:"customize"`
}

type cloneCustomize struct {
	Hostname string   `json:"hostname"`
	User     string   `json:"user"`
	Password string   `json:"password"`
	SSHKeys  []string `json:"sshKeys"`
	Packages []string `json:"packages"`
	// RawUserData replaces the generated cloud-config (Linux only).
	RawUserData string `json:"rawUserData"`
	// Profile is a saved cloud-init profile (Linux only) that the fields
	// above override or extend; see mergeProfile.
	Profile      int64 `json:"profile"`
	passwordHash string
}

const (
	mechVirtSysprep = "virt-sysprep"
	mechSealed      = "sealed template"
)

// chooseSysprep picks how a clone gets a new identity, or explains why it
// cannot. Sealed templates need nothing at clone time; otherwise Linux
// needs libguestfs on the node, and Windows can only be generalized by
// Windows itself (sealing).
func chooseSysprep(guest string, mark templateMark, haveVirtSysprep bool) (string, error) {
	if mark.Sealed != "" && mark.Sealed == guest {
		return mechSealed, nil
	}
	if guest == "windows" {
		return "", badRequest("a Windows guest can only be generalized by Windows itself: boot the source, use \"Seal and convert to template\" (runs sysprep /generalize through the guest agent), then clone the template")
	}
	if haveVirtSysprep {
		return mechVirtSysprep, nil
	}
	return "", badRequest("sysprep is not possible here. Either install libguestfs-tools (virt-sysprep) on the node, " +
		"or boot the source VM with a connected guest agent and use \"Seal and convert to template\", then clone the template")
}

// guestFamily decides which guest family a clone's identity handling targets.
func guestFamily(requested string, mark templateMark, domXML string) string {
	switch {
	case requested == "linux" || requested == "windows":
		return requested
	case mark.Sealed != "":
		return mark.Sealed
	case strings.Contains(domXML, "microsoft.com/win"):
		return "windows"
	}
	return "linux"
}

// buildSeed makes the ISO that carries the clone's credentials: a NoCloud
// seed for Linux, an unattend answer file for Windows.
func buildSeed(guest, cloneName string, c cloneCustomize) ([]byte, error) {
	host := c.Hostname
	if host == "" {
		host = cloneName
	}
	if guest == "windows" {
		if len(c.SSHKeys) > 0 || len(c.Packages) > 0 {
			return nil, badRequest("Windows unattended setup supports a computer name, an administrator account and its password only (no SSH keys or packages)")
		}
		img, err := unattendConfig{ComputerName: host, User: c.User, Password: c.Password}.image()
		if err != nil {
			return nil, badRequest(err.Error())
		}
		return img, nil
	}
	cfg := cloudinit.Config{Hostname: host, User: c.User, Password: c.Password, SSHKeys: c.SSHKeys, Packages: c.Packages, RawUserData: c.RawUserData, PasswordHash: c.passwordHash}
	cfg.EnsureGuestAgent()
	img, err := cfg.Seed()
	if err != nil {
		return nil, badRequest("cloud-init: " + err.Error())
	}
	return img, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func hasVirtSysprep(h *hostconn.Host) bool {
	return h.HasTool("virt-sysprep")
}

// cloneInfo tells the UI what a clone of this VM can do on this node.
func (s *Server) cloneInfo(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	mark, err := getMark(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		fail(w, err)
		return
	}
	guest := guestFamily("", mark, raw)
	have := hasVirtSysprep(h)
	out := map[string]any{"template": mark.Template, "sealed": mark.Sealed, "guest": guest, "virtSysprep": have}
	if mech, err := chooseSysprep(guest, mark, have); err == nil {
		out["sysprep"] = mech
	} else {
		out["sysprepProblem"] = err.Error()
	}
	writeJSON(w, out)
}

func (s *Server) vmClone(w http.ResponseWriter, r *http.Request) {
	h, src, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var q cloneRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	if !vmNameRE.MatchString(q.Name) {
		fail(w, badRequest("name must start with a letter or digit and contain only letters, digits, dot, dash and underscore"))
		return
	}
	if _, err := h.DomainLookupByName(q.Name); err == nil {
		fail(w, badRequest("a VM named "+q.Name+" already exists on this node"))
		return
	}
	state, _, _, _, _, err := h.DomainGetInfo(src)
	if err != nil {
		fail(w, err)
		return
	}
	if stateName(state) != "shutoff" {
		fail(w, badRequest("shut the source VM down before cloning it"))
		return
	}
	mark, err := getMark(h, src)
	if err != nil {
		fail(w, err)
		return
	}
	if q.Linked && !mark.Template {
		fail(w, badRequest("linked clones need a template as their source: convert the source to a template first, so its disks can no longer change"))
		return
	}
	raw, err := h.DomainGetXMLDesc(src, libvirt.DomainXMLInactive)
	if err != nil {
		fail(w, err)
		return
	}
	var x domainXML
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		fail(w, err)
		return
	}

	// Which devices are copied, shared or dropped.
	type diskPlan struct{ srcPath, newName string }
	var plan []diskPlan
	oldSeed := ""
	for _, k := range x.Devices.Disks {
		switch {
		case k.Device == "disk" && k.ReadOnly == nil:
			if k.Source.File == "" {
				fail(w, badRequest(fmt.Sprintf("disk %s is not a plain file in a storage pool (only file-backed disks can be cloned)", k.Target.Dev)))
				return
			}
			name := q.Name + ".qcow2"
			if len(plan) > 0 {
				name = q.Name + "-" + k.Target.Dev + ".qcow2"
			}
			plan = append(plan, diskPlan{k.Source.File, name})
		case k.Device == "cdrom" && k.Source.File != "" && path.Base(k.Source.File) == seedVolumeName(src.Name):
			oldSeed = k.Source.File
		}
	}
	if len(plan) == 0 {
		fail(w, badRequest("the source VM has no disk to clone"))
		return
	}

	guest := guestFamily(q.Guest, mark, raw)
	mechanism := ""
	if q.Sysprep {
		if mechanism, err = chooseSysprep(guest, mark, hasVirtSysprep(h)); err != nil {
			fail(w, err)
			return
		}
	}
	var seed []byte
	if q.Customize != nil {
		c := *q.Customize
		if c.Profile != 0 {
			if guest == "windows" {
				fail(w, badRequest("cloud-init profiles apply to Linux guests only"))
				return
			}
			prof, err := s.resolveProfile(c.Profile)
			if err != nil {
				fail(w, err)
				return
			}
			cfg := mergeProfile(prof, ciInput{c.Hostname, c.User, c.Password, c.SSHKeys, c.Packages, c.RawUserData})
			c = cloneCustomize{Hostname: cfg.Hostname, User: cfg.User, Password: cfg.Password, SSHKeys: cfg.SSHKeys,
				Packages: cfg.Packages, RawUserData: cfg.RawUserData, passwordHash: cfg.PasswordHash}
		}
		if seed, err = buildSeed(guest, q.Name, c); err != nil {
			fail(w, err)
			return
		}
	}

	var uuid, nvramNote string
	err = s.task(r, "clone", q.Name+" (from "+src.Name+")", h.Name, func(progress func(float64)) error {
		var created []libvirt.StorageVol
		var defined *libvirt.Domain
		rollback := func(cause error) error {
			var left []string
			if defined != nil {
				flags := libvirt.DomainUndefineNvram | libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata
				if err := h.DomainUndefineFlags(*defined, flags); err != nil {
					left = append(left, "VM definition "+q.Name+": "+err.Error())
				}
			}
			for i := len(created) - 1; i >= 0; i-- {
				if err := h.StorageVolDelete(created[i], 0); err != nil {
					left = append(left, "volume "+created[i].Name+": "+err.Error())
				}
			}
			if len(left) > 0 {
				return fmt.Errorf("%w (rollback incomplete, left behind: %s)", cause, strings.Join(left, "; "))
			}
			return cause
		}

		newPaths := map[string]string{}
		var firstPool libvirt.StoragePool
		var newDisks []string
		for i, d := range plan {
			progress(float64(i) / float64(len(plan)+1))
			srcVol, err := lookupVolByPath(h, d.srcPath)
			if err != nil {
				return rollback(fmt.Errorf("disk %s is not in any storage pool: %w", d.srcPath, err))
			}
			pool, err := h.StoragePoolLookupByVolume(srcVol)
			if err != nil {
				return rollback(err)
			}
			if i == 0 {
				firstPool = pool
			}
			vraw, err := h.StorageVolGetXMLDesc(srcVol, 0)
			if err != nil {
				return rollback(err)
			}
			var vx struct {
				Capacity uint64 `xml:"capacity"`
				Format   struct {
					Type string `xml:"type,attr"`
				} `xml:"target>format"`
			}
			if err := xml.Unmarshal([]byte(vraw), &vx); err != nil {
				return rollback(err)
			}
			var nv libvirt.StorageVol
			if q.Linked {
				nv, err = h.StorageVolCreateXML(pool, linkedVolumeXML(d.newName, vx.Capacity, d.srcPath, vx.Format.Type), 0)
			} else {
				nv, err = h.StorageVolCreateXMLFrom(pool, volumeXML(d.newName, vx.Format.Type, vx.Capacity), srcVol, 0)
			}
			if err != nil {
				return rollback(fmt.Errorf("disk %s: %w", d.newName, err))
			}
			created = append(created, nv)
			np, err := h.StorageVolGetPath(nv)
			if err != nil {
				return rollback(err)
			}
			newPaths[d.srcPath] = np
			newDisks = append(newDisks, np)
		}

		edit := cloneEdit{Name: q.Name, Disks: newPaths}
		if oldSeed != "" {
			edit.Disks[oldSeed] = ""
		}
		if seed != nil {
			sv, err := h.StorageVolCreateXML(firstPool, volumeXML(seedVolumeName(q.Name), "raw", uint64(len(seed))), 0)
			if err != nil {
				return rollback(fmt.Errorf("seed image: %w", err))
			}
			created = append(created, sv)
			if err := h.StorageVolUpload(sv, bytes.NewReader(seed), 0, 0, 0); err != nil {
				return rollback(fmt.Errorf("seed image upload: %w", err))
			}
			if edit.AddCDROM, err = h.StorageVolGetPath(sv); err != nil {
				return rollback(err)
			}
		}
		newXML, err := cloneDomainXML(raw, edit)
		if err != nil {
			return rollback(err)
		}
		if n, ok := parseNvramEl(raw); ok && n.Format == "qcow2" {
			// The source's qcow2 store names itself as template; the clone's
			// new store is built from the distribution's template.
			newXML = withRawNvramTemplate(newXML, rawNvramTemplate(raw, n))
		}

		if mechanism == mechVirtSysprep {
			progress(float64(len(plan)) / float64(len(plan)+1))
			if out, err := h.Sysprep(newDisks...); err != nil {
				return rollback(fmt.Errorf("virt-sysprep failed: %v: %s", err, tail(out, 1500)))
			}
		}

		dom, err := h.DomainDefineXMLFlags(newXML, libvirt.DomainDefineValidate)
		if err != nil {
			return rollback(err)
		}
		defined = &dom
		uuid = cluster.FormatUUID(dom.UUID)
		nvramNote = useQcow2Nvram(h, dom)
		progress(0.98)
		if q.Start {
			if err := nfsStartHint(h, h.DomainCreate(dom)); err != nil {
				return fmt.Errorf("the clone was created but did not start: %w", err)
			}
		}
		return nil
	})
	if err != nil && uuid == "" {
		fail(w, err)
		return
	}
	resp := map[string]any{"uuid": uuid, "sysprep": mechanism, "guest": guest}
	if nvramNote != "" {
		resp["warning"] = nvramNote
	}
	if err != nil {
		resp["startError"] = err.Error()
	}
	writeJSON(w, resp)
}

// lookupVolByPath finds a volume by its file path, letting the pools rescan
// once if it was created outside libvirt.
func lookupVolByPath(h *hostconn.Host, p string) (libvirt.StorageVol, error) {
	vol, err := h.StorageVolLookupByPath(p)
	if err == nil {
		return vol, nil
	}
	if pools, _, perr := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive); perr == nil {
		for _, pool := range pools {
			h.StoragePoolRefresh(pool, 0)
		}
	}
	return h.StorageVolLookupByPath(p)
}

func linkedVolumeXML(name string, capacity uint64, backing, backingFormat string) string {
	bf := ""
	if backingFormat != "" {
		bf = fmt.Sprintf("<format type='%s'/>", esc(backingFormat))
	}
	return fmt.Sprintf(`<volume><name>%s</name><capacity unit='bytes'>%d</capacity><target><format type='qcow2'/></target><backingStore><path>%s</path>%s</backingStore></volume>`,
		esc(name), capacity, esc(backing), bf)
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
