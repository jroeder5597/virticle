package api

import (
	"strings"
	"testing"
)

func TestCheckBridge(t *testing.T) {
	nics := []hostNIC{
		{Name: "eth0", Type: "physical"},
		{Name: "virbr0", Type: "bridge", Kind: "bridge"},
		{Name: "ovsbr0", Type: "other", Kind: "openvswitch"},
		{Name: "tap1", Type: "tap", Kind: "tun"},
	}
	for _, ok := range []string{"virbr0", "ovsbr0"} {
		if err := checkBridge(nics, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if err := checkBridge(nics, "eth0"); err == nil || !strings.Contains(err.Error(), "eth0 is a physical interface of this node, not a bridge") {
		t.Errorf("a physical interface: %v", err)
	}
	if err := checkBridge(nics, "tap1"); err == nil || !strings.Contains(err.Error(), "not a bridge") {
		t.Errorf("a tap: %v", err)
	}
	if err := checkBridge(nics, "br-nope"); err == nil || !strings.Contains(err.Error(), "no interface named br-nope") {
		t.Errorf("a missing name: %v", err)
	}
}
