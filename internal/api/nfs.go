package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// Shared storage over NFS, added to every node of the datacenter in one step.
//
// What the app decides, so the user does not have to:
//
//   - The pool is a libvirt "netfs" pool with the same name and the same mount point (/mnt/NAME) on every node.
//     A VM's definition holds the full path of its disk files; only if that path is the same on both nodes can a
//     migration leave the disk where it is.
//   - The NFS version and the mount options are libvirt's own: `mount -t nfs`, which negotiates the highest
//     version both sides offer (4.2 with a current server, otherwise 4.1, 4 or 3) and mounts "hard". libvirt does
//     not let a netfs pool pass other options, and the defaults are the right ones: hard means a program waits
//     for a server that went away instead of getting an I/O error, so a VM pauses and continues rather than
//     seeing corrupt data (a soft mount would hand errors to the guest). NFS 4 also gives the cluster file
//     locks, which is what stops two QEMU processes from opening one disk.
//   - The pool is autostarted: libvirt mounts it when the node boots, before VMs autostart.
//   - The share must let the node write before it is kept (checkWrite), so the user finds out at "Add", not at the
//     first VM start.

var (
	nfsNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	nfsServerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`)
	nfsExportRE = regexp.MustCompile(`^/[A-Za-z0-9._+=,@/-]*$`)
)

type nfsRequest struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	Path   string `json:"path"`
	// Nodes limits the share to these nodes (ids). Empty means every online node.
	Nodes []int64 `json:"nodes"`
}

// nfsMount is where the share is mounted on every node.
func nfsMount(name string) string { return "/mnt/" + name }

func (q *nfsRequest) check() error {
	q.Name, q.Server, q.Path = strings.TrimSpace(q.Name), strings.TrimSpace(q.Server), strings.TrimSpace(q.Path)
	switch {
	case q.Name == "":
		return badRequest("give the storage a name")
	case !nfsNameRE.MatchString(q.Name):
		return badRequest("the name may use letters, digits, - and _ (up to 32 characters) and starts with a letter or digit; it also names the mount directory " + nfsMount("NAME"))
	case q.Server == "":
		return badRequest("give the NFS server (name or address)")
	case !nfsServerRE.MatchString(q.Server):
		return badRequest("the server must be a host name or an IP address")
	case q.Path == "":
		return badRequest("give the export path on the server, for example /export/vms")
	case !nfsExportRE.MatchString(q.Path) || strings.Contains(q.Path, ".."):
		return badRequest("the export path must be absolute and use only letters, digits and . _ + = , @ / -")
	}
	q.Path = path.Clean(q.Path)
	return nil
}

func nfsPoolXML(name, server, export string) string {
	return fmt.Sprintf(`<pool type='netfs'><name>%s</name><source><host name='%s'/><dir path='%s'/><format type='auto'/></source><target><path>%s</path></target></pool>`,
		esc(name), esc(server), esc(export), esc(nfsMount(name)))
}

// nfsHint turns what mount or libvirt said into a sentence that says what to do. server and export are what the
// user typed, node names the node that failed; uid is the node's qemu user id (0 when unknown).
func nfsHint(msg, node, server, export string) string {
	m := strings.ToLower(msg)
	first := firstLine(msg)
	switch {
	case strings.Contains(m, "access denied by server"), strings.Contains(m, "permission denied") && strings.Contains(m, "mount"):
		return fmt.Sprintf("the server %s does not allow %s to mount %s. On the server, add this node's address (or its whole subnet) to the clients of the export", server, node, export)
	case strings.Contains(m, "no such file or directory") && strings.Contains(m, "mount.nfs"), strings.Contains(m, "does not exist") && strings.Contains(m, "export"):
		return fmt.Sprintf("the server %s has no export %s. Check the path on the server (exportfs -v lists the exports)", server, export)
	case strings.Contains(m, "name or service not known"), strings.Contains(m, "failed to resolve"), strings.Contains(m, "bad nfs server"):
		return fmt.Sprintf("%s cannot resolve %s. Use the server's IP address or fix the name resolution on the node", node, server)
	case strings.Contains(m, "need a /sbin/mount."), strings.Contains(m, "mount.nfs: command not found"), strings.Contains(m, "mount point"), strings.Contains(m, "unknown filesystem type 'nfs"):
		return node + " has no NFS client tools. Run Prepare for the node again (Nodes), which installs them, then add the storage again"
	case strings.Contains(m, "connection refused"), strings.Contains(m, "no route to host"), strings.Contains(m, "timed out"), strings.Contains(m, "network is unreachable"):
		return fmt.Sprintf("%s cannot reach the NFS server %s. Check the address, that the NFS service runs there and that its firewall allows NFS (port 2049)", node, server)
	}
	return fmt.Sprintf("mounting %s:%s on %s failed: %s", server, export, node, first)
}

// writeHint is what to tell the user when the share mounts but a file cannot be created in it. The text does not
// name the node, so that nodes failing the same way give one message (addSummary joins them).
func writeHint(msg, server, export string, uid, gid int) string {
	m := strings.ToLower(msg)
	if strings.Contains(m, "read-only file system") {
		return fmt.Sprintf("%s:%s is exported read-only. Export it with rw", server, export)
	}
	if strings.Contains(m, "permission denied") || strings.Contains(m, "operation not permitted") {
		fix := "export it with no_root_squash for the nodes, or map every user to the VM user with all_squash and anonuid, anongid set to the VM user's id, and make the export directory owned by that id"
		if uid > 0 {
			fix = fmt.Sprintf("export it with no_root_squash for the nodes, or with all_squash,anonuid=%d,anongid=%d (the VM user's id) and make the export directory owned by %d:%d", uid, gid, uid, gid)
		}
		return fmt.Sprintf("%s:%s mounts, but VMs could not use it: the server turns root into an unprivileged user (root_squash), so files made by root cannot be written by the VM user. On the server, %s", server, export, fix)
	}
	return fmt.Sprintf("%s:%s mounts, but a file could not be created there: %s", server, export, firstLine(msg))
}

// nodeNFSInfo reads what adding needs to know about a node in one command: NFS client present, SELinux state,
// the VM user's id, reachability of the server.
const nfsFactsCmd = `command -v mount.nfs >/dev/null 2>&1 || [ -x /sbin/mount.nfs ] && echo client=yes || echo client=no
if command -v getenforce >/dev/null 2>&1; then echo selinux=$(getenforce 2>/dev/null); if command -v getsebool >/dev/null 2>&1; then echo virt_use_nfs=$(getsebool virt_use_nfs 2>/dev/null | awk '{print $NF}'); fi; fi
for u in qemu libvirt-qemu; do if id -u $u >/dev/null 2>&1; then echo qemu=$(id -u $u):$(id -g $u); break; fi; done`

type nfsFacts struct {
	Client     bool
	SELinux    string // enforcing | permissive | disabled | "" (not present)
	VirtUseNFS string // on | off | ""
	UID, GID   int
}

func parseNFSFacts(out string) nfsFacts {
	var f nfsFacts
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "client":
			f.Client = v == "yes"
		case "selinux":
			f.SELinux = strings.ToLower(v)
		case "virt_use_nfs":
			f.VirtUseNFS = strings.ToLower(v)
		case "qemu":
			a, b, _ := strings.Cut(v, ":")
			f.UID, _ = strconv.Atoi(a)
			f.GID, _ = strconv.Atoi(b)
		}
	}
	return f
}

// nfsSpec is what to add on every node.
type nfsSpec struct{ Name, Server, Export string }

func (sp nfsSpec) same(x poolXML) bool {
	return x.Type == "netfs" && strings.EqualFold(x.Source.Host.Name, sp.Server) && path.Clean(x.Source.Dir.Path) == path.Clean(sp.Export) && path.Clean(x.Target.Path) == nfsMount(sp.Name)
}

// addNFSToNode makes the pool exist, run and autostart on one node. Repeating it is safe. A failure leaves nothing
// of this attempt behind on the node (a pool that was there before is not touched).
func addNFSToNode(h *hostconn.Host, sp nfsSpec) nodeOutcome {
	res := nodeOutcome{Node: 0, NodeName: h.Name}
	fail := func(msg string) nodeOutcome { res.Error = msg; return res }

	if p, err := h.StoragePoolLookupByName(sp.Name); err == nil {
		var x poolXML
		if raw, err := h.StoragePoolGetXMLDesc(p, 0); err == nil {
			xml.Unmarshal([]byte(raw), &x)
		}
		if !sp.same(x) {
			return fail(fmt.Sprintf("already has a storage pool named %s with other settings (%s). Remove or rename that pool first", sp.Name, describePool(x)))
		}
		if act, _ := h.StoragePoolIsActive(p); act == 0 {
			if err := h.StoragePoolCreate(p, 0); err != nil {
				return fail(nfsHint(err.Error(), h.Name, sp.Server, sp.Export))
			}
		}
		h.StoragePoolSetAutostart(p, 1)
		res.OK, res.Already = true, true
		return res
	}

	out, _ := h.Run(nfsFactsCmd)
	f := parseNFSFacts(out)
	if !f.Client {
		return fail("has no NFS client tools. Run Prepare for the node again (Nodes), which installs them, then add the storage again")
	}
	if f.SELinux == "enforcing" && f.VirtUseNFS == "off" {
		return fail("SELinux blocks VMs from using NFS (virt_use_nfs is off). Run Prepare for the node again (Nodes), which turns it on, then add the storage again")
	}
	if out, err := h.Run(reachCmd(sp.Server, "2049")); err != nil {
		return fail(fmt.Sprintf("cannot reach the NFS server %s on port 2049 (%s). Check the address, that the NFS service runs there and that its firewall allows NFS", sp.Server, firstLine(out)))
	}

	pool, err := h.StoragePoolDefineXML(nfsPoolXML(sp.Name, sp.Server, sp.Export), 0)
	if err != nil {
		return fail(err.Error())
	}
	undo := func() {
		h.StoragePoolDestroy(pool)
		h.StoragePoolDelete(pool, 0) // removes the empty mount directory that Build made
		h.StoragePoolUndefine(pool)
	}
	// Build creates the mount directory; it is already there on a second attempt, which is not an error.
	h.StoragePoolBuild(pool, libvirt.StoragePoolBuildNew)
	if err := h.StoragePoolCreate(pool, 0); err != nil {
		undo()
		return fail(nfsHint(err.Error(), h.Name, sp.Server, sp.Export))
	}
	if err := checkWrite(h, pool, nfsMount(sp.Name), f.UID); err != nil {
		undo()
		return fail(writeHint(err.Error(), sp.Server, sp.Export, f.UID, f.GID))
	}
	if err := h.StoragePoolSetAutostart(pool, 1); err != nil {
		undo()
		return fail("could not set the pool to start with the node: " + err.Error())
	}
	res.OK = true
	return res
}

func describePool(x poolXML) string {
	if x.Type == "netfs" {
		return "NFS " + x.Source.Host.Name + ":" + x.Source.Dir.Path + " at " + x.Target.Path
	}
	return x.Type + " at " + x.Target.Path
}

// checkWrite creates, inspects and removes a small qcow2 volume in the pool the way a VM disk is made, so a share
// that mounts but does not work for VMs is found now. Two things can be wrong: root cannot write there at all
// (root_squash, read-only export), or root can write but the file ends up owned by someone the VM user cannot
// act as: with root_squash the server makes the file belong to "nobody", libvirt cannot hand it to the VM user,
// and QEMU later gets "permission denied".
func checkWrite(h *hostconn.Host, pool libvirt.StoragePool, mount string, qemuUID int) error {
	// Every node of the datacenter checks the same share at the same time, so the file name must differ.
	var rnd [6]byte
	rand.Read(rnd[:])
	name := "virticle-write-test-" + hex.EncodeToString(rnd[:]) + ".qcow2"
	vol, err := h.StorageVolCreateXML(pool, volumeXML(name, "qcow2", 1<<20), 0)
	if err != nil {
		return err
	}
	defer h.StorageVolDelete(vol, 0)
	if qemuUID > 0 && safePathRE.MatchString(mount+"/"+name) {
		if out, err := h.Run("stat -c '%u %a' " + shq(mount+"/"+name)); err == nil {
			uid, mode := parseOwner(out)
			if ownerProblem(uid, mode, qemuUID) {
				return fmt.Errorf("permission denied: a file made by root belongs to the user %d, not to the VM user", uid)
			}
		}
	}
	return nil
}

// parseOwner reads `stat -c '%u %a'`.
func parseOwner(out string) (uid, mode int) {
	f := strings.Fields(out)
	if len(f) != 2 {
		return -1, 0
	}
	uid, _ = strconv.Atoi(f[0])
	m, _ := strconv.ParseInt(f[1], 8, 32)
	return uid, int(m)
}

// ownerProblem says whether a file just made by root can be used by the VM user. It can when it belongs to root
// (no_root_squash: libvirt gives it to the VM user at VM start), when it already belongs to the VM user
// (all_squash with that id), or when everybody may write it.
func ownerProblem(fileUID, mode, qemuUID int) bool {
	switch {
	case fileUID < 0:
		return false // could not tell
	case fileUID == 0 || fileUID == qemuUID:
		return false
	}
	return mode&0o006 != 0o006
}

func (s *Server) onlineNodes() []cluster.NodeStatus {
	var out []cluster.NodeStatus
	for _, n := range s.Nodes.Status() {
		if n.State == "online" {
			out = append(out, n)
		}
	}
	return out
}

type nfsResult struct {
	Complete bool          `json:"complete"`
	Message  string        `json:"message"`
	Nodes    []nodeOutcome `json:"nodes"`
	Skipped  []string      `json:"skipped,omitempty"`
}

// addShared is POST /api/storage/shared: add the NFS share to every online node (or the listed ones).
func (s *Server) addShared(w http.ResponseWriter, r *http.Request) {
	var req nfsRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := req.check(); err != nil {
		fail(w, err)
		return
	}
	want := map[int64]bool{}
	for _, id := range req.Nodes {
		want[id] = true
	}
	var targets []cluster.NodeStatus
	var skipped []string
	for _, n := range s.Nodes.Status() {
		if len(want) > 0 && !want[n.ID] {
			continue
		}
		if n.State != "online" {
			skipped = append(skipped, nodeLabel(n)+" (offline)")
			continue
		}
		targets = append(targets, n)
	}
	if len(targets) == 0 {
		fail(w, badRequest("no online node to add the storage to"))
		return
	}
	sp := nfsSpec{req.Name, req.Server, req.Path}
	var results []nodeOutcome
	err := s.task(r, "add shared storage", req.Name, "datacenter", func(progress func(float64)) error {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, n := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var o nodeOutcome
				h, err := s.Nodes.Host(n.ID)
				if err != nil {
					o = nodeOutcome{Error: "is not connected: " + err.Error()}
				} else {
					o = addNFSToNode(h, sp)
				}
				o.Node, o.NodeName = n.ID, nodeLabel(n)
				mu.Lock()
				results = append(results, o)
				mu.Unlock()
			}()
		}
		wg.Wait()
		sort.Slice(results, func(i, j int) bool { return results[i].Node < results[j].Node })
		msg, complete := addSummary(req.Name, results)
		if !complete {
			return fmt.Errorf("%s", msg)
		}
		return nil
	})
	msg, complete := addSummary(req.Name, results)
	if len(results) == 0 { // the task could not start
		fail(w, err)
		return
	}
	if len(skipped) > 0 {
		msg += ". Offline, not added: " + strings.Join(skipped, ", ") + ". It is offered again when they are back"
	}
	_ = err // a partial result is reported in the body, not as an HTTP error
	s.Nodes.Publish(cluster.Event{Type: "pool"})
	writeJSON(w, nfsResult{Complete: complete, Message: msg, Nodes: results, Skipped: skipped})
}

type sharedView struct {
	sharedGroup
	Capacity   uint64 `json:"capacity"`
	Allocation uint64 `json:"allocation"`
	Available  uint64 `json:"available"`
	Detail     string `json:"detail,omitempty"`
}

func (s *Server) listShared(w http.ResponseWriter, r *http.Request) {
	res := gatherNodes(s, nodeTimeout, collectPools)
	pools, errs := mergeNodes(res, func(p poolInfo, id int64, name string) nodePool { return nodePool{p, id, name} })
	var nfs []nodeNFS
	byNode := map[string]nodePool{} // pool name -> a running member
	detail := map[string]string{}
	for _, p := range pools {
		if p.Type != "netfs" {
			continue
		}
		nfs = append(nfs, nodeNFS{Node: p.Node, Name: p.NodeName, Pool: p.Name, Server: p.Server, Export: p.Export, Target: p.Target, State: p.State, Autostart: p.Autostart})
		if _, ok := byNode[p.Name]; !ok && p.State == "running" {
			byNode[p.Name] = p
		}
		if p.State == "unavailable" {
			detail[p.Name] = p.Detail
		}
	}
	var all []sharedMember
	for _, n := range s.onlineNodes() {
		all = append(all, sharedMember{Node: n.ID, NodeName: nodeLabel(n)})
	}
	out := []sharedView{}
	for _, g := range groupShared(nfs, all) {
		v := sharedView{sharedGroup: g, Detail: detail[g.Name]}
		if p, ok := byNode[g.Name]; ok {
			v.Capacity, v.Allocation, v.Available = p.Capacity, p.Allocation, p.Available
		}
		out = append(out, v)
	}
	writeJSON(w, map[string]any{"shared": out, "errors": errs})
}

// refsDir reports whether a domain definition names a file below dir (a disk, an ISO, a snapshot overlay).
func refsDir(domXML, dir string) bool {
	d := strings.TrimRight(dir, "/") + "/"
	return strings.Contains(domXML, "'"+d) || strings.Contains(domXML, `"`+d)
}

// poolUsers names the VMs of a node (running or not) whose definition refers to a file below dir.
func poolUsers(h *hostconn.Host, dir string) ([]string, error) {
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range doms {
		x, err := h.DomainGetXMLDesc(d, libvirt.DomainXMLInactive)
		if err != nil {
			return nil, fmt.Errorf("reading VM %s: %w", d.Name, err)
		}
		if refsDir(x, dir) {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// removeRefusal is the sentence for a removal that is not allowed, from the VMs that use the storage per node.
func removeRefusal(name string, users map[string][]string) string {
	var parts []string
	var nodes []string
	for n := range users {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		parts = append(parts, strings.Join(users[n], ", ")+" on "+n)
	}
	return fmt.Sprintf("cannot remove %s: disks of %s are on it. Move those disks to other storage (the VM's Hardware tab) or delete the VMs first", name, strings.Join(parts, "; "))
}

// removeShared is DELETE /api/storage/shared/{name}: stop and forget the pool on every node. It is refused while
// a defined VM has a file on it, and while a node that might have such a VM cannot be asked.
func (s *Server) removeShared(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nfsNameRE.MatchString(name) {
		fail(w, badRequest("no such shared storage"))
		return
	}
	var offline []string
	type member struct {
		n cluster.NodeStatus
		h *hostconn.Host
		p libvirt.StoragePool
		x poolXML
	}
	var members []member
	for _, n := range s.Nodes.Status() {
		if n.State != "online" {
			offline = append(offline, nodeLabel(n))
			continue
		}
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			offline = append(offline, nodeLabel(n))
			continue
		}
		p, err := h.StoragePoolLookupByName(name)
		if err != nil {
			continue
		}
		var x poolXML
		if raw, err := h.StoragePoolGetXMLDesc(p, 0); err == nil {
			xml.Unmarshal([]byte(raw), &x)
		}
		if x.Type != "netfs" {
			fail(w, badRequest(name+" is not an NFS storage"))
			return
		}
		members = append(members, member{n, h, p, x})
	}
	if len(members) == 0 {
		fail(w, badRequest("no such shared storage"))
		return
	}
	if len(offline) > 0 {
		fail(w, badRequest("cannot remove "+name+" while "+strings.Join(offline, ", ")+" cannot be reached: a VM there might have disks on it. Bring the node back, or remove it from the datacenter first"))
		return
	}
	// Every online node is asked about every VM, not only the members: a VM may use the files through a
	// differently named pool or a plain path.
	users := map[string][]string{}
	for _, m := range members {
		vms, err := poolUsers(m.h, m.x.Target.Path)
		if err != nil {
			fail(w, err)
			return
		}
		if len(vms) > 0 {
			users[nodeLabel(m.n)] = vms
		}
	}
	if len(users) > 0 {
		fail(w, badRequest(removeRefusal(name, users)))
		return
	}
	var results []nodeOutcome
	var mu sync.Mutex
	err := s.task(r, "remove shared storage", name, "datacenter", func(func(float64)) error {
		var wg sync.WaitGroup
		for _, m := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o := nodeOutcome{Node: m.n.ID, NodeName: nodeLabel(m.n), OK: true}
				if act, _ := m.h.StoragePoolIsActive(m.p); act != 0 {
					if err := m.h.StoragePoolDestroy(m.p); err != nil {
						o.OK, o.Error = false, "could not unmount: "+firstLine(err.Error())+". Something on the node still uses the share (a VM, or a shell in "+m.x.Target.Path+")"
					}
				}
				if o.OK {
					if err := m.h.StoragePoolUndefine(m.p); err != nil {
						o.OK, o.Error = false, err.Error()
					}
				}
				mu.Lock()
				results = append(results, o)
				mu.Unlock()
			}()
		}
		wg.Wait()
		sort.Slice(results, func(i, j int) bool { return results[i].Node < results[j].Node })
		var bad []string
		for _, o := range results {
			if !o.OK {
				bad = append(bad, o.NodeName+": "+o.Error)
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("not removed on: %s", strings.Join(bad, "; "))
		}
		return nil
	})
	s.Nodes.Publish(cluster.Event{Type: "pool"})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "nodes": results})
}

var openDeniedRE = regexp.MustCompile(`(?:Could not open|Cannot access storage file|Failed to open file|Unable to read from file) '(/[^']+)'`)

// deniedOnNFS is the sentence for a start that failed with "permission denied" on a file of an NFS share.
func deniedOnNFS(node, file, share string, f nfsFacts) string {
	if f.SELinux == "enforcing" && f.VirtUseNFS == "off" {
		return fmt.Sprintf("%s cannot use %s: SELinux on %s blocks VMs from using NFS (virt_use_nfs is off). Run Prepare for the node again (Nodes), which turns it on", "the VM", file, node)
	}
	fix := "export it with no_root_squash for the nodes, or map every user to the VM user with all_squash and anonuid, anongid set to the VM user's id, and make the export directory owned by that id"
	if f.UID > 0 {
		fix = fmt.Sprintf("export it with no_root_squash for the nodes, or with all_squash,anonuid=%d,anongid=%d (the VM user's id) and make the export directory owned by %d:%d", f.UID, f.GID, f.UID, f.GID)
	}
	return fmt.Sprintf("The VM user on %s may not open %s on the NFS share %s. The usual cause is a server that turns root into an unprivileged user (root_squash), so the file does not belong to the VM user. On the server, %s", node, file, share, fix)
}

// nfsStartHint replaces a bare "permission denied" for a file on an NFS pool by what to change. Other errors
// pass through.
func nfsStartHint(h *hostconn.Host, err error) error {
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		return err
	}
	m := openDeniedRE.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	pools, perr := poolDescs(h)
	if perr != nil {
		return err
	}
	for _, p := range pools {
		if p.isNFS() && strings.HasPrefix(m[1], strings.TrimRight(p.Target, "/")+"/") {
			out, _ := h.Run(nfsFactsCmd)
			return badRequest(deniedOnNFS(h.Name, m[1], p.shareText(), parseNFSFacts(out)) + "\n\nlibvirt said: " + firstLine(err.Error()))
		}
	}
	return err
}

// nfsGuard refuses to start a VM whose disk is on an NFS share that does not answer: QEMU would block on the
// mount instead of failing.
func nfsGuard(h *hostconn.Host, domXML string) error {
	pools, err := poolsByPathOpt(h, false)
	if err != nil {
		return nil
	}
	for dir, p := range pools {
		if p.desc.isNFS() && p.down && refsDir(domXML, dir) {
			return badRequest(fmt.Sprintf("Not started: the storage %s (%s) does not answer from %s, so the VM's disk cannot be read. Check the NFS server and the network; the VM can be started when the storage is back", p.desc.Name, p.desc.shareText(), h.Name))
		}
	}
	return nil
}

// nfsPoolGuard refuses work on an NFS pool whose mount does not answer.
func nfsPoolGuard(h *hostconn.Host, p libvirt.StoragePool) error {
	raw, err := h.StoragePoolGetXMLDesc(p, 0)
	if err != nil {
		return nil
	}
	var x poolXML
	if xml.Unmarshal([]byte(raw), &x) != nil || x.Type != "netfs" || x.Target.Path == "" {
		return nil
	}
	if act, _ := h.StoragePoolIsActive(p); act == 0 {
		return nil
	}
	if st := nfsProber.probe(h, []string{x.Target.Path})[x.Target.Path]; st != mountUp {
		return badRequest(fmt.Sprintf("The storage %s (%s:%s) is unavailable on %s: %s", p.Name, x.Source.Host.Name, x.Source.Dir.Path, h.Name, unavailableText(x.Source.Host.Name, st)))
	}
	return nil
}

// lookupPool finds a pool by name for work on it; an NFS pool that does not answer is refused (nfsPoolGuard).
func lookupPool(h *hostconn.Host, name string) (libvirt.StoragePool, error) {
	p, err := h.StoragePoolLookupByName(name)
	if err != nil {
		return p, err
	}
	return p, nfsPoolGuard(h, p)
}
