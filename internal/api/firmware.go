package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/domxml"
	"virticle/internal/hostconn"
)

// fwCaps is what a node can do for new VMs, derived from libvirt's domain
// capabilities for x86_64/q35/kvm.
type fwCaps struct {
	UEFI       bool     `json:"uefi"`
	SecureBoot bool     `json:"secureBoot"`
	TPM        bool     `json:"tpm"`
	TPMModels  []string `json:"tpmModels"`
}

func parseFwCaps(raw string) (fwCaps, error) {
	var x struct {
		OS struct {
			Supported string `xml:"supported,attr"`
			Enums     []struct {
				Name   string   `xml:"name,attr"`
				Values []string `xml:"value"`
			} `xml:"enum"`
			Loader struct {
				Supported string `xml:"supported,attr"`
				Enums     []struct {
					Name   string   `xml:"name,attr"`
					Values []string `xml:"value"`
				} `xml:"enum"`
			} `xml:"loader"`
		} `xml:"os"`
		Devices struct {
			TPM struct {
				Supported string `xml:"supported,attr"`
				Enums     []struct {
					Name   string   `xml:"name,attr"`
					Values []string `xml:"value"`
				} `xml:"enum"`
			} `xml:"tpm"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return fwCaps{}, err
	}
	var c fwCaps
	c.TPMModels = []string{}
	has := func(vals []string, v string) bool {
		for _, s := range vals {
			if s == v {
				return true
			}
		}
		return false
	}
	for _, e := range x.OS.Enums {
		if e.Name == "firmware" && has(e.Values, "efi") {
			c.UEFI = x.OS.Supported == "yes"
		}
	}
	for _, e := range x.OS.Loader.Enums {
		if e.Name == "secure" && has(e.Values, "yes") {
			c.SecureBoot = c.UEFI && x.OS.Loader.Supported == "yes"
		}
	}
	if x.Devices.TPM.Supported == "yes" {
		var models, backends []string
		for _, e := range x.Devices.TPM.Enums {
			switch e.Name {
			case "model":
				models = e.Values
			case "backendModel":
				backends = e.Values
			}
		}
		if has(backends, "emulator") {
			for _, m := range []string{"tpm-crb", "tpm-tis"} { // crb first: what Windows 11 and recent Linux prefer
				if has(models, m) {
					c.TPMModels = append(c.TPMModels, m)
				}
			}
			c.TPM = len(c.TPMModels) > 0
		}
	}
	return c, nil
}

func nodeFwCaps(h *hostconn.Host) (fwCaps, error) {
	raw, err := h.ConnectGetDomainCapabilities(libvirt.OptString{}, libvirt.OptString{"x86_64"}, libvirt.OptString{"q35"}, libvirt.OptString{"kvm"}, 0)
	if err != nil {
		return fwCaps{}, err
	}
	return parseFwCaps(raw)
}

// checkFirmware returns a clear error when the request asks for something the
// node cannot do.
func (q createVMRequest) checkFirmware(c fwCaps) error {
	switch q.Firmware {
	case "", "bios":
		if q.SecureBoot {
			return badRequest("Secure Boot needs UEFI firmware")
		}
	case "uefi":
		if !c.UEFI {
			return badRequest("this node cannot run UEFI guests: libvirt reports no usable EFI firmware (is the ovmf package installed?)")
		}
		if q.SecureBoot && !c.SecureBoot {
			return badRequest("this node has no Secure Boot capable UEFI firmware")
		}
	default:
		return badRequest("firmware must be bios or uefi")
	}
	if q.TPM && !c.TPM {
		return badRequest("this node cannot provide an emulated TPM 2.0 (libvirt reports no emulator backend: install swtpm and swtpm-tools)")
	}
	return nil
}

// fallBackToBIOS drops a firmware that only came from the server default when
// the node cannot run it, so an omitted field never fails on an older node.
func (q *createVMRequest) fallBackToBIOS(c fwCaps) bool {
	if q.fwDefaulted && q.Firmware == "uefi" && !c.UEFI && !q.SecureBoot {
		q.Firmware = "bios"
		return true
	}
	return false
}

// tpmModel picks the device model: tpm-crb where the node has it.
func (c fwCaps) tpmModel() string {
	if len(c.TPMModels) > 0 {
		return c.TPMModels[0]
	}
	return "tpm-crb"
}

func tpmXML(model string) string {
	return fmt.Sprintf("<tpm model='%s'><backend type='emulator' version='2.0'/></tpm>", esc(model))
}

func (s *Server) firmwareCaps(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := nodeFwCaps(h)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, c)
}

// fwInfo is the firmware state shown on the Hardware tab.
type fwInfo struct {
	Firmware   string `json:"firmware"` // bios | uefi
	SecureBoot bool   `json:"secureBoot"`
	NVRAM      string `json:"nvram,omitempty"`
	TPMModel   string `json:"tpmModel,omitempty"`
	TPMVersion string `json:"tpmVersion,omitempty"`
}

func firmwareInfo(raw string) fwInfo {
	var x struct {
		OS struct {
			Firmware string `xml:"firmware,attr"`
			Loader   struct {
				Secure string `xml:"secure,attr"`
				Path   string `xml:",chardata"`
			} `xml:"loader"`
			NVRAM struct {
				Path string `xml:",chardata"`
			} `xml:"nvram"`
			Features []struct {
				Name    string `xml:"name,attr"`
				Enabled string `xml:"enabled,attr"`
			} `xml:"firmware>feature"`
		} `xml:"os"`
		Devices struct {
			TPM *struct {
				Model   string `xml:"model,attr"`
				Backend struct {
					Version string `xml:"version,attr"`
				} `xml:"backend"`
			} `xml:"tpm"`
		} `xml:"devices"`
	}
	f := fwInfo{Firmware: "bios"}
	if xml.Unmarshal([]byte(raw), &x) != nil {
		return f
	}
	p := strings.ToLower(x.OS.Loader.Path)
	if x.OS.Firmware == "efi" || strings.Contains(p, "ovmf") || strings.Contains(p, "edk2") {
		f.Firmware = "uefi"
	}
	f.SecureBoot = x.OS.Loader.Secure == "yes"
	for _, ft := range x.OS.Features {
		if ft.Name == "secure-boot" && ft.Enabled == "yes" {
			f.SecureBoot = true
		}
	}
	f.NVRAM = strings.TrimSpace(x.OS.NVRAM.Path)
	if t := x.Devices.TPM; t != nil {
		f.TPMModel, f.TPMVersion = t.Model, t.Backend.Version
	}
	return f
}

// vmTPM adds or removes the emulated TPM of a shut-off VM in its saved
// definition. Switching between BIOS and UEFI is deliberately not offered:
// the guest's boot entries and disk layout depend on it.
func (s *Server) vmTPM(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if c.running {
		fail(w, badRequest("power the VM off first: a TPM can only be added or removed while the VM is shut off"))
		return
	}
	_, cfg, err := c.docs()
	if err != nil {
		fail(w, err)
		return
	}
	have := cfg.Devices("tpm")
	kind := "remove TPM"
	if req.Enabled {
		kind = "add TPM"
	}
	// libvirt cannot attach or detach a TPM in the saved definition through
	// the device API, so the saved XML is edited in place (one element
	// inserted or removed, the rest untouched) and defined again.
	err = s.task(r, kind, c.dom.Name, c.h.Name, func(func(float64)) error {
		var edit domxml.Splice
		if req.Enabled {
			if len(have) > 0 {
				return badRequest("this VM already has a TPM")
			}
			caps, err := nodeFwCaps(c.h)
			if err != nil {
				return err
			}
			if !caps.TPM {
				return badRequest("this node cannot provide an emulated TPM 2.0 (install swtpm and swtpm-tools)")
			}
			edit = cfg.AppendChild(cfg.Root.Child("devices"), tpmXML(caps.tpmModel()))
		} else {
			if len(have) == 0 {
				return badRequest("this VM has no TPM")
			}
			edit = cfg.Remove(have[0])
		}
		out, err := domxml.Apply(cfg.Src, []domxml.Splice{edit})
		if err != nil {
			return err
		}
		_, err = c.h.DomainDefineXMLFlags(out, libvirt.DomainDefineValidate)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	res := c.result("config")
	if !req.Enabled {
		res.addNote("The TPM's saved state stays on the node until the VM is deleted; adding a TPM again reuses it.")
	}
	writeJSON(w, res)
}
