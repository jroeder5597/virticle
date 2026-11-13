package api

import (
	"encoding/xml"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// What the controller does not do while an NFS share does not answer.
//
// Two kinds of call wait for the server of a dead share, and both can hold up a whole node:
//
//  1. Storage calls that read the share: libvirt's refresh of the pool (it scans the directory), reading a
//     volume's file. The refresh holds the lock of the pool, so every later call about that pool, or listing
//     the pools, waits behind it. The functions below leave dead NFS pools out of every loop that refreshes or
//     reads pools.
//  2. Calls about a running VM whose disk is on the share: QEMU waits for its disk and no longer answers its
//     monitor, so libvirt calls that ask QEMU (statistics, block info) wait up to libvirt's own limit (30
//     seconds) for it. The periodic statistics round and the VM page leave those VMs out (stalledOn).
//
// What stays: libvirt calls that only read libvirt's own tables (the list of pools, their state and size, the
// state of a VM) are not touched; the probe of poolhealth.go is the one thing that looks at the mount.

// downMounts lists the mount points of the node that did not answer the last probe. It reads the prober's last
// answer and does no I/O.
func (pp *poolProber) downMounts(h *hostconn.Host) []string {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	e := pp.cache[h]
	if e == nil || pp.now().Sub(e.at) > probeStale {
		return nil
	}
	var out []string
	for p, st := range e.state {
		if st != mountUp {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// livePools lists the active pools of a node, without the NFS pools whose mount does not answer. Use it for
// every loop that refreshes pools or reads volumes.
func livePools(h *hostconn.Host) ([]libvirt.StoragePool, error) {
	pools, _, err := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive)
	if err != nil {
		return nil, err
	}
	type item struct {
		p libvirt.StoragePool
		x poolXML
	}
	var items []item
	var mounts []string
	for _, p := range pools {
		var x poolXML
		if raw, err := h.StoragePoolGetXMLDesc(p, 0); err == nil {
			xml.Unmarshal([]byte(raw), &x)
		}
		items = append(items, item{p, x})
		if x.Type == "netfs" && x.Target.Path != "" {
			mounts = append(mounts, x.Target.Path)
		}
	}
	health := nfsProber.probe(h, mounts)
	var out []libvirt.StoragePool
	for _, it := range items {
		if st, probed := health[it.x.Target.Path]; probed && it.x.Type == "netfs" && st != mountUp {
			continue
		}
		out = append(out, it.p)
	}
	return out, nil
}

// refreshLive lets every pool that answers rescan its directory, so that files made outside libvirt show up.
func refreshLive(h *hostconn.Host) {
	pools, err := livePools(h)
	if err != nil {
		return
	}
	for _, p := range pools {
		h.StoragePoolRefresh(p, 0)
	}
}

// --- VMs on a share that does not answer ---

// domXMLCache keeps the definition of each running VM for a while, so that finding the VMs on a dead share does
// not ask libvirt about the VM that is stuck.
var domXMLCache = struct {
	sync.Mutex
	m map[*hostconn.Host]map[string]cachedXML
}{m: map[*hostconn.Host]map[string]cachedXML{}}

type cachedXML struct {
	at  time.Time
	xml string
}

const domXMLTTL = 2 * time.Minute

// stalledOn splits the running VMs of a node into those with a disk file below one of the dead mounts and the
// others. With no dead mount it returns (nil, nil) at once and asks libvirt nothing.
func stalledOn(h *hostconn.Host) (others, stalled []libvirt.Domain) {
	down := nfsProber.freshDown(h)
	if len(down) == 0 {
		return nil, nil
	}
	doms, _, err := h.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive)
	if err != nil {
		return nil, nil
	}
	now := time.Now()
	domXMLCache.Lock()
	cache := domXMLCache.m[h]
	if cache == nil {
		cache = map[string]cachedXML{}
		domXMLCache.m[h] = cache
	}
	domXMLCache.Unlock()
	for _, d := range doms {
		key := cluster.FormatUUID(d.UUID)
		domXMLCache.Lock()
		c, ok := cache[key]
		domXMLCache.Unlock()
		if !ok || now.Sub(c.at) > domXMLTTL {
			raw, err := h.DomainGetXMLDesc(d, libvirt.DomainXMLInactive)
			if err != nil {
				if ok {
					raw = c.xml
				} else {
					others = append(others, d)
					continue
				}
			}
			c = cachedXML{now, raw}
			domXMLCache.Lock()
			cache[key] = c
			domXMLCache.Unlock()
		}
		on := false
		for _, m := range down {
			if refsDir(c.xml, m) {
				on = true
				break
			}
		}
		if on {
			stalled = append(stalled, d)
		} else {
			others = append(others, d)
		}
	}
	return others, stalled
}

// warmDomainXML remembers the definitions of the node's running VMs while all is well, so that stalledOn has
// them when a share goes down. It is called by the statistics round, at most once per domXMLTTL per node,
// and only for a node that has an NFS pool.
func warmDomainXML(h *hostconn.Host) {
	if !nfsProber.hasMounts(h) || len(nfsProber.downMounts(h)) > 0 {
		return
	}
	domXMLCache.Lock()
	cache := domXMLCache.m[h]
	if cache == nil {
		cache = map[string]cachedXML{}
		domXMLCache.m[h] = cache
	}
	domXMLCache.Unlock()
	doms, _, err := h.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive)
	if err != nil {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	for _, d := range doms {
		key := cluster.FormatUUID(d.UUID)
		seen[key] = true
		domXMLCache.Lock()
		c, ok := cache[key]
		domXMLCache.Unlock()
		if ok && now.Sub(c.at) < domXMLTTL/2 {
			continue
		}
		if raw, err := h.DomainGetXMLDesc(d, libvirt.DomainXMLInactive); err == nil {
			domXMLCache.Lock()
			cache[key] = cachedXML{now, raw}
			domXMLCache.Unlock()
		}
	}
	domXMLCache.Lock()
	for k := range cache {
		if !seen[k] {
			delete(cache, k)
		}
	}
	domXMLCache.Unlock()
}

// freshDown is downMounts for work that is about to ask libvirt about disks. Asking for the size of a disk
// makes libvirt look at the image file, and a libvirt thread that does so on a share that just went away waits for
// the server until it is back, with the lock of the VM. So when the node has NFS mounts and the last answer is
// not safe to act on (an "up" older than probeFresh, or anything older than probeStale), the mounts are probed
// again first, waiting at most probeLimit. A "down" answer is acted on at once: nothing will be asked.
func (pp *poolProber) freshDown(h *hostconn.Host) []string {
	if pp.hasMounts(h) {
		pp.mu.Lock()
		e := pp.cache[h]
		need := e == nil
		if e != nil {
			age := pp.now().Sub(e.at)
			need = age >= probeStale
			for _, st := range e.state {
				if st == mountUp && age >= probeFresh {
					need = true
				}
			}
		}
		pp.mu.Unlock()
		if need {
			pp.refresh(h)
		}
	}
	return pp.downMounts(h)
}

// hasMounts reports whether anybody asked about NFS mounts of the node lately.
func (pp *poolProber) hasMounts(h *hostconn.Host) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return len(pp.asked[h]) > 0
}

// stalledRecords are statistics records for VMs that were left out because their disk share does not answer:
// the state only, so the VM still shows as running.
func stalledRecords(stalled []libvirt.Domain) []libvirt.DomainStatsRecord {
	var out []libvirt.DomainStatsRecord
	for _, d := range stalled {
		out = append(out, libvirt.DomainStatsRecord{Dom: d, Params: []libvirt.TypedParam{
			{Field: libvirt.DomainStatsStateState, Value: *libvirt.NewTypedParamValueInt(int32(libvirt.DomainRunning))},
		}})
	}
	return out
}

// onMount reports whether a file lies below one of the mount points.
func onMount(file string, mounts []string) (string, bool) {
	for _, m := range mounts {
		if strings.HasPrefix(file, strings.TrimRight(m, "/")+"/") {
			return m, true
		}
	}
	return "", false
}

// storageDownText says, for each dead mount, which share it is and what happens to the VM.
func storageDownText(h *hostconn.Host, mounts map[string]bool) []string {
	pools, _ := poolDescs(h)
	var out []string
	for m := range mounts {
		what := m
		if p, ok := pools[m]; ok && p.isNFS() {
			what = p.Name + " (" + p.shareText() + ")"
		}
		out = append(out, "The disk of this VM is on the storage "+what+", which does not answer. The VM waits for it and continues when it is back; nothing is lost.")
	}
	sort.Strings(out)
	return out
}
