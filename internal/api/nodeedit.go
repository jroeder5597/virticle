package api

// Changing how a node is reached: address, SSH user, jump host, how the user
// gets root, and the address other nodes use. The node keeps its id, so its
// maintenance flag and everything else keyed to it stay.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

const nodeTestTimeout = 8 * time.Second

// nodeEditReq holds the new settings; a missing field keeps the current value.
type nodeEditReq struct {
	Addr             *string `json:"addr"`
	User             *string `json:"user"`
	Jump             *string `json:"jump"`
	Privilege        *string `json:"privilege"` // root | sudo | none | auto (detect)
	MigrationAddress *string `json:"migrationAddress"`
	// Force saves without a successful trial connection, to fix a node that
	// is down.
	Force bool `json:"force"`
}

func trimPtr(p *string, cur string) string {
	if p == nil {
		return cur
	}
	return strings.TrimSpace(*p)
}

// mergeNodeEdit applies a request to the current record and validates the
// result. detect is true when the privilege has to be detected rather than
// taken from the request.
func mergeNodeEdit(cur store.Node, req nodeEditReq) (n store.Node, detect bool, err error) {
	n = cur
	n.Addr = trimPtr(req.Addr, cur.Addr)
	n.User = trimPtr(req.User, cur.User)
	n.Jump = trimPtr(req.Jump, cur.Jump)
	n.MigrationAddr = trimPtr(req.MigrationAddress, cur.MigrationAddr)
	if n.User == "" {
		n.User = "root"
	}
	if n.Addr == "" {
		return n, false, badRequest("address is required")
	}
	if h, _ := splitAddr(n.Addr); !validAddrHost(h) {
		return n, false, badRequest("the address must be a host name or an IP address, optionally with :port")
	}
	if !userRE.MatchString(n.User) {
		return n, false, badRequest("unsupported characters in the SSH user")
	}
	if n.Jump != "" {
		u, a, ok := strings.Cut(n.Jump, "@")
		if h, _ := splitAddr(a); !ok || !userRE.MatchString(u) || !validAddrHost(h) {
			return n, false, badRequest("the jump host must be user@address")
		}
	}
	if n.MigrationAddr != "" {
		if h, _ := splitAddr(n.MigrationAddr); !validAddrHost(h) {
			return n, false, badRequest("the migration address must be a host name or an IP address")
		}
	}
	connChanged := n.Addr != cur.Addr || n.User != cur.User || n.Jump != cur.Jump
	switch {
	case req.Privilege != nil && *req.Privilege != "auto" && *req.Privilege != "":
		if !hostconn.Privilege(*req.Privilege).Valid() {
			return n, false, badRequest("privilege must be root, helper, sudo, none or auto")
		}
		n.Privilege = *req.Privilege
	case req.Privilege != nil || connChanged || cur.Privilege == "":
		// "auto", or the login changed so the old detection no longer applies.
		detect = true
	}
	return n, detect, nil
}

type nodeEditResult struct {
	Node     cluster.NodeStatus  `json:"node"`
	Test     *cluster.TestResult `json:"test,omitempty"`
	Warnings []string            `json:"warnings,omitempty"`
	Notes    []string            `json:"notes,omitempty"`
}

func (s *Server) nodeID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		return 0, store.ErrNotFound
	}
	return id, nil
}

// runningTasksOn lists running tasks that involve a node, by name or address.
func (s *Server) runningTasksOn(n store.Node, hostname string) []string {
	tasks, err := s.Store.Tasks(200)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range tasks {
		if t.Status != "running" {
			continue
		}
		for _, name := range []string{n.Addr, hostname} {
			if name != "" && (t.Node == name || strings.Contains(t.Target, name)) {
				out = append(out, t.Kind+" "+t.Target)
				break
			}
		}
	}
	return out
}

// testNodeConnection tries the settings without saving anything.
func (s *Server) testNodeConnection(w http.ResponseWriter, r *http.Request) {
	id, err := s.nodeID(r)
	if err != nil {
		fail(w, err)
		return
	}
	cur, err := s.Store.GetNode(id)
	if err != nil {
		fail(w, err)
		return
	}
	var req nodeEditReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	n, _, err := mergeNodeEdit(cur, req)
	if err != nil {
		fail(w, err)
		return
	}
	res, err := s.Nodes.TestConnect(n, nodeTestTimeout)
	if len(res.Steps) == 0 {
		// Nothing was tried (the settings could not even be turned into a target).
		failConn(w, err, nil)
		return
	}
	// A failed test is an answer, not an error: the steps say what failed.
	out := nodeEditResult{Test: &res}
	if err == nil {
		out.Warnings = checkWarnings(cur, n, &res)
		out.Warnings = append(out.Warnings, s.cpuBaselineStep(id, &res)...)
	}
	writeJSON(w, out)
}

// checkWarnings compares a successful test with the saved node and the new
// settings. It returns the warnings and adds the matching steps: whether this
// is the same libvirt host as before, and a privilege setting above what the
// login really has.
func checkWarnings(cur, n store.Node, res *cluster.TestResult) []string {
	w := append(identityWarnings(cur, res), privilegeWarnings(n, res)...)
	var steps []cluster.TestStep
	for _, st := range res.Steps {
		if st.ID == "privilege" && len(privilegeWarnings(n, res)) > 0 {
			st.Status = cluster.StepWarning
			st.Detail = fmt.Sprintf("set to %s but the node reports: %s", n.Privilege, res.PrivilegeNote)
		}
		steps = append(steps, st)
		if st.ID == "libvirt" && cur.HostUUID != "" && res.HostUUID != "" {
			id := cluster.TestStep{ID: "identity", Title: "Same libvirt host as before", Status: cluster.StepOK, Detail: "host UUID " + res.HostUUID}
			if len(identityWarnings(cur, res)) > 0 {
				id.Status, id.Detail = cluster.StepWarning, fmt.Sprintf("host UUID is %s, the node was %s; VMs and storage seen here are those of the new machine", res.HostUUID, cur.HostUUID)
			}
			steps = append(steps, id)
		}
	}
	res.Steps = steps
	return w
}

func identityWarnings(cur store.Node, res *cluster.TestResult) []string {
	if res != nil && cur.HostUUID != "" && res.HostUUID != "" && !strings.EqualFold(cur.HostUUID, res.HostUUID) {
		return []string{fmt.Sprintf("This is not the same libvirt host as before: its host UUID is %s, the node was %s. If you pointed the node at a different machine, VMs and storage seen here are those of the new machine.", res.HostUUID, cur.HostUUID)}
	}
	return nil
}

func privilegeWarnings(n store.Node, res *cluster.TestResult) []string {
	if res == nil || n.Privilege == "" || n.Privilege == res.Privilege {
		return nil
	}
	rank := map[string]int{"none": 0, "helper": 1, "sudo": 2, "root": 3}
	if rank[n.Privilege] > rank[res.Privilege] {
		return []string{fmt.Sprintf("Privilege is set to %s but the node reports: %s. Commands that need root will fail.", n.Privilege, res.PrivilegeNote)}
	}
	return nil
}

// failConn reports a refused connection test as a 422 with the exact error.
func failConn(w http.ResponseWriter, err error, res *cluster.TestResult) {
	if _, ok := err.(badRequest); ok {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	json.NewEncoder(w).Encode(map[string]any{
		"error":      "connection test failed: " + err.Error(),
		"connection": true,
		"hostKey":    strings.Contains(err.Error(), "key mismatch"),
		"test":       res,
	})
}

func (s *Server) editNode(w http.ResponseWriter, r *http.Request) {
	id, err := s.nodeID(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req nodeEditReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	cur, err := s.Store.GetNode(id)
	if err != nil {
		fail(w, err)
		return
	}
	hostname := ""
	if st, ok := s.nodeStatus(id); ok {
		hostname = st.Hostname
	}
	if busy := s.runningTasksOn(cur, hostname); len(busy) > 0 {
		conflict(w, "not while a task is running on this node ("+strings.Join(busy, ", ")+"); wait for it or cancel it first")
		return
	}
	n, detect, err := mergeNodeEdit(cur, req)
	if err != nil {
		fail(w, err)
		return
	}
	out := nodeEditResult{}
	if !req.Force {
		res, err := s.Nodes.TestConnect(n, nodeTestTimeout)
		if err != nil {
			failConn(w, err, &res)
			return
		}
		if detect {
			n.Privilege = res.Privilege
		}
		if res.HostUUID != "" {
			n.HostUUID = res.HostUUID
		}
		out.Warnings = checkWarnings(cur, n, &res)
		out.Warnings = append(out.Warnings, s.cpuBaselineStep(id, &res)...)
		out.Test = &res
	} else if detect {
		n.Privilege = "" // detected on the first connection
		out.Notes = append(out.Notes, "Saved without a trial connection; the privilege is detected when the node connects.")
	}
	if n.Addr != cur.Addr {
		out.Notes = append(out.Notes, "The address changed, so the node's SSH host key is trusted on first use at the new address.")
	}
	if n.Addr != cur.Addr || n.MigrationAddr != cur.MigrationAddr {
		out.Notes = append(out.Notes, "The migration certificate of this node, if it has one, no longer covers the new address; issue it again on the node page (Migration tab).")
	}
	if err := s.Store.UpdateNodeConnection(id, n); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			err = badRequest("another node already uses this address")
		}
		fail(w, err)
		return
	}
	n.ID, n.Maintenance = id, cur.Maintenance
	s.Nodes.Replace(n)
	log.Printf("node %d edited by %s: %s", id, userOf(r), describeNodeChange(cur, n))
	out.Node, _ = s.nodeStatus(id)
	writeJSON(w, out)
}

func describeNodeChange(a, b store.Node) string {
	var c []string
	add := func(name, x, y string) {
		if x != y {
			c = append(c, fmt.Sprintf("%s %q -> %q", name, x, y))
		}
	}
	add("address", a.Addr, b.Addr)
	add("user", a.User, b.User)
	add("jump", a.Jump, b.Jump)
	add("privilege", a.Privilege, b.Privilege)
	add("migration address", a.MigrationAddr, b.MigrationAddr)
	if len(c) == 0 {
		return "no change"
	}
	return strings.Join(c, ", ")
}

// forgetHostKey drops the pinned SSH host key of a node (for a reinstalled
// machine) and reconnects, which trusts the new key on first use.
func (s *Server) forgetHostKey(w http.ResponseWriter, r *http.Request) {
	id, err := s.nodeID(r)
	if err != nil {
		fail(w, err)
		return
	}
	cur, err := s.Store.GetNode(id)
	if err != nil {
		fail(w, err)
		return
	}
	n, err := s.Nodes.ForgetHostKey(cur.Addr)
	if err != nil {
		fail(w, err)
		return
	}
	log.Printf("node %d (%s): host key forgotten by %s (%d entries)", id, cur.Addr, userOf(r), n)
	s.Nodes.Reconnect(id)
	writeJSON(w, map[string]any{"removed": n})
}
