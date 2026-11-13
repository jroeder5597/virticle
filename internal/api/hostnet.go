package api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/nodeproto"
)

// Host bridges: Linux bridges, optionally VLAN-aware, created and deleted on a
// node with a rollback the node itself owns (the node command's net-* verbs,
// internal/nodecmd). This file is the controller side: what the UI shows, the
// checks that need libvirt, and the task that applies a change, waits for the
// node to come back and confirms.
//
//	GET  /api/nodes/{node}/hostnet           state of the node's network and its bridges
//	POST /api/nodes/{node}/hostnet/plan      what a change would do
//	POST /api/nodes/{node}/hostnet/apply     the change, as a task
//	POST /api/nodes/{node}/hostnet/rollback  roll back the pending change by hand

const (
	// hostnetGrace is how long after the node's own timer the task keeps waiting, so that a node that rolled
	// itself back can be seen to have done so.
	hostnetGrace      = 45 * time.Second
	hostnetPoll       = 2 * time.Second
	hostnetCallLimit  = 15 * time.Second
	hostnetNudgeEvery = 10 * time.Second

	// hostnetWaiting is the task progress once the node has started the change and the task waits for it to come back.
	hostnetWaiting = 0.2

	// MsgNodeDidNotReturn is what the task says when the node does not answer again.
	MsgNodeDidNotReturn = "The node did not come back in time. It restores its previous network settings by itself; check again in a minute."
)

// hostnetBody is the request body of plan and apply.
type hostnetBody struct {
	Action         string `json:"action"` // create | delete | set-vlans
	Bridge         string `json:"bridge"`
	Uplink         string `json:"uplink"`
	MoveAddresses  bool   `json:"moveAddresses"`
	VLANAware      bool   `json:"vlanAware"`
	VLANs          string `json:"vlans"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type hostnetBridge struct {
	nodeproto.NetBridge
	Networks []string `json:"networks"` // libvirt networks that use the bridge
	VMs      []string `json:"vms"`      // VMs with an interface on the bridge itself
	// ReadOnly says why the bridge cannot be changed ("" when it can).
	ReadOnly string `json:"readOnly,omitempty"`
}

type hostnetState struct {
	Stack               string              `json:"stack"`
	StackTested         bool                `json:"stackTested"`
	Supported           bool                `json:"supported"`
	StackNote           string              `json:"stackNote,omitempty"`
	LibvirtVersion      string              `json:"libvirtVersion"`
	VLANCapable         bool                `json:"vlanCapable"` // libvirt 11.0.0 or newer
	ControllerAddresses []string            `json:"controllerAddresses"`
	ManagementInterface string              `json:"managementInterface,omitempty"`
	Interfaces          []hostNIC           `json:"interfaces"`
	Bridges             []hostnetBridge     `json:"bridges"`
	Status              nodeproto.NetStatus `json:"status"`
	// NextBridgeName is the first free name of the form vsw0, vsw1 ... for a new virtual switch.
	NextBridgeName string `json:"nextBridgeName"`
}

// nextSwitchName returns the first of vsw0, vsw1 ... that no interface of the node already uses.
func nextSwitchName(taken []string) string {
	used := map[string]bool{}
	for _, t := range taken {
		used[t] = true
	}
	for i := 0; ; i++ {
		if n := "vsw" + strconv.Itoa(i); !used[n] {
			return n
		}
	}
}

func libvirtVer(v uint64) string { return fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000) }

// controllerAddrs are the addresses the controller uses for the node itself.
func (s *Server) controllerAddrs(id int64) []string {
	cfg, ok := s.Nodes.Config(id)
	if !ok {
		return nil
	}
	return cluster.ResolveAddr(cfg.Addr)
}

// managementInterface is the interface that holds one of addrs.
func managementInterface(nics []hostNIC, addrs []string) string {
	for _, n := range nics {
		for _, a := range n.Addresses {
			ip, _, _ := strings.Cut(a, "/")
			for _, c := range addrs {
				if ip == c {
					return n.Name
				}
			}
		}
	}
	return ""
}

// bridgeUsers returns the libvirt networks and the VMs that use a bridge of the node.
func bridgeUsers(h *hostconn.Host) (nets map[string][]string, vms map[string][]string, err error) {
	nets, vms = map[string][]string{}, map[string][]string{}
	list, err := collectNetworks(h)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range list {
		if n.Bridge != "" {
			nets[n.Bridge] = append(nets[n.Bridge], n.Name)
		}
	}
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range doms {
		raw, err := h.DomainGetXMLDesc(d, libvirt.DomainXMLInactive)
		if err != nil {
			continue
		}
		for _, b := range domainBridges(raw) {
			vms[b] = append(vms[b], d.Name)
		}
	}
	return nets, vms, nil
}

// domainBridges lists the host bridges the interfaces of a domain are plugged into directly.
func domainBridges(raw string) []string {
	var x struct {
		Ifaces []struct {
			Type   string `xml:"type,attr"`
			Source struct {
				Bridge string `xml:"bridge,attr"`
			} `xml:"source"`
		} `xml:"devices>interface"`
	}
	if xml.Unmarshal([]byte(raw), &x) != nil {
		return nil
	}
	var r []string
	for _, i := range x.Ifaces {
		if i.Type == "bridge" && i.Source.Bridge != "" {
			r = append(r, i.Source.Bridge)
		}
	}
	return r
}

func (s *Server) hostnetState(id int64, h *hostconn.Host) (hostnetState, error) {
	info, err := h.NetInfo()
	if err != nil {
		return hostnetState{}, err
	}
	nics, err := hostNICList(h)
	if err != nil {
		return hostnetState{}, err
	}
	st := hostnetState{Stack: info.Stack, StackTested: info.StackTested, Supported: info.Supported, StackNote: info.StackNote,
		Interfaces: nics, Status: info.Status, ControllerAddresses: s.controllerAddrs(id), Bridges: []hostnetBridge{}}
	ver, _ := h.ConnectGetLibVersion()
	st.LibvirtVersion, st.VLANCapable = libvirtVer(ver), uint64(ver) >= nodeproto.LibvirtVLANMin
	st.ManagementInterface = managementInterface(nics, st.ControllerAddresses)
	nets, vms, uerr := bridgeUsers(h)
	if uerr != nil {
		return hostnetState{}, uerr
	}
	// Bridges the NIC list shows that the node command left out are not Linux bridges (Open vSwitch): shown read-only.
	seen := map[string]bool{}
	for _, b := range info.Bridges {
		seen[b.Name] = true
		hb := hostnetBridge{NetBridge: b, Networks: nets[b.Name], VMs: vms[b.Name]}
		if hb.Networks == nil {
			hb.Networks = []string{}
		}
		if hb.VMs == nil {
			hb.VMs = []string{}
		}
		switch {
		case !b.Managed && strings.HasPrefix(b.Name, "virbr"):
			hb.ReadOnly = "managed by libvirt"
		case !b.Managed:
			hb.ReadOnly = "not created by virticle"
		}
		st.Bridges = append(st.Bridges, hb)
	}
	for _, n := range nics {
		if isBridgeDevice(n) && !seen[n.Name] {
			st.Bridges = append(st.Bridges, hostnetBridge{NetBridge: nodeproto.NetBridge{Name: n.Name, Ports: []string{}}, Networks: nets[n.Name],
				VMs: vms[n.Name], ReadOnly: "not a Linux bridge"})
		}
	}
	sort.Slice(st.Bridges, func(i, j int) bool { return st.Bridges[i].Name < st.Bridges[j].Name })
	taken := []string{}
	for _, n := range nics {
		taken = append(taken, n.Name)
	}
	for _, b := range st.Bridges {
		taken = append(taken, b.Name)
	}
	st.NextBridgeName = nextSwitchName(taken)
	return st, nil
}

func (s *Server) hostnetGet(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	st, err := s.hostnetState(id, h)
	if err != nil {
		fail(w, hostnetErr(err))
		return
	}
	writeJSON(w, st)
}

// hostnetErr gives the "prepare again" hint for a node whose node command is missing or old.
func hostnetErr(err error) error {
	if errors.Is(err, hostconn.ErrNoPrivilege) {
		return badRequest("This node cannot change its network from here: its node command is missing or older than this controller. Run `virticle node prepare` for it again.")
	}
	return err
}

// hostnetRequest turns the body into the node's request and runs the checks that need libvirt.
func (s *Server) hostnetRequest(id int64, h *hostconn.Host, b hostnetBody) (nodeproto.NetRequest, error) {
	req := nodeproto.NetRequest{Action: b.Action, Bridge: b.Bridge, Uplink: b.Uplink, MoveAddresses: b.MoveAddresses, VLANAware: b.VLANAware,
		VLANs: b.VLANs, TimeoutSeconds: b.TimeoutSeconds, ControllerAddrs: s.controllerAddrs(id)}
	ver, err := h.ConnectGetLibVersion()
	if err != nil {
		return req, err
	}
	req.LibvirtVersion = uint64(ver)
	if err := checkHostnetRequest(req); err != nil {
		return req, err
	}
	if req.Action == nodeproto.NetDelete {
		nets, vms, err := bridgeUsers(h)
		if err != nil {
			return req, err
		}
		if err := bridgeInUse(req.Bridge, nets[req.Bridge], vms[req.Bridge]); err != nil {
			return req, err
		}
	}
	if req.Action == nodeproto.NetSetVLAN {
		list, err := collectNetworks(h)
		if err != nil {
			return req, err
		}
		if err := vlanStillUsed(req, list); err != nil {
			return req, err
		}
	}
	return req, nil
}

// vlanStillUsed refuses a VLAN change that would cut the tag of a libvirt network on the bridge.
func vlanStillUsed(req nodeproto.NetRequest, nets []networkInfo) error {
	canon, err := nodeproto.ParseVLANs(req.VLANs)
	if err != nil {
		return nil // the node reports the bad list
	}
	for _, n := range nets {
		if n.Bridge != req.Bridge || n.VLAN == 0 {
			continue
		}
		if !req.VLANAware {
			return badRequest(fmt.Sprintf("the libvirt network %s uses VLAN %d on the bridge %s; delete or change it first", n.Name, n.VLAN, req.Bridge))
		}
		if !nodeproto.VLANAllowed(canon, n.VLAN) {
			return badRequest(fmt.Sprintf("the libvirt network %s uses VLAN %d on the bridge %s, which the new list (%s) would not allow; delete or change the network first", n.Name, n.VLAN, req.Bridge, canon))
		}
	}
	return nil
}

// checkHostnetRequest is the part of the checks that needs no node.
func checkHostnetRequest(req nodeproto.NetRequest) error {
	switch req.Action {
	case nodeproto.NetCreate, nodeproto.NetDelete, nodeproto.NetSetVLAN:
	default:
		return badRequest("action must be create, delete or set-vlans")
	}
	if req.Bridge == "" {
		return badRequest("the bridge name is required")
	}
	if req.VLANAware && req.Action != nodeproto.NetDelete && req.LibvirtVersion < nodeproto.LibvirtVLANMin {
		return badRequest("VLANs on a Linux bridge need libvirt 11.0.0 or newer; this node has " + libvirtVer(req.LibvirtVersion))
	}
	if req.Action == nodeproto.NetSetVLAN && !req.VLANAware && req.VLANs != "" {
		return badRequest("VLAN ids need a VLAN-aware bridge")
	}
	return nil
}

// bridgeInUse refuses to delete a bridge that libvirt networks or VMs use.
func bridgeInUse(bridge string, nets, vms []string) error {
	if len(nets) > 0 {
		return badRequest(fmt.Sprintf("the libvirt network %s uses the bridge %s; delete or change it first", strings.Join(nets, ", "), bridge))
	}
	if len(vms) > 0 {
		return badRequest(fmt.Sprintf("the VM %s is connected to the bridge %s; move its interface elsewhere first", strings.Join(vms, ", "), bridge))
	}
	return nil
}

func (s *Server) hostnetPlan(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var b hostnetBody
	if err := readJSON(r, &b); err != nil {
		fail(w, err)
		return
	}
	req, err := s.hostnetRequest(id, h, b)
	if err != nil {
		fail(w, err)
		return
	}
	p, err := h.NetPlan(req)
	if err != nil {
		fail(w, hostnetNodeRefusal(err))
		return
	}
	writeJSON(w, p)
}

// hostnetNodeRefusal turns a refusal of the node command (its first line) into a 400 with that text.
func hostnetNodeRefusal(err error) error {
	if errors.Is(err, hostconn.ErrNoPrivilege) {
		return hostnetErr(err)
	}
	return badRequest(strings.TrimPrefix(err.Error(), "virticle-node: "))
}

func (s *Server) hostnetApply(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var b hostnetBody
	if err := readJSON(r, &b); err != nil {
		fail(w, err)
		return
	}
	req, err := s.hostnetRequest(id, h, b)
	if err != nil {
		fail(w, err)
		return
	}
	node := h.Name
	err = s.taskAsync(r, "host network", req.Bridge, node, func(progress func(float64)) error {
		return s.hostnetRun(id, h, req, progress)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"bridge": req.Bridge})
}

func (s *Server) hostnetRollback(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &b); err != nil {
		fail(w, err)
		return
	}
	if b.ID == "" {
		fail(w, badRequest("the id of the change is required"))
		return
	}
	if err := h.NetRollback(b.ID); err != nil {
		fail(w, hostnetNodeRefusal(err))
		return
	}
	st, err := h.NetStatus()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, st)
}

// hostnetRun is the task: apply, wait for the node to come back, confirm.
func (s *Server) hostnetRun(id int64, h *hostconn.Host, req nodeproto.NetRequest, progress func(float64)) error {
	res, err := h.NetApply(req)
	if err != nil {
		return hostnetNodeRefusal(err)
	}
	// The change is saved and started on the node: from here on the task is waiting for the node (the UI shows this step).
	progress(hostnetWaiting)
	w := netWaiter{
		node: func() (netNode, error) {
			cur, err := s.Nodes.Host(id)
			if err != nil {
				return nil, err
			}
			return hostNode{cur}, nil
		},
		reconnect: func() { s.Nodes.Reconnect(id) },
		now:       time.Now, sleep: time.Sleep,
	}
	deadline := time.Now().Add(time.Duration(res.TimeoutSeconds)*time.Second + hostnetGrace)
	return w.wait(res.ID, deadline)
}

// --- waiting for the node ---

// netNode is what the wait needs from a node connection.
type netNode interface {
	NetStatus() (nodeproto.NetStatus, error)
	NetConfirm(id string) error
	LibvirtAnswers() error
}

type hostNode struct{ h *hostconn.Host }

func (n hostNode) NetStatus() (nodeproto.NetStatus, error) { return n.h.NetStatus() }
func (n hostNode) NetConfirm(id string) error              { return n.h.NetConfirm(id) }
func (n hostNode) LibvirtAnswers() error {
	_, err := n.h.ConnectGetLibVersion()
	return err
}

type netWaiter struct {
	node      func() (netNode, error) // the node's connection as it is now
	reconnect func()                  // drop the connection and make a new one
	now       func() time.Time
	sleep     func(time.Duration)
}

// withLimit runs f and gives up waiting for it after hostnetCallLimit: a dead SSH connection may not answer
// for a long time.
func withLimit[T any](f func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	c := make(chan res, 1)
	go func() { v, err := f(); c <- res{v, err} }()
	ctx, cancel := context.WithTimeout(context.Background(), hostnetCallLimit)
	defer cancel()
	select {
	case r := <-c:
		return r.v, r.err
	case <-ctx.Done():
		var z T
		return z, errors.New("no answer")
	}
}

// rollbackWords explains in plain words why the node restored its settings.
func rollbackWords(c nodeproto.NetChange) string {
	switch c.Reason {
	case "timer":
		return "The node rolled the change back by itself because it was not confirmed in time. Its previous network settings are active again."
	case "apply failed":
		return "Applying the change failed on the node, and it restored its previous network settings. " + c.Message
	case "check failed":
		return "The new network settings did not work on the node (no address), and it restored its previous network settings. " + c.Message
	}
	return "The change was rolled back on the node. Its previous network settings are active again. " + c.Message
}

// wait follows change id until it is confirmed, rolled back, or the deadline passes.
func (w netWaiter) wait(id string, deadline time.Time) error {
	var lastNudge time.Time
	for w.now().Before(deadline) {
		broken := false
		n, err := w.node()
		if err != nil {
			broken = true
		} else {
			st, err := withLimit(n.NetStatus)
			if err != nil {
				broken = true
			} else if ch, ok := findChange(st, id); ok {
				switch ch.State {
				case nodeproto.NetConfirmed:
					return nil
				case nodeproto.NetRolledBack, nodeproto.NetFailed:
					return errors.New(strings.TrimSpace(rollbackWords(ch)))
				case nodeproto.NetApplied:
					// The node is back: libvirt must answer over the new connection before the change is kept.
					if _, err := withLimit(func() (struct{}, error) { return struct{}{}, n.LibvirtAnswers() }); err == nil {
						if _, err := withLimit(func() (struct{}, error) { return struct{}{}, n.NetConfirm(id) }); err == nil {
							return nil
						}
					}
				}
			}
		}
		if broken && w.now().Sub(lastNudge) >= hostnetNudgeEvery {
			lastNudge = w.now()
			w.reconnect()
		}
		w.sleep(hostnetPoll)
	}
	return errors.New(MsgNodeDidNotReturn)
}

func findChange(st nodeproto.NetStatus, id string) (nodeproto.NetChange, bool) {
	for _, c := range st.Recent {
		if c.ID == id {
			return c, true
		}
	}
	return nodeproto.NetChange{}, false
}

// --- libvirt network on a VLAN-aware Linux bridge ---

// linuxVLAN is what is needed to decide whether a libvirt network may carry a VLAN tag on a Linux bridge.
type linuxVLAN struct {
	LibvirtVersion uint64
	Found          bool   // the bridge is a Linux bridge of the node
	Aware          bool   // VLAN filtering is on
	VLANs          string // the ids its uplink allows
}

// linuxVLANFor reads the facts for the bridge from the node.
func linuxVLANFor(h *hostconn.Host, bridge string) *linuxVLAN {
	v := &linuxVLAN{}
	if ver, err := h.ConnectGetLibVersion(); err == nil {
		v.LibvirtVersion = uint64(ver)
	}
	if info, err := h.NetInfo(); err == nil {
		for _, b := range info.Bridges {
			if b.Name == bridge {
				v.Found, v.Aware, v.VLANs = true, b.VLANFiltering, b.VLANs
			}
		}
	}
	return v
}

// vlanXML checks a tag against the bridge and returns the <vlan> element for a libvirt network.
func (v *linuxVLAN) vlanXML(bridge string, tag int) (string, error) {
	if tag < 1 || tag > 4094 {
		return "", badRequest("VLAN tag must be between 1 and 4094")
	}
	switch {
	case v == nil || !v.Found:
		return "", badRequest("cannot tell whether " + bridge + " is a VLAN-aware Linux bridge of this node; the node's network state is not available (is it prepared?)")
	case v.LibvirtVersion < nodeproto.LibvirtVLANMin:
		return "", badRequest("a VLAN tag on a Linux bridge needs libvirt 11.0.0 or newer; this node has " + libvirtVer(v.LibvirtVersion))
	case !v.Aware:
		return "", badRequest("the bridge " + bridge + " is not VLAN-aware; turn on VLAN filtering for it first (host network)")
	case !nodeproto.VLANAllowed(v.VLANs, tag):
		allowed := v.VLANs
		if allowed == "" {
			allowed = "none"
		}
		return "", badRequest("VLAN " + strconv.Itoa(tag) + " is not allowed on the bridge " + bridge + " (its uplink allows: " + allowed + ")")
	}
	return fmt.Sprintf("  <vlan><tag id='%d'/></vlan>\n", tag), nil
}

// --- the same network on the other nodes ---

// peerNode is what the "same network on other nodes" helper needs to know about one other node.
type peerNode struct {
	Node     int64
	Name     string
	Bridges  []nodeproto.NetBridge
	Networks []string // names of its libvirt networks
}

type peerChoice struct {
	Node int64  `json:"node"`
	Name string `json:"name"`
}

// sameNetworkPeers lists the nodes on which a network of the given name can be made on a managed virtual
// switch of the same name: the node has that switch, has no network of that name yet and, for a VLAN tag
// (vlan > 0), the switch filters VLANs and allows the tag.
func sameNetworkPeers(bridge, network string, vlan int, others []peerNode) []peerChoice {
	out := []peerChoice{}
	for _, p := range others {
		ok := false
		for _, b := range p.Bridges {
			if b.Name != bridge || !b.Managed {
				continue
			}
			ok = vlan == 0 || (b.VLANFiltering && nodeproto.VLANAllowed(b.VLANs, vlan))
		}
		for _, n := range p.Networks {
			if n == network {
				ok = false
			}
		}
		if ok {
			out = append(out, peerChoice{Node: p.Node, Name: p.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// hostnetPeers is GET .../hostnet/peers?bridge=&network=&vlan=: the other nodes that could get the same network.
func (s *Server) hostnetPeers(w http.ResponseWriter, r *http.Request) {
	id, _, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	bridge, network := q.Get("bridge"), q.Get("network")
	vlan, _ := strconv.Atoi(q.Get("vlan"))
	if bridge == "" || network == "" {
		fail(w, badRequest("bridge and network are required"))
		return
	}
	res := gatherNodes(s, nodeTimeout, func(h *hostconn.Host) ([]peerNode, error) {
		cfg, _ := s.Nodes.ConfigOfHost(h)
		if cfg.ID == id {
			return nil, nil
		}
		info, err := h.NetInfo()
		if err != nil {
			return nil, err
		}
		nets, err := collectNetworks(h)
		if err != nil {
			return nil, err
		}
		p := peerNode{Node: cfg.ID, Bridges: info.Bridges}
		for _, n := range nets {
			p.Networks = append(p.Networks, n.Name)
		}
		return []peerNode{p}, nil
	})
	others := []peerNode{}
	for _, r := range res {
		if len(r.Items) == 1 { // a node that failed or is not prepared is left out
			p := r.Items[0]
			p.Name = r.Name
			others = append(others, p)
		}
	}
	writeJSON(w, map[string]any{"nodes": sameNetworkPeers(bridge, network, vlan, others)})
}
