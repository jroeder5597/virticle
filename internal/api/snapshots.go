package api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// Snapshot kinds (libvirt 11.3 on Debian 13, verified on the lab): see snapkind.go, which decides the kind of a
// new snapshot in one place. A VM keeps one kind until all its snapshots are deleted.
//
// An internal snapshot lives inside the qcow2 file and always works for create, revert and delete. An external one
// is an overlay file (plus "<vm>.<snap>.mem" when the running state is saved) next to the disk. Deleting an external
// snapshot merges the overlay into its backing file; with libvirt's AppArmor driver that write is denied once the
// per-VM profile has been generated from the domain definition ("block-commit: Permission denied"), and libvirt
// leaves the disk marked as being in an unfinished snapshot delete. Nodes with AppArmor therefore only get
// internal snapshots.

var snapNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

const snapNameRule = "snapshot name must start with a letter or digit and contain only letters, digits, dot, dash and underscore (max 63 characters)"

// snapDomXML is the part of a domain definition snapshotting cares about.
type snapDomXML struct {
	OS struct {
		Firmware string `xml:"firmware,attr"`
		NVRAM    struct {
			Format string `xml:"format,attr"`
			Path   string `xml:",chardata"`
		} `xml:"nvram"`
		Loader struct {
			Type string `xml:"type,attr"`
		} `xml:"loader"`
	} `xml:"os"`
	Devices struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
			Source struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
			ReadOnly *struct{} `xml:"readonly"`
		} `xml:"disk"`
		Channels []struct {
			Target struct {
				Name  string `xml:"name,attr"`
				State string `xml:"state,attr"`
			} `xml:"target"`
		} `xml:"channel"`
	} `xml:"devices"`
}

// snapXML is a snapshot's own XML (the embedded domain copy is ignored).
type snapXML struct {
	Name         string `xml:"name"`
	Description  string `xml:"description"`
	State        string `xml:"state"`
	CreationTime int64  `xml:"creationTime"`
	Parent       struct {
		Name string `xml:"name"`
	} `xml:"parent"`
	Memory struct {
		Snapshot string `xml:"snapshot,attr"`
		File     string `xml:"file,attr"`
	} `xml:"memory"`
	Disks []struct {
		Name     string `xml:"name,attr"`
		Snapshot string `xml:"snapshot,attr"`
		Source   struct {
			File string `xml:"file,attr"`
		} `xml:"source"`
	} `xml:"disks>disk"`
}

type snapInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Created     int64    `json:"created"` // unix seconds
	State       string   `json:"state"`   // VM state when taken
	Parent      string   `json:"parent,omitempty"`
	IsCurrent   bool     `json:"isCurrent"`
	HasMemory   bool     `json:"hasMemory"`
	Kind        string   `json:"kind"` // internal | external
	Children    int      `json:"children"`
	Files       []string `json:"files"` // overlay and memory files of an external snapshot
	// Scheduled: made by the snapshot scheduler. Deletable is false when libvirt can never delete it on this
	// node (Reason says why); whether a selection can be deleted is decided by the plan endpoint.
	Scheduled bool   `json:"scheduled"`
	Deletable bool   `json:"deletable"`
	Reason    string `json:"reason,omitempty"`
}

func snapInfoOf(x snapXML) snapInfo {
	si := snapInfo{
		Name: x.Name, Description: x.Description, Created: x.CreationTime, State: x.State,
		Parent: x.Parent.Name, HasMemory: x.Memory.Snapshot != "" && x.Memory.Snapshot != "no",
		Kind: "internal", Files: []string{},
	}
	if x.Memory.Snapshot == "external" {
		si.Kind = "external"
		si.Files = append(si.Files, x.Memory.File)
	}
	for _, d := range x.Disks {
		if d.Snapshot == "external" {
			si.Kind = "external"
			si.Files = append(si.Files, d.Source.File)
		}
	}
	return si
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := snapshotList(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	annotateSnapshots(out, nodeIsAppArmor(h))
	writeJSON(w, out)
}

// snapshotList returns every snapshot in topological order (parents first).
func snapshotList(h *hostconn.Host, dom libvirt.Domain) ([]snapInfo, error) {
	snaps, _, err := h.DomainListAllSnapshots(dom, 1, uint32(libvirt.DomainSnapshotListTopological))
	if err != nil {
		return nil, err
	}
	current := ""
	if cur, err := h.DomainSnapshotCurrent(dom, 0); err == nil {
		current = cur.Name
	}
	out := []snapInfo{}
	kids := map[string]int{}
	for _, sn := range snaps {
		raw, err := h.DomainSnapshotGetXMLDesc(sn, 0)
		if err != nil {
			var lv libvirt.Error
			if errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomainSnapshot) {
				continue // deleted by a running task since the list was taken
			}
			return nil, err
		}
		var x snapXML
		if err := xml.Unmarshal([]byte(raw), &x); err != nil {
			return nil, err
		}
		si := snapInfoOf(x)
		si.IsCurrent = si.Name == current
		kids[si.Parent]++
		out = append(out, si)
	}
	for i := range out {
		out[i].Children = kids[out[i].Name]
	}
	return out, nil
}

type snapshotRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Memory      bool   `json:"memory"`
	// Quiesce freezes the guest file systems through the guest agent. Absent
	// means automatic: freeze when the agent is connected and no memory is
	// saved, otherwise take the snapshot without freezing. true insists on
	// the freeze and fails without an agent; false never freezes.
	Quiesce *bool `json:"quiesce"`
	// Mode is "" (choose automatically), "internal" or "external"; it only
	// matters for a live snapshot with memory.
	Mode string `json:"mode"`
}

func (s *Server) createSnapshotHandler(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var q snapshotRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	name, err := s.createSnapshot(r, h, dom, q)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"name": name})
}

// createSnapshot validates the request, builds the snapshot XML for the VM's
// current state and creates the snapshot as a task.
func (s *Server) createSnapshot(r *http.Request, h *hostconn.Host, dom libvirt.Domain, q snapshotRequest) (string, error) {
	if q.Name == "" {
		q.Name = "snap-" + time.Now().UTC().Format("20060102-150405")
	}
	if !snapNameRE.MatchString(q.Name) {
		return "", badRequest(snapNameRule)
	}
	st, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		return "", err
	}
	active := stateName(st) != "shutoff"
	raw, err := h.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return "", err
	}
	var dx snapDomXML
	if err := xml.Unmarshal([]byte(raw), &dx); err != nil {
		return "", err
	}
	explicitQuiesce := q.Quiesce != nil && *q.Quiesce
	if !active && (q.Memory || explicitQuiesce) {
		return "", badRequest("a VM that is shut off has no memory to save and nothing to freeze; clear those options")
	}
	agent := "no guest agent channel"
	for _, c := range dx.Devices.Channels {
		if c.Target.Name == "org.qemu.guest_agent.0" {
			agent = "the agent channel exists but nothing inside the guest answers"
			if c.Target.State == "connected" {
				agent = ""
			}
		}
	}
	// Automatic freeze: only with a connected agent, a running guest and no memory.
	autoQuiesce := q.Quiesce == nil && active && !q.Memory && agent == ""
	quiesce := explicitQuiesce || autoQuiesce
	if explicitQuiesce {
		if q.Memory {
			return "", badRequest("freezing the guest file systems only applies to disk-only snapshots; clear either 'include memory' or 'quiesce'")
		}
		if agent != "" {
			return "", badRequest("quiesce needs the QEMU guest agent, which is not connected (" + agent + "); install and start qemu-guest-agent in the guest, or take the snapshot without quiesce")
		}
	}

	switch q.Mode {
	case "", kindInternal, kindExternal:
	default:
		return "", badRequest(`mode must be "", "internal" or "external"`)
	}
	list, err := snapshotList(h, dom)
	if err != nil {
		return "", err
	}
	in, err := snapInputsOf(h, dom, list)
	if err != nil {
		return "", err
	}
	in.Active, in.WithState = active, q.Memory
	choice := chooseSnapshotKind(in)
	if !choice.Allowed {
		return "", badRequest(choice.Reason)
	}
	if q.Mode != "" && q.Mode != choice.Kind {
		return "", badRequest("this VM can only take " + choice.Kind + " snapshots right now; leave the mode empty")
	}
	external := choice.Kind == kindExternal

	var (
		body  strings.Builder
		flags libvirt.DomainSnapshotCreateFlags
	)
	fmt.Fprintf(&body, "<domainsnapshot>\n  <name>%s</name>\n", q.Name)
	if q.Description != "" {
		fmt.Fprintf(&body, "  <description>%s</description>\n", esc(q.Description))
	}
	if external {
		prefix := strings.Map(func(r rune) rune {
			if r == '/' || r == '\'' || r == '"' || r == '<' || r == '>' || r == '&' || r < 32 {
				return '_'
			}
			return r
		}, dom.Name)
		var dir string
		var disks strings.Builder
		for _, d := range dx.Devices.Disks {
			if d.Device != "disk" || d.Type != "file" || d.ReadOnly != nil || d.Source.File == "" {
				fmt.Fprintf(&disks, "    <disk name='%s' snapshot='no'/>\n", esc(d.Target.Dev))
				continue
			}
			if dir == "" {
				dir = path.Dir(d.Source.File)
			}
			fmt.Fprintf(&disks, "    <disk name='%s' snapshot='external'>\n      <source file='%s'/>\n    </disk>\n",
				esc(d.Target.Dev), esc(path.Join(path.Dir(d.Source.File), prefix+"."+q.Name+".qcow2")))
		}
		if dir == "" {
			return "", badRequest("this VM has no writable file-backed disk; an external snapshot needs one")
		}
		if q.Memory {
			fmt.Fprintf(&body, "  <memory snapshot='external' file='%s'/>\n", esc(path.Join(dir, prefix+"."+q.Name+".mem")))
			if stateName(st) == "running" {
				flags |= libvirt.DomainSnapshotCreateLive // do not pause the guest while memory is written
			}
		} else {
			body.WriteString("  <memory snapshot='no'/>\n")
			flags |= libvirt.DomainSnapshotCreateDiskOnly
			if quiesce {
				flags |= libvirt.DomainSnapshotCreateQuiesce
			}
		}
		fmt.Fprintf(&body, "  <disks>\n%s  </disks>\n", disks.String())
	}
	body.WriteString("</domainsnapshot>")

	xmlBody, withMemory := body.String(), active && q.Memory
	err = s.taskAsync(r, "snapshot create", dom.Name, h.Name, func(progress func(float64)) error {
		defer refreshPools(h)
		create := func() error {
			_, err := h.DomainSnapshotCreateXML(dom, xmlBody, uint32(flags))
			if err != nil && autoQuiesce && flags&libvirt.DomainSnapshotCreateQuiesce != 0 {
				// The automatic freeze must never make the snapshot fail
				// (an agent that cannot freeze, as on Windows without VSS).
				_, err = h.DomainSnapshotCreateXML(dom, xmlBody, uint32(flags&^libvirt.DomainSnapshotCreateQuiesce))
			}
			return err
		}
		if withMemory {
			return withJobProgress(h, dom, progress, create)
		}
		return create()
	})
	return q.Name, err
}

func (s *Server) snapshot(r *http.Request) (*hostconn.Host, libvirt.Domain, libvirt.DomainSnapshot, error) {
	h, dom, err := s.domain(r)
	if err != nil {
		return nil, dom, libvirt.DomainSnapshot{}, err
	}
	name := r.PathValue("name")
	if !snapNameRE.MatchString(name) {
		return nil, dom, libvirt.DomainSnapshot{}, badRequest(snapNameRule)
	}
	sn, err := h.DomainSnapshotLookupByName(dom, name, 0)
	return h, dom, sn, err
}

func (s *Server) revertSnapshot(w http.ResponseWriter, r *http.Request) {
	h, dom, sn, err := s.snapshot(r)
	if err != nil {
		fail(w, err)
		return
	}
	var q struct {
		Start string `json:"start"` // "" (as saved), "running" or "paused"
		Force bool   `json:"force"`
	}
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	var flags libvirt.DomainSnapshotRevertFlags
	switch q.Start {
	case "":
	case "running":
		flags |= libvirt.DomainSnapshotRevertRunning
	case "paused":
		flags |= libvirt.DomainSnapshotRevertPaused
	default:
		fail(w, badRequest(`start must be "", "running" or "paused"`))
		return
	}
	if q.Force {
		flags |= libvirt.DomainSnapshotRevertForce
	}
	err = s.taskAsync(r, "snapshot revert", dom.Name+"@"+sn.Name, h.Name, func(func(float64)) error {
		defer refreshPools(h)
		return h.DomainRevertToSnapshot(sn, uint32(flags))
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.autoSyncClock(userOf(r), "snapshot revert", h, dom)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	h, dom, sn, err := s.snapshot(r)
	if err != nil {
		fail(w, err)
		return
	}
	// libvirt's own DELETE_CHILDREN flag refuses external snapshots
	// ("deletion of external children disk snapshots not supported"), so
	// descendants are deleted one by one, deepest first. Deleting a snapshot
	// that has children but without this option merges it into them.
	withChildren := r.URL.Query().Get("children") == "1"
	err = s.taskAsync(r, "snapshot delete", dom.Name+"@"+sn.Name, h.Name, func(progress func(float64)) error {
		defer refreshPools(h)
		victims := []string{sn.Name}
		if withChildren {
			list, err := snapshotList(h, dom)
			if err != nil {
				return err
			}
			victims = append(descendants(list, sn.Name), sn.Name)
		}
		for i, name := range victims {
			v, err := h.DomainSnapshotLookupByName(dom, name, 0)
			if err != nil {
				return err
			}
			if err := h.DomainSnapshotDelete(v, 0); err != nil {
				if i > 0 {
					return fmt.Errorf("deleted %d of %d snapshots, then %s failed: %w", i, len(victims), name, err)
				}
				return err
			}
			progress(float64(i+1) / float64(len(victims)))
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// descendants lists every snapshot below root, deepest first. list must be in
// topological order (parents before children), so walking it backwards visits
// children before their parents.
func descendants(list []snapInfo, root string) []string {
	in := map[string]bool{root: true}
	var order []string
	for _, si := range list {
		if in[si.Parent] {
			in[si.Name] = true
			order = append(order, si.Name)
		}
	}
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	return order
}

// refreshPools lets storage pools rescan their directories so overlay and
// memory files created or removed by libvirt show up in volume listings.
func refreshPools(h *hostconn.Host) {
	pools, _, err := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive)
	if err != nil {
		return
	}
	for _, p := range pools {
		h.StoragePoolRefresh(p, 0)
	}
}

// apparmorNodes caches, per node name, whether QEMU is confined by AppArmor.
var apparmorNodes sync.Map

// snapInputsOf gathers the facts chooseSnapshotKind needs, apart from the VM state and the request.
func snapInputsOf(h *hostconn.Host, dom libvirt.Domain, list []snapInfo) (snapInputs, error) {
	raw, err := h.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return snapInputs{}, err
	}
	var dx snapDomXML
	if err := xml.Unmarshal([]byte(raw), &dx); err != nil {
		return snapInputs{}, err
	}
	st, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		return snapInputs{}, err
	}
	// libvirt refuses internal snapshots of pflash firmware unless its variable store is qcow2; VMs made before
	// that stay on external ones.
	rawUEFI := (dx.OS.Firmware == "efi" || dx.OS.Loader.Type == "pflash" || strings.TrimSpace(dx.OS.NVRAM.Path) != "") && dx.OS.NVRAM.Format != "qcow2"
	in := snapInputs{Active: stateName(st) != "shutoff", RawNVRAM: rawUEFI, AppArmor: nodeIsAppArmor(h)}
	in.HasInt, in.HasExt = treeKinds(list)
	return in, nil
}

func apparmorKnown(h *hostconn.Host) (bool, bool) {
	if v, ok := apparmorNodes.Load(h.Name); ok {
		return v.(bool), true
	}
	caps, err := h.ConnectGetCapabilities()
	if err != nil {
		return false, false
	}
	aa := secModelIsAppArmor(caps)
	apparmorNodes.Store(h.Name, aa)
	return aa, true
}

// nodeIsAppArmor reports whether QEMU is confined by AppArmor on the node (false when that cannot be read).
func nodeIsAppArmor(h *hostconn.Host) bool {
	aa, _ := apparmorKnown(h)
	return aa
}

func secModelIsAppArmor(caps string) bool {
	var x struct {
		Models []string `xml:"host>secmodel>model"`
	}
	if xml.Unmarshal([]byte(caps), &x) != nil {
		return false
	}
	for _, m := range x.Models {
		if m == "apparmor" {
			return true
		}
	}
	return false
}

// guestAgentConnected reports whether the guest agent channel of a running
// domain is connected.
func guestAgentConnected(h *hostconn.Host, dom libvirt.Domain) bool {
	raw, err := h.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return false
	}
	var dx snapDomXML
	if xml.Unmarshal([]byte(raw), &dx) != nil {
		return false
	}
	for _, c := range dx.Devices.Channels {
		if c.Target.Name == "org.qemu.guest_agent.0" && c.Target.State == "connected" {
			return true
		}
	}
	return false
}
