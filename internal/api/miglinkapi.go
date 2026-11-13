package api

// Reading and changing the migration network and the limit of a node:
//
//	GET /api/nodes/{node}/migration-network   the choices and the current setting
//	PUT /api/nodes/{node}/migration-network   {"address": "", "limit": ""} change it (administrator)
//
// The edit dialog of a node goes through applyMigrationNetwork as well when only these two fields change.

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/migpki"
	"virticle/internal/store"
)

type migNetReq struct {
	// Address is the address other nodes send migration data to; empty: the management address.
	Address *string `json:"address"`
	// Limit is "", "none" or "custom:<Mbit/s>" (see limitSetting).
	Limit *string `json:"limit"`
}

type migNetResult struct {
	migNetInfo
	Warnings []string `json:"warnings,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

func (s *Server) migNetGet(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, s.migNetInfoOf(id, cur))
}

// migNetInfoOf describes a node; the interface list comes from the node when it is online.
func (s *Server) migNetInfoOf(id int64, n store.Node) migNetInfo {
	var nics []hostNIC
	if h, err := s.Nodes.Host(id); err == nil {
		nics, _ = hostNICList(h)
	}
	return describeMigNet(n, nics)
}

func (s *Server) migNetPut(w http.ResponseWriter, r *http.Request) {
	id, err := s.nodeID(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req migNetReq
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
	addr, limit := cur.MigrationAddr, cur.MigrationLimit
	if req.Address != nil {
		addr = *req.Address
	}
	if req.Limit != nil {
		limit = *req.Limit
	}
	res, err := s.applyMigrationNetwork(id, cur, addr, limit, userOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// normalizeMigAddr checks an address and turns the management address into the empty setting.
func normalizeMigAddr(cur store.Node, addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil
	}
	h, _ := splitAddr(addr)
	if !validAddrHost(h) {
		return "", badRequest("the migration network must be an IP address or host name of the node")
	}
	mgmtHost, _ := splitAddr(cur.Addr)
	if strings.EqualFold(h, mgmtHost) {
		return "", nil
	}
	return addr, nil
}

// applyMigrationNetwork stores the migration network and limit of a node, checks that the other nodes can reach
// the new address, and renews the node's migration certificate at once when it does not cover it. Problems with
// the other nodes are warnings: the setting is saved either way.
func (s *Server) applyMigrationNetwork(id int64, cur store.Node, addr, limit, who string) (migNetResult, error) {
	addr, err := normalizeMigAddr(cur, addr)
	if err != nil {
		return migNetResult{}, err
	}
	l, err := parseLimit(limit)
	if err != nil {
		return migNetResult{}, err
	}
	limit = l.String()
	var res migNetResult
	addrChanged := addr != cur.MigrationAddr
	if !addrChanged && limit == cur.MigrationLimit {
		res.migNetInfo = s.migNetInfoOf(id, cur)
		return res, nil
	}
	if addrChanged {
		res.Warnings = append(res.Warnings, s.checkMigrationReach(id, cur, addr)...)
	}
	if err := s.Store.SetNodeMigration(id, addr, limit); err != nil {
		return migNetResult{}, err
	}
	s.Nodes.SetMigration(id, addr, limit)
	next := cur
	next.MigrationAddr, next.MigrationLimit = addr, limit
	if addrChanged {
		s.forgetPeerHostKeys(id, cur)
		note, warn := s.renewMigCert(id)
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}
	s.Nodes.Publish(cluster.Event{Type: "node", Node: id, Detail: "migration-network"})
	log.Printf("node %d: %s", id, "migration network changed by "+who+": "+describeMigChange(cur, next))
	res.migNetInfo = s.migNetInfoOf(id, next)
	return res, nil
}

func describeMigChange(a, b store.Node) string {
	var c []string
	if a.MigrationAddr != b.MigrationAddr {
		c = append(c, fmt.Sprintf("address %q -> %q", a.MigrationAddr, b.MigrationAddr))
	}
	if a.MigrationLimit != b.MigrationLimit {
		c = append(c, fmt.Sprintf("limit %q -> %q", a.MigrationLimit, b.MigrationLimit))
	}
	return strings.Join(c, ", ")
}

// reachState says what a TCP test from h to host:port found.
func reachState(h *hostconn.Host, host, port string) string {
	out, err := h.Run(reachCmd(host, port))
	switch {
	case err == nil:
		return "open"
	case strings.Contains(err.Error(), "124"):
		return "silent" // no answer at all: typically a firewall dropping the packets
	case strings.Contains(out, "unreachable") || strings.Contains(out, "No route"):
		return "unreachable"
	}
	return "closed" // refused: the host answered, nothing listens (yet)
}

// checkMigrationReach tests, from every other node, that the new address of node id can be reached: on the
// migration ports (49152-49215, tested with 49152) and on the SSH port that file copies use.
func (s *Server) checkMigrationReach(id int64, cur store.Node, addr string) []string {
	host, port := migrationTarget(store.Node{Addr: cur.Addr, MigrationAddr: addr})
	if port == "" {
		port = "22"
	}
	name := host
	if st, ok := s.nodeStatus(id); ok {
		name = nodeLabel(st)
	}
	var (
		mu  sync.Mutex
		out []string
		wg  sync.WaitGroup
	)
	add := func(m string) { mu.Lock(); out = append(out, m); mu.Unlock() }
	for _, n := range s.Nodes.Status() {
		if n.ID == id {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			label := nodeLabel(n)
			h, err := s.Nodes.Host(n.ID)
			if err != nil {
				add(fmt.Sprintf("%s is offline, so it could not check that it can reach %s at %s.", label, name, host))
				return
			}
			switch reachState(h, host, "49152") {
			case "silent", "unreachable":
				add(fmt.Sprintf("%s cannot reach %s at %s on the migration ports (TCP 49152-49215). Check the route and the firewall of %s.", label, name, host, name))
			}
			if reachState(h, host, port) != "open" {
				add(fmt.Sprintf("%s cannot open SSH (TCP %s) on %s at %s. Files (offline migration, backups) are copied over SSH on this network, so those fail until it is allowed.", label, port, name, host))
			}
		}()
	}
	wg.Wait()
	return out
}

// forgetPeerHostKeys removes, on the other nodes, the SSH host key recorded for the address the node had.
// The entry for the new address is made by the next pre-flight (ensurePeerTrust). Best effort.
func (s *Server) forgetPeerHostKeys(id int64, old store.Node) {
	ep, err := endpointFor(old)
	if err != nil {
		return
	}
	for _, n := range s.Nodes.Status() {
		if n.ID == id {
			continue
		}
		if o, err := s.Nodes.Host(n.ID); err == nil && hasNodeCommand(o) {
			if err := o.PeerUnsetHostKey(ep.Host, ep.port()); err != nil {
				log.Printf("forgetting the host key of %s on %s: %v", ep.Host, o.Name, err)
			}
		}
	}
}

// renewMigCert makes the node's migration certificate cover the address it now has, at once, when encrypted
// migration is set up on the node. It returns a sentence for the result: a note when all is well, a warning
// when the certificate could not be renewed.
func (s *Server) renewMigCert(id int64) (note, warn string) {
	if s.migAuto == nil {
		return "", ""
	}
	cfg, ok := s.Nodes.Config(id)
	if !ok || cfg.MigTLSOff {
		return "", ""
	}
	h, err := s.Nodes.Host(id)
	if err != nil {
		s.migAuto.Reset(id)
		return "", "The node is offline, so its migration certificate was not renewed; it is renewed automatically when the node is back."
	}
	if !h.CanRoot() {
		return "", ""
	}
	info, _, _, err := s.readMigTLS(h)
	if err != nil {
		return "", "The migration certificate of the node could not be checked: " + firstLine(err.Error())
	}
	switch {
	case info.State == migpki.StateNotSetUp:
		return "", "" // encrypted migration is not set up on this node: nothing to renew
	case info.State == migpki.StateOK:
		return "The node's migration certificate already covers the new address.", ""
	}
	if e := renewNow(s.migAuto, id); e != "" {
		return "", "The migration certificate could not be renewed for the new address: " + e + " It is tried again automatically."
	}
	info, _, _, err = s.readMigTLS(h)
	switch {
	case err != nil:
		return "", "The migration certificate was renewed, but reading it back failed: " + firstLine(err.Error())
	case info.State != migpki.StateOK:
		return "", "The migration certificate was renewed but is not valid yet (" + strings.Join(info.Problems, "; ") + ")."
	}
	return "The node's migration certificate was renewed for the new address.", ""
}

// renewNow runs the automatic certificate upkeep for a node at once, whatever its retry interval says, and
// returns the failure ("" when it worked or had nothing to do).
func renewNow(a *migTLSAuto, id int64) string {
	a.Reset(id)
	a.Try(id)
	return a.Error(id)
}
