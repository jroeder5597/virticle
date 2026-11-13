package nodecmd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// matchList matches value against a networkd list of shell patterns: a pattern
// with "!" in front excludes, and when there are patterns without it, one of
// them must match.
func matchList(v, value string) bool {
	pos, neg, any := false, false, false
	for _, p := range strings.Fields(v) {
		excl := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		ok, _ := path.Match(p, value)
		if excl {
			neg = neg || ok
			continue
		}
		any = true
		pos = pos || ok
	}
	return !neg && (!any || pos)
}

// hostVirt is what the node runs in, for the match key Virtualization: the
// virtual machine type and the container type, each empty when there is none.
type hostVirt func() (vm, container string)

// virtMatches follows systemd's ConditionVirtualization: "yes" is any, "no" is
// none, "vm" and "container" are the two classes, anything else a type name;
// "!" in front negates the whole value.
func virtMatches(v string, vm, container string) bool {
	neg := strings.HasPrefix(v, "!")
	v = strings.TrimPrefix(v, "!")
	var ok bool
	switch v {
	case "yes", "true", "1":
		ok = vm != "" || container != ""
	case "no", "false", "0":
		ok = vm == "" && container == ""
	case "vm":
		ok = vm != ""
	case "container":
		ok = container != ""
	default:
		ok = v == vm || v == container
	}
	return ok != neg
}

// matchNetwork evaluates the [Match] section of a .network file for a link.
// It evaluates Name, OriginalName, MACAddress, PermanentMACAddress, Kind, Type,
// WLANInterfaceType and Virtualization, which together cover the files
// systemd ships in /usr/lib/systemd/network. A key that this code does not
// evaluate is an error: guessing could move the wrong configuration. virt may
// be nil.
func matchNetwork(secs []iniSec, l netLink, virt hostVirt) (bool, error) {
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
			if !matchList(v, l.Name) {
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
		case "Kind":
			// The kernel's link kind (veth, bridge, sit, ...). A physical card has none.
			if !matchList(v, l.Kind) {
				return false, nil
			}
		case "Type":
			// The device type from sysfs (wlan, bridge, bond, ...). A physical ethernet card has none.
			if !matchList(v, l.deviceType()) {
				return false, nil
			}
		case "WLANInterfaceType":
			if l.deviceType() != "wlan" {
				return false, nil
			}
			return false, fmt.Errorf("it uses the match key %s, and %s is wireless", k, l.Name)
		case "Virtualization":
			if virt == nil {
				return false, fmt.Errorf("it uses the match key %s, which cannot be evaluated here", k)
			}
			vm, ctr := virt()
			if !virtMatches(v, vm, ctr) {
				return false, nil
			}
		default:
			return false, fmt.Errorf("it uses the match key %s, which cannot be evaluated here", k)
		}
	}
	return true, nil
}

// detectVirt asks systemd-detect-virt what this node runs in. A failure to ask
// counts as "none".
func (c *Ctx) detectVirt() (vm, container string) {
	ask := func(flag string) string {
		out, _ := c.netRun("systemd-detect-virt", flag)
		out = strings.TrimSpace(out)
		if out == "none" {
			return ""
		}
		return out
	}
	return ask("--vm"), ask("--container")
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
		ok, err := matchNetwork(parseINI(string(b)), l, c.detectVirt)
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

// leaseIAID finds the IAID of the DHCP client identifier the uplink's current IPv4 lease was taken with.
// networkd derives the IAID from the interface name, so the bridge would present another client
// identifier than the uplink did, and a DHCP server that keys its leases by it hands out another
// address. The lease files are "private data" of networkd: this is best effort and returns "" when
// the file or its format is not what is expected (the move then goes on without the IAID).
func (c *Ctx) leaseIAID(l netLink) string {
	var addr string
	for _, a := range l.globalAddrs() {
		if a.Family == "inet" {
			addr = a.Local
			break
		}
	}
	if addr == "" {
		return ""
	}
	files, _ := filepath.Glob(c.hostPath("/run/systemd/netif/leases/*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var a, id string
		for _, ln := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(ln, "ADDRESS="); ok {
				a = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(ln, "CLIENTID="); ok {
				id = strings.TrimSpace(v)
			}
		}
		// ff + IAID (4 bytes) + DUID: the DUID-based identifier (RFC 4361).
		if a == addr && len(id) >= 10 && strings.EqualFold(id[:2], "ff") {
			if _, err := strconv.ParseUint(id[2:10], 16, 32); err == nil {
				return "0x" + strings.ToLower(id[2:10])
			}
		}
	}
	return ""
}

// withIAID adds IAID=... to the DHCPv4 section of a moved configuration, unless it names a client identifier itself.
func withIAID(text, iaid string) string {
	if iaid == "" {
		return text
	}
	secs := parseINI(text)
	var b strings.Builder
	done := false
	for i := range secs {
		if secs[i].Name == "DHCPv4" || secs[i].Name == "DHCP" {
			if len(secs[i].values("IAID")) > 0 || len(secs[i].values("ClientIdentifier")) > 0 {
				return text
			}
			if !done {
				secs[i].Lines = append(secs[i].Lines, "IAID="+iaid)
				done = true
			}
		}
	}
	if !done {
		secs = append(secs, iniSec{Name: "DHCPv4", Lines: []string{"IAID=" + iaid}})
	}
	for _, s := range secs {
		b.WriteString("[" + s.Name + "]\n" + strings.Join(s.Lines, "\n") + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func linkMTU(secs []iniSec) string {
	if s := secByName(secs, "Link"); s != nil {
		if v := s.values("MTUBytes"); len(v) > 0 {
			return v[len(v)-1]
		}
	}
	return ""
}
