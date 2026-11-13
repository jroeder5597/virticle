package api

// Persistent dirty bitmaps that a killed QEMU left in a qcow2 image.
//
// Backups and replication keep one persistent bitmap per disk and checkpoint (named virticle-...). When QEMU is
// killed (power loss, kill -9, a crashed host) the bitmaps are left marked "in use": nothing can use them again,
// and qemu-img refuses to resize the image while they are there ("Bitmap ... is inconsistent"). The checkpoint
// record in libvirt's own store then points at a bitmap that is no good.
//
// healBitmaps removes such bitmaps and the records that point at them:
//   - VM shut off: qemu-img lists the bitmaps of the image, one with the "in-use" flag cannot belong to a running
//     QEMU, and "qemu-img bitmap --remove" takes the image's write lock, so it fails (and changes nothing) if a
//     QEMU has the file open after all;
//   - VM running: QEMU reports a bitmap as "inconsistent" and block-dirty-bitmap-remove drops it.
// Only bitmaps with the names this program uses are touched. The next backup or replication run of the VM is a
// full one, which is what it would have been anyway.

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// ownBitmap says whether a bitmap name is one this program made (checkpoints, and the temporary one of a backup job).
func ownBitmap(n string) bool {
	return strings.HasPrefix(n, checkpointPrefix) || strings.HasPrefix(n, "backup-")
}

// staleBitmapsOf reads `qemu-img info --output=json` (one image or a chain: the first image counts) and returns
// the bitmaps this program made that are marked in use, which on an image nobody has open means inconsistent.
func staleBitmapsOf(info string) []string {
	type img struct {
		Spec struct {
			Data struct {
				Bitmaps []struct {
					Name  string   `json:"name"`
					Flags []string `json:"flags"`
				} `json:"bitmaps"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	var one img
	var many []img
	if json.Unmarshal([]byte(info), &many) == nil && len(many) > 0 {
		one = many[0]
	} else if json.Unmarshal([]byte(info), &one) != nil {
		return nil
	}
	var out []string
	for _, b := range one.Spec.Data.Bitmaps {
		if !ownBitmap(b.Name) {
			continue
		}
		for _, f := range b.Flags {
			if f == "in-use" || f == "inconsistent" {
				out = append(out, b.Name)
				break
			}
		}
	}
	return out
}

// removeOfflineBitmap removes the bitmap that belonged to a checkpoint from the disks of a shut-off VM. libvirt
// cannot delete a checkpoint of an inactive domain ("cannot delete checkpoint for inactive domain"), so after the
// record is dropped the bitmap would stay in the image for good, tracking every write of later runs for nothing,
// one more with every replication or backup that is stopped while the VM is shut off. Only names this program
// made are removed, and only from an image no QEMU has open (the verb fails on a locked one).
func removeOfflineBitmap(h *hostconn.Host, dom libvirt.Domain, name string) {
	if !strings.HasPrefix(name, checkpointPrefix) {
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return
	}
	md, err := parseMigDomain(raw)
	if err != nil {
		return
	}
	for _, d := range md.Disks {
		if d.Device != "disk" || d.Type != "file" || d.Path == "" {
			continue
		}
		info, err := h.QemuInfo(d.Path, false)
		if err != nil || !hasBitmap(info, name) {
			continue
		}
		if err := h.QcowBitmapRemove(d.Path, name); err != nil {
			log.Printf("removing the bitmap %s from %s: %v", name, d.Path, err)
		}
	}
}

// hasBitmap says whether the first image of a `qemu-img info --output=json` result has a bitmap of that name.
func hasBitmap(info, name string) bool {
	type img struct {
		Spec struct {
			Data struct {
				Bitmaps []struct {
					Name string `json:"name"`
				} `json:"bitmaps"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	var one img
	var many []img
	if json.Unmarshal([]byte(info), &many) == nil && len(many) > 0 {
		one = many[0]
	} else if json.Unmarshal([]byte(info), &one) != nil {
		return false
	}
	for _, b := range one.Spec.Data.Bitmaps {
		if b.Name == name {
			return true
		}
	}
	return false
}

// healBitmaps removes the inconsistent bitmaps of the VM's disks and the checkpoint records of them. It returns
// the names removed. A failure is returned too, but what could be done was done.
func healBitmaps(h *hostconn.Host, dom libvirt.Domain) ([]string, error) {
	st, _, err := h.DomainGetState(dom, 0)
	if err != nil {
		return nil, err
	}
	var removed []string
	if stateName(uint8(st)) == "shutoff" {
		raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
		if err != nil {
			return nil, err
		}
		md, err := parseMigDomain(raw)
		if err != nil {
			return nil, err
		}
		var firstErr error
		for _, d := range md.Disks {
			if d.Device != "disk" || d.Type != "file" || d.Path == "" {
				continue
			}
			info, err := h.QemuInfo(d.Path, false)
			if err != nil {
				continue // not an image qemu-img can read: nothing to heal here
			}
			names := staleBitmapsOf(info)
			if len(names) == 0 {
				continue
			}
			if err := h.QcowBitmapRemove(d.Path, names...); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed = append(removed, names...)
		}
		forgetCheckpoints(h, dom, removed)
		return removed, firstErr
	}
	removed = removeInconsistentLiveBitmaps(h, dom)
	forgetCheckpoints(h, dom, removed)
	return removed, nil
}

// forgetCheckpoints deletes the checkpoint records whose bitmaps were removed (metadata only: there is nothing
// left to merge). The temporary "backup-" bitmaps have no record.
func forgetCheckpoints(h *hostconn.Host, dom libvirt.Domain, bitmaps []string) {
	seen := map[string]bool{}
	for _, n := range bitmaps {
		if seen[n] || !strings.HasPrefix(n, checkpointPrefix) {
			continue
		}
		seen[n] = true
		if c, err := h.DomainCheckpointLookupByName(dom, n, 0); err == nil {
			if err := h.DomainCheckpointDelete(c, libvirt.DomainCheckpointDeleteMetadataOnly); err != nil {
				log.Printf("checkpoint %s of %s: %v", n, dom.Name, err)
			}
		}
	}
}

// removeInconsistentLiveBitmaps asks the running QEMU for bitmaps it reports as inconsistent and removes them.
func removeInconsistentLiveBitmaps(h *hostconn.Host, dom libvirt.Domain) []string {
	out, err := h.QEMUDomainMonitorCommand(dom, `{"execute":"query-named-block-nodes"}`, 0)
	if err != nil {
		return nil
	}
	var r struct {
		Return []struct {
			Node    string `json:"node-name"`
			Bitmaps []struct {
				Name         string `json:"name"`
				Inconsistent bool   `json:"inconsistent"`
			} `json:"dirty-bitmaps"`
		} `json:"return"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return nil
	}
	var removed []string
	for _, node := range r.Return {
		for _, b := range node.Bitmaps {
			if !b.Inconsistent || !ownBitmap(b.Name) {
				continue
			}
			cmd, _ := json.Marshal(map[string]any{"execute": "block-dirty-bitmap-remove", "arguments": map[string]string{"node": node.Node, "name": b.Name}})
			if _, err := h.QEMUDomainMonitorCommand(dom, string(cmd), 0); err != nil {
				log.Printf("removing the inconsistent bitmap %s of %s: %v", b.Name, dom.Name, err)
				continue
			}
			removed = append(removed, b.Name)
		}
	}
	return removed
}
