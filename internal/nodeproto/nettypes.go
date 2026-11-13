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
	Uplink string `json:"uplink,omitempty"`
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
	VLANs          string   `json:"vlans,omitempty"` // the ids the uplink allows (managed bridges)
	Uplink         string   `json:"uplink,omitempty"`
	Ports          []string `json:"ports"`
	MovedAddresses bool     `json:"movedAddresses,omitempty"`
}

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
