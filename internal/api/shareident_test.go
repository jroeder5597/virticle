package api

import (
	"testing"

	"virticle/internal/hostconn"
)

// One server, spelled as a name on one node and as its address on the other: the same share.
func TestUnifyServers(t *testing.T) {
	old := resolveServerFn
	defer func() { resolveServerFn = old }()
	a, b := &hostconn.Host{Name: "a"}, &hostconn.Host{Name: "b"}
	resolveServerFn = func(h *hostconn.Host, server string) []string {
		switch server {
		case "nas.lan":
			return []string{"10.0.0.5"}
		case "other.lan":
			return []string{"10.0.0.9"}
		}
		return []string{server}
	}
	mk := func(server, export string) map[string]poolDesc {
		return map[string]poolDesc{"/mnt/s": {Name: "s", Type: "netfs", Target: "/mnt/s", Server: server, Export: export, Active: true}}
	}
	sp, dp := mk("10.0.0.5", "/export"), mk("nas.lan", "/export")
	unifyServers(a, b, sp, dp)
	if !sameShare(sp["/mnt/s"], dp["/mnt/s"]) {
		t.Errorf("a name and an address of one server are one share: %+v", dp["/mnt/s"])
	}
	sp, dp = mk("10.0.0.5", "/export"), mk("other.lan", "/export")
	unifyServers(a, b, sp, dp)
	if sameShare(sp["/mnt/s"], dp["/mnt/s"]) {
		t.Error("different servers stay different")
	}
	sp, dp = mk("10.0.0.5", "/export"), mk("nas.lan", "/other")
	unifyServers(a, b, sp, dp)
	if sameShare(sp["/mnt/s"], dp["/mnt/s"]) {
		t.Error("a different export is a different share")
	}
}

func TestUnifyNFS(t *testing.T) {
	old := resolveServerFn
	defer func() { resolveServerFn = old }()
	resolveServerFn = func(h *hostconn.Host, server string) []string {
		if server == "nas.lan" {
			return []string{"10.0.0.5"}
		}
		return []string{server}
	}
	hosts := map[int64]*hostconn.Host{1: {Name: "a"}, 2: {Name: "b"}}
	pools := []nodeNFS{
		{Node: 2, Pool: "s", Server: "nas.lan", Export: "/e", Target: "/mnt/s"},
		{Node: 1, Pool: "s", Server: "10.0.0.5", Export: "/e", Target: "/mnt/s"},
	}
	unifyNFS(pools, hosts)
	g := groupShared(pools, []sharedMember{{Node: 1}, {Node: 2}})
	if len(g) != 1 || len(g[0].Nodes) != 2 || len(g[0].Differ) != 0 {
		t.Errorf("%+v", g)
	}
}
