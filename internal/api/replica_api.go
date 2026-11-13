package api

// The endpoints and the scheduler hook of replication.
//
//	GET    /api/nodes/{node}/vms/{uuid}/replication          status of the VM's replication
//	PUT    /api/nodes/{node}/vms/{uuid}/replication          set it up or change the interval
//	DELETE /api/nodes/{node}/vms/{uuid}/replication          stop (?deleteReplica=1 also removes the copy)
//	POST   /api/nodes/{node}/vms/{uuid}/replication/run      replicate now
//	POST   /api/nodes/{node}/vms/{uuid}/replication/cancel   cancel the run in progress
//	GET    /api/nodes/{node}/vms/{uuid}/replica              on the standby: the copy, and what a failover would do
//	POST   /api/nodes/{node}/vms/{uuid}/replica/failover     run the VM here from the copy
//	DELETE /api/nodes/{node}/vms/{uuid}/replica              remove the copy

import (
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// replPools are the choices for the standby's pool: the same name as the VM's own pool when it has room,
// else the pool with the most free space. The user is never asked.
func (s *Server) replicaPoolFor(src *hostconn.Host, dst *hostconn.Host, md migDomain, need uint64) (string, error) {
	prefer := ""
	if pools, err := poolDirs(src); err == nil {
		for _, d := range md.Disks {
			for _, p := range pools {
				if d.Path != "" && path.Dir(d.Path) == p.Dir {
					prefer = p.Name
				}
			}
			if prefer != "" {
				break
			}
		}
	}
	dirs, err := poolDirs(dst)
	if err != nil {
		return "", err
	}
	// Never a share: the replica must not be on storage the source node sees as well.
	if descs, derr := poolDescs(dst); derr == nil {
		local := dirs[:0:0]
		for _, d := range dirs {
			if !descs[d.Dir].isNFS() {
				local = append(local, d)
			}
		}
		dirs = local
	}
	type cand struct {
		name string
		free int64
	}
	var ok []cand
	var best int64
	for _, d := range dirs {
		f, err := bkFreeBytes(dst, d.Dir)
		if err != nil {
			continue
		}
		if f > best {
			best = f
		}
		if uint64(f) >= need {
			ok = append(ok, cand{d.Name, f})
		}
	}
	if len(ok) == 0 {
		if len(dirs) == 0 {
			return "", badRequest(dst.Name + " has no running directory-backed storage pool for the replica")
		}
		return "", badRequest(fmt.Sprintf("no storage pool on %s has enough free space for the replica: about %s are needed, the most free in one pool is %s", dst.Name, fmtBytes(need), fmtBytes(uint64(best))))
	}
	for _, c := range ok {
		if c.name == prefer {
			return c.name, nil
		}
	}
	sort.SliceStable(ok, func(i, j int) bool { return ok[i].free > ok[j].free })
	return ok[0].name, nil
}

// replicaChecks says what stops or may spoil a start of the VM on the standby: the problems (refused) and
// the warnings (shown).
func (s *Server) replicaChecks(src, dst *hostconn.Host, md migDomain) (bad, warn []string) {
	for _, i := range md.Ifs {
		switch {
		case i.Type == "network":
			n, err := dst.NetworkLookupByName(i.Network)
			if err != nil {
				bad = append(bad, "the network "+i.Network+" does not exist on "+dst.Name+" (create it there first, see Datacenter, Network)")
			} else if act, _ := dst.NetworkIsActive(n); act == 0 {
				bad = append(bad, "the network "+i.Network+" exists on "+dst.Name+" but is not active")
			}
		case i.Type == "bridge" && i.Bridge != "":
			if !safeIfRE.MatchString(i.Bridge) {
				bad = append(bad, "unsupported bridge name "+i.Bridge)
			} else if _, err := dst.Run("test -d /sys/class/net/" + shq(i.Bridge) + "/bridge"); err != nil {
				bad = append(bad, "the bridge "+i.Bridge+" does not exist on "+dst.Name)
			}
		case i.Type == "direct" && i.Dev != "":
			if !safeIfRE.MatchString(i.Dev) {
				bad = append(bad, "unsupported interface name "+i.Dev)
			} else if _, err := dst.Run("test -e /sys/class/net/" + shq(i.Dev)); err != nil {
				bad = append(bad, "the interface "+i.Dev+" (macvtap parent) does not exist on "+dst.Name)
			}
		}
	}
	if md.Machine != "" {
		opt := func(v string) libvirt.OptString {
			if v == "" {
				return nil
			}
			return libvirt.OptString{v}
		}
		if _, err := dst.ConnectGetDomainCapabilities(opt(md.Emulator), opt(md.Arch), opt(md.Machine), nil, 0); err != nil {
			bad = append(bad, fmt.Sprintf("%s cannot run machine type %s with %s: the nodes need the same distribution family and a QEMU at least as new as the source's", dst.Name, md.Machine, md.Emulator))
		}
	}
	if md.NVRAM != "" {
		dir := path.Dir(md.NVRAM)
		if st, err := dst.Stat(dir); err != nil || !st[0].Exists || st[0].Kind != "dir" {
			bad = append(bad, "the directory "+dir+" for the UEFI variables does not exist on "+dst.Name+" (is OVMF installed?)")
		}
	}
	if md.CPUMode == "host-passthrough" {
		warn = append(warn, "the VM uses the host CPU model (host-passthrough): it starts on "+dst.Name+" only if that node has an identical CPU")
	}
	if md.TPM {
		warn = append(warn, "the virtual TPM is not copied: the VM starts on the standby with an empty TPM, and anything bound to it (BitLocker, for instance) asks for its recovery key")
	}
	for _, d := range md.Disks {
		if d.Path != "" && (d.Device == "cdrom" || d.Device == "floppy") && path.Base(d.Path) != seedVolumeName(md.Name) {
			warn = append(warn, "the install media "+d.Path+" is left out of the replica")
		}
	}
	return bad, warn
}

type replSetupReq struct {
	TargetNode   int64 `json:"targetNode"`
	EveryMinutes int   `json:"everyMinutes"`
	ReplaceStale bool  `json:"replaceStale"`
}

func (s *Server) putReplication(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	srcID, _, _ := s.host(r)
	req := replSetupReq{EveryMinutes: 15}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := replIntervalOK(req.EveryMinutes); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	if staleGuard(h, dom) != nil {
		fail(w, staleGuard(h, dom))
		return
	}
	if mk, _ := getMark(h, dom); mk.Template {
		fail(w, badRequest(dom.Name+" is a template; templates are not replicated"))
		return
	}
	old := parseReplication(meta)
	if old != nil && old.TargetNode == req.TargetNode && !req.ReplaceStale {
		// Only the interval changes.
		old.EveryMinutes = req.EveryMinutes
		old.Enabled = true
		nm, err := setMetaChildren(meta, map[string]string{replChild: old.element()})
		if err == nil {
			err = setDomainMeta(h, dom, nm)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	dn, ok := s.nodeStatus(req.TargetNode)
	if !ok || req.TargetNode == srcID {
		fail(w, badRequest("choose another node as the standby"))
		return
	}
	dst, err := s.Nodes.Host(req.TargetNode)
	if err != nil {
		fail(w, badRequest("the standby node is not connected: "+err.Error()))
		return
	}
	if dn.Maintenance {
		fail(w, badRequest(dst.Name+" is in maintenance; choose another standby"))
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive)
	if err != nil {
		fail(w, err)
		return
	}
	md, err := parseMigDomain(raw)
	if err != nil {
		fail(w, badRequest("cannot read the domain definition: "+err.Error()))
		return
	}
	if err := refuseSharedDisks(h, dst, md); err != nil {
		fail(w, err)
		return
	}
	disks, _, allQcow2, need, err := inspectBackupDisks(h, md, "replicated")
	if err != nil {
		fail(w, err)
		return
	}
	_ = disks
	if !allQcow2 {
		fail(w, badRequest("a disk of this VM is not a qcow2 image: only qcow2 disks can track changes, so a raw disk would be copied whole on every run. Convert it to qcow2 first (move the disk to a pool as qcow2)"))
		return
	}
	need += need/10 + importFreeMargin
	pool, err := s.replicaPoolFor(h, dst, md, need)
	if err != nil {
		fail(w, err)
		return
	}
	bad, warn := s.replicaChecks(h, dst, md)
	if len(bad) > 0 {
		fail(w, badRequest("this VM cannot start on "+dst.Name+": "+strings.Join(bad, "; ")))
		return
	}
	cfg := replConfig{Enabled: true, TargetNode: req.TargetNode, TargetName: dst.Name, Pool: pool, EveryMinutes: req.EveryMinutes, ReplaceStale: req.ReplaceStale}
	if req.ReplaceStale {
		if err := s.checkStaleTarget(dst, dom); err != nil {
			fail(w, err)
			return
		}
	}
	run, err := s.planReplica(srcID, h, dom, cfg, false)
	if err != nil {
		fail(w, err)
		return
	}
	// A different standby than before: the old copy goes first.
	if old != nil && old.TargetNode != req.TargetNode {
		if err := s.removeReplicaOf(*old, dom); err != nil {
			fail(w, err)
			return
		}
	}
	if err := run.ensureStandby(); err != nil {
		fail(w, err)
		return
	}
	id, _, err := s.launchReplica(r, run)
	if err != nil {
		// Set up, but the first copy waits (another run is going); the scheduler starts it.
		writeJSON(w, map[string]any{"ok": true, "warnings": warn, "note": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "task": id, "warnings": warn, "pool": pool})
}

// checkStaleTarget makes sure the standby's VM of this identity is the old copy and is shut off.
func (s *Server) checkStaleTarget(dst *hostconn.Host, dom libvirt.Domain) error {
	d, err := dst.DomainLookupByUUID(dom.UUID)
	if err != nil {
		return nil // nothing there any more: a plain setup
	}
	meta, _ := domainMeta(dst, d)
	if parseStale(meta) == nil {
		return badRequest("the VM " + d.Name + " on " + dst.Name + " is not the old copy of this VM; it is not replaced")
	}
	if st, _, err := dst.DomainGetState(d, 0); err != nil || stateName(uint8(st)) != "shutoff" {
		return badRequest("the old copy " + d.Name + " on " + dst.Name + " is running; shut it off first (it must not run beside the live VM)")
	}
	return nil
}

func (s *Server) deleteReplication(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	cfg := parseReplication(meta)
	if cfg == nil {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if replRunning(cfg0uuid(dom)) {
		fail(w, badRequest("a replication run of this VM is in progress; cancel it first"))
		return
	}
	err = s.task(r, "replication stop", dom.Name, h.Name, func(func(float64)) error {
		return s.stopReplication(h, dom, *cfg, r.URL.Query().Get("deleteReplica") == "1")
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func cfg0uuid(dom libvirt.Domain) string { return cluster.FormatUUID(dom.UUID) }

// stopReplication removes the setting from the VM and its checkpoints, and the copy when asked.
func (s *Server) stopReplication(h *hostconn.Host, dom libvirt.Domain, cfg replConfig, deleteCopy bool) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	meta, err := setMetaChildren(old, map[string]string{replChild: ""})
	if err != nil {
		return err
	}
	if err := setDomainMeta(h, dom, meta); err != nil {
		return err
	}
	dropReplCheckpoints(h, dom)
	setReplError(cluster.FormatUUID(dom.UUID), nil)
	if deleteCopy {
		return s.removeReplicaOf(cfg, dom)
	}
	return nil
}

// dropReplCheckpoints deletes the replication checkpoints of the VM (not the backups' ones), newest first.
func dropReplCheckpoints(h *hostconn.Host, dom libvirt.Domain) {
	cks, err := listCheckpoints(h, dom)
	if err != nil {
		return
	}
	for _, c := range cks {
		if isReplTarget(c.Target) {
			_ = deleteCheckpoint(h, dom, c.Name)
		}
	}
}

// removeReplicaOf deletes the copy a replication wrote on its standby: the replica domain, and the replica
// directory. It does nothing for a standby whose VM of this identity is not a replica. An offline standby is
// an error the caller reports: the copy is left there and can be deleted on its own page later.
func (s *Server) removeReplicaOf(cfg replConfig, src libvirt.Domain) error {
	dst, err := s.Nodes.Host(cfg.TargetNode)
	if err != nil {
		return fmt.Errorf("the standby node %s is not connected, so its copy was not removed; delete the replica there when the node is back", cfg.TargetName)
	}
	d, err := dst.DomainLookupByUUID(src.UUID)
	if err != nil {
		if cfg.Dir != "" {
			_ = dst.RemoveTree(cfg.Dir)
		}
		return nil
	}
	return deleteReplicaDomain(dst, d)
}

// deleteReplicaDomain removes a replica: its definition and its directory.
func deleteReplicaDomain(h *hostconn.Host, d libvirt.Domain) error {
	meta, err := domainMeta(h, d)
	if err != nil {
		return err
	}
	rm := parseReplicaMeta(meta)
	if rm == nil {
		return badRequest(d.Name + " on " + h.Name + " is not a replica; it was not removed")
	}
	flags := libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineNvram | libvirt.DomainUndefineCheckpointsMetadata
	if err := h.DomainUndefineFlags(d, flags); err != nil {
		return err
	}
	if rm.Dir != "" {
		if err := h.RemoveTree(rm.Dir); err != nil {
			return fmt.Errorf("the replica was removed but its files in %s could not be deleted: %w", rm.Dir, err)
		}
	}
	return nil
}

func (s *Server) replicationRun(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	srcID, _, _ := s.host(r)
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	cfg := parseReplication(meta)
	if cfg == nil {
		fail(w, badRequest("replication of this VM is not set up"))
		return
	}
	run, err := s.planReplica(srcID, h, dom, *cfg, false)
	if err != nil {
		fail(w, err)
		return
	}
	id, _, err := s.launchReplica(r, run)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "task": id})
}

func (s *Server) replicationCancel(w http.ResponseWriter, r *http.Request) {
	_, dom, err := s.domainAny(r)
	if err != nil {
		fail(w, err)
		return
	}
	bkRuns.Lock()
	run := replRuns[cluster.FormatUUID(dom.UUID)]
	bkRuns.Unlock()
	if run == nil {
		fail(w, badRequest("no replication run of this VM is in progress"))
		return
	}
	run.cancel()
	writeJSON(w, map[string]bool{"ok": true})
}

// nextRun is when the scheduler starts the next run (0 when it does not).
func (s *Server) replNext(cfg replConfig, now time.Time) int64 {
	if s.NoScheduler || !cfg.Enabled {
		return 0
	}
	if cfg.LastOK == 0 {
		return now.Unix()
	}
	t := time.Unix(cfg.LastOK, 0).Add(time.Duration(cfg.EveryMinutes) * time.Minute)
	if t.Before(now) {
		t = now
	}
	return t.Unix()
}

func (s *Server) getReplication(w http.ResponseWriter, r *http.Request) {
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
	uuid := cluster.FormatUUID(dom.UUID)
	resp := map[string]any{"configured": false, "now": time.Now().Unix()}
	if st := parseStale(meta); st != nil {
		resp["stale"] = map[string]any{"toNode": st.ToNode, "toName": st.ToName, "at": st.At}
	}
	cfg := parseReplication(meta)
	if cfg == nil {
		writeJSON(w, resp)
		return
	}
	resp["configured"] = true
	resp["config"] = replSummary{TargetNode: cfg.TargetNode, TargetName: cfg.TargetName, Pool: cfg.Pool, EveryMinutes: cfg.EveryMinutes, Enabled: cfg.Enabled,
		LastOK: cfg.LastOK, LastBytes: cfg.LastBytes, LastKind: cfg.LastKind, LastSecs: cfg.LastSecs, Next: s.replNext(*cfg, time.Now())}
	resp["running"] = replRunning(uuid)
	resp["runner"] = "this"
	if s.NoScheduler {
		resp["runner"] = "disabled"
	}
	if msg, at := replErrorOf(uuid); msg != "" {
		resp["error"], resp["errorAt"] = msg, at.Unix()
	}
	// What the standby says about its copy.
	if dst, err := s.Nodes.Host(cfg.TargetNode); err != nil {
		resp["standbyOffline"] = true
	} else if d, err := dst.DomainLookupByUUID(dom.UUID); err == nil {
		if m2, err := domainMeta(dst, d); err == nil {
			if rm := parseReplicaMeta(m2); rm != nil {
				resp["replica"] = replicaSummary{SourceNode: rm.SourceNode, SourceName: rm.SourceName, Every: rm.Every, Last: rm.Last, Checked: rm.Checked, Points: rm.Points, Bytes: rm.Bytes,
					Late: replicaLate(maxInt64(rm.Last, rm.Checked), rm.Every, time.Now())}
			}
		}
	}
	writeJSON(w, resp)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// --- the scheduler's entry point ---

// runReplicaSchedule starts a run of dom if its replication says one is due. The run is a background task;
// this only plans and launches it. It is what the scheduler loop calls (and what the lab drives directly).
func (s *Server) runReplicaSchedule(srcID int64, h *hostconn.Host, dom libvirt.Domain, cfg replConfig) {
	uuid := cluster.FormatUUID(dom.UUID)
	if replRunning(uuid) {
		return
	}
	if cfg.PausedCopy {
		finishPausedCopy(h, dom)
		return
	}
	if !cfg.Enabled {
		return
	}
	now := time.Now()
	replStatus.Lock()
	failedAt := replStatus.failed[uuid]
	replStatus.Unlock()
	if now.Sub(failedAt) < schedRetry {
		return
	}
	if cfg.LastOK > 0 && now.Sub(time.Unix(cfg.LastOK, 0)) < time.Duration(cfg.EveryMinutes)*time.Minute {
		return
	}
	if _, err := s.Nodes.Host(cfg.TargetNode); err != nil {
		return // the standby is away: the copy ages and the alert says so, no run can help
	}
	r := schedRequest()
	run, err := s.planReplica(srcID, h, dom, cfg, false)
	if err != nil {
		replStatus.Lock()
		replStatus.failed[uuid] = now
		replStatus.err[uuid], replStatus.errAt[uuid] = err.Error(), now
		replStatus.Unlock()
		s.recordFailure(r, "replicate", replTarget2(dom.Name, cfg.TargetName), h.Name, err)
		logf("scheduled replication of %s on %s failed: %v", dom.Name, h.Name, err)
		return
	}
	if _, _, err := s.launchReplica(r, run); err != nil {
		if _, busy := err.(badRequest); !busy {
			s.recordFailure(r, "replicate", replTarget2(dom.Name, cfg.TargetName), h.Name, err)
		}
	}
}
