package nodecmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"virticle/internal/nodeproto"
)

// The NetworkManager backend. It never edits the administrator's profile of
// the uplink except for turning its autoconnect off (the old value is kept
// and put back on rollback and delete). It adds two profiles of its own:
//
//	virticle-BRIDGE        type bridge, interface BRIDGE, a copy of the ipv4/ipv6 settings of the uplink's profile
//	virticle-port-UPLINK   type ethernet on UPLINK, a port of the bridge
//
// The profile names are the marker NetworkManager itself shows; the managed
// bridge list of the node command is the authoritative one.
//
// No NetworkManager checkpoint is used: the rollback is the node command's
// own timer, the same for every configuration system.

type nmStack struct{}

// nmCopyKeys are the ipv4 and ipv6 properties of the uplink's profile that
// the bridge's profile takes over. Anything not listed keeps NetworkManager's default.
var nmCopyKeys = map[string]bool{
	"ipv4.method": true, "ipv4.addresses": true, "ipv4.gateway": true, "ipv4.dns": true, "ipv4.dns-search": true,
	"ipv4.dns-priority": true, "ipv4.routes": true, "ipv4.route-metric": true, "ipv4.route-table": true,
	"ipv4.ignore-auto-dns": true, "ipv4.ignore-auto-routes": true, "ipv4.never-default": true, "ipv4.may-fail": true,
	"ipv4.dhcp-client-id": true, "ipv4.dhcp-hostname": true, "ipv4.dhcp-send-hostname": true, "ipv4.dhcp-timeout": true,
	"ipv4.dhcp-iaid": true,
	"ipv6.method":    true, "ipv6.addresses": true, "ipv6.gateway": true, "ipv6.dns": true, "ipv6.dns-search": true,
	"ipv6.dns-priority": true, "ipv6.routes": true, "ipv6.route-metric": true, "ipv6.route-table": true,
	"ipv6.ignore-auto-dns": true, "ipv6.ignore-auto-routes": true, "ipv6.never-default": true, "ipv6.may-fail": true,
	"ipv6.addr-gen-mode": true, "ipv6.ip6-privacy": true, "ipv6.dhcp-iaid": true, "ipv6.dhcp-duid": true, "ipv6.ra-timeout": true,
}

// nmTerse parses `nmcli -t` field output: KEY:VALUE per line, ':' and '\' escaped by a backslash in values.
func nmTerse(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if v[i] == '\\' && i+1 < len(v) {
				i++
			}
			b.WriteByte(v[i])
		}
		m[k] = b.String()
	}
	return m
}

func nmVersionAtLeast(out string, major, minor int) bool {
	f := strings.Fields(out)
	if len(f) == 0 {
		return false
	}
	p := strings.Split(strings.TrimRight(f[len(f)-1], ".\n"), ".")
	if len(p) < 2 {
		return false
	}
	a, e1 := strconv.Atoi(p[0])
	b, e2 := strconv.Atoi(p[1])
	return e1 == nil && e2 == nil && (a > major || (a == major && b >= minor))
}

func (nmStack) plan(c *Ctx, in *planInput) (*netPlanned, error) {
	switch in.Req.Action {
	case nodeproto.NetCreate:
		return nmCreate(c, in)
	case nodeproto.NetDelete:
		return nmDelete(in), nil
	}
	return nmSetVLANs(in), nil
}

func nmBridgeID(s *bridgeSpec) string { return "virticle-" + s.Bridge }
func nmPortID(s *bridgeSpec) string   { return "virticle-port-" + s.Uplink }

func nmCreate(c *Ctx, in *planInput) (*netPlanned, error) {
	s := in.New
	s.Data = map[string]string{}
	pl := &netPlanned{Marker: "put", Spec: s}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Create the Linux bridge %s as the NetworkManager connection %s.", s.Bridge, nmBridgeID(s)))
	if out, err := c.netRun("nmcli", "--version"); err == nil && nmVersionAtLeast(out, 1, 46) {
		s.Data["portKeys"] = "new"
	}
	ids, _ := c.netRun("nmcli", "-t", "-f", "NAME", "connection", "show")
	for _, l := range strings.Split(ids, "\n") {
		if l == nmBridgeID(s) || (s.Uplink != "" && l == nmPortID(s)) {
			return nil, Fail("NetworkManager already has a connection named %s", l)
		}
	}
	if in.Uplink != nil {
		up := in.Uplink
		has := len(up.globalAddrs()) > 0
		uuid, _ := c.netRun("nmcli", "-t", "-g", "GENERAL.CON-UUID", "device", "show", up.Name)
		uuid = strings.TrimSpace(uuid)
		if uuid == "" || uuid == "--" {
			if has {
				return nil, Fail("%s has addresses, but NetworkManager has no active connection on it; they cannot be moved safely", up.Name)
			}
		} else {
			out, err := c.netRun("nmcli", "-t", "-f", "connection.id,connection.type,connection.autoconnect,802-3-ethernet.mtu,ipv4,ipv6", "connection", "show", "uuid", uuid)
			if err != nil {
				return nil, Fail("cannot read the NetworkManager connection of %s: %v", up.Name, err)
			}
			p := nmTerse(out)
			if p["connection.type"] != "802-3-ethernet" {
				return nil, Fail("the connection of %s is of type %s, not a plain ethernet one", up.Name, p["connection.type"])
			}
			s.Data["origUUID"], s.Data["origID"], s.Data["origAutoconnect"] = uuid, p["connection.id"], p["connection.autoconnect"]
			if v := p["802-3-ethernet.mtu"]; v != "" && v != "auto" && v != "0" && v != "--" {
				s.Data["mtu"] = v
			}
			pl.Saves = append(pl.Saves, netSave{"nm-original-profile.txt", []string{"nmcli", "-t", "-s", "connection", "show", "uuid", uuid}})
			if s.MoveAddresses {
				var kv []string
				for k, v := range p {
					if nmCopyKeys[k] && v != "" && v != "--" {
						kv = append(kv, k, v)
					}
				}
				sortPairs(kv)
				b, _ := json.Marshal(kv)
				s.Moved = string(b)
				if has {
					method := "static"
					if p["ipv4.method"] == "auto" {
						method = "dhcp"
					}
					mv, txt := moveSummary(in, s, method, p["ipv4.gateway"])
					pl.Move = mv
					pl.Summary = append(pl.Summary, txt, fmt.Sprintf("%s becomes a plain port of %s without addresses; its own connection %q is kept, but set not to start by itself.", up.Name, s.Bridge, p["connection.id"]))
				}
			}
		}
		if !s.MoveAddresses {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s becomes a plain port of %s.", up.Name, s.Bridge))
			if has {
				pl.Warn = append(pl.Warn, fmt.Sprintf("The addresses of %s are removed and not moved to the bridge.", up.Name))
			}
		}
	}
	if s.VLANAware {
		pl.Summary = append(pl.Summary, vlanSummary(s))
	}
	pl.Steps = nmCreateSteps(s)
	if s.Uplink != "" && s.MoveAddresses && len(in.Expect) > 0 {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: s.Bridge, Argv: in.Expect, Secs: 40})
	}
	pl.Restore = nmUndoSteps(s, true)
	return pl, nil
}

func sortPairs(kv []string) {
	// sort by key, keeping pairs together
	for i := 2; i < len(kv); i += 2 {
		for j := i; j >= 2 && kv[j] < kv[j-2]; j -= 2 {
			kv[j], kv[j-2] = kv[j-2], kv[j]
			kv[j+1], kv[j-1] = kv[j-1], kv[j+1]
		}
	}
}

func nmPortVLANs(s *bridgeSpec) string {
	if !s.VLANAware {
		return ""
	}
	v := "1 pvid untagged"
	if t := taggedVLANs(s.VLANs); t != "" {
		v += "," + t
	}
	return v
}

func nmCreateSteps(s *bridgeSpec) []netStep {
	br := []string{"nmcli", "connection", "add", "type", "bridge", "ifname", s.Bridge, "con-name", nmBridgeID(s),
		"connection.autoconnect", "yes", "bridge.stp", "no"}
	if s.MoveAddresses && s.MAC != "" {
		br = append(br, "bridge.mac-address", s.MAC)
	}
	if s.Data["mtu"] != "" && s.MoveAddresses {
		br = append(br, "802-3-ethernet.mtu", s.Data["mtu"])
	}
	if s.VLANAware {
		br = append(br, "bridge.vlan-filtering", "yes", "bridge.vlan-default-pvid", "1")
	}
	var kv []string
	if s.Moved != "" {
		_ = json.Unmarshal([]byte(s.Moved), &kv)
	}
	if len(kv) == 0 {
		kv = []string{"ipv4.method", "disabled", "ipv6.method", "disabled"}
	}
	br = append(br, kv...)
	steps := []netStep{{Kind: "run", Argv: br}}
	if s.Uplink != "" {
		ctl, ptype := "master", "slave-type"
		if s.Data["portKeys"] == "new" {
			ctl, ptype = "controller", "port-type"
		}
		port := []string{"nmcli", "connection", "add", "type", "ethernet", "ifname", s.Uplink, "con-name", nmPortID(s),
			ctl, s.Bridge, ptype, "bridge", "connection.autoconnect", "yes"}
		if s.Data["mtu"] != "" {
			port = append(port, "802-3-ethernet.mtu", s.Data["mtu"])
		}
		if v := nmPortVLANs(s); v != "" {
			port = append(port, "bridge-port.vlans", v)
		}
		steps = append(steps, netStep{Kind: "run", Argv: port})
		if u := s.Data["origUUID"]; u != "" {
			steps = append(steps, stepRun("nmcli", "connection", "modify", "uuid", u, "connection.autoconnect", "no"))
		}
	}
	steps = append(steps, stepRun("nmcli", "--wait", "0", "connection", "up", "id", nmBridgeID(s)))
	if s.Uplink != "" {
		steps = append(steps, stepRun("nmcli", "--wait", "0", "connection", "up", "id", nmPortID(s)))
	}
	return steps
}

// nmUndoSteps remove the two profiles and bring the uplink's own profile back.
func nmUndoSteps(s *bridgeSpec, ignore bool) []netStep {
	mk := func(ign bool, argv ...string) netStep { return netStep{Kind: "run", Argv: argv, Ignore: ign} }
	var steps []netStep
	if s.Uplink != "" {
		steps = append(steps, mk(true, "nmcli", "connection", "delete", "id", nmPortID(s)))
	}
	steps = append(steps, mk(true, "nmcli", "connection", "delete", "id", nmBridgeID(s)), mk(true, "ip", "link", "del", s.Bridge))
	if u := s.Data["origUUID"]; u != "" {
		ac := s.Data["origAutoconnect"]
		if ac == "" {
			ac = "yes"
		}
		steps = append(steps,
			mk(ignore, "nmcli", "connection", "modify", "uuid", u, "connection.autoconnect", ac),
			mk(ignore, "nmcli", "--wait", "0", "connection", "up", "uuid", u))
		if s.MoveAddresses && len(s.UplinkExpect) > 0 {
			steps = append(steps, netStep{Kind: "wait-addr", Path: s.Uplink, Argv: s.UplinkExpect, Secs: 40, Ignore: ignore})
		}
	}
	return steps
}

func nmDelete(in *planInput) *netPlanned {
	s := in.Old
	pl := &netPlanned{Marker: "remove", Spec: s}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Delete the Linux bridge %s (the NetworkManager connection %s), which virticle created.", s.Bridge, nmBridgeID(s)))
	if s.Uplink != "" {
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s gets its own connection back (%s).", s.Uplink, strings.Join(s.UplinkExpect, ", ")))
	}
	pl.Saves = []netSave{{"nm-connections.txt", []string{"nmcli", "-t", "connection", "show"}}}
	pl.Steps = nmUndoSteps(s, false)
	pl.Restore = append(nmCreateSteps(s), []netStep{}...)
	pl.Restore = ignoreAll(pl.Restore)
	return pl
}

func nmSetVLANs(in *planInput) *netPlanned {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = vlanChangeSummary(old, n)
	mod := func(s *bridgeSpec) []netStep {
		yn := "no"
		if s.VLANAware {
			yn = "yes"
		}
		steps := []netStep{stepRun("nmcli", "connection", "modify", "id", nmBridgeID(s), "bridge.vlan-filtering", yn, "bridge.vlan-default-pvid", "1")}
		if s.Uplink != "" {
			steps = append(steps, stepRun("nmcli", "connection", "modify", "id", nmPortID(s), "bridge-port.vlans", nmPortVLANs(s)))
		}
		return steps
	}
	pl.Steps = append(mod(n), vlanCommands(old, n)...)
	pl.Restore = ignoreAll(append(mod(old), vlanCommands(n, old)...))
	return pl
}
