package nodecmd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"virticle/internal/nodeproto"
)

// The systemd-networkd backend, also used for netplan with the networkd
// renderer. It writes networkd files of its own and leaves the files of the
// administrator and of netplan alone:
//
//	/etc/systemd/network/05-virticle-BRIDGE.netdev            the bridge (VLAN filtering lives here)
//	/etc/systemd/network/05-virticle-BRIDGE.network           the bridge's addresses: a copy of the uplink's configuration
//	/etc/systemd/network/05-virticle-BRIDGE-UPLINK.network    the uplink as a plain port (and its allowed VLANs)
//
// networkd uses the first .network file, by file name, that matches an
// interface. "05-" sorts before the "10-netplan-*.network" files netplan
// generates, so the uplink file wins without netplan's YAML being edited (it
// cannot be done reliably: netplan merges the lists of several files, so a
// later file cannot remove an address) and without cloud-init rewriting it at
// boot. The plan checks that no file with a lower name matches the uplink.
// The files survive a reboot and netplan apply.

type networkdStack struct{}

var networkdDirs = []string{"/etc/systemd/network", "/run/systemd/network", "/usr/lib/systemd/network", "/lib/systemd/network"}

const networkdEtc = "/etc/systemd/network"

// --- a small reader for networkd's ini files ---

type iniSec struct {
	Name  string
	Lines []string // "Key=Value", comments and empty lines dropped
}

func parseINI(s string) []iniSec {
	var secs []iniSec
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		if l[0] == '[' && strings.HasSuffix(l, "]") {
			secs = append(secs, iniSec{Name: l[1 : len(l)-1]})
			continue
		}
		if len(secs) > 0 {
			secs[len(secs)-1].Lines = append(secs[len(secs)-1].Lines, l)
		}
	}
	return secs
}

func (s iniSec) values(key string) []string {
	var r []string
	for _, l := range s.Lines {
		if k, v, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key {
			r = append(r, strings.TrimSpace(v))
		}
	}
	return r
}

func secByName(secs []iniSec, name string) *iniSec {
	for i := range secs {
		if secs[i].Name == name {
			return &secs[i]
		}
	}
	return nil
}

// matchNetwork evaluates the [Match] section of a .network file for a link.
// A key that this code does not evaluate is an error: guessing could move the
// wrong configuration.
func matchNetwork(secs []iniSec, l netLink) (bool, error) {
	m := secByName(secs, "Match")
	if m == nil {
		return true, nil
	}
	for _, ln := range m.Lines {
		k, v, _ := strings.Cut(ln, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch k {
		case "Name", "OriginalName":
			pos, neg, any := false, false, false
			for _, p := range strings.Fields(v) {
				if strings.HasPrefix(p, "!") {
					if ok, _ := path.Match(p[1:], l.Name); ok {
						neg = true
					}
					continue
				}
				any = true
				if ok, _ := path.Match(p, l.Name); ok {
					pos = true
				}
			}
			if neg || (any && !pos) {
				return false, nil
			}
		case "MACAddress", "PermanentMACAddress":
			ok := false
			for _, p := range strings.Fields(v) {
				ok = ok || strings.EqualFold(p, l.MAC)
			}
			if !ok {
				return false, nil
			}
		default:
			return false, fmt.Errorf("it uses the match key %s, which cannot be evaluated here", k)
		}
	}
	return true, nil
}

type ndFile struct {
	Path    string // below the root, e.g. /etc/systemd/network/10-netplan-eth0.network
	Content string
}

// findNetworkFile finds the .network file networkd uses for l: the first
// match in file name order, a name in /etc hiding the same name below /run
// and /usr/lib.
func (c *Ctx) findNetworkFile(l netLink) (*ndFile, error) {
	files := map[string]string{}
	for _, d := range networkdDirs {
		ents, _ := os.ReadDir(c.hostPath(d))
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".network") && !e.IsDir() {
				if _, seen := files[e.Name()]; !seen {
					files[e.Name()] = filepath.Join(d, e.Name())
				}
			}
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(c.hostPath(files[n]))
		if err != nil {
			continue
		}
		ok, err := matchNetwork(parseINI(string(b)), l)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", files[n], err)
		}
		if ok {
			return &ndFile{files[n], string(b)}, nil
		}
	}
	return nil, nil
}

// movedConfig is the configuration of the uplink's .network file that the
// bridge takes over: every section but [Match], without the keys that make the
// interface a port or give it a MAC address of its own.
func movedConfig(secs []iniSec) (text, method, gateway string) {
	var b strings.Builder
	method = "static"
	for _, s := range secs {
		if s.Name == "Match" {
			continue
		}
		var keep []string
		for _, l := range s.Lines {
			k, v, _ := strings.Cut(l, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch {
			case s.Name == "Network" && (k == "Bridge" || k == "Bond" || k == "VLAN" || k == "MACVLAN" || k == "VXLAN"):
				continue
			case s.Name == "Link" && k == "MACAddress":
				continue
			case s.Name == "Network" && k == "DHCP" && v != "no" && v != "false" && v != "":
				method = "dhcp"
			case (s.Name == "Network" || s.Name == "Route") && k == "Gateway" && gateway == "":
				gateway = v
			}
			keep = append(keep, k+"="+v)
		}
		if len(keep) > 0 {
			b.WriteString("[" + s.Name + "]\n" + strings.Join(keep, "\n") + "\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n", method, gateway
}

func linkMTU(secs []iniSec) string {
	if s := secByName(secs, "Link"); s != nil {
		if v := s.values("MTUBytes"); len(v) > 0 {
			return v[len(v)-1]
		}
	}
	return ""
}

// --- the files ---

func ndNetdevPath(s *bridgeSpec) string { return networkdEtc + "/05-virticle-" + s.Bridge + ".netdev" }
func ndBridgePath(s *bridgeSpec) string { return networkdEtc + "/05-virticle-" + s.Bridge + ".network" }
func ndUplinkPath(s *bridgeSpec) string {
	return networkdEtc + "/05-virticle-" + s.Bridge + "-" + s.Uplink + ".network"
}

const ndHeader = "# Written by virticle. Changing it by hand is fine; deleting the bridge in virticle removes the file.\n"

func ndNetdev(s *bridgeSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[NetDev]\nName=" + s.Bridge + "\nKind=bridge\n")
	if s.MoveAddresses && s.MAC != "" {
		b.WriteString("MACAddress=" + s.MAC + "\n")
	}
	if s.VLANAware {
		b.WriteString("\n[Bridge]\nVLANFiltering=yes\nDefaultPVID=1\n")
	}
	return b.String()
}

func ndBridgeNetwork(s *bridgeSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[Match]\nName=" + s.Bridge + "\n\n")
	if s.Moved != "" {
		b.WriteString(s.Moved)
	} else {
		b.WriteString("[Network]\nLinkLocalAddressing=no\nIPv6AcceptRA=no\nConfigureWithoutCarrier=yes\n\n[Link]\nRequiredForOnline=no\n")
	}
	return b.String()
}

// taggedVLANs is the canonical list without id 1, which is the untagged default.
func taggedVLANs(canon string) string {
	var out []string
	for _, r := range nodeproto.VLANRanges(canon) {
		lo, hi := r[0], r[1]
		if lo == 1 {
			lo = 2
		}
		if lo > hi {
			continue
		}
		if lo == hi {
			out = append(out, fmt.Sprint(lo))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", lo, hi))
		}
	}
	return strings.Join(out, ",")
}

func ndUplinkNetwork(s *bridgeSpec) string {
	var b strings.Builder
	b.WriteString(ndHeader + "[Match]\nName=" + s.Uplink + "\n\n[Network]\nBridge=" + s.Bridge + "\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\n")
	if s.Data["mtu"] != "" {
		b.WriteString("MTUBytes=" + s.Data["mtu"] + "\n")
	}
	b.WriteString("RequiredForOnline=enslaved\n")
	if s.VLANAware {
		b.WriteString("\n[BridgeVLAN]\nPVID=1\nEgressUntagged=1\n")
		if t := taggedVLANs(s.VLANs); t != "" {
			b.WriteString("\n[BridgeVLAN]\nVLAN=" + t + "\n")
		}
	}
	return b.String()
}

func ndWrites(s *bridgeSpec) []netStep {
	steps := []netStep{
		{Kind: "write", Path: ndNetdevPath(s), Content: ndNetdev(s), Mode: 0o644},
		{Kind: "write", Path: ndBridgePath(s), Content: ndBridgeNetwork(s), Mode: 0o644},
	}
	if s.Uplink != "" {
		steps = append(steps, netStep{Kind: "write", Path: ndUplinkPath(s), Content: ndUplinkNetwork(s), Mode: 0o644})
	}
	return steps
}

func ndRemoves(s *bridgeSpec) []netStep {
	steps := []netStep{{Kind: "remove", Path: ndNetdevPath(s)}, {Kind: "remove", Path: ndBridgePath(s)}}
	if s.Uplink != "" {
		steps = append(steps, netStep{Kind: "remove", Path: ndUplinkPath(s)})
	}
	return steps
}

// ndTearDown puts the uplink back: networkd forgets the files (already removed
// or restored by the caller), the bridge goes, the uplink is configured again from its own file.
func ndTearDown(s *bridgeSpec, ignore bool) []netStep {
	mk := func(argv ...string) netStep { return netStep{Kind: "run", Argv: argv, Ignore: ignore} }
	steps := []netStep{mk("networkctl", "reload")}
	if s.Uplink != "" {
		steps = append(steps, mk("ip", "link", "set", s.Uplink, "nomaster"))
	}
	steps = append(steps, netStep{Kind: "run", Argv: []string{"ip", "link", "del", s.Bridge}, Ignore: true})
	if s.Uplink != "" {
		steps = append(steps, mk("networkctl", "reconfigure", s.Uplink))
		if s.MoveAddresses && len(s.UplinkExpect) > 0 {
			steps = append(steps, netStep{Kind: "wait-addr", Path: s.Uplink, Argv: s.UplinkExpect, Secs: 40, Ignore: ignore})
		}
	}
	return steps
}

// ndBringUp creates the bridge from the files and enslaves the uplink.
func ndBringUp(s *bridgeSpec, ignore bool) []netStep {
	mk := func(argv ...string) netStep { return netStep{Kind: "run", Argv: argv, Ignore: ignore} }
	steps := []netStep{mk("networkctl", "reload"), {Kind: "wait-link", Path: s.Bridge, Secs: 20, Ignore: ignore}}
	re := []string{"networkctl", "reconfigure", s.Bridge}
	if s.Uplink != "" {
		re = append(re, s.Uplink)
	}
	return append(steps, mk(re...))
}

func (networkdStack) plan(c *Ctx, in *planInput) (*netPlanned, error) {
	switch in.Req.Action {
	case nodeproto.NetCreate:
		return ndCreate(c, in)
	case nodeproto.NetDelete:
		return ndDelete(in), nil
	}
	return ndSetVLANs(in), nil
}

func vlanSummary(s *bridgeSpec) string {
	if !s.VLANAware {
		return "VLAN filtering stays off."
	}
	return fmt.Sprintf("The bridge is VLAN-aware (VLAN filtering on); its own address stays untagged (VLAN 1)%s.", func() string {
		if s.Uplink == "" {
			return ""
		}
		return fmt.Sprintf(", and %s allows the VLAN ids %s", s.Uplink, s.VLANs)
	}())
}

func moveSummary(in *planInput, s *bridgeSpec, method, gw string) (*nodeproto.NetAddressMove, string) {
	var addrs []string
	for _, a := range in.Uplink.globalAddrs() {
		addrs = append(addrs, a.CIDR())
	}
	mv := &nodeproto.NetAddressMove{From: in.Uplink.Name, To: s.Bridge, Method: method, Addresses: addrs, Gateway: gw, MAC: s.MAC}
	if gw == "" {
		mv.Gateway = defaultGateway(in.Snap, in.Uplink.Name)
	}
	txt := fmt.Sprintf("%s takes over the configuration of %s (%s, %s) and %s gets the same MAC address %s", s.Bridge, in.Uplink.Name, method, strings.Join(addrs, ", "), s.Bridge, s.MAC)
	if method == "dhcp" {
		txt += ", so that the DHCP server hands out the same address again (a lease follows the MAC address)"
	}
	return mv, txt + "."
}

func ndCreate(c *Ctx, in *planInput) (*netPlanned, error) {
	s := in.New
	s.Data = map[string]string{}
	pl := &netPlanned{Marker: "put", Spec: s}
	pl.Saves = []netSave{{"networkctl-list.txt", []string{"networkctl", "list", "--no-pager"}}}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Create the Linux bridge %s.", s.Bridge))
	if in.Uplink != nil {
		up := in.Uplink
		has := len(up.globalAddrs()) > 0
		f, err := c.findNetworkFile(*up)
		if err != nil && (s.MoveAddresses || has) {
			return nil, Fail("cannot tell which networkd configuration applies to %s: %v", up.Name, err)
		}
		if f == nil && has {
			return nil, Fail("%s has addresses, but no networkd file configures them (set by hand with ip?); they cannot be moved safely", up.Name)
		}
		if f != nil {
			if ours := path.Base(ndUplinkPath(s)); path.Base(f.Path) <= ours {
				return nil, Fail("%s configures %s and sorts before the file this change writes (%s); rename it so that it sorts later, or choose another uplink", f.Path, up.Name, ours)
			}
			secs := parseINI(f.Content)
			s.Data["mtu"] = linkMTU(secs)
			if s.MoveAddresses {
				text, method, gw := movedConfig(secs)
				s.Moved = text
				if has {
					mv, txt := moveSummary(in, s, method, gw)
					pl.Move = mv
					pl.Summary = append(pl.Summary, txt, fmt.Sprintf("%s becomes a plain port of %s without addresses.", up.Name, s.Bridge))
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
	pl.Steps = append(ndWrites(s), ndBringUp(s, false)...)
	if s.Uplink != "" && len(in.Expect) > 0 && s.MoveAddresses {
		pl.Steps = append(pl.Steps, netStep{Kind: "wait-addr", Path: s.Bridge, Argv: in.Expect, Secs: 40})
		pl.Steps = append(pl.Steps, stepRunIgnore("ip", "addr", "flush", "dev", s.Uplink))
	}
	// The undo: the written files are removed by the generic restore.
	pl.Restore = ndTearDown(s, true)
	return pl, nil
}

func ndDelete(in *planInput) *netPlanned {
	s := in.Old
	pl := &netPlanned{Marker: "remove", Spec: s}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Delete the Linux bridge %s, which virticle created.", s.Bridge))
	if s.Uplink != "" {
		if s.MoveAddresses && len(s.UplinkExpect) > 0 {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s gets its own configuration (%s) back.", s.Uplink, strings.Join(s.UplinkExpect, ", ")))
		} else {
			pl.Summary = append(pl.Summary, fmt.Sprintf("%s stops being a port and is configured by its own file again.", s.Uplink))
		}
	}
	pl.Steps = append(ndRemoves(s), ndTearDown(s, false)...)
	pl.Restore = ndBringUp(s, true)
	return pl
}

// vlanIDs is the set of ids a spec allows on its uplink (without the default id 1).
func vlanIDs(s *bridgeSpec) map[int]bool {
	m := map[int]bool{}
	if !s.VLANAware {
		return m
	}
	for _, r := range nodeproto.VLANRanges(taggedVLANs(s.VLANs)) {
		for i := r[0]; i <= r[1]; i++ {
			m[i] = true
		}
	}
	return m
}

func rangesOfIDs(m map[int]bool) []string {
	ids := make([]int, 0, len(m))
	for i := range m {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	var out []string
	for i := 0; i < len(ids); i++ {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if i == j {
			out = append(out, fmt.Sprint(ids[i]))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		}
		i = j
	}
	return out
}

// vlanCommands are the runtime commands that turn the VLAN settings of from into those of to.
func vlanCommands(from, to *bridgeSpec) []netStep {
	var add, del = map[int]bool{}, map[int]bool{}
	a, b := vlanIDs(from), vlanIDs(to)
	for i := range b {
		if !a[i] {
			add[i] = true
		}
	}
	for i := range a {
		if !b[i] {
			del[i] = true
		}
	}
	filt := func(on bool) netStep {
		v := "0"
		if on {
			v = "1"
		}
		return stepRun("ip", "link", "set", to.Bridge, "type", "bridge", "vlan_filtering", v)
	}
	var steps []netStep
	if to.VLANAware && !from.VLANAware {
		steps = append(steps, filt(true))
	}
	if from.Uplink != "" {
		for _, r := range rangesOfIDs(add) {
			steps = append(steps, stepRun("bridge", "vlan", "add", "dev", from.Uplink, "vid", r))
		}
		for _, r := range rangesOfIDs(del) {
			steps = append(steps, stepRun("bridge", "vlan", "del", "dev", from.Uplink, "vid", r))
		}
	}
	if !to.VLANAware && from.VLANAware {
		steps = append(steps, filt(false))
	}
	return steps
}

func ignoreAll(steps []netStep) []netStep {
	out := make([]netStep, len(steps))
	for i, s := range steps {
		s.Ignore = true
		out[i] = s
	}
	return out
}

func vlanChangeSummary(old, n *bridgeSpec) []string {
	if !n.VLANAware {
		return []string{fmt.Sprintf("Turn VLAN filtering off on %s.", n.Bridge)}
	}
	return []string{fmt.Sprintf("%s: VLAN filtering on, allowed VLAN ids %s (before: %s).", n.Bridge, n.VLANs, func() string {
		if !old.VLANAware {
			return "VLAN filtering off"
		}
		return old.VLANs
	}())}
}

func ndSetVLANs(in *planInput) *netPlanned {
	old, n := in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Summary = vlanChangeSummary(old, n)
	pl.Steps = []netStep{
		{Kind: "write", Path: ndNetdevPath(n), Content: ndNetdev(n), Mode: 0o644},
	}
	if n.Uplink != "" {
		pl.Steps = append(pl.Steps, netStep{Kind: "write", Path: ndUplinkPath(n), Content: ndUplinkNetwork(n), Mode: 0o644})
	}
	pl.Steps = append(pl.Steps, stepRun("networkctl", "reload"))
	pl.Steps = append(pl.Steps, vlanCommands(old, n)...)
	pl.Restore = append([]netStep{stepRunIgnore("networkctl", "reload")}, ignoreAll(vlanCommands(n, old))...)
	return pl
}
