package api

// Checks before an unplanned failover ("Start here from the copy"). The controller cannot reach the source node,
// but that does not mean the VM is down: only the controller's own network path may be broken. Two questions are
// asked from the standby node, which may have a better view:
//
//  1. Can the standby reach the source node directly (the node-to-node peer path), and is the VM running there?
//     Then the failover is refused: Planned failover or shutting the VM down is the way.
//  2. Does the VM still answer on the network? For each network card on a bridge that the standby has too,
//     the standby asks (an ARP request) for the addresses the VM had at its last copy. An
//     answer from the VM's own MAC address means it is running somewhere: starting a second copy needs an
//     explicit "Start anyway".
//
// What cannot be checked is said in one line; it never stops the failover. decideFailoverChecks is the whole
// decision as a pure function; failoverChecks gathers the facts.

import (
	"encoding/xml"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// nicProbe is what is known about one network card of the VM and what the probe found.
type nicProbe struct {
	MAC    string
	Bridge string // the bridge the standby probes on; empty when the card cannot be probed
	IPs    []string
	// Skip says why the card was not probed: "nat" (its network exists per node), "noaddr" (no address known),
	// "nobridge" (the standby has no such bridge), "nobraddr" (it has no IPv4 address on that bridge to ask from),
	// "notool" (neither arping nor ping and ip on the standby).
	Skip    string
	Replies []hostconn.ProbeReply
}

// foFacts is everything the decision needs.
type foFacts struct {
	Source, Standby string
	// Check 1.
	Asked    bool   // the standby was asked
	AskError string // why it could not be asked or did not answer ("" when it answered)
	VMState  string // the VM's state on the source, as the source node reported it
	// Check 2.
	NICs []nicProbe
}

// foChecks is the result.
type foChecks struct {
	// Refuse is set when the failover must not go on, with the reason.
	Refuse string `json:"refuse,omitempty"`
	// Warning is set when the VM answers on the network; the failover then needs "Start anyway".
	Warning string `json:"warning,omitempty"`
	// Checked says in one line what was looked at.
	Checked string `json:"checked"`
	// Answers lists the addresses that answered from the VM's own MAC address.
	Answers []string `json:"answers,omitempty"`
}

// vmAlive says whether a VM state reported by a node means the VM may be executing.
func vmAlive(state string) bool {
	switch state {
	case "running", "paused", "blocked", "idle", "pmsuspended", "inshutdown":
		return true
	}
	return false
}

func joinAnd(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}

// couldNotAsk is true for a failure that says the peer path is not set up, which differs from a node that does
// not answer.
func couldNotAsk(err string) bool {
	for _, k := range []string{"Permission denied", "Host key verification", "no node key", "unknown subcommand", "is not installed", "unsupported", "no such"} {
		if strings.Contains(err, k) {
			return true
		}
	}
	return false
}

func decideFailoverChecks(f foFacts) foChecks {
	var res foChecks
	var parts []string
	// 1. The source node, from the standby.
	switch {
	case !f.Asked:
		parts = append(parts, f.Source+" could not be asked from "+f.Standby)
	case f.AskError == "" && vmAlive(f.VMState):
		res.Refuse = f.Source + " is reachable from " + f.Standby + " and the VM is running there. Use Planned failover when the controller can reach it again, or shut the VM down on " + f.Source + "."
		res.Checked = res.Refuse
		return res
	case f.AskError == "":
		parts = append(parts, f.Source+" is reachable from "+f.Standby+" and the VM is not running there")
	case couldNotAsk(f.AskError):
		parts = append(parts, f.Source+" could not be asked from "+f.Standby+" (the node-to-node access is not set up)")
	default:
		parts = append(parts, f.Source+" does not answer from "+f.Standby)
	}
	// 2. The network.
	var silent []string
	skips := map[string]bool{}
	for _, n := range f.NICs {
		if n.Skip != "" {
			skips[n.Skip] = true
			if n.Skip == "nobridge" {
				skips["nobridge:"+n.Bridge] = true
			}
			continue
		}
		answered := map[string]bool{}
		for _, r := range n.Replies {
			if strings.EqualFold(r.MAC, n.MAC) {
				answered[r.IP] = true
			}
		}
		for _, ip := range n.IPs {
			if answered[ip] {
				res.Answers = append(res.Answers, ip)
			} else {
				silent = append(silent, ip)
			}
		}
	}
	sort.Strings(res.Answers)
	sort.Strings(silent)
	switch {
	case len(res.Answers) > 0:
		res.Warning = "This VM still answers on the network at " + joinAnd(res.Answers) + ". It is probably still running" + onNode(f.Source) + ". Starting it here gives two VMs with the same addresses."
		parts = append(parts, "the VM answers at "+joinAnd(res.Answers))
	case len(silent) > 0:
		parts = append(parts, "nothing answers at "+joinAnd(silent))
	default:
		why := "the VM has no address on a bridge that "
		switch {
		case skips["notool"]:
			why = "arping is not installed on " + f.Standby
		case skips["nobridge"]:
			why = f.Standby + " has no bridge for the VM's network"
		case skips["nobraddr"]:
			why = f.Standby + " has no IPv4 address on that bridge to ask from"
		case skips["noaddr"]:
			why = "no address of the VM is known"
		case skips["nat"]:
			why = "the VM's networks (NAT) exist per node"
		default:
			why = "the VM has no network card on a bridge"
		}
		parts = append(parts, "the network was not checked: "+why)
	}
	res.Checked = "Checked: " + strings.Join(parts, "; ") + "."
	return res
}

func onNode(name string) string {
	if name == "" {
		return ""
	}
	return " on " + name
}

// parseVMAddrs reads "MAC=ip,ip;MAC=ip" (replicaMeta.Addrs).
func parseVMAddrs(s string) map[string][]string {
	out := map[string][]string{}
	for _, e := range strings.Split(s, ";") {
		mac, ips, ok := strings.Cut(e, "=")
		if !ok || mac == "" {
			continue
		}
		for _, ip := range strings.Split(ips, ",") {
			if ip != "" {
				out[strings.ToLower(mac)] = append(out[strings.ToLower(mac)], ip)
			}
		}
	}
	return out
}

// vmAddrs is what the controller knows of the VM's addresses now, as replicaMeta.Addrs: what the guest agent
// says, else the DHCP leases. IPv4 only (the probe is ARP). "" when nothing is known.
func vmAddrs(h *hostconn.Host, dom libvirt.Domain) string {
	if st, _, err := h.DomainGetState(dom, 0); err != nil || stateName(uint8(st)) != "running" {
		return ""
	}
	ips, err := agentIPs(h, dom)
	if err != nil || len(ips) == 0 {
		ips = map[string][]string{}
		// The DHCP leases of a libvirt network, then the host's neighbour table (a card on a plain bridge has no lease
		// and, without an agent, is known only by the ARP entry of the host that talked to it).
		for _, src := range []libvirt.DomainInterfaceAddressesSource{libvirt.DomainInterfaceAddressesSrcLease, libvirt.DomainInterfaceAddressesSrcArp} {
			ifs, err := h.DomainInterfaceAddresses(dom, uint32(src), 0)
			if err != nil {
				continue
			}
			for _, i := range ifs {
				if len(i.Hwaddr) == 0 {
					continue
				}
				for _, a := range i.Addrs {
					ips[strings.ToLower(i.Hwaddr[0])] = append(ips[strings.ToLower(i.Hwaddr[0])], a.Addr)
				}
			}
			if len(ips) > 0 {
				break
			}
		}
	}
	var macs []string
	for m := range ips {
		macs = append(macs, m)
	}
	sort.Strings(macs)
	var out []string
	for _, m := range macs {
		var v4 []string
		for _, a := range ips[m] {
			if isProbeIPv4(a) {
				v4 = append(v4, a)
			}
		}
		sort.Strings(v4)
		if len(v4) > 0 {
			out = append(out, strings.ToLower(m)+"="+strings.Join(v4, ","))
		}
	}
	return strings.Join(out, ";")
}

// isProbeIPv4 is true for an address the probe can ask for: IPv4, not loopback or link-local.
func isProbeIPv4(a string) bool {
	a = strings.SplitN(a, "/", 2)[0]
	parts := strings.Split(a, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return parts[0] != "127" && !(parts[0] == "169" && parts[1] == "254") && parts[0] != "0"
}

// failoverChecks gathers the facts for the replica dom on the standby h and decides. It takes up to about seven
// seconds, because it waits for a source node that may be down.
func (s *Server) failoverChecks(h *hostconn.Host, dom libvirt.Domain, rm *replicaMeta, meta string) foChecks {
	f := foFacts{Source: rm.SourceName, Standby: h.Name}
	uuid := cluster.FormatUUID(dom.UUID)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.Asked, f.AskError, f.VMState = s.askSource(h, rm, uuid)
	}()
	go func() {
		defer wg.Done()
		f.NICs = s.probeNICs(h, dom, rm)
	}()
	wg.Wait()
	return decideFailoverChecks(f)
}

// reverseTrust remembers the pairs (source, standby) whose reverse peer trust was set up since this controller
// started.
var reverseTrust sync.Map

// ensureStandbyCanAskSource lets the standby node ask the source node's node command directly (the other
// direction of the trust the copies use). The checks before an unplanned failover need it: that is the moment the
// controller cannot set anything up any more. Best effort; once per pair and controller start.
func (s *Server) ensureStandbyCanAskSource(srcID int64, src, dst *hostconn.Host, dstID int64) {
	key := fmt.Sprintf("%d>%d", srcID, dstID)
	if _, done := reverseTrust.Load(key); done {
		return
	}
	sn, ok := s.nodeStatus(srcID)
	if !ok {
		return
	}
	ep, err := endpointFor(sn.Node)
	if err != nil {
		return
	}
	if err := s.ensurePeerTrust(dst, src, sn.Node, ep); err != nil {
		log.Printf("replication of %s: %s cannot ask %s directly (the checks before an unplanned failover will say so): %v", src.Name, dst.Name, src.Name, err)
		return
	}
	reverseTrust.Store(key, true)
}

// askSource asks the source node, from the standby, whether the VM runs there.
func (s *Server) askSource(h *hostconn.Host, rm *replicaMeta, uuid string) (asked bool, askErr, state string) {
	ns, ok := s.nodeStatus(rm.SourceNode)
	if !ok {
		return false, "", ""
	}
	ep, err := endpointFor(ns.Node)
	if err != nil {
		return false, "", ""
	}
	st, err := h.PeerAsk(ep.Host, ep.port(), ep.User, "vm-state", uuid, 0)
	if err != nil {
		return true, err.Error(), ""
	}
	return true, "", st
}

// probeNICs looks for the VM's addresses on each bridge the standby shares.
func (s *Server) probeNICs(h *hostconn.Host, dom libvirt.Domain, rm *replicaMeta) []nicProbe {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLSecure|libvirt.DomainXMLInactive)
	if err != nil {
		return nil
	}
	var x domainXML
	if xml.Unmarshal([]byte(raw), &x) != nil {
		return nil
	}
	known := parseVMAddrs(rm.Addrs)
	nets := map[string]string{} // network name -> bridge it uses, "" when it exists per node
	var nics []nicProbe
	for _, i := range x.Devices.Interfaces {
		n := nicProbe{MAC: strings.ToLower(i.MAC.Address)}
		switch i.Type {
		case "bridge":
			n.Bridge = i.Source.Bridge
		case "network":
			br, seen := nets[i.Source.Network]
			if !seen {
				if nw, err := h.NetworkLookupByName(i.Source.Network); err == nil {
					if nx, err := h.NetworkGetXMLDesc(nw, 0); err == nil {
						if nxm, err := parseNetXML(nx); err == nil {
							if info := nxm.info(); info.Forward == "bridge" {
								br = info.Bridge
							}
						}
					}
				}
				nets[i.Source.Network] = br
			}
			n.Bridge = br
		}
		n.IPs = known[n.MAC]
		switch {
		case n.Bridge == "":
			n.Skip = "nat"
		case len(n.IPs) == 0:
			n.Skip = "noaddr"
		}
		nics = append(nics, n)
	}
	// One probe per bridge, for all of its cards' addresses.
	byBridge := map[string][]int{}
	for i, n := range nics {
		if n.Skip == "" {
			byBridge[n.Bridge] = append(byBridge[n.Bridge], i)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for br, idx := range byBridge {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ips []string
			seen := map[string]bool{}
			for _, i := range idx {
				for _, ip := range nics[i].IPs {
					if !seen[ip] && len(ips) < 16 {
						seen[ip] = true
						ips = append(ips, ip)
					}
				}
			}
			replies, tool, problem, err := h.VMProbe(br, ips)
			mu.Lock()
			defer mu.Unlock()
			for _, i := range idx {
				switch {
				case err != nil || tool == "none":
					nics[i].Skip = "notool"
				case problem == "nobridge":
					nics[i].Skip = "nobridge"
				case problem == "noaddr":
					nics[i].Skip = "nobraddr"
				default:
					nics[i].Replies = replies
				}
			}
		}()
	}
	wg.Wait()
	return nics
}
