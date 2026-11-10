package api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/hostconn"
)

// templateMark is the state kept in the domain's <metadata>: whether the VM
// is a template and whether it was sealed (made identity-free) for a guest
// family, "linux" or "windows".
type templateMark struct {
	Template bool
	Sealed   string
}

func getMark(h *hostconn.Host, dom libvirt.Domain) (templateMark, error) {
	raw, err := h.DomainGetMetadata(dom, int32(libvirt.DomainMetadataElement), libvirt.OptString{vpNS}, libvirt.DomainAffectConfig)
	if err != nil {
		var lv libvirt.Error
		if errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomainMetadata) {
			return templateMark{}, nil
		}
		return templateMark{}, err
	}
	var el struct {
		XMLName xml.Name
		Sealed  string `xml:"sealed,attr"`
	}
	if err := xml.Unmarshal([]byte(raw), &el); err != nil || el.XMLName.Local != "template" {
		return templateMark{}, nil
	}
	return templateMark{Template: true, Sealed: el.Sealed}, nil
}

func setMark(h *hostconn.Host, dom libvirt.Domain, m templateMark) error {
	var meta libvirt.OptString // unset removes the element
	if m.Template {
		// libvirt adds the namespace itself; declaring it here makes it fail.
		el := "<template"
		if m.Sealed != "" {
			el += fmt.Sprintf(" sealed=%q", m.Sealed)
		}
		meta = libvirt.OptString{el + "/>"}
	}
	return h.DomainSetMetadata(dom, int32(libvirt.DomainMetadataElement), meta, libvirt.OptString{"vp"}, libvirt.OptString{vpNS}, libvirt.DomainAffectConfig)
}

// diskPaths lists the file paths of a definition's disks (not CD-ROMs).
func diskPaths(x *domainXML) []string {
	var out []string
	for _, k := range x.Devices.Disks {
		if k.Device == "disk" && k.Source.File != "" {
			out = append(out, k.Source.File)
		}
	}
	return out
}

// linkedClones returns "pool/volume" for every volume in the node's pools
// that has one of paths as its backing store.
func linkedClones(h *hostconn.Host, paths []string) ([]string, error) {
	want := map[string]bool{}
	for _, p := range paths {
		want[path.Clean(p)] = true
	}
	pools, _, err := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pools {
		h.StoragePoolRefresh(p, 0) // pick up volumes created outside libvirt
		vols, _, err := h.StoragePoolListAllVolumes(p, 1, 0)
		if err != nil {
			return nil, err
		}
		for _, v := range vols {
			raw, err := h.StorageVolGetXMLDesc(v, 0)
			if err != nil {
				continue // vanished
			}
			var x struct {
				Backing struct {
					Path string `xml:"path"`
				} `xml:"backingStore"`
			}
			if xml.Unmarshal([]byte(raw), &x) == nil && x.Backing.Path != "" && want[path.Clean(x.Backing.Path)] {
				out = append(out, p.Name+"/"+v.Name)
			}
		}
	}
	return out, nil
}

func dependentsError(vols []string) badRequest {
	return badRequest("linked clones depend on these disks (delete or flatten them first): " + strings.Join(vols, ", "))
}

// vmTemplate marks a shut-off VM as a template, or turns it back into a VM.
func (s *Server) vmTemplate(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Template bool `json:"template"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		fail(w, err)
		return
	}
	if stateName(state) != "shutoff" {
		fail(w, badRequest("power the VM off first: a template is never run"))
		return
	}
	old, err := getMark(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	if !req.Template && old.Template {
		raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
		if err != nil {
			fail(w, err)
			return
		}
		var x domainXML
		if err := xml.Unmarshal([]byte(raw), &x); err != nil {
			fail(w, err)
			return
		}
		deps, err := linkedClones(h, diskPaths(&x))
		if err != nil {
			fail(w, err)
			return
		}
		if len(deps) > 0 {
			fail(w, dependentsError(deps))
			return
		}
	}
	kind := "convert to VM"
	if req.Template {
		kind = "convert to template"
	}
	err = s.task(r, kind, dom.Name, h.Name, func(func(float64)) error {
		return setMark(h, dom, templateMark{Template: req.Template, Sealed: old.Sealed})
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// vmSeal makes a running guest identity-free through the guest agent, waits
// for it to power off and marks it as a template.
func (s *Server) vmSeal(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		fail(w, err)
		return
	}
	if stateName(state) != "running" {
		fail(w, badRequest("sealing runs a script in the guest: start the VM and wait for the guest agent first"))
		return
	}
	var family string
	err = s.task(r, "seal template", dom.Name, h.Name, func(progress func(float64)) error {
		id, err := agentOS(h, dom)
		if err != nil {
			return fmt.Errorf("cannot talk to the guest agent (is it installed and connected?): %w", err)
		}
		var pid int
		timeout := 3 * time.Minute
		if id == "mswindows" {
			family, timeout = "windows", 30*time.Minute
			// sysprep runs for minutes and powers the guest off by itself.
			if pid, err = agentStart(h, dom, windowsSysprep, windowsSysprepArgs...); err != nil {
				return fmt.Errorf("starting sysprep: %w", err)
			}
		} else {
			family = "linux"
			if pid, err = agentStart(h, dom, "/bin/sh", "-c", linuxSealScript); err != nil {
				return fmt.Errorf("starting the seal script: %w", err)
			}
			res, err := agentWait(h, dom, pid, time.Minute)
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("seal script failed (exit %d): %s%s", res.ExitCode, res.Out, res.Err)
			}
		}
		progress(0.2)
		deadline := time.Now().Add(timeout)
		for {
			st, _, _, _, _, err := h.DomainGetInfo(dom)
			if err != nil {
				return err
			}
			if stateName(st) == "shutoff" {
				break
			}
			if family == "windows" {
				// A sysprep that failed exits with an error and leaves the guest running.
				if raw, err := agentCmd(h, dom, "guest-exec-status", map[string]int{"pid": pid}); err == nil {
					var es struct {
						Exited   bool   `json:"exited"`
						ExitCode int    `json:"exitcode"`
						OutData  string `json:"out-data"`
					}
					if json.Unmarshal(raw, &es) == nil && es.Exited && es.ExitCode != 0 {
						return fmt.Errorf("sysprep failed with exit code %d (see %%WINDIR%%\\System32\\Sysprep\\Panther\\setuperr.log in the guest); it is NOT marked as template", es.ExitCode)
					}
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the guest did not power off within %s; it is NOT marked as template", timeout)
			}
			time.Sleep(2 * time.Second)
		}
		progress(0.9)
		return setMark(h, dom, templateMark{Template: true, Sealed: family})
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "guest": family})
}
