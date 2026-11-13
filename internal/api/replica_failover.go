package api

// Failover: running the VM on the standby from its replica.
//
// Planned (the source node is online): the VM on the source is shut down, one last run copies what changed
// since the last point (nothing is lost), the VM starts on the standby, and replication is set up in the
// other direction. Unplanned (the source node is offline, or it no longer has the VM): the VM starts from the
// newest usable point; what changed after that point is lost. There is no fencing: the controller cannot
// stop a node it cannot reach.
//
// When the old node comes back, its VM is an old copy. The controller marks it ("stale copy"), turns its
// replication off, makes sure it is not running (it would run the VM a second time with the same MAC and IP
// addresses) and raises an alert. Its disks are never deleted by the controller unless the user chose
// "Replicate back", which replaces them with a current copy.

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// failover kinds
const (
	foPlanned   = "planned"
	foUnplanned = "unplanned" // the source node cannot be reached
	foGone      = "gone"      // the source node answers but no longer has the VM (or it is not the original)
	foNone      = "none"
)

type foDecision struct {
	Kind       string `json:"kind"`
	SourceNode string `json:"sourceNode"`
	NodeState  string `json:"nodeState"`         // online | offline | removed
	VMState    string `json:"vmState,omitempty"` // the source VM: running | shutoff | ...
	Message    string `json:"message,omitempty"` // why the decision is as it is
	srcHost    *hostconn.Host
	srcDom     libvirt.Domain
	srcMeta    string
	haveSrcDom bool
}

// failoverDecision looks at the source of the replica dom (on the standby h) and says which failover applies.
func (s *Server) failoverDecision(rm *replicaMeta, dom libvirt.Domain) foDecision {
	d := foDecision{SourceNode: rm.SourceName}
	ns, ok := s.nodeStatus(rm.SourceNode)
	if !ok {
		d.NodeState, d.Kind = "removed", foGone
		d.Message = "the source node is no longer known to this controller"
		return d
	}
	d.NodeState = ns.State
	sh, err := s.Nodes.Host(rm.SourceNode)
	if ns.State != "online" || err != nil {
		d.NodeState, d.Kind = "offline", foUnplanned
		d.Message = rm.SourceName + " is offline"
		return d
	}
	sd, err := sh.DomainLookupByUUID(dom.UUID)
	if err != nil {
		d.Kind = foGone
		d.Message = rm.SourceName + " is online but no longer has this VM"
		return d
	}
	meta, _ := domainMeta(sh, sd)
	if parseReplicaMeta(meta) != nil || parseStale(meta) != nil {
		d.Kind = foGone
		d.Message = "the VM on " + rm.SourceName + " is itself a copy"
		return d
	}
	cfg := parseReplication(meta)
	d.srcHost, d.srcDom, d.srcMeta, d.haveSrcDom = sh, sd, meta, true
	if st, _, err := sh.DomainGetState(sd, 0); err == nil {
		d.VMState = stateName(uint8(st))
	}
	d.Kind = foPlanned
	if cfg == nil {
		d.Message = "replication was stopped on the source VM"
	}
	return d
}

type replicaPointInfo struct {
	Seq   int    `json:"seq"`
	Time  int64  `json:"time"`
	Kind  string `json:"kind"`
	Bytes uint64 `json:"bytes"`
}

func (s *Server) getReplica(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domainAny(r)
	if err != nil {
		fail(w, err)
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	rm := parseReplicaMeta(meta)
	if rm == nil {
		fail(w, badRequest("this VM is not a replica"))
		return
	}
	dec := s.failoverDecision(rm, dom)
	resp := map[string]any{
		"sourceNode": rm.SourceNode, "sourceName": rm.SourceName, "every": rm.Every, "last": rm.Last, "checked": rm.Checked,
		"bytes": rm.Bytes, "failover": dec, "now": time.Now().Unix(),
		"late": replicaLate(maxInt64(rm.Last, rm.Checked), rm.Every, time.Now()), "running": replRunning(cluster.FormatUUID(dom.UUID)),
	}
	if msg, at := replErrorOf(cluster.FormatUUID(dom.UUID)); msg != "" {
		resp["error"], resp["errorAt"] = msg, at.Unix()
	}
	st := replStore{h: h, dir: rm.Dir}
	if names, err := st.list(); err == nil {
		pts, _ := st.points(names)
		var infos []replicaPointInfo
		for i := len(pts) - 1; i >= 0; i-- {
			infos = append(infos, replicaPointInfo{Seq: pts[i].Seq, Time: pts[i].timeOf().Unix(), Kind: pts[i].Kind, Bytes: pts[i].Sent})
		}
		resp["points"] = infos
		if len(pts) > 0 {
			resp["pointTime"] = pts[len(pts)-1].timeOf().Unix()
			resp["tpm"] = pts[len(pts)-1].TPM
		}
	} else {
		resp["error"] = "the replica directory cannot be read: " + err.Error()
	}
	writeJSON(w, resp)
}

type failoverReq struct {
	// Mode is what the page showed the user: "planned" or "unplanned" ("gone" counts as unplanned). The
	// request is refused when the situation has changed since.
	Mode string `json:"mode"`
}

func (s *Server) replicaFailover(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domainAny(r)
	if err != nil {
		fail(w, err)
		return
	}
	nodeID, _, _ := s.host(r)
	var req failoverReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	rm := parseReplicaMeta(meta)
	if rm == nil {
		fail(w, badRequest("this VM is not a replica"))
		return
	}
	if replRunning(cluster.FormatUUID(dom.UUID)) {
		fail(w, badRequest("a copy is being written to this replica right now; try again when it is done"))
		return
	}
	dec := s.failoverDecision(rm, dom)
	want := dec.Kind
	if want == foGone {
		want = foUnplanned
	}
	if req.Mode != want {
		fail(w, conflictError("the situation changed while the page was open: "+dec.Message+". Reload the page and read the confirmation again"))
		return
	}
	err = s.taskAsync(r, "failover", dom.Name, h.Name, func(progress func(float64)) error {
		if dec.Kind == foPlanned {
			return s.plannedFailover(r, h, dom, nodeID, rm, dec, progress)
		}
		return s.activateReplica(h, dom, nodeID, rm, failedOverMeta{FromNode: rm.SourceNode, FromName: rm.SourceName, At: time.Now().Unix()}, progress)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type conflictError string

func (c conflictError) Error() string { return string(c) }

// plannedFailover shuts the source VM down, copies the last changes, starts the VM here and turns the
// replication around.
func (s *Server) plannedFailover(req *http.Request, h *hostconn.Host, dom libvirt.Domain, nodeID int64, rm *replicaMeta, dec foDecision, progress func(float64)) error {
	sh, sd := dec.srcHost, dec.srcDom
	cfg := parseReplication(dec.srcMeta)
	if cfg == nil || cfg.TargetNode != nodeID {
		cfg = &replConfig{Enabled: true, TargetNode: nodeID, TargetName: h.Name, Pool: rm.Pool, EveryMinutes: rm.Every, Dir: rm.Dir}
	}
	cfg.Dir = rm.Dir
	// 1. The source VM goes down cleanly.
	wasRunning := false
	if st, _, err := sh.DomainGetState(sd, 0); err == nil && stateName(uint8(st)) != "shutoff" {
		wasRunning = true
		if err := sh.DomainShutdown(sd); err != nil {
			return fmt.Errorf("could not shut the VM down on %s: %w", sh.Name, err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for {
			if st, _, err := sh.DomainGetState(sd, 0); err == nil && stateName(uint8(st)) == "shutoff" {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the VM did not shut down on %s within 3 minutes. Nothing was changed and it keeps running. Shut it down from inside the guest (or force it off) and try again", sh.Name)
			}
			time.Sleep(2 * time.Second)
		}
	}
	progress(0.1)
	restart := func(why error) error {
		if wasRunning {
			if err := sh.DomainCreate(sd); err != nil {
				return withNote(why, "the VM could not be started again on "+sh.Name+": "+err.Error()+". Start it there by hand")
			}
			return withNote(why, "the VM was started again on "+sh.Name+"; nothing was lost")
		}
		return why
	}
	// 2. The last changes.
	srcID := rm.SourceNode
	run, err := s.planReplica(srcID, sh, sd, *cfg, true)
	if err != nil {
		return restart(err)
	}
	if err := claimRepl(run); err != nil {
		return restart(err)
	}
	run.progress = func(f float64) { progress(0.1 + 0.5*f) }
	err = run.run()
	releaseRepl(run)
	setReplError(run.uuid, err)
	if err != nil {
		return restart(fmt.Errorf("the last copy of the changes failed: %w", err))
	}
	// 3. Here.
	progress(0.6)
	fo := failedOverMeta{FromNode: srcID, FromName: sh.Name, At: time.Now().Unix(), Planned: true}
	if err := s.activateReplicaStage(h, dom, nodeID, rm, fo, func(f float64) { progress(0.6 + 0.3*f) }); err != nil {
		var moved *movedError
		if !errors.As(err, &moved) {
			return restart(err)
		}
		// The disks are in place here: the VM must not be started again over there.
		s.markOldVM(sh, sd, h.Name, nodeID)
		return err
	}
	// 4. The old VM becomes the old copy; replication turns around.
	s.markOldVM(sh, sd, h.Name, nodeID)
	progress(0.95)
	if nd, err := h.DomainLookupByUUID(dom.UUID); err == nil {
		if err := s.reverseReplication(req, h, nd, nodeID, sh, rm.Every); err != nil {
			log.Printf("failover of %s: replication back to %s not set up: %v", dom.Name, sh.Name, err)
			return withNote(nil2("the VM runs on "+h.Name+" now"), "replication back to "+sh.Name+" was not set up: "+err.Error()+". Use \"Replicate back\" on the VM")
		}
	}
	return nil
}

func nil2(msg string) error { return errors.New(msg) }

// markOldVM turns the VM on the old node into the old copy: marked, never to autostart, without replication.
func (s *Server) markOldVM(sh *hostconn.Host, sd libvirt.Domain, toName string, toNode int64) {
	old, err := domainMeta(sh, sd)
	if err != nil {
		log.Printf("marking the old VM %s: %v", sd.Name, err)
		return
	}
	meta, err := setMetaChildren(old, map[string]string{
		replChild:     "",
		failedOverChd: "",
		staleChild:    staleMeta{ToNode: toNode, ToName: toName, At: time.Now().Unix()}.element(),
	})
	if err == nil {
		err = setDomainMeta(sh, sd, meta)
	}
	if err != nil {
		log.Printf("marking the old VM %s: %v", sd.Name, err)
	}
	if err := sh.DomainSetAutostart(sd, 0); err != nil {
		log.Printf("marking the old VM %s: autostart off: %v", sd.Name, err)
	}
	dropReplCheckpoints(sh, sd)
}

// reverseReplication sets up replication from the VM that took over back to the old node, replacing the old
// copy there once the first complete copy has arrived.
func (s *Server) reverseReplication(req *http.Request, h *hostconn.Host, dom libvirt.Domain, nodeID int64, old *hostconn.Host, every int) error {
	oldID := int64(0)
	for _, n := range s.Nodes.Status() {
		if hh, err := s.Nodes.Host(n.ID); err == nil && hh == old {
			oldID = n.ID
		}
	}
	if oldID == 0 {
		return errors.New("the old node is not connected")
	}
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
	cfg := replConfig{Enabled: true, TargetNode: oldID, TargetName: old.Name, Pool: pool, EveryMinutes: every, ReplaceStale: true}
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

// movedError says the replica's disks are in place on this node: from then on a failure must not start the
// VM on the old node again.
type movedError struct{ err error }

func (m *movedError) Error() string { return m.err.Error() }
func (m *movedError) Unwrap() error { return m.err }

// activateReplica runs the replica as the VM on this node and starts it.
func (s *Server) activateReplica(h *hostconn.Host, dom libvirt.Domain, nodeID int64, rm *replicaMeta, fo failedOverMeta, progress func(float64)) error {
	return s.activateReplicaStage(h, dom, nodeID, rm, fo, progress)
}

// activateReplicaStage turns the replica into a VM and starts it. It can be run again after any failure.
func (s *Server) activateReplicaStage(h *hostconn.Host, dom libvirt.Domain, nodeID int64, rm *replicaMeta, fo failedOverMeta, progress func(float64)) (err error) {
	st := replStore{h: h, dir: rm.Dir}
	names, err := st.list()
	if err != nil {
		return fmt.Errorf("the replica directory cannot be read: %w", err)
	}
	all, _ := st.points(names)
	if len(all) == 0 {
		return badRequest("this replica has no complete copy yet; there is nothing to start from")
	}
	_, pd, err := destPool(h, rm.Pool)
	if err != nil {
		return err
	}
	newest := all[len(all)-1]
	finalPath := func(p *replPoint, d replDisk) string { return pd.Dir + "/" + d.Vol }
	// Are the disks already in place (an earlier try got that far)?
	inPlace := true
	for _, d := range newest.Disks {
		ex, err := h.Exists(finalPath(newest, d))
		if err != nil {
			return err
		}
		if !ex[finalPath(newest, d)] {
			inPlace = false
		}
		if _, ok := names[replBaseName(newest.Gen, d.Target)]; ok {
			inPlace = false
		}
	}
	sel := newest
	if !inPlace {
		kept, _, err := st.settle()
		if err != nil {
			return fmt.Errorf("checking the replica: %w", err)
		}
		if len(kept) == 0 {
			return errors.New("no copy on this node can be read any more; the replica is damaged")
		}
		sel = kept[0] // the newest point whose files all read
		for _, d := range sel.Disks {
			if ex, err := h.Exists(finalPath(sel, d)); err != nil || ex[finalPath(sel, d)] {
				return fmt.Errorf("a file %s exists on %s already; remove or rename it, then try again", finalPath(sel, d), h.Name)
			}
		}
		if sel.NVRAM != "" {
			if ex, err := h.Exists(sel.NVRAMPath); err != nil || ex[sel.NVRAMPath] {
				return fmt.Errorf("the UEFI variable file %s exists on %s already; remove it, then try again", sel.NVRAMPath, h.Name)
			}
		}
		names, _ = st.list()
		// The points above the chosen one (none, when the newest is usable) are dropped.
		for _, p := range all {
			if p.Seq > sel.Seq {
				if err := h.Remove(st.file(replManifestName(p.Seq))); err != nil {
					return err
				}
			}
		}
		progress(0.1)
		// Write the whole chain into the base. Cut short, this can be run again: the chain reads the same.
		for i, d := range sel.Disks {
			chain, err := st.chain(d.File, names)
			if err != nil {
				return err
			}
			base := chain[len(chain)-1]
			if len(chain) > 1 {
				if err := h.ReplCommit(st.dir, d.File, base); err != nil {
					return fmt.Errorf("applying the copy to %s: %w", base, err)
				}
			}
			progress(0.1 + 0.6*float64(i+1)/float64(len(sel.Disks)))
		}
		// From here on the base holds the point; move the files into the pool.
		for _, d := range sel.Disks {
			chain, _ := st.chain(d.File, names)
			base := chain[len(chain)-1]
			if err := h.Move(st.file(base), finalPath(sel, d)); err != nil {
				return &movedError{fmt.Errorf("moving the disk %s into the pool: %w", d.Vol, err)}
			}
		}
	}
	if sel.NVRAM != "" {
		ex, _ := h.Exists(sel.NVRAMPath)
		if !ex[sel.NVRAMPath] {
			if err := h.Copy(st.file(sel.NVRAM), sel.NVRAMPath); err != nil {
				return &movedError{fmt.Errorf("placing the UEFI variables at %s: %w", sel.NVRAMPath, err)}
			}
		}
	}
	progress(0.8)
	if pool, perr := h.StoragePoolLookupByName(rm.Pool); perr == nil {
		_ = h.StoragePoolRefresh(pool, 0)
	}
	// The definition of the chosen point; the replica marker goes, the failover record comes.
	nd, err := h.DomainDefineXMLFlags(sel.DomainXML, libvirt.DomainDefineValidate)
	if err != nil {
		return &movedError{fmt.Errorf("defining the VM from the copy: %w", err)}
	}
	old, err := domainMeta(h, nd)
	if err != nil {
		return &movedError{err}
	}
	meta, err := setMetaChildren(old, map[string]string{replicaChild: "", failedOverChd: fo.element()})
	if err == nil {
		err = setDomainMeta(h, nd, meta)
	}
	if err != nil {
		return &movedError{err}
	}
	if sel.Autostart {
		_ = h.DomainSetAutostart(nd, 1)
	}
	if rm.Dir != "" {
		if err := h.RemoveTree(rm.Dir); err != nil {
			log.Printf("failover of %s: removing %s: %v", dom.Name, rm.Dir, err)
		}
	}
	setReplError(cluster.FormatUUID(dom.UUID), nil)
	progress(0.9)
	if err := h.DomainCreate(nd); err != nil {
		return &movedError{fmt.Errorf("the VM was set up from the copy of %s but did not start: %w", sel.timeOf().Local().Format("15:04"), err)}
	}
	return nil
}

func (s *Server) deleteReplicaHandler(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domainAny(r)
	if err != nil {
		fail(w, err)
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	rm := parseReplicaMeta(meta)
	if rm == nil {
		fail(w, badRequest("this VM is not a replica"))
		return
	}
	if replRunning(cluster.FormatUUID(dom.UUID)) {
		fail(w, badRequest("a copy is being written to this replica right now; cancel it on the source VM first"))
		return
	}
	err = s.task(r, "replica delete", dom.Name, h.Name, func(func(float64)) error {
		// The source VM, when it can be reached, stops replicating.
		if sh, err := s.Nodes.Host(rm.SourceNode); err == nil {
			if sd, err := sh.DomainLookupByUUID(dom.UUID); err == nil {
				if sm, _ := domainMeta(sh, sd); parseReplication(sm) != nil {
					nm, err := setMetaChildren(sm, map[string]string{replChild: ""})
					if err == nil {
						err = setDomainMeta(sh, sd, nm)
					}
					if err != nil {
						log.Printf("replica delete: source %s: %v", sd.Name, err)
					}
					dropReplCheckpoints(sh, sd)
				}
			}
		}
		return deleteReplicaDomain(h, dom)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// --- the old node comes back ---

// reconcileFailovers finds the old VMs of failovers whose node is back and makes them safe. It runs with the
// alert checks (once a minute). A VM that took over keeps a record of where it came from; when that node is
// online and still holds a running or replicating VM of the same identity, that VM is marked as the old copy,
// its replication is switched off, and if it runs it is powered off at once: two copies of one VM with the
// same MAC and IP addresses must not run. Its disks are left alone.
func (s *Server) reconcileFailovers() {
	for _, n := range s.Nodes.Status() {
		if n.State != "online" {
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
			if err != nil || meta == "" {
				continue
			}
			fo := parseFailedOver(meta)
			if fo == nil || parseReplicaMeta(meta) != nil {
				continue
			}
			s.reconcileOne(h, n.ID, d, fo)
		}
	}
}

func (s *Server) reconcileOne(h *hostconn.Host, nodeID int64, d libvirt.Domain, fo *failedOverMeta) {
	oh, err := s.Nodes.Host(fo.FromNode)
	if err != nil {
		return // still away
	}
	od, err := oh.DomainLookupByUUID(d.UUID)
	if err != nil {
		return // the old VM is gone: nothing to guard
	}
	ometa, err := domainMeta(oh, od)
	if err != nil {
		return
	}
	if parseStale(ometa) != nil || parseReplicaMeta(ometa) != nil {
		return // already handled, or it is the replica of the running VM now
	}
	if ofo := parseFailedOver(ometa); ofo != nil && ofo.At >= fo.At {
		return // the VM over there took over later than this one: this one is the old copy, handled from its own record
	}
	state, _, err := oh.DomainGetState(od, 0)
	if err != nil {
		return
	}
	s.markOldVM(oh, od, h.Name, nodeID)
	if stateName(uint8(state)) != "shutoff" {
		if err := oh.DomainDestroy(od); err != nil {
			log.Printf("old copy of %s on %s: powering it off: %v", od.Name, oh.Name, err)
		} else {
			log.Printf("old copy of %s on %s was running beside the VM on %s and was powered off; its disks are untouched", od.Name, oh.Name, h.Name)
		}
	}
}

// vmBelongsToReplication says whether a request to delete the VM should also remove its replica.
func (s *Server) removeReplicaAfterDelete(meta string, dom libvirt.Domain) error {
	cfg := parseReplication(meta)
	if cfg == nil {
		return nil
	}
	return s.removeReplicaOf(*cfg, dom)
}

var _ = path.Base
var _ = strings.TrimSpace
