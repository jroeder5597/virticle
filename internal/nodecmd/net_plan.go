package nodecmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"virticle/internal/nodeproto"
)

// Planning: validate a request against what the node has now, ask the
// configuration system's backend for the steps, and describe the result.

var bridgeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,14}$`)

// netStack is one configuration system of a node.
type netStack interface {
	// plan computes the steps for a validated request.
	plan(c *Ctx, in *planInput) (*netPlanned, error)
}

// planInput is a validated request together with what the node has now.
type planInput struct {
	Req    *nodeproto.NetRequest
	Snap   *netSnap
	Old    *bridgeSpec // delete, set-vlans: the managed bridge as it is
	New    *bridgeSpec // create, set-vlans: the bridge as it is to be
	Uplink *netLink    // create: the uplink as it is now
	Expect []string    // create: what the uplink has now: static addresses, "dhcp4", or "any"
	Name   string      // the stack's public name
}

type stackInfo struct {
	Name      string // networkmanager | netplan-networkd | networkd | ifupdown | unknown
	Tested    bool
	Supported bool
	Note      string
	impl      netStack
}

func (c *Ctx) unitActive(unit string) bool {
	_, err := c.netRun("systemctl", "is-active", "--quiet", unit)
	return err == nil
}

// detectStack finds out, at run time, which system configures the node's network.
func (c *Ctx) detectStack() stackInfo {
	switch {
	case c.unitActive("NetworkManager") && c.HasTool("nmcli"):
		return stackInfo{Name: "networkmanager", Tested: true, Supported: true, impl: nmStack{}} // tried on Rocky Linux 10 (NetworkManager 1.56)
	case c.unitActive("systemd-networkd"):
		name := "networkd"
		if m, _ := filepath.Glob(c.hostPath("etc/netplan/*.yaml")); len(m) > 0 {
			name = "netplan-networkd"
		}
		// Tried on a real node: netplan with networkd (Debian 13). Plain networkd runs the same code, but was not.
		return stackInfo{Name: name, Tested: name == "netplan-networkd", Supported: true, impl: networkdStack{}}
	}
	if b, err := os.ReadFile(c.hostPath("/etc/network/interfaces")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "iface" && f[1] != "lo" {
				return stackInfo{Name: "ifupdown", Note: "This node is configured with ifupdown (/etc/network/interfaces). Creating bridges from here is not supported for ifupdown; configure the bridge on the node itself."}
			}
		}
	}
	return stackInfo{Name: "unknown", Note: "Neither NetworkManager nor systemd-networkd is running on this node, so its network configuration system is not supported."}
}

func (c *Ctx) readRequest() (*nodeproto.NetRequest, error) {
	b, err := io.ReadAll(io.LimitReader(c.Stdin, 64<<10+1))
	if err != nil || len(b) > 64<<10 {
		return nil, Die("the request is missing or too large")
	}
	var req nodeproto.NetRequest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, Die("the request is not valid: %v", err)
	}
	return &req, nil
}

// mgmtLink is the interface that holds an address the controller uses for this node.
func (c *Ctx) mgmtLink(snap *netSnap, req *nodeproto.NetRequest) string {
	addrs := map[string]bool{}
	for _, a := range req.ControllerAddrs {
		addrs[a] = true
	}
	if f := strings.Fields(os.Getenv("SSH_CONNECTION")); len(f) == 4 {
		addrs[f[2]] = true
	}
	for _, l := range snap.Links {
		for _, a := range l.Addrs {
			if addrs[a.Local] {
				return l.Name
			}
		}
	}
	return ""
}

func uplinkExpect(l netLink) []string {
	var static []string
	dhcp, any := false, false
	for _, a := range l.globalAddrs() {
		any = true
		switch {
		case a.Family == "inet" && a.Dynamic:
			dhcp = true
		case a.Family == "inet" && !a.Dynamic:
			static = append(static, a.CIDR())
		}
	}
	sort.Strings(static)
	if dhcp {
		static = append(static, "dhcp4")
	}
	if len(static) == 0 && any {
		static = []string{"any"}
	}
	return static
}

func defaultGateway(snap *netSnap, dev string) string {
	for _, r := range snap.Routes {
		if r.Dev == dev && r.Dst == "default" && r.Gateway != "" {
			return r.Gateway
		}
	}
	return ""
}

// buildPlan validates req and computes the plan. Nothing on the system changes.
func (c *Ctx) buildPlan(req *nodeproto.NetRequest) (*planInput, *netPlanned, stackInfo, error) {
	st := c.detectStack()
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = nodeproto.NetDefaultTimeout
	}
	if req.TimeoutSeconds < nodeproto.NetMinTimeout || req.TimeoutSeconds > nodeproto.NetMaxTimeout {
		return nil, nil, st, Die("the time to confirm must be between %d and %d seconds", nodeproto.NetMinTimeout, nodeproto.NetMaxTimeout)
	}
	if !bridgeNameRE.MatchString(req.Bridge) || libvirtOwned(req.Bridge) {
		return nil, nil, st, Die("the bridge name must be 1 to 15 characters: lower-case letters, digits, dash and underscore, starting with a letter, and not one libvirt uses (virbr, vnet)")
	}
	if !st.Supported {
		return nil, nil, st, Fail("%s", st.Note)
	}
	if p := c.pendingMeta(); p != nil {
		return nil, nil, st, Fail("the network change %s is still pending (%s); confirm it or roll it back first", p.ID, p.State)
	}
	snap, err := c.readSnap()
	if err != nil {
		return nil, nil, st, err
	}
	managed := c.loadManaged()
	in := &planInput{Req: req, Snap: snap, Name: st.Name}
	mgmt := c.mgmtLink(snap, req)
	existing := snap.link(req.Bridge)

	// VLAN options
	vlans := ""
	if req.VLANAware {
		if req.Action != nodeproto.NetDelete && req.LibvirtVersion < nodeproto.LibvirtVLANMin {
			return nil, nil, st, Fail("VLANs on a Linux bridge need libvirt 11.0.0 or newer (this node has %s)", libvirtVersionString(req.LibvirtVersion))
		}
		if vlans, err = nodeproto.ParseVLANs(req.VLANs); err != nil {
			return nil, nil, st, Die("%v", err)
		}
	} else if req.VLANs != "" {
		return nil, nil, st, Die("VLAN ids need a VLAN-aware bridge")
	}

	switch req.Action {
	case nodeproto.NetCreate:
		if existing != nil {
			return nil, nil, st, Fail("the node already has an interface named %s", req.Bridge)
		}
		spec := &bridgeSpec{Bridge: req.Bridge, Uplink: req.Uplink, MoveAddresses: req.MoveAddresses, VLANAware: req.VLANAware, VLANs: vlans,
			Stack: st.Name, Created: c.netNow().UTC().Format("2006-01-02T15:04:05Z"), Data: map[string]string{}}
		in.New = spec
		if req.Uplink == "" {
			if req.MoveAddresses {
				return nil, nil, st, Die("addresses can only move from an uplink: choose one")
			}
			break
		}
		up := snap.link(req.Uplink)
		switch {
		case libvirtOwned(req.Uplink):
			return nil, nil, st, Fail("%s is managed by libvirt and is not touched", req.Uplink)
		case up == nil:
			return nil, nil, st, Fail("the node has no interface named %s", req.Uplink)
		case up.SlaveKind == "bond":
			return nil, nil, st, Fail("%s is a member of the bond %s; bonds are not supported as uplinks", up.Name, up.Master)
		case up.Master != "":
			return nil, nil, st, Fail("%s is already a port of %s", up.Name, up.Master)
		}
		if ok, why := c.isPhysical(*up); !ok {
			return nil, nil, st, Fail("%s cannot be an uplink: %s", up.Name, why)
		}
		in.Uplink, in.Expect = up, uplinkExpect(*up)
		spec.MAC, spec.MTU, spec.UplinkExpect = up.MAC, up.MTU, in.Expect
		has := len(up.globalAddrs()) > 0
		switch {
		case mgmt == up.Name && !req.MoveAddresses:
			return nil, nil, st, Fail("%s carries the controller's connection to this node; turn on moving its addresses to the bridge, or choose another uplink", up.Name)
		case has && !req.MoveAddresses && mgmt == "":
			return nil, nil, st, Fail("%s has addresses and it is not known which interface the controller uses to reach this node; turn on moving its addresses to the bridge", up.Name)
		case req.MoveAddresses && !has:
			// nothing to move: not an error, the plan says so
		}
	case nodeproto.NetDelete, nodeproto.NetSetVLAN:
		old := managed[req.Bridge]
		if old == nil {
			if existing != nil {
				return nil, nil, st, Fail("%s was not created by this feature and is only shown, not changed", req.Bridge)
			}
			return nil, nil, st, Fail("the node has no bridge named %s", req.Bridge)
		}
		in.Old = old
		if req.Action == nodeproto.NetDelete {
			if existing != nil {
				for _, p := range c.bridgePorts(req.Bridge) {
					switch {
					case p == old.Uplink:
					case libvirtOwned(p) || strings.HasPrefix(p, "tap"):
						return nil, nil, st, Fail("virtual machines are attached to %s (port %s); detach them first", req.Bridge, p)
					default:
						return nil, nil, st, Fail("%s has another port, %s, which this feature did not add; remove it first", req.Bridge, p)
					}
				}
			}
			break
		}
		if existing == nil {
			return nil, nil, st, Fail("the bridge %s does not exist right now", req.Bridge)
		}
		n := old.clone()
		n.VLANAware, n.VLANs = req.VLANAware, vlans
		if n.VLANAware == old.VLANAware && n.VLANs == old.VLANs {
			return nil, nil, st, Fail("the VLAN settings of %s are already like this", req.Bridge)
		}
		in.New = n
	default:
		return nil, nil, st, Die("unknown action %q", req.Action)
	}
	pl, err := st.impl.plan(c, in)
	if err != nil {
		return nil, nil, st, err
	}
	return in, pl, st, nil
}

func libvirtVersionString(v uint64) string {
	if v == 0 {
		return "an unknown version"
	}
	return fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000)
}

// publicPlan turns the internal plan into the printed one.
func (c *Ctx) publicPlan(in *planInput, pl *netPlanned, st stackInfo) nodeproto.NetPlan {
	p := nodeproto.NetPlan{Stack: st.Name, StackTested: st.Tested, Summary: append([]string(nil), pl.Summary...), AddressMove: pl.Move,
		Warnings: append([]string(nil), pl.Warn...), TimeoutSeconds: in.Req.TimeoutSeconds}
	for _, s := range pl.Steps {
		switch s.Kind {
		case "write":
			p.Files = append(p.Files, nodeproto.NetPlanFile{Path: s.Path, Action: "write", Content: s.Content})
		case "remove":
			p.Files = append(p.Files, nodeproto.NetPlanFile{Path: s.Path, Action: "remove"})
		}
		p.Commands = append(p.Commands, s.String())
	}
	p.ManagementInterface = c.mgmtLink(in.Snap, in.Req)
	if p.ManagementInterface != "" {
		switch {
		case in.Uplink != nil && in.Uplink.Name == p.ManagementInterface:
			p.ManagementAffected = true
		case in.Req.Action == nodeproto.NetDelete && in.Req.Bridge == p.ManagementInterface:
			p.ManagementAffected = true
		}
	}
	if p.ManagementAffected {
		p.Summary = append(p.Summary, fmt.Sprintf("The controller reaches this node through %s. The connection drops for a moment while the change is applied; the node keeps its address, and the controller reconnects at the address it has stored.", p.ManagementInterface))
	} else if p.ManagementInterface == "" {
		p.Warnings = append(p.Warnings, "It is not known which interface carries the controller's connection (a jump host or address translation, perhaps). Only the rollback timer protects the connection.")
	}
	p.Summary = append(p.Summary, fmt.Sprintf("The previous settings are saved first. If the controller does not confirm within %d seconds (it cannot reach the node, say), the node restores them by itself.", in.Req.TimeoutSeconds))
	if !st.Tested {
		p.Warnings = append(p.Warnings, "This configuration system has not been tested on a real node yet.")
	}
	return p
}
