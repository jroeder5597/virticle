package nodecmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"virticle/internal/nodeproto"
)

// A fake node network for the host network verbs: a table of interfaces that
// `ip -j -d addr show` prints, systemd units that are active, canned output
// for other tools, a recorded list of everything that was run, and a fake
// clock and rollback timer.

type fAddr struct {
	cidr    string
	dynamic bool
}

type fLink struct {
	name, mac, master, kind, slaveKind string
	mtu                                int
	addrs                              []fAddr
	vlanFiltering                      bool
}

type fakeNet struct {
	t       *testing.T
	mu      sync.Mutex
	links   []fLink
	active  map[string]bool
	outputs map[string]string // joined argv -> stdout
	fail    map[string]error  // joined argv prefix -> error
	onRun   func(f *fakeNet, argv []string)
	events  []string // "run ARGV", "arm ID", "cancel ID", "detach ID" in order
	now     time.Time
	armed   map[string]int
	detach  []string
}

func newFakeNet(t *testing.T) *fakeNet {
	return &fakeNet{t: t, active: map[string]bool{}, outputs: map[string]string{}, fail: map[string]error{},
		now: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), armed: map[string]int{}}
}

func (f *fakeNet) link(name string) *fLink {
	for i := range f.links {
		if f.links[i].name == name {
			return &f.links[i]
		}
	}
	return nil
}

func (f *fakeNet) addAddr(name, cidr string, dynamic bool) {
	if l := f.link(name); l != nil {
		l.addrs = append(l.addrs, fAddr{cidr, dynamic})
	}
}

func (f *fakeNet) ipJSON(only string) string {
	var out []map[string]any
	for _, l := range f.links {
		if only != "" && l.name != only {
			continue
		}
		m := map[string]any{"ifname": l.name, "address": l.mac, "mtu": l.mtu, "operstate": "UP", "link_type": "ether"}
		if l.master != "" {
			m["master"] = l.master
		}
		info := map[string]any{}
		if l.kind != "" {
			info["info_kind"] = l.kind
		}
		if l.slaveKind != "" {
			info["info_slave_kind"] = l.slaveKind
		}
		if l.kind == "bridge" {
			vf := 0
			if l.vlanFiltering {
				vf = 1
			}
			info["info_data"] = map[string]any{"vlan_filtering": vf}
		}
		if len(info) > 0 {
			m["linkinfo"] = info
		}
		ai := []map[string]any{}
		for _, a := range l.addrs {
			ip, pl, _ := strings.Cut(a.cidr, "/")
			var n int
			fmt.Sscan(pl, &n)
			fam := "inet"
			if strings.Contains(ip, ":") {
				fam = "inet6"
			}
			e := map[string]any{"family": fam, "local": ip, "prefixlen": n, "scope": "global"}
			if a.dynamic {
				e["dynamic"] = true
			}
			ai = append(ai, e)
		}
		m["addr_info"] = ai
		out = append(out, m)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (f *fakeNet) run(argv []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join(argv, " ")
	f.events = append(f.events, "run "+key)
	for p, err := range f.fail {
		if strings.HasPrefix(key, p) {
			return "", err
		}
	}
	if f.onRun != nil {
		f.onRun(f, argv)
	}
	switch {
	case len(argv) >= 3 && argv[0] == "systemctl" && argv[1] == "is-active":
		if f.active[argv[len(argv)-1]] {
			return "", nil
		}
		return "", fmt.Errorf("inactive")
	case strings.HasPrefix(key, "ip -j -d addr show dev "):
		return f.ipJSON(argv[len(argv)-1]), nil
	case key == "ip -j -d addr show":
		return f.ipJSON(""), nil
	case strings.HasPrefix(key, "ip -j") && strings.Contains(key, "route"):
		if o, ok := f.outputs[key]; ok {
			return o, nil
		}
		return "[]", nil
	case key == "bridge -j vlan show":
		return "[]", nil
	}
	return f.outputs[key], nil
}

func (f *fakeNet) ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if strings.HasPrefix(e, "run "+prefix) {
			return true
		}
	}
	return false
}

func (f *fakeNet) index(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// the fake timer
type fakeTimer struct{ f *fakeNet }

func (t fakeTimer) Arm(id string, secs int) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.events = append(t.f.events, "arm "+id)
	t.f.armed[id] = secs
	return nil
}

func (t fakeTimer) Cancel(id string) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.events = append(t.f.events, "cancel "+id)
	delete(t.f.armed, id)
	return nil
}

func (t fakeTimer) Detach(id string) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.events = append(t.f.events, "detach "+id)
	t.f.detach = append(t.f.detach, id)
	return nil
}

// netNode is a fake node with the network of a typical cloud image: eth0 with
// a DHCP or static address, a networkd file for it, and a fake system.
type netNode struct {
	*tnode
	f *fakeNet
}

const dhcpNetwork = `[Match]
MACAddress=52:54:00:aa:bb:cc
Name=eth0

[Network]
DHCP=ipv4
LinkLocalAddressing=ipv6

[DHCP]
RouteMetric=100
UseMTU=true
`

const staticNetwork = `[Match]
Name=eth0

[Network]
Address=192.0.2.10/24
Gateway=192.0.2.1
DNS=192.0.2.53
Domains=lab.example

[Link]
MTUBytes=1500
`

// newNetNode builds a node whose eth0 has addrs from the networkd file content.
func newNetNode(t *testing.T, networkFile string, dynamic bool) *netNode {
	n := &netNode{tnode: newNode(t), f: newFakeNet(t)}
	f := n.f
	f.links = []fLink{
		{name: "eth0", mac: "52:54:00:aa:bb:cc", mtu: 1500, addrs: []fAddr{{"192.0.2.10/24", dynamic}}},
		{name: "eth1", mac: "52:54:00:aa:bb:dd", mtu: 1500},
	}
	f.active["systemd-networkd"] = true
	f.outputs["ip -j route show"] = `[{"dst":"default","gateway":"192.0.2.1","dev":"eth0","protocol":"dhcp"}]`
	for _, d := range []string{"sys/class/net/eth0/device", "sys/class/net/eth1/device", "etc/netplan", "etc/systemd/network"} {
		if err := os.MkdirAll(n.abs(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	n.put("etc/netplan/50-cloud-init.yaml", "network: {}\n")
	n.put("etc/systemd/network/10-netplan-eth0.network", networkFile)
	n.cfg.Self = "/usr/local/libexec/virticle-node"
	n.cfg.Net = &NetHooks{Run: f.run, Timer: fakeTimer{f}, Now: func() time.Time { return f.now },
		Sleep: func(d time.Duration) { f.now = f.now.Add(d) }}
	// A reconfigure of the new bridge gives it the uplink's addresses, like networkd would.
	f.onRun = func(f *fakeNet, argv []string) {
		key := strings.Join(argv, " ")
		switch {
		case strings.HasPrefix(key, "networkctl reload"):
			for _, p := range []string{"etc/systemd/network/05-virticle-br0.netdev"} {
				if _, err := os.Stat(n.abs(p)); err == nil && f.link("br0") == nil {
					f.links = append(f.links, fLink{name: "br0", kind: "bridge", mtu: 1500, mac: "52:54:00:aa:bb:cc",
						vlanFiltering: strings.Contains(readFile(n.abs(p)), "VLANFiltering=yes")})
				}
			}
		case strings.HasPrefix(key, "networkctl reconfigure br0"):
			if br := f.link("br0"); br != nil && len(br.addrs) == 0 {
				if _, err := os.Stat(n.abs("etc/systemd/network/05-virticle-br0.network")); err == nil {
					br.addrs = append([]fAddr(nil), f.link("eth0").addrs...)
				}
			}
			f.link("eth0").master = "br0"
		case strings.HasPrefix(key, "ip addr flush dev eth0"):
			f.link("eth0").addrs = nil
		case key == "ip link del br0":
			for i := range f.links {
				if f.links[i].name == "br0" {
					f.links = append(f.links[:i], f.links[i+1:]...)
					break
				}
			}
			f.link("eth0").master = ""
		case strings.HasPrefix(key, "networkctl reconfigure eth0") && f.link("br0") == nil:
			f.link("eth0").addrs = []fAddr{{"192.0.2.10/24", dynamic}}
		}
	}
	return n
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func (n *netNode) req(r nodeproto.NetRequest) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func (n *netNode) plan(r nodeproto.NetRequest) nodeproto.NetPlan {
	n.t.Helper()
	out := n.ok(n.req(r), "net-plan")
	var p nodeproto.NetPlan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		n.t.Fatalf("plan output: %v: %s", err, out)
	}
	return p
}

func (n *netNode) apply(r nodeproto.NetRequest) string {
	n.t.Helper()
	out := n.ok(n.req(r), "net-apply")
	var res nodeproto.NetApplyResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.ID == "" {
		n.t.Fatalf("apply output: %v: %s", err, out)
	}
	return res.ID
}

func (n *netNode) status() nodeproto.NetStatus {
	n.t.Helper()
	var st nodeproto.NetStatus
	if err := json.Unmarshal([]byte(n.ok("", "net-status")), &st); err != nil {
		n.t.Fatal(err)
	}
	return st
}

func (n *netNode) state(id string) string {
	n.t.Helper()
	for _, c := range n.status().Recent {
		if c.ID == id {
			return c.State
		}
	}
	n.t.Fatalf("change %s not in the status", id)
	return ""
}

func (n *netNode) refusedPlan(why string, r nodeproto.NetRequest, wantInMsg string) {
	n.t.Helper()
	out, code := n.run(n.req(r), "net-plan")
	if code == 0 {
		n.t.Errorf("%s: the plan was accepted: %s", why, out)
		return
	}
	if !strings.Contains(out, wantInMsg) {
		n.t.Errorf("%s: message %q does not contain %q", why, out, wantInMsg)
	}
}

var _ = filepath.Join

func jsonInto(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
}
