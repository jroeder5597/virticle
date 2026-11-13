package api

import (
	"encoding/xml"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// Mounting a shared pool that did not come up with the node.
//
// libvirt mounts a pool that is set to autostart when its storage daemon starts. If the NFS server is away
// then (a node that boots in a power cut, a server that restarts at the same time), the mount fails, the pool
// stays stopped, and libvirt does not try again: the share would stay unmounted until someone starts the pool by
// hand. So when a node has been up for less than healWindow and an autostart NFS pool is stopped while the server
// answers on its NFS port, the controller starts the pool. A pool somebody stopped later, on a node that has been
// up for longer, is left alone.
const healWindow = time.Hour

var healing sync.Map // *hostconn.Host -> struct{}: an attempt is running

// healSharedPools starts, in the background, the stopped autostart NFS pools of a node that booted recently.
func healSharedPools(h *hostconn.Host) {
	if _, busy := healing.LoadOrStore(h, struct{}{}); busy {
		return
	}
	go func() {
		defer healing.Delete(h)
		pools, _, err := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsInactive|libvirt.ConnectListStoragePoolsAutostart|libvirt.ConnectListStoragePoolsNetfs)
		if err != nil || len(pools) == 0 {
			return
		}
		if !bootedRecently(h) {
			return
		}
		for _, p := range pools {
			raw, err := h.StoragePoolGetXMLDesc(p, 0)
			if err != nil {
				continue
			}
			var x poolXML
			if xml.Unmarshal([]byte(raw), &x) != nil || x.Type != "netfs" || !nfsServerRE.MatchString(x.Source.Host.Name) {
				continue
			}
			if _, err := h.Run(reachCmd(x.Source.Host.Name, "2049")); err != nil {
				continue // the server still does not answer: nothing to do yet
			}
			if err := h.StoragePoolCreate(p, 0); err != nil {
				log.Printf("node %s: mounting the storage %s that did not start with the node: %v", h.Name, p.Name, firstLine(err.Error()))
				continue
			}
			nfsProber.forget(h)
			log.Printf("node %s: the storage %s did not start with the node (its server was away); it is mounted now", h.Name, p.Name)
		}
	}()
}

// bootedRecently reports whether the node has been up for less than healWindow.
func bootedRecently(h *hostconn.Host) bool {
	out, err := h.Run("cut -d. -f1 /proc/uptime")
	if err != nil {
		return false
	}
	secs, err := strconv.Atoi(strings.TrimSpace(out))
	return err == nil && time.Duration(secs)*time.Second < healWindow
}
