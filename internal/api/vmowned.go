package api

import (
	"path"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// vmOwnedExtras lists the files a deleted VM leaves behind besides its disks,
// all of which the VM itself owns: the overlay, memory and earlier disk files
// its snapshot metadata names (external snapshots), and the cloud-init seed
// volume this app made for it when it sits next to a disk of the VM (it may
// have been ejected from the VM). Paths already in have are not repeated.
func vmOwnedExtras(h *hostconn.Host, dom libvirt.Domain, have []string) ([]string, error) {
	known := map[string]bool{}
	for _, p := range have {
		known[path.Clean(p)] = true
	}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if p = path.Clean(p); !known[p] {
			known[p] = true
			out = append(out, p)
		}
	}
	snaps, _, err := h.DomainListAllSnapshots(dom, 1, 0)
	if err != nil {
		return nil, err
	}
	for _, sn := range snaps {
		x, err := h.DomainSnapshotGetXMLDesc(sn, uint32(libvirt.DomainSnapshotXMLSecure))
		if err != nil {
			return nil, err
		}
		f, err := parseSnapshotFiles(x)
		if err != nil {
			return nil, err
		}
		add(f.Memory)
		for _, p := range f.Disks {
			add(p)
		}
		for _, p := range f.Domain {
			add(p)
		}
	}
	for _, p := range have {
		seed := path.Join(path.Dir(p), seedVolumeName(dom.Name))
		if known[seed] {
			continue
		}
		if _, err := h.StorageVolLookupByPath(seed); err == nil {
			add(seed)
		}
	}
	return out, nil
}

// withTPMForCleanup makes libvirt's undefine remove TPM state that a removed
// TPM left behind. libvirt keeps the swtpm state directory when a TPM device
// is taken out of a definition (adding one again reuses it), and deletes it
// only for a TPM that is in the definition being undefined (the Tpm flag of
// undefine). So a VM without a TPM gets one in its saved definition right
// before it is deleted. It returns the definition to put back if the delete
// then fails ("" when nothing was changed). Failure here never blocks the
// delete.
func withTPMForCleanup(h *hostconn.Host, dom libvirt.Domain) string {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive)
	if err != nil || strings.Contains(raw, "<tpm") {
		return ""
	}
	caps, err := nodeFwCaps(h)
	if err != nil || !caps.TPM {
		return ""
	}
	i := strings.LastIndex(raw, "</devices>")
	if i < 0 {
		return ""
	}
	with := raw[:i] + tpmXML(caps.tpmModel()) + "\n  " + raw[i:]
	if _, err := h.DomainDefineXMLFlags(with, libvirt.DomainDefineValidate); err != nil {
		return ""
	}
	return raw
}
