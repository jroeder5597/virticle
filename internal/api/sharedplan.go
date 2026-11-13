package api

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Shared storage, the pure part: which files a migration can leave where they are, and how the pools of the
// nodes add up to shared storage. Nothing here talks to a node.
//
// A pool is "shared" when it is an NFS pool (libvirt type netfs) and the other node has an NFS pool with the
// same server, the same export and the same mount path. The mount path matters because a VM's definition holds
// the path of its disk files: the same path on both nodes must lead to the same file.

// poolDesc is what the planning needs to know about one storage pool of a node.
type poolDesc struct {
	Name   string
	Type   string // dir | netfs | fs | ...
	Target string // directory on the node
	Server string // netfs: NFS server
	Export string // netfs: path exported by the server
	Active bool
}

func (p poolDesc) isNFS() bool { return p.Type == "netfs" && p.Server != "" }

// shareKey identifies the files behind an NFS pool; two pools with the same key show the same files.
func (p poolDesc) shareKey() string {
	return strings.ToLower(p.Server) + ":" + path.Clean(p.Export) + "@" + path.Clean(p.Target)
}

// sameShare reports whether a and b are the same NFS share mounted at the same path.
func sameShare(a, b poolDesc) bool { return a.isNFS() && b.isNFS() && a.shareKey() == b.shareKey() }

// shareText names a share the way an administrator writes it.
func (p poolDesc) shareText() string { return p.Server + ":" + path.Clean(p.Export) }

// diskPlace says what the migration does with the files in one directory.
type diskPlace struct {
	Dir string
	// Shared: both nodes mount the same NFS share there; nothing is copied.
	Shared bool
	// Pool is the source node's pool for the directory (the zero value if it has none).
	Pool poolDesc
	// Problem, when set, blocks the migration; it says what to do.
	Problem string
	// Note says why a copy happens, when the source is NFS but the target is not the same share.
	Note string
}

// placeDir decides for one directory. src and dst map a pool's directory to the pool.
func placeDir(dir, srcName, dstName string, src, dst map[string]poolDesc) diskPlace {
	sp, ok := src[dir]
	pl := diskPlace{Dir: dir, Pool: sp}
	if !ok || !sp.isNFS() {
		return pl // local: the existing rules (copy, space check) apply
	}
	dp, has := dst[dir]
	switch {
	case !has:
		pl.Problem = fmt.Sprintf("the shared storage %s (%s) is not set up on %s. Add it there: Storage, Add shared storage (the same name and server) and try again", sp.Name, sp.shareText(), dstName)
	case !dp.isNFS():
		pl.Note = fmt.Sprintf("%s has a different pool for %s, not the NFS share %s, so the files are copied", dstName, dir, sp.shareText())
	case !sameShare(sp, dp):
		pl.Note = fmt.Sprintf("%s mounts %s at %s, not %s (the other node's share), so the files are copied", dstName, dp.shareText(), dir, sp.shareText())
	case !dp.Active:
		pl.Shared = true
		pl.Problem = fmt.Sprintf("the pool %s on %s is not running. Start it (Storage) and try again", dp.Name, dstName)
	default:
		pl.Shared = true
	}
	return pl
}

// diskSummary is the plain-words line of the pre-flight about disks: what is copied and what is not.
// shared and local list "vda (pool)" entries.
func diskSummary(shared, local []string) string {
	switch {
	case len(shared) == 0 && len(local) == 0:
		return "the VM has no disk to move"
	case len(local) == 0:
		return "no disk is copied: every disk is on shared storage that both nodes mount (" + strings.Join(shared, ", ") + ")"
	case len(shared) == 0:
		return "all disks are copied to the target: " + strings.Join(local, ", ")
	}
	return "not copied, on shared storage: " + strings.Join(shared, ", ") + ". Copied, on local storage: " + strings.Join(local, ", ")
}

// --- shared storage across the datacenter ---

// nodeNFS is one node's NFS pool.
type nodeNFS struct {
	Node      int64
	Name      string // node name
	Pool      string
	Server    string
	Export    string
	Target    string
	State     string
	Autostart bool
}

// sharedMember is one node that has the pool.
type sharedMember struct {
	Node      int64  `json:"node"`
	NodeName  string `json:"nodeName"`
	State     string `json:"state"`
	Autostart bool   `json:"autostart"`
	// Differs is set when this node's pool of the same name points at another share or path.
	Differs bool `json:"differs,omitempty"`
}

// sharedGroup is one NFS pool name across the nodes: shared storage when more than one node has it.
type sharedGroup struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	Export string `json:"export"`
	Target string `json:"target"`
	// Nodes that have the pool with these exact settings.
	Nodes []sharedMember `json:"nodes"`
	// Missing are the online nodes that do not have it; Differ have a pool of this name with other settings.
	Missing []sharedMember `json:"missing"`
	Differ  []sharedMember `json:"differ"`
	// State is "running" when every member node's pool runs, "unavailable" when any is unavailable, else the
	// first other state.
	State string `json:"state"`
}

// groupShared groups the NFS pools of the nodes by name. The first node (by id) that has a name gives the
// group its settings; nodes with other settings go to Differ. all lists every online node.
func groupShared(pools []nodeNFS, all []sharedMember) []sharedGroup {
	byName := map[string]*sharedGroup{}
	var order []string
	sort.SliceStable(pools, func(i, j int) bool { return pools[i].Node < pools[j].Node })
	for _, p := range pools {
		g, ok := byName[p.Pool]
		m := sharedMember{Node: p.Node, NodeName: p.Name, State: p.State, Autostart: p.Autostart}
		if !ok {
			g = &sharedGroup{Name: p.Pool, Server: p.Server, Export: p.Export, Target: p.Target}
			byName[p.Pool] = g
			order = append(order, p.Pool)
		}
		if strings.EqualFold(g.Server, p.Server) && path.Clean(g.Export) == path.Clean(p.Export) && path.Clean(g.Target) == path.Clean(p.Target) {
			g.Nodes = append(g.Nodes, m)
		} else {
			m.Differs = true
			g.Differ = append(g.Differ, m)
		}
	}
	var out []sharedGroup
	sort.Strings(order)
	for _, name := range order {
		g := byName[name]
		have := map[int64]bool{}
		for _, m := range g.Nodes {
			have[m.Node] = true
		}
		for _, m := range g.Differ {
			have[m.Node] = true
		}
		for _, n := range all {
			if !have[n.Node] {
				g.Missing = append(g.Missing, sharedMember{Node: n.Node, NodeName: n.NodeName})
			}
		}
		if g.Missing == nil {
			g.Missing = []sharedMember{} // JSON: an empty list, not null
		}
		if g.Differ == nil {
			g.Differ = []sharedMember{}
		}
		g.State = "running"
		for _, m := range g.Nodes {
			switch {
			case m.State == "unavailable":
				g.State = "unavailable"
			case m.State != "running" && g.State == "running":
				g.State = m.State
			}
		}
		out = append(out, *g)
	}
	return out
}

// --- adding to every node ---

// nodeOutcome is what adding the share did on one node.
type nodeOutcome struct {
	Node     int64  `json:"node"`
	NodeName string `json:"nodeName"`
	OK       bool   `json:"ok"`
	// Already is set when the node had the pool with these settings and nothing was changed.
	Already bool   `json:"already,omitempty"`
	Error   string `json:"error,omitempty"`
	// Version is the NFS version the node mounted with ("4.2", "3"), when known.
	Version string `json:"version,omitempty"`
}

// versionNote is the one sentence that goes with an NFS version 3 mount, or "" for anything else. Version 3 has
// no locking in the protocol itself: the file locks that stop two nodes from opening one disk image go through
// a separate lock service (rpc.statd and lockd) that must be reachable from every node.
func versionNote(res []nodeOutcome) string {
	var nodes []string
	for _, r := range res {
		if r.OK && r.Version == "3" {
			nodes = append(nodes, r.NodeName)
		}
	}
	if len(nodes) == 0 {
		return ""
	}
	return "Mounted with NFS version 3 on " + strings.Join(nodes, ", ") + ": the locks that keep two nodes from running one VM disk need the server's lock service (rpc.statd and lockd) reachable from every node. If the server can offer NFS 4, enable it and add the storage again."
}

// parseMountVersion reads the NFS version out of the options column of /proc/mounts ("vers=4.2").
func parseMountVersion(opts string) string {
	for _, o := range strings.Split(strings.TrimSpace(opts), ",") {
		if v, ok := strings.CutPrefix(o, "vers="); ok {
			return v
		}
	}
	return ""
}

// addSummary turns per-node outcomes into one sentence for the user and says whether the result is complete.
// Nothing is rolled back across nodes: a node that has the share keeps it, and the sentence names the nodes
// that failed so the user can fix them and press the button again (it is safe to repeat).
func addSummary(name string, res []nodeOutcome) (msg string, complete bool) {
	var ok, order []string
	why := map[string][]string{} // error text -> nodes that failed with it
	for _, r := range res {
		if r.OK {
			ok = append(ok, r.NodeName)
			continue
		}
		if _, seen := why[r.Error]; !seen {
			order = append(order, r.Error)
		}
		why[r.Error] = append(why[r.Error], r.NodeName)
	}
	var bad []string
	for _, e := range order {
		bad = append(bad, strings.Join(why[e], ", ")+": "+e)
	}
	switch {
	case len(bad) == 0:
		return fmt.Sprintf("%s is mounted on %s", name, strings.Join(ok, ", ")), true
	case len(ok) == 0:
		return fmt.Sprintf("%s could not be added on any node. %s", name, strings.Join(bad, "; ")), false
	}
	return fmt.Sprintf("%s is mounted on %s, but not everywhere. Not added: %s. Fix that and add it again; nodes that have it are left as they are", name, strings.Join(ok, ", "), strings.Join(bad, "; ")), false
}
