package nodecmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The system side of the host network verbs: running tools, the clock, the
// rollback timer, and reading what the kernel has now.

// NetHooks replaces the system side in tests.
type NetHooks struct {
	// Run runs a tool (argv[0] is its name) and returns standard output.
	// Nil: the real tool, started through Ctx.Command.
	Run func(argv []string) (string, error)
	// Timer arms, cancels and detaches. Nil: systemd-run and systemctl.
	Timer NetTimer
	// Now and Sleep default to the real clock.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// NetTimer is everything that has to outlive the SSH session of net-apply.
type NetTimer interface {
	// Arm schedules `net-rollback ID timer` to run after seconds, independent of any session.
	Arm(id string, seconds int) error
	// Cancel stops the armed rollback.
	Cancel(id string) error
	// Detach starts `net-run ID` in a unit of its own and returns once it is started.
	Detach(id string) error
}

func (c *Ctx) netHooks() *NetHooks {
	if c.Cfg.Net != nil {
		return c.Cfg.Net
	}
	return &NetHooks{}
}

// netRun runs a tool and returns its standard output. The error carries the
// first line of the tool's standard error.
func (c *Ctx) netRun(argv ...string) (string, error) {
	if h := c.netHooks(); h.Run != nil {
		return h.Run(argv)
	}
	cmd, err := c.Command(argv[0], argv[1:]...)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if l, _, _ := strings.Cut(msg, "\n"); l != "" {
			return out.String(), fmt.Errorf("%s: %s", argv[0], l)
		}
		return out.String(), fmt.Errorf("%s: %v", argv[0], err)
	}
	return out.String(), nil
}

func (c *Ctx) netNow() time.Time {
	if h := c.netHooks(); h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (c *Ctx) netSleep(d time.Duration) {
	if h := c.netHooks(); h.Sleep != nil {
		h.Sleep(d)
		return
	}
	select {
	case <-time.After(d):
	case <-c.Context.Done():
	}
}

func (c *Ctx) netTimer() NetTimer {
	if h := c.netHooks(); h.Timer != nil {
		return h.Timer
	}
	return systemdTimer{c}
}

// systemdTimer uses transient systemd units, which belong to the system
// manager and not to the SSH session that started them.
type systemdTimer struct{ c *Ctx }

func (t systemdTimer) self() string {
	if t.c.Cfg.Self != "" {
		return t.c.Cfg.Self
	}
	return NodePath
}

func rollbackUnit(id string) string { return "virticle-net-rollback-" + id }
func applyUnit(id string) string    { return "virticle-net-apply-" + id }

func (t systemdTimer) Arm(id string, seconds int) error {
	_, err := t.c.netRun("systemd-run", "--quiet", "--collect",
		"--unit="+rollbackUnit(id),
		"--description=virticle host network rollback "+id,
		fmt.Sprintf("--on-active=%ds", seconds),
		"--timer-property=AccuracySec=1s",
		t.self(), "net-rollback", id, "timer")
	return err
}

func (t systemdTimer) Cancel(id string) error {
	_, err := t.c.netRun("systemctl", "stop", rollbackUnit(id)+".timer")
	return err
}

func (t systemdTimer) Detach(id string) error {
	_, err := t.c.netRun("systemd-run", "--quiet", "--collect",
		"--unit="+applyUnit(id),
		"--description=virticle host network change "+id,
		t.self(), "net-run", id)
	return err
}

// --- reading the kernel's state ---

type netAddr struct {
	Family  string // inet | inet6
	Local   string
	Prefix  int
	Scope   string
	Dynamic bool
}

func (a netAddr) CIDR() string { return fmt.Sprintf("%s/%d", a.Local, a.Prefix) }

// global: a configured address that is not link-local.
func (a netAddr) global() bool {
	if a.Scope == "link" || a.Scope == "host" {
		return false
	}
	ip, err := netip.ParseAddr(a.Local)
	return err == nil && !ip.IsLinkLocalUnicast() && !ip.IsLoopback()
}

type netLink struct {
	Name, MAC, State, Kind, SlaveKind, Master string
	MTU                                       int
	Addrs                                     []netAddr
	VLANFiltering                             bool
}

func (l netLink) globalAddrs() []netAddr {
	var r []netAddr
	for _, a := range l.Addrs {
		if a.global() {
			r = append(r, a)
		}
	}
	return r
}

type netRoute struct {
	Dst, Gateway, Dev, Protocol string
}

type netSnap struct {
	Links  []netLink
	Routes []netRoute
	// PortVLANs maps a port to the VLAN ids it allows, as "bridge vlan" prints them.
	PortVLANs map[string]string
}

func (s *netSnap) link(name string) *netLink {
	for i := range s.Links {
		if s.Links[i].Name == name {
			return &s.Links[i]
		}
	}
	return nil
}

type ipLinkJSON struct {
	Name      string `json:"ifname"`
	Address   string `json:"address"`
	Master    string `json:"master"`
	MTU       int    `json:"mtu"`
	OperState string `json:"operstate"`
	LinkType  string `json:"link_type"`
	LinkInfo  *struct {
		Kind      string `json:"info_kind"`
		SlaveKind string `json:"info_slave_kind"`
		Data      struct {
			VLANFiltering int `json:"vlan_filtering"`
		} `json:"info_data"`
	} `json:"linkinfo"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
		Dynamic   bool   `json:"dynamic"`
	} `json:"addr_info"`
}

func parseIPLinks(out string) ([]netLink, error) {
	var raw []ipLinkJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("cannot read the output of ip: %v", err)
	}
	var links []netLink
	for _, r := range raw {
		if r.Name == "lo" || r.LinkType == "loopback" {
			continue
		}
		l := netLink{Name: r.Name, MAC: r.Address, Master: r.Master, MTU: r.MTU, State: strings.ToLower(r.OperState)}
		if r.LinkInfo != nil {
			l.Kind, l.SlaveKind = r.LinkInfo.Kind, r.LinkInfo.SlaveKind
			l.VLANFiltering = r.LinkInfo.Data.VLANFiltering == 1
		}
		for _, a := range r.AddrInfo {
			l.Addrs = append(l.Addrs, netAddr{Family: a.Family, Local: a.Local, Prefix: a.PrefixLen, Scope: a.Scope, Dynamic: a.Dynamic})
		}
		links = append(links, l)
	}
	return links, nil
}

func (c *Ctx) readSnap() (*netSnap, error) {
	out, err := c.netRun("ip", "-j", "-d", "addr", "show")
	if err != nil {
		return nil, Fail("cannot list the network interfaces: %v", err)
	}
	links, err := parseIPLinks(out)
	if err != nil {
		return nil, Fail("%v", err)
	}
	s := &netSnap{Links: links, PortVLANs: map[string]string{}}
	for _, fam := range [][]string{{"-j", "route", "show"}, {"-j", "-6", "route", "show"}} {
		o, err := c.netRun(append([]string{"ip"}, fam...)...)
		if err != nil {
			continue
		}
		var rs []struct {
			Dst, Gateway, Dev, Protocol string
		}
		if json.Unmarshal([]byte(o), &rs) == nil {
			for _, r := range rs {
				s.Routes = append(s.Routes, netRoute{r.Dst, r.Gateway, r.Dev, r.Protocol})
			}
		}
	}
	if o, err := c.netRun("bridge", "-j", "vlan", "show"); err == nil {
		s.PortVLANs = parseBridgeVLANs(o)
	}
	return s, nil
}

// parseBridgeVLANs reads `bridge -j vlan show`.
func parseBridgeVLANs(out string) map[string]string {
	var rs []struct {
		Ifname string `json:"ifname"`
		Vlans  []struct {
			Vlan    int `json:"vlan"`
			VlanEnd int `json:"vlanEnd"`
		} `json:"vlans"`
	}
	res := map[string]string{}
	if json.Unmarshal([]byte(out), &rs) != nil {
		return res
	}
	for _, r := range rs {
		var parts []string
		for _, v := range r.Vlans {
			if v.VlanEnd > v.Vlan {
				parts = append(parts, fmt.Sprintf("%d-%d", v.Vlan, v.VlanEnd))
			} else {
				parts = append(parts, fmt.Sprint(v.Vlan))
			}
		}
		res[r.Ifname] = strings.Join(parts, ",")
	}
	return res
}

// ports lists the ports of a bridge from sysfs.
func (c *Ctx) bridgePorts(br string) []string {
	ents, err := os.ReadDir(filepath.Join(c.Cfg.sys(), "class/net", br, "brif"))
	if err != nil {
		return nil
	}
	var r []string
	for _, e := range ents {
		r = append(r, e.Name())
	}
	sort.Strings(r)
	return r
}

// isPhysical: a real network device (it has a device link in sysfs and no kernel link kind) that is not wireless.
func (c *Ctx) isPhysical(l netLink) (physical bool, why string) {
	if l.Kind != "" {
		return false, fmt.Sprintf("%s is a %s interface, not a physical one", l.Name, l.Kind)
	}
	base := filepath.Join(c.Cfg.sys(), "class/net", l.Name)
	if _, err := os.Lstat(filepath.Join(base, "device")); err != nil {
		return false, fmt.Sprintf("%s is not a physical interface", l.Name)
	}
	if _, err := os.Lstat(filepath.Join(base, "wireless")); err == nil {
		return false, fmt.Sprintf("%s is a wireless interface", l.Name)
	}
	return true, ""
}

// libvirtOwned names that libvirt creates and manages; they are never touched.
func libvirtOwned(name string) bool {
	for _, p := range []string{"virbr", "vnet", "macvtap", "vif"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
