package nodeproto

import (
	"strconv"
	"strings"
)

// BondStatus is what /proc/net/bonding/NAME says about a bond, reduced to what the UI shows.
type BondStatus struct {
	Name string `json:"name"`
	// Mode is "active-backup", "802.3ad" or the kernel's own words for another mode.
	Mode string `json:"mode"`
	// ActiveMember is the member that carries the traffic in active-backup mode.
	ActiveMember string       `json:"activeMember,omitempty"`
	Members      []BondMember `json:"members"`
	// LACP is set in 802.3ad mode.
	LACP *BondLACPState `json:"lacp,omitempty"`
}

// BondMember is one card of a bond.
type BondMember struct {
	Name         string `json:"name"`
	Up           bool   `json:"up"` // the bond's own view of the link (MII status)
	SpeedMbps    int    `json:"speedMbps,omitempty"`
	LinkFailures int    `json:"linkFailures,omitempty"`
}

// BondLACPState is the state of the LACP negotiation. Answered is false when the switch at the other end sent
// nothing (the partner MAC address is all zeros): the bond then passes traffic on one link at best.
type BondLACPState struct {
	Answered   bool   `json:"answered"`
	PartnerMAC string `json:"partnerMac,omitempty"`
}

func zeroMAC(m string) bool {
	m = strings.TrimSpace(m)
	return m == "" || strings.Trim(m, "0:") == ""
}

// ParseBonding reads the text of /proc/net/bonding/NAME.
func ParseBonding(name, text string) BondStatus {
	st := BondStatus{Name: name, Members: []BondMember{}}
	var cur *BondMember
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case k == "Bonding Mode":
			switch {
			case strings.Contains(v, "active-backup"):
				st.Mode = "active-backup"
			case strings.Contains(v, "802.3ad"):
				st.Mode = "802.3ad"
			default:
				st.Mode = v
			}
		case k == "Currently Active Slave":
			if v != "None" {
				st.ActiveMember = v
			}
		case k == "Slave Interface":
			st.Members = append(st.Members, BondMember{Name: v})
			cur = &st.Members[len(st.Members)-1]
			section = "slave"
		case k == "Active Aggregator Info":
			section = "aggregator"
		case k == "details partner lacp pdu":
			section = "partner"
		case k == "details actor lacp pdu":
			section = "actor"
		case cur != nil && k == "MII Status":
			cur.Up = v == "up"
		case cur != nil && k == "Speed":
			cur.SpeedMbps, _ = strconv.Atoi(strings.Fields(v + " ")[0])
		case cur != nil && k == "Link Failure Count":
			cur.LinkFailures, _ = strconv.Atoi(v)
		case section == "aggregator" && k == "Partner Mac Address":
			if st.LACP == nil {
				st.LACP = &BondLACPState{}
			}
			st.LACP.PartnerMAC = v
			st.LACP.Answered = !zeroMAC(v)
		}
	}
	if st.Mode == "802.3ad" && st.LACP == nil {
		st.LACP = &BondLACPState{}
	}
	return st
}
