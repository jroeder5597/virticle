package api

import (
	"net/http"
	"strings"
)

// Which kind of snapshot a VM gets. libvirt cannot delete snapshots in a tree that mixes internal ones (stored
// inside the qcow2 file) and external ones (overlay files), so a VM keeps one kind until all its snapshots are gone.
// What libvirt can do, from experiments on the lab (libvirt 11.3 on Debian 13 with AppArmor):
//
//	state            internal                                        external
//	running          only with the running state saved               with or without the running state
//	                 (without: "must include the memory state");
//	                 not with a raw UEFI variable store
//	shut off         yes (also with a raw UEFI variable store)       yes (disk only)
//
// On a node that confines QEMU with AppArmor an external snapshot cannot be relied on to delete: libvirt's
// per-VM profile denies writes to a backing file ("deny w") whenever it generates the profile from the domain
// definition (a started or restarted VM, a shut-off VM, after a revert), and nothing can override a deny rule. So
// such nodes only get internal snapshots, which means: running with the running state saved, or shut off.

const (
	kindInternal = "internal"
	kindExternal = "external"
)

// snapInputs is everything the choice depends on.
type snapInputs struct {
	Active    bool // running or paused
	RawNVRAM  bool // UEFI with a raw variable store: no internal snapshot of a running VM
	AppArmor  bool // the node confines QEMU with AppArmor
	HasInt    bool // the VM already has an internal snapshot
	HasExt    bool // the VM already has an external snapshot
	WithState bool // the request: save the running state too (ignored when shut off)
}

// snapChoice is the outcome for one request.
type snapChoice struct {
	Allowed bool   `json:"allowed"`
	Kind    string `json:"kind,omitempty"` // internal or external when allowed
	Reason  string `json:"reason,omitempty"`
}

const (
	reasonMixed          = "This VM has snapshots of both kinds, which libvirt cannot manage together. Delete its snapshots before taking another."
	reasonNoStateWhenOff = "A VM that is shut off has no running state to save."
	reasonKeepState      = "This VM's earlier snapshots only work with the running state saved. Delete them to choose again."
	reasonAppArmorState  = "On this node a running VM can only be snapshotted with its running state saved: a snapshot of the disk alone could not be deleted again. Save the running state, or shut the VM down first."
	reasonStuckExternal  = "This node cannot delete this VM's earlier snapshots, so it does not take new ones."
	reasonRunningFirmw   = "A running snapshot of this VM cannot sit next to its earlier ones. Delete the earlier snapshots."
	reasonFirmware       = "This VM's UEFI settings are stored in a form that does not allow a snapshot of the running VM."
)

// chooseSnapshotKind is the one place that decides the kind of a new snapshot, or why none can be taken.
func chooseSnapshotKind(in snapInputs) snapChoice {
	if !in.Active && in.WithState {
		return snapChoice{Reason: reasonNoStateWhenOff}
	}
	if in.HasInt && in.HasExt {
		return snapChoice{Reason: reasonMixed}
	}
	// What libvirt can take in this state.
	canInt := !in.Active || (in.WithState && !in.RawNVRAM)
	canExt := !in.AppArmor
	// What the VM's existing snapshots allow.
	if in.HasInt {
		canExt = false
	}
	if in.HasExt {
		canInt = false
	}
	switch {
	case canExt && canInt:
		// Nothing decides yet. A running VM with its state saved prefers an external snapshot (the guest keeps
		// running, and the layout survives live migration); a shut-off VM keeps the snapshot inside the disk.
		if in.Active {
			return snapChoice{Allowed: true, Kind: kindExternal}
		}
		return snapChoice{Allowed: true, Kind: kindInternal}
	case canInt:
		return snapChoice{Allowed: true, Kind: kindInternal}
	case canExt:
		return snapChoice{Allowed: true, Kind: kindExternal}
	}
	// Refused: say which fact is in the way, the existing snapshots before the node.
	switch {
	case in.HasExt && in.AppArmor:
		return snapChoice{Reason: reasonStuckExternal}
	case in.HasInt && in.Active && !in.WithState:
		return snapChoice{Reason: reasonKeepState}
	case in.HasInt && in.Active && in.RawNVRAM:
		return snapChoice{Reason: reasonRunningFirmw}
	case in.AppArmor && in.Active && !in.WithState:
		return snapChoice{Reason: reasonAppArmorState}
	case in.Active && in.RawNVRAM:
		return snapChoice{Reason: reasonFirmware}
	}
	return snapChoice{Reason: reasonMixed}
}

// snapOptions is what the Take snapshot dialog needs: whether each choice of the checkbox works, and one reason
// for the button when neither does.
type snapOptions struct {
	WithState snapChoice `json:"withState"`
	DiskOnly  snapChoice `json:"diskOnly"`
	Reason    string     `json:"reason,omitempty"` // set when no snapshot can be taken now
	Mixed     string     `json:"mixed,omitempty"`  // one line about a tree that already mixes both kinds
}

func snapshotOptions(in snapInputs) snapOptions {
	in.WithState = true
	o := snapOptions{WithState: chooseSnapshotKind(in)}
	in.WithState = false
	o.DiskOnly = chooseSnapshotKind(in)
	if !o.WithState.Allowed && !o.DiskOnly.Allowed {
		// The reason of the choice the user would expect first: the running state for a running VM.
		o.Reason = o.DiskOnly.Reason
		if in.Active {
			o.Reason = o.WithState.Reason
			// A shut-off VM may still be possible.
			off := in
			off.Active = false
			if chooseSnapshotKind(off).Allowed {
				o.Reason += " Shutting the VM down also works."
			}
		}
	}
	return o
}

// treeKinds reports which kinds the snapshots of a VM are.
func treeKinds(list []snapInfo) (hasInt, hasExt bool) {
	for _, si := range list {
		if si.Kind == kindExternal {
			hasExt = true
		} else {
			hasInt = true
		}
	}
	return
}

// mixedTreeNote is one plain line about a tree that mixes both kinds ("" when it does not): which snapshots libvirt
// will not delete one by one, and what works instead. Experiments (lab, libvirt 11.3): an external snapshot below an
// internal one cannot be deleted, an internal snapshot with an external one below it cannot be deleted, deleting
// from the tips down works where nothing is stuck, and reverting to the internal snapshot first (which discards the
// changes since) makes the rest deletable.
func mixedTreeNote(list []snapInfo, apparmor bool) string {
	if h, e := treeKinds(list); !h || !e {
		return ""
	}
	byName := map[string]snapInfo{}
	for _, si := range list {
		byName[si.Name] = si
	}
	stuck := map[string]bool{}
	for _, si := range list {
		if p, ok := byName[si.Parent]; ok && si.Kind == kindExternal && p.Kind != kindExternal {
			stuck[si.Name], stuck[p.Name] = true, true
		}
	}
	if len(stuck) == 0 {
		return ""
	}
	revertTo := ""
	var names []string
	for _, si := range list { // topological: the first internal one is the nearest to the root
		if stuck[si.Name] {
			names = append(names, si.Name)
			if revertTo == "" && si.Kind != kindExternal {
				revertTo = si.Name
			}
		}
	}
	line := "libvirt cannot delete " + strings.Join(names, ", ") + " one by one because this VM's snapshots are of mixed kinds."
	if revertTo != "" && !apparmor { // on an AppArmor node the external ones stay undeletable
		line += " Reverting to " + revertTo + " first (changes made since are lost) lets you delete them, or delete the VM."
	} else {
		line += " Delete the VM to remove them."
	}
	return line
}

func (s *Server) snapshotOptionsHandler(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	list, err := snapshotList(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	in, err := snapInputsOf(h, dom, list)
	if err != nil {
		fail(w, err)
		return
	}
	o := snapshotOptions(in)
	o.Mixed = mixedTreeNote(list, in.AppArmor)
	writeJSON(w, o)
}
