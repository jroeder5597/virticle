package api

import (
	"context"
	"strings"
	"testing"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

func mixedHost(name string, p hostconn.Privilege) *hostconn.Host {
	h := &hostconn.Host{Name: name}
	h.SetPrivilege(p)
	return h
}

// A pair of which only one node is in helper mode cannot copy files between
// the nodes: the pre-flight row is red and says what to do. (The pairs that
// need no file copy pass without opening a connection; that part needs live
// nodes and is exercised on the lab.)
func TestCheckNodeTransferMixedPairIsRed(t *testing.T) {
	plan := &migPlan{Files: fileSet{list: []*migFile{{Path: "/var/lib/libvirt/images/a.qcow2", Owned: true, Size: 1}}}}
	node := store.Node{Addr: "192.0.2.10", User: "virticle"}
	s := &Server{}
	for _, c := range []struct{ src, dst hostconn.Privilege }{
		{hostconn.PrivHelper, hostconn.PrivRoot},
		{hostconn.PrivRoot, hostconn.PrivHelper},
		{hostconn.PrivHelper, hostconn.PrivSudo},
		{hostconn.PrivSudo, hostconn.PrivHelper},
	} {
		var checks checkList
		s.checkNodeTransfer(mixedHost("a", c.src), mixedHost("b", c.dst), node, plan, &checks)
		if len(checks) != 1 || checks[0].OK || checks[0].Check != "SSH between nodes" || checks[0].Severity != "error" {
			t.Fatalf("%s -> %s: %+v", c.src, c.dst, checks)
		}
		if !strings.Contains(checks[0].Message, "set up differently") || !strings.Contains(checks[0].Message, "1 file(s)") {
			t.Errorf("%s -> %s: message %q", c.src, c.dst, checks[0].Message)
		}
		if !checks.blocked() {
			t.Errorf("%s -> %s: the migration is not blocked", c.src, c.dst)
		}
	}
}

func TestCopyToNodeRefusesMixedPair(t *testing.T) {
	a, b := mixedHost("a", hostconn.PrivHelper), mixedHost("b", hostconn.PrivRoot)
	err := copyToNode(context.Background(), a, b, migEndpoint{User: "root", Host: "192.0.2.10"}, "/x/a", "/x/b", "600", nil)
	if err == nil || !strings.Contains(err.Error(), "one node uses the scoped helper account") {
		t.Errorf("copy between a helper node and a root node: %v", err)
	}
}
