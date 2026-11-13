package nodecmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"virticle/internal/nodeproto"
)

// Several uplinks: a switch with two or more physical cards has a bond device that holds them, and the bond is
// the bridge's single port. The first card keeps its place in the older fields of bridgeSpec (Uplink, MAC,
// UplinkExpect, Data), the others are Members.

// cardSpec is one physical card of a switch, with what is needed to give it back.
type cardSpec struct {
	Name   string   `json:"name"`
	Expect []string `json:"expect,omitempty"` // what the card had before: addresses, "dhcp4" or "any"
	MTU    int      `json:"mtu,omitempty"`    // the card's MTU before it joined
	// NetworkManager: the card's own profile, which is switched off while the card belongs to the switch.
	UUID        string `json:"uuid,omitempty"`
	ID          string `json:"id,omitempty"`
	Autoconnect string `json:"autoconnect,omitempty"`
}

// vlanAddrSpec is the node's own address on a VLAN of the bridge.
type vlanAddrSpec struct {
	ID           int    `json:"id"`
	Method       string `json:"method"`
	Address      string `json:"address,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	DefaultRoute bool   `json:"defaultRoute,omitempty"`
}

func (v vlanAddrSpec) iface(s *bridgeSpec) string { return nodeproto.VLANInterface(s.Bridge, v.ID) }

func (s *bridgeSpec) cards() []cardSpec {
	if s.Uplink == "" {
		return nil
	}
	first := cardSpec{Name: s.Uplink, MTU: s.MTU, Expect: s.UplinkExpect, UUID: s.Data["origUUID"], ID: s.Data["origID"], Autoconnect: s.Data["origAutoconnect"]}
	return append([]cardSpec{first}, s.Members...)
}

func (s *bridgeSpec) setCards(cs []cardSpec) {
	if s.Data == nil {
		s.Data = map[string]string{}
	}
	for _, k := range []string{"origUUID", "origID", "origAutoconnect"} {
		delete(s.Data, k)
	}
	s.Uplink, s.UplinkExpect, s.Members = "", nil, nil
	if len(cs) == 0 {
		return
	}
	s.Uplink, s.UplinkExpect, s.MTU = cs[0].Name, cs[0].Expect, cs[0].MTU
	for k, v := range map[string]string{"origUUID": cs[0].UUID, "origID": cs[0].ID, "origAutoconnect": cs[0].Autoconnect} {
		if v != "" {
			s.Data[k] = v
		}
	}
	s.Members = append([]cardSpec(nil), cs[1:]...)
}

func (s *bridgeSpec) cardNames() []string { return cardNamesOf(s.cards()) }

func (s *bridgeSpec) card(name string) *cardSpec {
	for _, c := range s.cards() {
		if c.Name == name {
			c := c
			return &c
		}
	}
	return nil
}

// port is the bridge's single port: the bond, or the one card.
func (s *bridgeSpec) port() string {
	if s.Bond != "" {
		return s.Bond
	}
	return s.Uplink
}

// mtu is the MTU every layer of the switch gets ("" when none is configured).
func (s *bridgeSpec) mtu() string {
	if s.MTUSet > 0 {
		return strconv.Itoa(s.MTUSet)
	}
	return s.Data["mtu"]
}

var vswNameRE = regexp.MustCompile(`^vsw([0-9]+)$`)

// bondName derives the bond's name from the switch's: vsw0 gets vswbond0.
func bondName(bridge string) string {
	if m := vswNameRE.FindStringSubmatch(bridge); m != nil && len("vswbond"+m[1]) <= 15 {
		return "vswbond" + m[1]
	}
	n := "b-" + bridge
	if len(n) > 15 {
		n = n[:15]
	}
	return n
}

func bondModeOK(m string) bool { return m == nodeproto.BondFailover || m == nodeproto.BondLACP }

// bondModeArgs are the arguments of ip link ... type bond for a mode.
func bondModeArgs(mode string) []string {
	if mode == nodeproto.BondLACP {
		return []string{"mode", "802.3ad", "lacp_rate", "fast", "xmit_hash_policy", "layer2+3"}
	}
	return []string{"mode", "active-backup"}
}

func bondOptionsNM(mode string) string {
	if mode == nodeproto.BondLACP {
		return "mode=802.3ad,miimon=100,lacp_rate=fast,xmit_hash_policy=layer2+3"
	}
	return "mode=active-backup,miimon=100"
}

func (s *bridgeSpec) vlanAddr(id int) *vlanAddrSpec {
	for i := range s.VLANAddrs {
		if s.VLANAddrs[i].ID == id {
			return &s.VLANAddrs[i]
		}
	}
	return nil
}

func (s *bridgeSpec) vlanAddrIDs() []int {
	var ids []int
	for _, v := range s.VLANAddrs {
		ids = append(ids, v.ID)
	}
	sort.Ints(ids)
	return ids
}

// cardSpeed is the link speed in Mbit/s that sysfs reports; 0 when unknown.
func (c *Ctx) cardSpeed(name string) int {
	b, err := os.ReadFile(filepath.Join(c.Cfg.sys(), "class/net", name, "speed"))
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if v < 0 {
		return 0
	}
	return v
}

// lacpWarnings are the things about the cards that matter for 802.3ad.
func (c *Ctx) lacpWarnings(cards []string) []string {
	var w []string
	known := 0
	speeds := map[int]bool{}
	for _, n := range cards {
		sp := c.cardSpeed(n)
		if sp == 0 {
			w = append(w, fmt.Sprintf("%s does not report its link speed. LACP needs it: the card may not carry traffic in this mode.", n))
			continue
		}
		known++
		speeds[sp] = true
	}
	if len(speeds) > 1 {
		var parts []string
		for _, n := range cards {
			if sp := c.cardSpeed(n); sp > 0 {
				parts = append(parts, fmt.Sprintf("%s %d Mbit/s", n, sp))
			}
		}
		w = append(w, "The cards run at different speeds ("+strings.Join(parts, ", ")+"). LACP only combines cards of the same speed; the others stay unused.")
	}
	return w
}

// cardDiff says what changes in the cards between two specs of the same bridge.
type cardDiff struct {
	Added, Removed []cardSpec
	BondCreated    bool // from has no bond, to has one
	BondDropped    bool // from has a bond, to has none (only to undo)
	ModeChanged    bool
}

func diffCards(from, to *bridgeSpec) cardDiff {
	var d cardDiff
	for _, c := range to.cards() {
		if from.card(c.Name) == nil {
			d.Added = append(d.Added, c)
		}
	}
	for _, c := range from.cards() {
		if to.card(c.Name) == nil {
			d.Removed = append(d.Removed, c)
		}
	}
	d.BondCreated = from.Bond == "" && to.Bond != ""
	d.BondDropped = from.Bond != "" && to.Bond == ""
	d.ModeChanged = from.Bond != "" && to.Bond != "" && from.BondMode != to.BondMode
	return d
}

func cardNamesOf(cs []cardSpec) []string {
	var r []string
	for _, c := range cs {
		r = append(r, c.Name)
	}
	return r
}
