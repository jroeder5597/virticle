package nodeproto

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The JSON shapes of the host network verbs (net-info, net-plan, net-apply,
// net-confirm, net-rollback, net-status). The node command prints them, the
// controller reads them.

// Actions of a NetRequest.
const (
	NetCreate  = "create"    // create a Linux bridge
	NetDelete  = "delete"    // delete a bridge this feature created
	NetSetVLAN = "set-vlans" // change the VLAN settings of a bridge this feature created

	NetSetUplinks  = "set-uplinks"         // change the physical cards of a bridge this feature created (a bond when there are two or more)
	NetAddVLANAddr = "add-vlan-address"    // give the node an address on a VLAN of a VLAN-aware bridge
	NetDelVLANAddr = "remove-vlan-address" // remove such an address
	NetSetMTU      = "set-mtu"             // change the MTU of a bridge this feature created, on every layer
)

// Bond modes of a virtual switch with two or more uplinks.
const (
	BondFailover = "failover" // active-backup: one card carries the traffic, another takes over; works with any switch
	BondLACP     = "lacp"     // 802.3ad: needs LACP on the switch the cards are plugged into
)

// Ways to get the node's address on a VLAN.
const (
	AddrDHCP   = "dhcp"
	AddrStatic = "static"
)

// States of a NetChange.
const (
	NetArmed      = "armed"       // saved, rollback timer armed, apply not started
	NetApplying   = "applying"    // the change is being applied
	NetApplied    = "applied"     // applied, waiting for the controller to confirm
	NetConfirmed  = "confirmed"   // permanent
	NetRolledBack = "rolled-back" // the previous settings were restored
	NetFailed     = "failed"      // the restore itself did not complete: see Message
)

// Limits of the rollback timer, in seconds.
const (
	NetDefaultTimeout = 90
	NetMinTimeout     = 30
	NetMaxTimeout     = 600
)

// LibvirtVLANMin is the libvirt version (as libvirt numbers it: 11.0.0) from
// which a VLAN tag works on interfaces of a Linux bridge.
const LibvirtVLANMin = 11000000

// NetRequest is the change to make; it goes to stdin of net-plan and net-apply.
type NetRequest struct {
	Action string `json:"action"`
	Bridge string `json:"bridge"`
	// Uplink is the one physical interface that becomes a port of a new bridge (optional).
	// Uplinks lists every physical card of the switch (create, set-uplinks): two or more make a bond that
	// is the bridge's single port. Uplink and Uplinks together are an error.
	Uplink  string   `json:"uplink,omitempty"`
	Uplinks []string `json:"uplinks,omitempty"`
	// BondMode is failover (default) or lacp; only with two or more uplinks, or on a switch that has a bond.
	BondMode string `json:"bondMode,omitempty"`
	// The node's own address on a VLAN (add-vlan-address, remove-vlan-address): VLANID, then
	// AddressMethod dhcp or static; Address is a CIDR for static. Gateway (static) and DefaultRoute make
	// the VLAN carry the node's default route; without them the VLAN adds no default route.
	VLANID        int    `json:"vlanId,omitempty"`
	AddressMethod string `json:"addressMethod,omitempty"`
	Address       string `json:"address,omitempty"`
	Gateway       string `json:"gateway,omitempty"`
	DefaultRoute  bool   `json:"defaultRoute,omitempty"`
	// MTU of the switch on every layer (create, set-mtu); 0 leaves the cards as they are. 1280..9000.
	MTU int `json:"mtu,omitempty"`
	// MoveAddresses: the bridge takes over the addresses, routes and DNS of the uplink.
	MoveAddresses bool `json:"moveAddresses,omitempty"`
	// VLANAware turns on VLAN filtering on the bridge. VLANs are the ids the uplink allows
	// ("2-4094" when empty), e.g. "10,20,30-40".
	VLANAware bool   `json:"vlanAware,omitempty"`
	VLANs     string `json:"vlans,omitempty"`
	// TimeoutSeconds is the time the controller has to confirm (default 90, 30..600).
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// ControllerAddrs are the addresses the controller reaches the node at.
	ControllerAddrs []string `json:"controllerAddrs,omitempty"`
	// LibvirtVersion is the node's libvirt version as libvirt numbers it (11.3.0 is 11003000).
	LibvirtVersion uint64 `json:"libvirtVersion,omitempty"`
}

// NetPlan is what net-plan prints: what net-apply would do.
type NetPlan struct {
	Stack       string          `json:"stack"` // networkmanager | netplan-networkd | networkd | ifupdown
	StackTested bool            `json:"stackTested"`
	Summary     []string        `json:"summary"` // plain sentences
	Files       []NetPlanFile   `json:"files,omitempty"`
	Commands    []string        `json:"commands,omitempty"` // in order
	AddressMove *NetAddressMove `json:"addressMove,omitempty"`
	// ManagementInterface is the interface that carries the controller's connection ("" when not known).
	ManagementInterface string   `json:"managementInterface,omitempty"`
	ManagementAffected  bool     `json:"managementAffected"`
	Warnings            []string `json:"warnings,omitempty"`
	TimeoutSeconds      int      `json:"timeoutSeconds"`
}

// NetPlanFile is one file the change writes or removes.
type NetPlanFile struct {
	Path    string `json:"path"`
	Action  string `json:"action"` // write | remove
	Content string `json:"content,omitempty"`
}

// NetAddressMove describes which configuration moves where.
type NetAddressMove struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Method    string   `json:"method"` // dhcp | static
	Addresses []string `json:"addresses"`
	Gateway   string   `json:"gateway,omitempty"`
	MAC       string   `json:"mac,omitempty"` // the bridge gets the uplink's MAC address
}

// NetApplyResult is what net-apply prints as soon as the change is saved, the rollback is armed and
// the apply has been started.
type NetApplyResult struct {
	ID             string `json:"id"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// NetChange is one change in net-status.
type NetChange struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Bridge      string `json:"bridge"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"` // for rolled-back: timer | manual | apply failed | check failed
	Message     string `json:"message,omitempty"`
	Started     string `json:"started"`            // RFC 3339
	Finished    string `json:"finished,omitempty"` // RFC 3339
	SecondsLeft int    `json:"secondsLeft,omitempty"`
}

// NetStatus is what net-status prints.
type NetStatus struct {
	Pending *NetChange  `json:"pending,omitempty"`
	Recent  []NetChange `json:"recent"` // newest first, the pending one included
}

// NetBridge is one Linux bridge of the node.
type NetBridge struct {
	Name           string   `json:"name"`
	Managed        bool     `json:"managed"` // created by this feature: may be changed and deleted
	VLANFiltering  bool     `json:"vlanFiltering"`
	VLANs          string   `json:"vlans,omitempty"`  // the ids the uplink allows (managed bridges)
	Uplink         string   `json:"uplink,omitempty"` // the first card
	Ports          []string `json:"ports"`
	MovedAddresses bool     `json:"movedAddresses,omitempty"`
	// Uplinks are all the physical cards; Bond is the bond device that holds them when there are
	// two or more (it stays, with one member, when cards are removed again); BondMode is failover or lacp.
	Uplinks  []string `json:"uplinks,omitempty"`
	Bond     string   `json:"bond,omitempty"`
	BondMode string   `json:"bondMode,omitempty"`
	// MTU is the MTU set for the switch (0: not set, the cards keep what they had).
	MTU int `json:"mtu,omitempty"`
	// VLANAddresses are the node's own addresses on VLANs of this switch.
	VLANAddresses []NetVLANAddr `json:"vlanAddresses,omitempty"`
}

// NetVLANAddr is one address of the node on a VLAN of a managed switch.
type NetVLANAddr struct {
	ID           int    `json:"id"`
	Interface    string `json:"interface"` // SWITCH.ID
	Method       string `json:"method"`    // dhcp | static
	Address      string `json:"address,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	DefaultRoute bool   `json:"defaultRoute,omitempty"`
}

// VLANInterface is the name of the VLAN device of a switch.
func VLANInterface(bridge string, id int) string { return fmt.Sprintf("%s.%d", bridge, id) }

// NetInfo is what net-info prints.
type NetInfo struct {
	Stack       string      `json:"stack"` // networkmanager | netplan-networkd | networkd | ifupdown | unknown
	StackTested bool        `json:"stackTested"`
	Supported   bool        `json:"supported"`
	StackNote   string      `json:"stackNote,omitempty"`
	Bridges     []NetBridge `json:"bridges"`
	Status      NetStatus   `json:"status"`
}

// ParseVLANs checks a VLAN list such as "10,20,30-40" and returns it in canonical form: ids 1..4094, sorted,
// merged ranges. Empty input means "2-4094".
func ParseVLANs(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "2-4094", nil
	}
	var set [4096]bool
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		if err == nil && isRange {
			b, err = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err != nil || a < 1 || b > 4094 || a > b {
			return "", fmt.Errorf("%q is not a VLAN id or range between 1 and 4094", part)
		}
		for i := a; i <= b; i++ {
			set[i] = true
		}
	}
	var ranges []string
	for i := 1; i <= 4094; i++ {
		if !set[i] {
			continue
		}
		j := i
		for j < 4094 && set[j+1] {
			j++
		}
		if i == j {
			ranges = append(ranges, strconv.Itoa(i))
		} else {
			ranges = append(ranges, fmt.Sprintf("%d-%d", i, j))
		}
		i = j
	}
	return strings.Join(ranges, ","), nil
}

// VLANRanges lists a canonical list as ranges.
func VLANRanges(canon string) [][2]int {
	var out [][2]int
	for _, p := range strings.Split(canon, ",") {
		lo, hi, isRange := strings.Cut(p, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			b, _ = strconv.Atoi(hi)
		}
		out = append(out, [2]int{a, b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// VLANAllowed reports whether id may be used on a VLAN-aware bridge whose uplink allows the canonical list
// canon. Id 1 is the untagged default and is always possible.
func VLANAllowed(canon string, id int) bool {
	if id == 1 {
		return true
	}
	for _, r := range VLANRanges(canon) {
		if id >= r[0] && id <= r[1] {
			return true
		}
	}
	return false
}
