package api

// Moving a replicated VM to its own standby node.
//
// The standby holds the VM's identity (same UUID and name) as the inert replica, so a migration there would
// collide with it. The rule is: the move is allowed (the VM keeps running, live or offline as any migration), the
// replica steps aside for it, and replication is set up again toward the node the VM left, with a full copy. This is
// also what node evacuation and rolling updates do with such a VM in a cluster of two nodes, where the standby is
// the only other node.
//
// Stepping aside means: the replica's definition is undefined (its directory with the points stays, untouched) and the
// source VM's replication setting is taken off it. If the migration fails, both are put back and nothing has changed;
// if it succeeds, the old replica directory is deleted and replication toward the old node starts.

import (
	"log"
	"net/http"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// migrationReplica says whether a migration of dom from src to node target has to step a replica aside: the VM's
// replication setting names that node.
func migrationReplica(src *hostconn.Host, dom libvirt.Domain, target int64) (*replConfig, bool) {
	meta, err := domainMeta(src, dom)
	if err != nil {
		return nil, false
	}
	cfg := parseReplication(meta)
	if cfg == nil || cfg.TargetNode != target {
		return nil, false
	}
	return cfg, true
}

// isOwnReplica says whether d on h is the replica of the VM with this UUID (same identity, inert).
func isReplicaDomain(h *hostconn.Host, d libvirt.Domain) bool {
	meta, err := domainMeta(h, d)
	return err == nil && parseReplicaMeta(meta) != nil
}

// replicaAside is a replica that stepped aside for a migration.
type replicaAside struct {
	s        *Server
	cfg      replConfig
	srcID    int64
	src      *hostconn.Host
	dom      libvirt.Domain
	dstID    int64
	dst      *hostconn.Host
	uuid     string
	replXML  string // the replica's definition
	replMeta string // its metadata
	dir      string // its directory
	claim    *replRun
}

// stepAsideReplica prepares the standby for the move. A copy in progress refuses the move (nothing was changed).
func (s *Server) stepAsideReplica(srcID int64, src *hostconn.Host, dom libvirt.Domain, dstID int64, dst *hostconn.Host, cfg replConfig) (*replicaAside, error) {
	uuid := cluster.FormatUUID(dom.UUID)
	a := &replicaAside{s: s, cfg: cfg, srcID: srcID, src: src, dom: dom, dstID: dstID, dst: dst, uuid: uuid, dir: cfg.Dir}
	a.claim = &replRun{uuid: uuid, name: dom.Name, srcID: srcID, final: true}
	if err := claimRepl(a.claim); err != nil {
		return nil, err
	}
	if d, err := dst.DomainLookupByUUID(dom.UUID); err == nil {
		meta, _ := domainMeta(dst, d)
		rm := parseReplicaMeta(meta)
		if rm == nil {
			releaseRepl(a.claim)
			return nil, badRequest("the node " + dst.Name + " has a VM with this identity that is not a replica of it (" + d.Name + "); it was left alone and the VM was not moved")
		}
		x, err := dst.DomainGetXMLDesc(d, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive)
		if err != nil {
			releaseRepl(a.claim)
			return nil, err
		}
		a.replXML, a.replMeta, a.dir = x, meta, rm.Dir
		flags := libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineNvram | libvirt.DomainUndefineCheckpointsMetadata
		if err := dst.DomainUndefineFlags(d, flags); err != nil {
			releaseRepl(a.claim)
			return nil, err
		}
	}
	// The VM's own setting goes: it must not name the node the VM is about to run on.
	if err := a.setSourceMeta(""); err != nil {
		a.putReplicaBack()
		releaseRepl(a.claim)
		return nil, err
	}
	dropReplCheckpoints(src, dom)
	return a, nil
}

func (a *replicaAside) setSourceMeta(cfgEl string) error {
	old, err := domainMeta(a.src, a.dom)
	if err != nil {
		return err
	}
	meta, err := setMetaChildren(old, map[string]string{replChild: cfgEl})
	if err != nil {
		return err
	}
	return setDomainMeta(a.src, a.dom, meta)
}

func (a *replicaAside) putReplicaBack() {
	if a.replXML == "" {
		return
	}
	nd, err := a.dst.DomainDefineXMLFlags(a.replXML, libvirt.DomainDefineValidate)
	if err != nil {
		log.Printf("migration of %s: the replica on %s could not be defined again: %v (its files are in %s; use \"Replicate\" on the VM again)", a.dom.Name, a.dst.Name, err, a.dir)
		return
	}
	if err := setDomainMeta(a.dst, nd, a.replMeta); err != nil {
		log.Printf("migration of %s: the replica's record on %s: %v", a.dom.Name, a.dst.Name, err)
	}
	_ = a.dst.DomainSetAutostart(nd, 0)
}

// failed puts everything back after a migration that did not move the VM. It returns a sentence for the error.
func (a *replicaAside) failed() string {
	defer releaseRepl(a.claim)
	// The migration may have moved the VM after all (the error said so): then the standby has it, and nothing is put back.
	if d, err := a.dst.DomainLookupByUUID(a.dom.UUID); err == nil && !isReplicaDomain(a.dst, d) {
		return ""
	}
	if _, err := a.src.DomainLookupByUUID(a.dom.UUID); err != nil {
		return ""
	}
	a.cfg.PausedCopy = false
	if err := a.setSourceMeta(a.cfg.element()); err != nil {
		return "replication of the VM could not be restored: " + err.Error() + ". Set it up again on the VM"
	}
	a.putReplicaBack()
	return "the replica on " + a.dst.Name + " is as it was"
}

// moved finishes after a migration that moved the VM: the old replica goes, replication starts toward the old node.
func (a *replicaAside) moved(req *http.Request) string {
	releaseRepl(a.claim) // the first copy back is a run of its own
	if a.dir != "" {
		if err := a.dst.RemoveTree(a.dir); err != nil {
			log.Printf("migration of %s: removing the old replica %s: %v", a.dom.Name, a.dir, err)
		}
	}
	nd, err := a.dst.DomainLookupByUUID(a.dom.UUID)
	if err != nil {
		return "the VM moved, but was not found afterwards to set replication up again"
	}
	if err := a.s.replicateBackTo(req, a.dst, nd, a.dstID, a.src, a.srcID, a.cfg.EveryMinutes); err != nil {
		log.Printf("migration of %s: replication back to %s not set up: %v", a.dom.Name, a.src.Name, err)
		return "replication to " + a.src.Name + " was not set up again: " + err.Error() + ". Set it up on the VM"
	}
	return ""
}

// dropReplicationTo switches replication off on the VMs of the other nodes that are copied to node id, which the
// controller is about to forget: nothing could be sent there any more, and a setting that names a node that does
// not exist would only fail every round. The copies on that node are not touched (it is no longer managed).
func (s *Server) dropReplicationTo(id int64) {
	for _, n := range s.Nodes.Status() {
		if n.ID == id {
			continue
		}
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		doms, _, err := h.ConnectListAllDomains(1, 0)
		if err != nil {
			continue
		}
		for _, d := range doms {
			meta, err := domainMeta(h, d)
			if err != nil {
				continue
			}
			cfg := parseReplication(meta)
			if cfg == nil || cfg.TargetNode != id {
				continue
			}
			nm, err := setMetaChildren(meta, map[string]string{replChild: ""})
			if err == nil {
				err = setDomainMeta(h, d, nm)
			}
			if err != nil {
				log.Printf("node %d removed: replication of %s on %s not switched off: %v", id, d.Name, h.Name, err)
				continue
			}
			dropReplCheckpoints(h, d)
			log.Printf("node %d removed: replication of %s on %s was switched off", id, d.Name, h.Name)
		}
	}
}

// replicateBackTo sets up replication from the VM on h to node old (a node it has just left or took over from) and
// starts the first, full copy.
func (s *Server) replicateBackTo(req *http.Request, h *hostconn.Host, dom libvirt.Domain, nodeID int64, old *hostconn.Host, oldID int64, every int) error {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive)
	if err != nil {
		return err
	}
	md, err := parseMigDomain(raw)
	if err != nil {
		return err
	}
	_, _, _, need, err := inspectBackupDisks(h, md, "replicated")
	if err != nil {
		return err
	}
	need += need/10 + importFreeMargin
	pool, err := s.replicaPoolFor(h, old, md, need)
	if err != nil {
		return err
	}
	cfg := replConfig{Enabled: true, TargetNode: oldID, TargetName: old.Name, Pool: pool, EveryMinutes: every}
	// The setting is stored first: when the first run cannot start now, the scheduler retries it and says so.
	if cur, err := domainMeta(h, dom); err == nil {
		if nm, err := setMetaChildren(cur, map[string]string{replChild: cfg.element()}); err == nil {
			_ = setDomainMeta(h, dom, nm)
		}
	}
	run, err := s.planReplica(nodeID, h, dom, cfg, false)
	if err != nil {
		return err
	}
	if err := run.ensureStandby(); err != nil {
		return err
	}
	if _, _, err := s.launchReplica(req, run); err != nil {
		return err
	}
	return nil
}
