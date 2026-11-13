package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"virticle/internal/nodeproto"
)

type fakeNetNode struct {
	status    nodeproto.NetStatus
	statusErr error
	libvirt   error
	confirmed []string
}

func (f *fakeNetNode) NetStatus() (nodeproto.NetStatus, error) { return f.status, f.statusErr }
func (f *fakeNetNode) NetConfirm(id string) error {
	f.confirmed = append(f.confirmed, id)
	return nil
}
func (f *fakeNetNode) LibvirtAnswers() error { return f.libvirt }

// fakeWaiter drives netWaiter with a fake clock; script(i) returns the node (or error) for the i-th look.
func fakeWaiter(script func(i int) (netNode, error)) (netWaiter, *int, *time.Time) {
	clock := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	looks, nudges := 0, 0
	w := netWaiter{
		node:      func() (netNode, error) { looks++; return script(looks) },
		reconnect: func() { nudges++ },
		now:       func() time.Time { return clock },
		sleep:     func(d time.Duration) { clock = clock.Add(d) },
	}
	return w, &nudges, &clock
}

func statusWith(state, reason string) nodeproto.NetStatus {
	return nodeproto.NetStatus{Recent: []nodeproto.NetChange{{ID: "c1", State: state, Reason: reason}}}
}

func TestHostnetWaitConfirmsWhenNodeIsBack(t *testing.T) {
	n := &fakeNetNode{status: statusWith("applied", "")}
	w, _, clock := fakeWaiter(func(int) (netNode, error) { return n, nil })
	if err := w.wait("c1", clock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.confirmed) != 1 || n.confirmed[0] != "c1" {
		t.Errorf("confirmed %v", n.confirmed)
	}
}

func TestHostnetWaitSurvivesTheConnectionDropping(t *testing.T) {
	n := &fakeNetNode{status: statusWith("applied", "")}
	w, nudges, clock := fakeWaiter(func(i int) (netNode, error) {
		switch {
		case i <= 3:
			return nil, errors.New("node is offline")
		case i <= 5:
			return &fakeNetNode{statusErr: errors.New("connection reset")}, nil
		}
		return n, nil
	})
	if err := w.wait("c1", clock.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.confirmed) != 1 || *nudges == 0 {
		t.Errorf("confirmed %v, reconnects %d", n.confirmed, *nudges)
	}
}

func TestHostnetWaitWaitsWhileApplying(t *testing.T) {
	n := &fakeNetNode{status: statusWith("applying", "")}
	w, _, clock := fakeWaiter(func(i int) (netNode, error) {
		if i == 4 {
			n.status = statusWith("applied", "")
		}
		return n, nil
	})
	if err := w.wait("c1", clock.Add(time.Minute)); err != nil || len(n.confirmed) != 1 {
		t.Fatalf("%v %v", err, n.confirmed)
	}
}

func TestHostnetWaitReportsARollback(t *testing.T) {
	for reason, want := range map[string]string{"timer": "not confirmed in time", "apply failed": "Applying the change failed", "check failed": "did not work"} {
		n := &fakeNetNode{status: statusWith("rolled-back", reason)}
		w, _, clock := fakeWaiter(func(int) (netNode, error) { return n, nil })
		err := w.wait("c1", clock.Add(time.Minute))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "previous network settings") {
			t.Errorf("%s: %v", reason, err)
		}
		if len(n.confirmed) != 0 {
			t.Errorf("%s: a rolled back change was confirmed", reason)
		}
	}
}

func TestHostnetWaitGivesUpWithThePlainMessage(t *testing.T) {
	w, nudges, clock := fakeWaiter(func(int) (netNode, error) { return nil, errors.New("node is offline") })
	err := w.wait("c1", clock.Add(90*time.Second))
	if err == nil || err.Error() != MsgNodeDidNotReturn {
		t.Errorf("error %v", err)
	}
	if *nudges < 2 || *nudges > 12 {
		t.Errorf("reconnect nudges: %d", *nudges)
	}
}

func TestHostnetWaitDoesNotConfirmWhileLibvirtIsSilent(t *testing.T) {
	n := &fakeNetNode{status: statusWith("applied", ""), libvirt: errors.New("no answer")}
	w, _, clock := fakeWaiter(func(int) (netNode, error) { return n, nil })
	err := w.wait("c1", clock.Add(30*time.Second))
	if err == nil || err.Error() != MsgNodeDidNotReturn || len(n.confirmed) != 0 {
		t.Errorf("%v %v", err, n.confirmed)
	}
}

func TestCheckHostnetRequest(t *testing.T) {
	ok := nodeproto.NetRequest{Action: nodeproto.NetCreate, Bridge: "br0", VLANAware: true, LibvirtVersion: 11003000}
	if err := checkHostnetRequest(ok); err != nil {
		t.Error(err)
	}
	old := ok
	old.LibvirtVersion = 10009000
	if err := checkHostnetRequest(old); err == nil || !strings.Contains(err.Error(), "libvirt 11.0.0") {
		t.Errorf("old libvirt: %v", err)
	}
	for _, r := range []nodeproto.NetRequest{{Action: "x", Bridge: "b"}, {Action: nodeproto.NetCreate}} {
		if err := checkHostnetRequest(r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
	del := nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0", VLANAware: true}
	if err := checkHostnetRequest(del); err != nil {
		t.Errorf("delete does not depend on the libvirt version: %v", err)
	}
}

func TestBridgeInUse(t *testing.T) {
	if bridgeInUse("br0", nil, nil) != nil {
		t.Error("free bridge refused")
	}
	if err := bridgeInUse("br0", []string{"lab"}, nil); err == nil || !strings.Contains(err.Error(), "network lab") {
		t.Errorf("network: %v", err)
	}
	if err := bridgeInUse("br0", nil, []string{"web1"}); err == nil || !strings.Contains(err.Error(), "VM web1") {
		t.Errorf("vm: %v", err)
	}
}

func TestDomainBridges(t *testing.T) {
	raw := `<domain><devices>
<interface type='bridge'><source bridge='br0'/></interface>
<interface type='network'><source network='default' bridge='virbr0'/></interface>
<interface type='bridge'><source bridge='br7'/></interface></devices></domain>`
	got := domainBridges(raw)
	if len(got) != 2 || got[0] != "br0" || got[1] != "br7" {
		t.Errorf("%v", got)
	}
}

func TestManagementInterface(t *testing.T) {
	nics := []hostNIC{{Name: "eth0", Addresses: []string{"192.0.2.10/24"}}, {Name: "eth1", Addresses: []string{"198.51.100.7/24"}}}
	if got := managementInterface(nics, []string{"198.51.100.7"}); got != "eth1" {
		t.Errorf("got %q", got)
	}
	if got := managementInterface(nics, []string{"203.0.113.1"}); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestLibvirtNetworkOnVLANAwareBridge(t *testing.T) {
	aware := &linuxVLAN{LibvirtVersion: 11003000, Found: true, Aware: true, VLANs: "10,20-30"}
	req := func(tag int, v *linuxVLAN) networkRequest {
		return networkRequest{Name: "n1", Mode: "bridge", BridgeName: "br0", VLAN: tag, linuxVLAN: v}
	}
	got, err := req(25, aware).build(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"<forward mode='bridge'/>", "<bridge name='br0'/>", "<vlan><tag id='25'/></vlan>"} {
		if !strings.Contains(got, w) {
			t.Errorf("XML lacks %s:\n%s", w, got)
		}
	}
	if strings.Contains(got, "virtualport") {
		t.Errorf("a Linux bridge has no virtualport:\n%s", got)
	}
	// the default VLAN is always possible; no tag, no <vlan>
	if _, err := req(1, aware).build(nil); err != nil {
		t.Error(err)
	}
	if got, _ := req(0, nil).build(nil); strings.Contains(got, "<vlan>") {
		t.Errorf("untagged network has a vlan:\n%s", got)
	}
	bad := []struct {
		name string
		r    networkRequest
		want string
	}{
		{"not allowed", req(15, aware), "not allowed on the bridge br0"},
		{"out of range", req(5000, aware), "between 1 and 4094"},
		{"unknown node state", req(10, nil), "cannot tell"},
		{"not a linux bridge of the node", req(10, &linuxVLAN{LibvirtVersion: 11003000}), "cannot tell"},
		{"libvirt too old", req(10, &linuxVLAN{LibvirtVersion: 10009000, Found: true, Aware: true, VLANs: "2-4094"}), "libvirt 11.0.0"},
		{"not vlan aware", req(10, &linuxVLAN{LibvirtVersion: 11003000, Found: true}), "not VLAN-aware"},
		{"no ids allowed", req(10, &linuxVLAN{LibvirtVersion: 11003000, Found: true, Aware: true}), "its uplink allows: none"},
	}
	for _, c := range bad {
		_, err := c.r.build(nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		} else if _, ok := err.(badRequest); !ok {
			t.Errorf("%s: not a badRequest", c.name)
		}
	}
	// Open vSwitch keeps working as before
	ovs := networkRequest{Name: "n1", Mode: "bridge", BridgeName: "ovsbr0", OVS: true, VLAN: 20}
	if got, err := ovs.build(nil); err != nil || !strings.Contains(got, "<virtualport type='openvswitch'/>") {
		t.Errorf("ovs: %v\n%s", err, got)
	}
}

func TestVLANStillUsed(t *testing.T) {
	nets := []networkInfo{{Name: "a", Bridge: "br0", VLAN: 10}, {Name: "b", Bridge: "br1", VLAN: 99}, {Name: "c", Bridge: "br0"}}
	req := nodeproto.NetRequest{Action: nodeproto.NetSetVLAN, Bridge: "br0", VLANAware: true, VLANs: "10,20-30"}
	if err := vlanStillUsed(req, nets); err != nil {
		t.Errorf("tag 10 stays allowed: %v", err)
	}
	req.VLANs = "20-30"
	if err := vlanStillUsed(req, nets); err == nil || !strings.Contains(err.Error(), "network a uses VLAN 10") {
		t.Errorf("tag 10 would be cut: %v", err)
	}
	req.VLANAware, req.VLANs = false, ""
	if err := vlanStillUsed(req, nets); err == nil {
		t.Error("turning filtering off cuts tag 10")
	}
	req.Bridge = "br2"
	if err := vlanStillUsed(req, nets); err != nil {
		t.Errorf("other bridge: %v", err)
	}
}

func TestNextSwitchName(t *testing.T) {
	for _, c := range []struct {
		taken []string
		want  string
	}{
		{nil, "vsw0"},
		{[]string{"eth0", "vsw0"}, "vsw1"},
		{[]string{"vsw1"}, "vsw0"},
		{[]string{"vsw0", "vsw1", "vsw2"}, "vsw3"},
	} {
		if got := nextSwitchName(c.taken); got != c.want {
			t.Errorf("nextSwitchName(%v) = %s, want %s", c.taken, got, c.want)
		}
	}
}

func TestSameNetworkPeers(t *testing.T) {
	br := func(name string, managed, aware bool, vlans string) nodeproto.NetBridge {
		return nodeproto.NetBridge{Name: name, Managed: managed, VLANFiltering: aware, VLANs: vlans}
	}
	others := []peerNode{
		{Node: 2, Name: "b", Bridges: []nodeproto.NetBridge{br("vsw0", true, true, "2-4094")}},
		{Node: 3, Name: "a", Bridges: []nodeproto.NetBridge{br("vsw0", true, true, "10,20")}},
		{Node: 4, Name: "c", Bridges: []nodeproto.NetBridge{br("vsw0", false, true, "2-4094")}},                            // not made by Virticle
		{Node: 5, Name: "d", Bridges: []nodeproto.NetBridge{br("vsw0", true, false, "")}},                                  // no VLANs
		{Node: 6, Name: "e", Bridges: []nodeproto.NetBridge{br("vsw0", true, true, "2-4094")}, Networks: []string{"vs-a"}}, // has it
		{Node: 7, Name: "f", Bridges: []nodeproto.NetBridge{br("vsw1", true, true, "2-4094")}},                             // other name
	}
	names := func(c []peerChoice) string {
		s := ""
		for _, p := range c {
			s += p.Name
		}
		return s
	}
	if got := names(sameNetworkPeers("vsw0", "vs-a", 30, others)); got != "b" {
		t.Errorf("vlan 30: %q, want b", got)
	}
	if got := names(sameNetworkPeers("vsw0", "vs-a", 20, others)); got != "ab" {
		t.Errorf("vlan 20: %q, want ab", got)
	}
	if got := names(sameNetworkPeers("vsw0", "vs-a", 0, others)); got != "abd" {
		t.Errorf("untagged: %q, want abd", got)
	}
}
