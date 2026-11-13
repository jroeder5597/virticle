package api

// The part of a backup run that is about incremental backups: gathering what
// decideBackup needs from the nodes, making the checkpoint with the backup,
// cleaning up checkpoints, and checking that a backup's files are all there.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"time"

	"virticle/internal/hostconn"
)

const whyNoList = "libvirt could not tell what it has tracked of this VM so far, so the disks were copied whole."

// planIncremental decides between a full and an incremental backup of the
// running or shut-off VM m.
func (s *Server) planIncremental(m *bkRun, pd poolDir, allQcow2 bool) backupPlan {
	in := incrInput{Live: m.live, AllQcow2: allQcow2, Target: m.target, Checkpoints: map[string]bool{}}
	for _, d := range m.disks {
		in.Layout = append(in.Layout, diskLayout{Target: d.Target, Source: d.Base, Virt: d.Virt})
	}
	if !m.live || !allQcow2 {
		return decideBackup(in)
	}
	if v, err := m.src.ConnectGetLibVersion(); err != nil || v < minCheckpointLibvirt {
		return decideBackup(in) // Supported is false
	}
	cks, err := listCheckpoints(m.src, m.dom)
	if err != nil {
		log.Printf("backup %s: listing checkpoints: %v", m.name, err)
		return backupPlan{Kind: kindFull, Reason: whyNoList}
	}
	in.Supported = true
	for _, c := range cks {
		in.Checkpoints[c.Name] = true
	}
	if items, err := scanPool(m.dst, pd); err == nil {
		for _, it := range items {
			if it.Err == "" && it.Manifest.UUID == m.uuid {
				in.Backups = append(in.Backups, chainBackup{Dir: it.Dir, M: it.Manifest})
			}
		}
	}
	in.Intact = func(dir string) bool {
		for _, b := range in.Backups {
			if b.Dir == dir {
				return filesIntact(m.dst, pd.Dir+"/"+dir, b.M) == nil
			}
		}
		return false
	}
	return decideBackup(in)
}

// filesIntact checks that every file of a backup is there, with the size its
// manifest records for the disk images.
func filesIntact(h *hostconn.Host, dir string, m *backupManifest) error {
	files := m.files()
	sizes, err := h.FilesSize(dir, files)
	if err != nil {
		return err
	}
	bytesOf := map[string]string{}
	for i, f := range files {
		if sizes[i] == "MISSING" {
			return fmt.Errorf("%s is missing", f)
		}
		bytesOf[f] = sizes[i]
	}
	for _, d := range m.Disks {
		if bytesOf[d.File] != strconv.FormatUint(d.Bytes, 10) {
			return fmt.Errorf("%s has %s bytes, the manifest says %d", d.File, bytesOf[d.File], d.Bytes)
		}
	}
	return nil
}

// newCheckpoint returns a name for the checkpoint of this run and the XML to create it.
func (m *bkRun) newCheckpoint(stamp time.Time) (name, xml string) {
	var b [3]byte
	_, _ = rand.Read(b[:])
	name = newCheckpointName(stamp, hex.EncodeToString(b[:]))
	var backedUp []string
	for _, d := range m.disks {
		backedUp = append(backedUp, d.Target)
	}
	return name, checkpointXML(name, m.target, backedUp, m.skip)
}

// removeNewCheckpoint deletes the checkpoint this run made, after a failure or
// a cancel. The checkpoint of the parent is untouched: libvirt merges the new
// bitmap back, so the next incremental backup still sees every change.
func (m *bkRun) removeNewCheckpoint() {
	if m.ckName == "" {
		return
	}
	if err := deleteCheckpoint(m.src, m.dom, m.ckName); err != nil {
		log.Printf("backup %s: removing checkpoint %s: %v", m.name, m.ckName, err)
	}
	m.ckName = ""
}

// cleanCheckpoints runs after a backup has been written: it deletes the
// checkpoints that can no longer be built on.
func (m *bkRun) cleanCheckpoints() {
	if m.ckName == "" {
		return
	}
	cks, err := listCheckpoints(m.src, m.dom)
	if err != nil {
		log.Printf("backup %s: listing checkpoints: %v", m.name, err)
		return
	}
	for _, n := range staleCheckpoints(cks, m.target, m.ckName) {
		if err := deleteCheckpoint(m.src, m.dom, n); err != nil {
			log.Printf("backup %s: removing checkpoint %s: %v", m.name, n, err)
		}
	}
}
