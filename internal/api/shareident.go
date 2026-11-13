package api

import (
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"virticle/internal/hostconn"
)

// The same NFS server under two spellings.
//
// libvirt keeps the server of a pool as the text it was given. One node may have been given the name of the
// server, another its address: the pools then look different although they show the same files. If they were
// taken for different shares, a migration would copy a disk file onto itself. So when two pools have the same
// export at the same mount path and their servers are spelled differently, each node is asked what its spelling
// resolves to, and when the answers share an address the two are the same share.

var (
	resolveCache = struct {
		sync.Mutex
		m map[string]resolved
	}{m: map[string]resolved{}}
	resolveTTL = 5 * time.Minute
	// resolveServerFn asks a node for the IPv4 addresses of a server name; replaced in tests.
	resolveServerFn = resolveServerOnNode
)

type resolved struct {
	at    time.Time
	addrs []string
}

// resolveServerOnNode returns the IPv4 addresses the node finds for the server (the address itself for an address).
func resolveServerOnNode(h *hostconn.Host, server string) []string {
	if !nfsServerRE.MatchString(server) {
		return nil
	}
	key := h.Name + "\x00" + server
	resolveCache.Lock()
	c, ok := resolveCache.m[key]
	resolveCache.Unlock()
	if ok && time.Since(c.at) < resolveTTL {
		return c.addrs
	}
	out, err := h.Run("timeout 5 getent ahostsv4 " + shq(server) + " | awk '{print $1}' | sort -u")
	var addrs []string
	if err == nil {
		for _, l := range strings.Fields(out) {
			addrs = append(addrs, l)
		}
	}
	resolveCache.Lock()
	resolveCache.m[key] = resolved{time.Now(), addrs}
	resolveCache.Unlock()
	return addrs
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// unifyServers makes the destination's pool description use the source's spelling of the server where both
// nodes show the same NFS server under different spellings of one address.
func unifyServers(src, dst *hostconn.Host, sp, dp map[string]poolDesc) {
	for dir, s := range sp {
		d, ok := dp[dir]
		if !ok || !s.isNFS() || !d.isNFS() || sameShare(s, d) || path.Clean(s.Export) != path.Clean(d.Export) {
			continue
		}
		if intersects(resolveServerFn(src, s.Server), resolveServerFn(dst, d.Server)) {
			d.Server = s.Server
			dp[dir] = d
		}
	}
}

// unifyNFS does the same for the shares of the whole datacenter: pools of one name that differ only in the
// spelling of the server get the spelling of the first. hosts maps a node id to its connection.
func unifyNFS(pools []nodeNFS, hosts map[int64]*hostconn.Host) {
	sort.SliceStable(pools, func(i, j int) bool { return pools[i].Node < pools[j].Node })
	first := map[string]int{} // pool name -> index of the first
	for i := range pools {
		p := &pools[i]
		j, ok := first[p.Pool]
		if !ok {
			first[p.Pool] = i
			continue
		}
		f := pools[j]
		if strings.EqualFold(f.Server, p.Server) || path.Clean(f.Export) != path.Clean(p.Export) || path.Clean(f.Target) != path.Clean(p.Target) {
			continue
		}
		hf, hp := hosts[f.Node], hosts[p.Node]
		if hf == nil || hp == nil {
			continue
		}
		if intersects(resolveServerFn(hf, f.Server), resolveServerFn(hp, p.Server)) {
			p.Server = f.Server
		}
	}
}

// revalidateNFSDirs lists the directories of the files that lie on an NFS pool of the node. An NFS client keeps
// the answer "no such file" for a while; a file another node made seconds ago would still look missing, and the
// check for a name clash on the target would pass although the file is there. Reading the directory makes the
// client look again.
func revalidateNFSDirs(h *hostconn.Host, files []string) {
	pools, err := poolDescs(h)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, f := range files {
		dir := path.Dir(f)
		if p, ok := pools[dir]; ok && p.isNFS() && !seen[dir] && safePathRE.MatchString(dir) {
			seen[dir] = true
			h.Run("timeout 5 ls -f " + shq(dir) + " >/dev/null 2>&1; true")
		}
	}
}

// shareID names the NFS share behind a pool: the server's address (what the node's spelling of it resolves to),
// the export and the mount path. It is empty for a pool that is not NFS, which is local to its node.
func shareID(h *hostconn.Host, p poolDesc) string {
	if !p.isNFS() {
		return ""
	}
	id := strings.ToLower(p.Server)
	if addrs := resolveServerFn(h, p.Server); len(addrs) > 0 {
		sort.Strings(addrs)
		id = addrs[0]
	}
	return id + ":" + path.Clean(p.Export) + "@" + path.Clean(p.Target)
}

// dedupShared keeps one entry for each backup on a shared pool, the one of the node with the lowest id. Entries
// on local pools are all kept.
func dedupShared(in []backupEntry) []backupEntry {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Node < in[j].Node })
	seen := map[string]bool{}
	var out []backupEntry
	for _, e := range in {
		if e.share != "" {
			k := e.share + "/" + e.Dir
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, e)
	}
	return out
}
