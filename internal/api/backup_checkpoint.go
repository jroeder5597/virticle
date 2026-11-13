package api

// The libvirt side of incremental backups: listing, deleting and dropping the
// checkpoints this program makes.
//
// What was found on libvirt 11.3 / QEMU 10 (Debian 13), and what it means here:
//
//   - A running VM keeps the bitmaps across a clean shutdown and start; after a
//     killed QEMU they are marked in use and the next incremental backup fails
//     with "checkpoint inconsistent". The backup then falls back to a full one.
//   - Deleting a checkpoint whose bitmap is gone fails unless the delete is
//     metadata only. libvirtd 11.3 crashed once when such checkpoints were
//     deleted oldest first, so a drop always goes newest first.
//   - An external snapshot carries the bitmaps to its overlay, an internal one
//     on a shut-off VM leaves them alone; both keep incremental backups valid. A
//     revert does not: bitmaps may then miss writes, so a revert drops them.
//   - A migration or a disk move does not take bitmaps along, so they drop them (not tried what libvirt does when they are left).
//   - An external snapshot moves the disk to a new overlay file; the disk layout then differs from the last backup and the next one is full.

import (
	"encoding/xml"
	"log"
	"sort"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// VIR_DOMAIN_CHECKPOINT_XML_SIZE, which go-libvirt does not name.
const checkpointXMLSize = 4

// minCheckpointLibvirt is the oldest libvirt (as it numbers versions) that is used for
// incremental backups: backup and checkpoints exist since 6.0, but the first releases had
// many bugs around bitmaps; 7.0 is the first with the interface in its final shape.
const minCheckpointLibvirt = 7000000

type ckXML struct {
	Name         string `xml:"name"`
	Description  string `xml:"description"`
	CreationTime int64  `xml:"creationTime"`
	Disks        []struct {
		Name string `xml:"name,attr"`
		Size uint64 `xml:"size,attr"`
	} `xml:"disks>disk"`
}

// listCheckpoints returns this program's checkpoints of the VM, newest first.
func listCheckpoints(h *hostconn.Host, dom libvirt.Domain) ([]ckInfo, error) {
	cks, _, err := h.DomainListAllCheckpoints(dom, 1, 0)
	if err != nil {
		return nil, err
	}
	var out []ckInfo
	for _, c := range cks {
		if len(c.Name) < len(checkpointPrefix) || c.Name[:len(checkpointPrefix)] != checkpointPrefix {
			continue
		}
		x, err := h.DomainCheckpointGetXMLDesc(c, 2) // no embedded domain definition
		if err != nil {
			continue
		}
		var v ckXML
		if xml.Unmarshal([]byte(x), &v) != nil {
			continue
		}
		out = append(out, ckInfo{Name: c.Name, Target: v.Description, Created: v.CreationTime})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

// changedBytes is how many bytes libvirt says changed since the checkpoint.
func changedBytes(h *hostconn.Host, dom libvirt.Domain, name string) (uint64, bool) {
	c, err := h.DomainCheckpointLookupByName(dom, name, 0)
	if err != nil {
		return 0, false
	}
	x, err := h.DomainCheckpointGetXMLDesc(c, checkpointXMLSize|2)
	if err != nil {
		return 0, false
	}
	var v ckXML
	if xml.Unmarshal([]byte(x), &v) != nil {
		return 0, false
	}
	var sum uint64
	for _, d := range v.Disks {
		sum += d.Size
	}
	return sum, true
}

// deleteCheckpoint removes one checkpoint. If libvirt cannot merge its bitmap
// (the bitmap is gone or marked in use after a crash), the record alone is deleted.
func deleteCheckpoint(h *hostconn.Host, dom libvirt.Domain, name string) error {
	c, err := h.DomainCheckpointLookupByName(dom, name, 0)
	if err != nil {
		return nil // already gone
	}
	if err := h.DomainCheckpointDelete(c, 0); err == nil {
		return nil
	}
	return h.DomainCheckpointDelete(c, libvirt.DomainCheckpointDeleteMetadataOnly)
}

// dropBackupCheckpoints deletes all of this program's checkpoints of the VM,
// newest first. The next backup of the VM is then a full one. It is called
// wherever the bitmaps can no longer be trusted or would get in the way: a
// snapshot revert, a migration, a disk move, a backup that found them broken.
// Errors are logged: a leftover checkpoint only ever makes a backup fall back
// to full.
func dropBackupCheckpoints(h *hostconn.Host, dom libvirt.Domain) {
	cks, err := listCheckpoints(h, dom)
	if err != nil {
		log.Printf("checkpoints of %s: %v", dom.Name, err)
		return
	}
	for _, c := range cks {
		if err := deleteCheckpoint(h, dom, c.Name); err != nil {
			log.Printf("dropping checkpoint %s of %s: %v", c.Name, dom.Name, err)
		}
	}
}
