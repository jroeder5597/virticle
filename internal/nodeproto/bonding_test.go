package nodeproto

import (
	"strings"
	"testing"
)

const bondActiveBackup = `Ethernet Channel Bonding Driver: v6.12.115+deb13.1-amd64

Bonding Mode: fault-tolerance (active-backup)
Primary Slave: None
Currently Active Slave: enp9s0
MII Status: up
MII Polling Interval (ms): 100
Up Delay (ms): 0
Down Delay (ms): 0
Peer Notification Delay (ms): 0

Slave Interface: enp9s0
MII Status: up
Speed: Unknown
Duplex: Unknown
Link Failure Count: 0
Permanent HW addr: 52:54:00:c3:fe:88
Slave queue ID: 0

Slave Interface: enp8s0
MII Status: down
Speed: 1000 Mbps
Duplex: full
Link Failure Count: 2
Permanent HW addr: 52:54:00:b3:33:3a
Slave queue ID: 0
`

const bondLACPHead = `Ethernet Channel Bonding Driver: v6.12.0-211.68.1.el10_2.x86_64

Bonding Mode: IEEE 802.3ad Dynamic link aggregation
Transmit Hash Policy: layer2+3 (2)
MII Status: up
MII Polling Interval (ms): 100

802.3ad info
LACP active: on
LACP rate: fast
Min links: 0
Aggregator selection policy (ad_select): stable
System priority: 65535
System MAC address: 52:54:00:47:3e:5d
Active Aggregator Info:
	Aggregator ID: 2
	Number of ports: 1
	Actor Key: 0
	Partner Key: 1
	Partner Mac Address: %PARTNER%

Slave Interface: enp8s0
MII Status: up
Speed: 1000 Mbps
Duplex: full
Link Failure Count: 0
Permanent HW addr: 52:54:00:47:3e:5d
Slave queue ID: 0
Aggregator ID: 2
details actor lacp pdu:
    system priority: 65535
    system mac address: 52:54:00:47:3e:5d
details partner lacp pdu:
    system priority: 65535
    system mac address: %PARTNER%
    port state: 0

Slave Interface: enp9s0
MII Status: up
Speed: 1000 Mbps
Duplex: full
Link Failure Count: 0
Permanent HW addr: 52:54:00:ab:00:f9
Slave queue ID: 0
Aggregator ID: 3
`

func lacpSample(partner string) string {
	return strings.ReplaceAll(bondLACPHead, "%PARTNER%", partner)
}

func TestParseBondingActiveBackup(t *testing.T) {
	st := ParseBonding("vswbond0", bondActiveBackup)
	if st.Mode != "active-backup" || st.ActiveMember != "enp9s0" || st.LACP != nil || len(st.Members) != 2 {
		t.Fatalf("%+v", st)
	}
	if m := st.Members[0]; m.Name != "enp9s0" || !m.Up || m.SpeedMbps != 0 {
		t.Errorf("member 0: %+v", m)
	}
	if m := st.Members[1]; m.Name != "enp8s0" || m.Up || m.SpeedMbps != 1000 || m.LinkFailures != 2 {
		t.Errorf("member 1: %+v", m)
	}
}

func TestParseBondingLACPWithoutPartner(t *testing.T) {
	st := ParseBonding("vswbond0", lacpSample("00:00:00:00:00:00"))
	if st.Mode != "802.3ad" || st.LACP == nil || st.LACP.Answered || st.ActiveMember != "" || len(st.Members) != 2 {
		t.Fatalf("%+v %+v", st, st.LACP)
	}
}

func TestParseBondingLACPWithPartner(t *testing.T) {
	st := ParseBonding("vswbond0", lacpSample("a0:b1:c2:d3:e4:f5"))
	if st.LACP == nil || !st.LACP.Answered || st.LACP.PartnerMAC != "a0:b1:c2:d3:e4:f5" {
		t.Fatalf("%+v", st.LACP)
	}
	if st.Members[0].SpeedMbps != 1000 || !st.Members[1].Up {
		t.Errorf("%+v", st.Members)
	}
}

func TestParseBondingEmpty(t *testing.T) {
	st := ParseBonding("x", "")
	if st.Mode != "" || len(st.Members) != 0 || st.LACP != nil {
		t.Errorf("%+v", st)
	}
}
