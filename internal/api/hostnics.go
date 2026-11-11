package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"virticle/internal/hostconn"
)

// Network interfaces of a node, read-only. One fixed command prints `ip -j -d
// addr show` and then the link speed of every interface from sysfs; nothing
// from the request reaches the shell.

const nicsCommand = `ip -j -d addr show; for i in $(ls /sys/class/net); do echo "SPEED $i $(cat /sys/class/net/$i/speed 2>/dev/null)"; done`

type hostNIC struct {
	Name  string `json:"name"`
	MAC   string `json:"mac,omitempty"`
	State string `json:"state"` // up | down | unknown
	Type  string `json:"type"`  // physical | bridge | bond | vlan | tap | other
	// Kind is the kernel's link kind as ip reports it ("bridge", "openvswitch", "veth", ...); empty for physical
	// devices. An Open vSwitch bridge has type "other" and kind "openvswitch".
	Kind      string   `json:"kind,omitempty"`
	Master    string   `json:"master,omitempty"`
	MTU       int      `json:"mtu"`
	SpeedMbps int      `json:"speedMbps,omitempty"`
	Addresses []string `json:"addresses"`
}

type ipLink struct {
	Name      string `json:"ifname"`
	Address   string `json:"address"`
	Master    string `json:"master"`
	MTU       int    `json:"mtu"`
	OperState string `json:"operstate"`
	LinkType  string `json:"link_type"`
	LinkInfo  *struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
	AddrInfo []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// parseHostNICs reads the output of nicsCommand. Lines that are neither the
// JSON array nor a SPEED line (a login banner, say) are ignored.
func parseHostNICs(out string) ([]hostNIC, error) {
	var js string
	speeds := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "["):
			js = line
		case strings.HasPrefix(line, "SPEED "):
			f := strings.Fields(line)
			if len(f) == 3 {
				if v, err := strconv.Atoi(f[2]); err == nil && v > 0 {
					speeds[f[1]] = v
				}
			}
		}
	}
	if js == "" {
		return nil, fmt.Errorf("no interface list in the output of ip")
	}
	var links []ipLink
	if err := json.Unmarshal([]byte(js), &links); err != nil {
		return nil, fmt.Errorf("reading the output of ip: %w", err)
	}
	res := make([]hostNIC, 0, len(links))
	for _, l := range links {
		if l.Name == "lo" || l.LinkType == "loopback" {
			continue
		}
		n := hostNIC{Name: l.Name, MAC: l.Address, Master: l.Master, MTU: l.MTU, Addresses: []string{}, SpeedMbps: speeds[l.Name]}
		switch strings.ToUpper(l.OperState) {
		case "UP":
			n.State = "up"
		case "DOWN", "LOWERLAYERDOWN", "NOTPRESENT":
			n.State = "down"
		default:
			n.State = "unknown"
		}
		kind := ""
		if l.LinkInfo != nil {
			kind = l.LinkInfo.Kind
		}
		n.Kind = kind
		switch kind {
		case "":
			n.Type = "physical"
		case "bridge", "bond", "vlan":
			n.Type = kind
		case "tun", "tap":
			n.Type = "tap"
		default:
			n.Type = "other"
		}
		for _, a := range l.AddrInfo {
			n.Addresses = append(n.Addresses, fmt.Sprintf("%s/%d", a.Local, a.PrefixLen))
		}
		res = append(res, n)
	}
	return res, nil
}

func hostNICList(h *hostconn.Host) ([]hostNIC, error) {
	out, err := h.Run(nicsCommand)
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w: %s", err, out)
	}
	return parseHostNICs(out)
}

func (s *Server) nodeInterfaces(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	nics, err := hostNICList(h)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"interfaces": nics})
}

// isBridgeDevice is true for a Linux bridge and for an Open vSwitch bridge.
func isBridgeDevice(n hostNIC) bool { return n.Type == "bridge" || n.Kind == "openvswitch" }

// checkBridge says why a name cannot be used as the host bridge of a VM interface: it is not a device of the node,
// or it is a device that is not a bridge. nics is the node's interface list.
func checkBridge(nics []hostNIC, name string) error {
	for _, n := range nics {
		if n.Name != name {
			continue
		}
		if !isBridgeDevice(n) {
			return badRequest(fmt.Sprintf("%s is a %s interface of this node, not a bridge", name, n.Type))
		}
		return nil
	}
	return badRequest(fmt.Sprintf("this node has no interface named %s", name))
}
