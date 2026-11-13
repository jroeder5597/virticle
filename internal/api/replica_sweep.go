package api

// Replication work directories (.virticle-rp.XXXXXX) that nobody owns. A run that is cut short by a stop of the
// controller (or of the node command) cannot clean up after itself, and the list of such leftovers lives in memory
// only (see sweepWork). A directory in a pool is a replica when a replica domain on that node names it, or when the
// replication setting of a VM on another node does; every other one of them is a staging directory, or what is left
// of a replica that was deleted by hand. One that is older than orphanAge and owned by nobody is removed. The
// check runs at most once an hour per node, from the planning of a run.

import (
	"log"
	"sync"
	"time"

	"virticle/internal/hostconn"
)

const (
	orphanAge      = 12 * time.Hour
	orphanInterval = time.Hour
)

var sweptAt = struct {
	sync.Mutex
	m map[int64]time.Time
}{m: map[int64]time.Time{}}

// orphanDirs says which of the listed work directories (name -> modification time) are to be removed: not in
// known (full paths) and older than orphanAge at now.
func orphanDirs(pool string, listed map[string]int64, known map[string]bool, now time.Time) []string {
	var out []string
	for name, mt := range listed {
		full := pool + "/" + name
		if known[full] || now.Sub(time.Unix(mt, 0)) < orphanAge {
			continue
		}
		out = append(out, full)
	}
	return out
}

// sweepReplicaOrphans removes the unowned replication work directories of a node (at most once an hour).
func (s *Server) sweepReplicaOrphans(nodeID int64, h *hostconn.Host) {
	now := time.Now()
	sweptAt.Lock()
	if now.Sub(sweptAt.m[nodeID]) < orphanInterval {
		sweptAt.Unlock()
		return
	}
	sweptAt.m[nodeID] = now
	sweptAt.Unlock()
	dirs, err := poolDirs(h)
	if err != nil || len(dirs) == 0 {
		return
	}
	known := s.knownReplicaDirs(nodeID, h)
	if known == nil {
		return // the answer is not complete: remove nothing
	}
	for _, pd := range dirs {
		listed, err := h.WorkList(pd.Dir)
		if err != nil {
			continue
		}
		for _, full := range orphanDirs(pd.Dir, listed, known, now) {
			if err := h.RemoveTree(full); err != nil {
				log.Printf("replication: removing the leftover directory %s on %s: %v", full, h.Name, err)
				continue
			}
			log.Printf("replication: removed the leftover directory %s on %s (a run was cut short; nothing refers to it)", full, h.Name)
		}
	}
}

// knownReplicaDirs collects the replica directories of node nodeID: the ones its replica domains name, and the ones
// the VMs of the other nodes name as their copy there. nil when a node that could say could not be read.
func (s *Server) knownReplicaDirs(nodeID int64, h *hostconn.Host) map[string]bool {
	known := map[string]bool{}
	for _, n := range s.Nodes.Status() {
		nh, err := s.Nodes.Host(n.ID)
		if err != nil {
			return nil
		}
		doms, _, err := nh.ConnectListAllDomains(1, 0)
		if err != nil {
			return nil
		}
		for _, d := range doms {
			meta, err := domainMeta(nh, d)
			if err != nil {
				return nil
			}
			if n.ID == nodeID {
				if rm := parseReplicaMeta(meta); rm != nil && rm.Dir != "" {
					known[rm.Dir] = true
				}
			}
			if cfg := parseReplication(meta); cfg != nil && cfg.TargetNode == nodeID && cfg.Dir != "" {
				known[cfg.Dir] = true
			}
		}
	}
	return known
}
