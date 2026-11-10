package api

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	"virt-proj/internal/store"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"nodes": s.Nodes.Status(), "publicKey": s.PubKey})
}

func (s *Server) addNode(w http.ResponseWriter, r *http.Request) {
	var n store.Node
	if err := readJSON(r, &n); err != nil {
		fail(w, err)
		return
	}
	n.Addr, n.User, n.Jump = strings.TrimSpace(n.Addr), strings.TrimSpace(n.User), strings.TrimSpace(n.Jump)
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
	s.Nodes.Add(n)
	writeJSON(w, n)
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return
	}
	if err := s.Store.DeleteNode(id); err != nil {
		fail(w, err)
		return
	}
	s.Nodes.Remove(id)
	writeJSON(w, map[string]bool{"ok": true})
}

type nodeInfo struct {
	Hostname   string `json:"hostname"`
	CPUModel   string `json:"cpuModel"`
	CPUs       int32  `json:"cpus"`
	MemoryKiB  uint64 `json:"memoryKiB"`
	FreeKiB    uint64 `json:"freeKiB"`
	LibvirtVer string `json:"libvirtVersion"`
}

func (s *Server) nodeInfo(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
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
	writeJSON(w, nodeInfo{
		Hostname: h.Name, CPUModel: string(bytes.TrimRight(mb, "\x00")), CPUs: cpus,
		MemoryKiB: mem, FreeKiB: free / 1024,
		LibvirtVer: strconv.FormatUint(ver/1000000, 10) + "." + strconv.FormatUint(ver/1000%1000, 10) + "." + strconv.FormatUint(ver%1000, 10),
	})
}
