package api

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// nodeView is a node as the UI lists it: its status plus what its privilege
// allows.
type nodeView struct {
	cluster.NodeStatus
	Capabilities map[string]capability `json:"capabilities"`
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	st := s.Nodes.Status()
	views := make([]nodeView, len(st))
	for i, n := range st {
		views[i] = nodeView{n, nodeCapabilities(n.Privilege)}
	}
	writeJSON(w, map[string]any{"nodes": views, "publicKey": s.PubKey})
}

func (s *Server) addNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		store.Node
		// TakeOver is set when the node was just taken over from another controller (see prepare.Options.TakeOver):
		// the other controller's migration certificates are then replaced without asking again.
		TakeOver bool `json:"takeOver"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, err)
		return
	}
	n := body.Node
	n.Addr, n.User, n.Jump = strings.TrimSpace(n.Addr), strings.TrimSpace(n.User), strings.TrimSpace(n.Jump)
	n.MigrationAddr = strings.TrimSpace(n.MigrationAddr)
	if n.MigrationAddr != "" {
		if h, _ := splitAddr(n.MigrationAddr); !validAddrHost(h) {
			fail(w, badRequest("the migration address must be a host name or an IP address"))
			return
		}
	}
	if n.Privilege == "auto" {
		n.Privilege = ""
	}
	if n.Privilege != "" && !hostconn.Privilege(n.Privilege).Valid() {
		fail(w, badRequest(accessModeMessage))
		return
	}
	if n.Addr == "" {
		fail(w, badRequest("address is required"))
		return
	}
	if n.User == "" {
		n.User = "root"
	}
	if _, err := s.Nodes.Target(n); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	n, err := s.Store.AddNode(n)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			err = badRequest("a node with this address already exists")
		}
		fail(w, err)
		return
	}
	if body.TakeOver {
		s.markTakeOver(n.ID)
	}
	s.Nodes.Add(n)
	writeJSON(w, n)
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return
	}
	s.dropPeerTrust(id) // best effort: the node's key and host key on the other nodes
	if err := s.Store.DeleteNode(id); err != nil {
		fail(w, err)
		return
	}
	s.Nodes.Remove(id)
	s.Nodes.Publish(cluster.Event{Type: "node", Node: id, Detail: "removed"})
	writeJSON(w, map[string]bool{"ok": true})
}

type nodeInfo struct {
	// How the node is reached, and what its login user may do.
	Addr              string                `json:"addr"`
	User              string                `json:"user"`
	Jump              string                `json:"jump,omitempty"`
	Privilege         string                `json:"privilege"`
	DetectedPrivilege string                `json:"detectedPrivilege,omitempty"`
	PrivilegeNote     string                `json:"privilegeNote,omitempty"`
	MigrationAddress  string                `json:"migrationAddress"`
	EffectiveAddress  string                `json:"effectiveMigrationAddress"` // what other nodes use
	ResolvedIPs       []string              `json:"resolvedIps,omitempty"`
	HostUUID          string                `json:"hostUuid,omitempty"`
	Capabilities      map[string]capability `json:"capabilities"`
	Online            bool                  `json:"online"`

	Hostname   string `json:"hostname"`
	CPUModel   string `json:"cpuModel"`
	CPUs       int32  `json:"cpus"`
	MemoryKiB  uint64 `json:"memoryKiB"`
	FreeKiB    uint64 `json:"freeKiB"`
	LibvirtVer string `json:"libvirtVersion"`
}

func (s *Server) nodeInfo(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	st, known := s.nodeStatus(id)
	if !known {
		fail(w, store.ErrNotFound)
		return
	}
	mh, _ := migrationTarget(st.Node)
	base := nodeInfo{
		Addr: st.Addr, User: st.User, Jump: st.Jump, Privilege: st.Privilege, DetectedPrivilege: st.DetectedPrivilege,
		PrivilegeNote: st.PrivilegeNote, MigrationAddress: st.MigrationAddr, EffectiveAddress: mh, HostUUID: st.HostUUID,
		ResolvedIPs: cluster.ResolveAddr(st.Addr), Capabilities: nodeCapabilities(st.Privilege), Online: err == nil, Hostname: st.Hostname,
	}
	if err != nil {
		writeJSON(w, base) // offline: only the connection settings
		return
	}
	model, mem, cpus, _, _, _, _, _, err := h.NodeGetInfo()
	if err != nil {
		fail(w, err)
		return
	}
	free, _ := h.NodeGetFreeMemory()
	ver, _ := h.ConnectGetLibVersion()
	mb := make([]byte, len(model))
	for i, c := range model {
		mb[i] = byte(c)
	}
	base.Hostname, base.CPUModel, base.CPUs = h.Name, string(bytes.TrimRight(mb, "\x00")), cpus
	base.MemoryKiB, base.FreeKiB = mem, free/1024
	base.LibvirtVer = strconv.FormatUint(ver/1000000, 10) + "." + strconv.FormatUint(ver/1000%1000, 10) + "." + strconv.FormatUint(ver%1000, 10)
	writeJSON(w, base)
}
