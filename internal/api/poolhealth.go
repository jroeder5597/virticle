package api

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"virticle/internal/hostconn"
)

// Health of NFS pools.
//
// A pool that libvirt shows as "running" can still be unusable: the NFS server may be down or unreachable.
// The share is mounted "hard" (see docs), so every program that touches a dead mount waits for the server
// instead of failing: libvirt's refresh of the pool, a stat of a disk file, and QEMU itself. The controller
// must not be one of those waiters, so it never asks libvirt about an NFS pool before a probe has shown the
// mount answers. The probe is a shell command that stats the mount point under `timeout -s KILL`; a process
// waiting on a hard NFS mount can be killed, so the probe always ends.
//
//	up       the mount answers
//	down     the mount did not answer within probeLimit (server or network down)
//	unmounted the path is not an NFS mount (somebody unmounted it, or it never mounted)

const (
	probeLimit = 4 * time.Second  // per mount, in seconds for the remote `timeout`
	probeTTL   = 5 * time.Second  // an answer this young is used as it is
	probeStale = 15 * time.Second // an answer this young is still used while a new probe runs in the background
	probeFresh = 2 * time.Second  // an "up" this young may be acted on (see probe)
	probeKeep  = 2 * time.Minute  // a node is probed in the background for this long after the last question about it
)

type mountState string

const (
	mountUp        mountState = "up"
	mountDown      mountState = "down"
	mountUnmounted mountState = "unmounted"
)

// probeCommand builds the shell command that tests the given mount points in parallel. The paths come from
// libvirt's pool definitions of the node, are checked by safePathRE and quoted.
func probeCommand(paths []string) string {
	var b strings.Builder
	secs := int(probeLimit / time.Second)
	for _, p := range paths {
		q := shq(p)
		fmt.Fprintf(&b, `( t=$(timeout -s KILL %d stat -f -c %%T %s 2>/dev/null); rc=$?; if [ $rc -ne 0 ]; then echo "down "%s; elif [ "$t" = nfs ] || [ "$t" = nfs4 ]; then echo "up "%s; else echo "unmounted "%s; fi ) & `, secs, q, q, q, q)
	}
	b.WriteString("wait")
	return b.String()
}

// parseProbe reads the output of probeCommand. Paths that did not report stay unset.
func parseProbe(out string) map[string]mountState {
	res := map[string]mountState{}
	for _, line := range strings.Split(out, "\n") {
		state, p, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || p == "" {
			continue
		}
		switch mountState(state) {
		case mountUp, mountDown, mountUnmounted:
			res[p] = mountState(state)
		}
	}
	return res
}

type probeEntry struct {
	at    time.Time
	state map[string]mountState
}

// poolProber keeps the state of the NFS mounts of each node.
//
// Pages must not wait for a dead server, so the answer is kept fresh in the background: the first question
// about a node starts a loop that probes its mounts every few seconds until nobody has asked for probeKeep, and
// a question that finds an answer older than probeTTL (but younger than probeStale) uses it and lets the loop
// catch up. Only a question about a mount never probed before waits, once, for the probe. One probe per node
// runs at a time; callers that arrive during a probe and need it wait for it and share its answer.
type poolProber struct {
	mu    sync.Mutex
	cache map[*hostconn.Host]*probeEntry
	busy  map[*hostconn.Host]chan struct{}
	asked map[*hostconn.Host]map[string]time.Time            // mount -> last time somebody asked about it
	loop  map[*hostconn.Host]bool                            // the background loop of the node runs
	run   func(h *hostconn.Host, cmd string) (string, error) // replaced in tests
	now   func() time.Time
	async func(f func()) // runs background work; replaced in tests
}

var nfsProber = &poolProber{
	cache: map[*hostconn.Host]*probeEntry{},
	busy:  map[*hostconn.Host]chan struct{}{},
	asked: map[*hostconn.Host]map[string]time.Time{},
	loop:  map[*hostconn.Host]bool{},
	run:   func(h *hostconn.Host, cmd string) (string, error) { return h.Run(cmd) },
	now:   time.Now,
	async: func(f func()) { go f() },
}

// probe returns the state of each path on the node, for work that is about to touch the mount (a refresh, a read
// of a volume, a scan of the directory). Only an answer that is safe to act on is used: "down" or "unmounted" for
// up to probeStale (nothing will be done on the mount, so a late "down" does no harm), and "up" only when it is
// younger than probeFresh; an older "up" is probed again first, waiting at most probeLimit. A path the node did
// not answer for is reported as down.
func (pp *poolProber) probe(h *hostconn.Host, paths []string) map[string]mountState {
	if len(paths) == 0 {
		return nil
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	pp.note(h, sorted)
	for {
		pp.mu.Lock()
		if e := pp.cache[h]; e != nil && covers(e.state, sorted) {
			age := pp.now().Sub(e.at)
			ok := age < probeStale
			for _, p := range sorted {
				if e.state[p] == mountUp && age >= probeFresh {
					ok = false
				}
			}
			if ok {
				pp.mu.Unlock()
				return pick(e.state, sorted)
			}
		}
		pp.mu.Unlock()
		pp.refresh(h) // waits for a probe in progress, or does one
		pp.mu.Lock()
		e := pp.cache[h]
		ok := e != nil && covers(e.state, sorted)
		pp.mu.Unlock()
		if ok {
			return pick(e.state, sorted)
		}
	}
}

// peek is probe for pages that only show the state: the last answer is used while it is younger than probeStale,
// and a newer one is fetched in the background, so a page never waits for a dead server. Do not decide to touch
// the mount from a peek.
func (pp *poolProber) peek(h *hostconn.Host, paths []string) map[string]mountState {
	if len(paths) == 0 {
		return nil
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	pp.note(h, sorted)
	for {
		pp.mu.Lock()
		if e := pp.cache[h]; e != nil && covers(e.state, sorted) {
			age := pp.now().Sub(e.at)
			if age < probeTTL {
				pp.mu.Unlock()
				return pick(e.state, sorted)
			}
			if age < probeStale {
				_, running := pp.busy[h]
				pp.mu.Unlock()
				if !running {
					pp.async(func() { pp.refresh(h) })
				}
				return pick(e.state, sorted)
			}
		}
		pp.mu.Unlock()
		pp.refresh(h) // waits for a probe in progress, or does one
		pp.mu.Lock()
		e := pp.cache[h]
		ok := e != nil && covers(e.state, sorted)
		pp.mu.Unlock()
		if ok {
			return pick(e.state, sorted)
		}
	}
}

// note records that somebody asked about the paths and makes sure the background loop of the node runs.
func (pp *poolProber) note(h *hostconn.Host, paths []string) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	m := pp.asked[h]
	if m == nil {
		m = map[string]time.Time{}
		pp.asked[h] = m
	}
	now := pp.now()
	for _, p := range paths {
		m[p] = now
	}
	if !pp.loop[h] {
		pp.loop[h] = true
		pp.async(func() { pp.keepFresh(h) })
	}
}

// keepFresh probes the node's mounts until nobody has asked about them for probeKeep.
func (pp *poolProber) keepFresh(h *hostconn.Host) {
	for {
		pp.mu.Lock()
		if !pp.anyAsked(h) {
			delete(pp.loop, h)
			delete(pp.asked, h)
			delete(pp.cache, h)
			pp.mu.Unlock()
			return
		}
		pp.mu.Unlock()
		pp.refresh(h)
		time.Sleep(probeTTL - time.Second)
	}
}

// anyAsked drops the paths nobody asked about lately and reports whether any is left. mu must be held.
func (pp *poolProber) anyAsked(h *hostconn.Host) bool {
	m := pp.asked[h]
	for p, at := range m {
		if pp.now().Sub(at) > probeKeep {
			delete(m, p)
		}
	}
	return len(m) > 0
}

// refresh probes every path asked about for the node and stores the answer. One refresh per node runs at a time;
// a caller that arrives meanwhile waits for it.
func (pp *poolProber) refresh(h *hostconn.Host) {
	pp.mu.Lock()
	if ch, running := pp.busy[h]; running {
		pp.mu.Unlock()
		<-ch
		return
	}
	ch := make(chan struct{})
	pp.busy[h] = ch
	var sorted []string
	for p := range pp.asked[h] {
		sorted = append(sorted, p)
	}
	pp.mu.Unlock()
	sort.Strings(sorted)

	state := map[string]mountState{}
	var good []string
	for _, p := range sorted {
		if safePathRE.MatchString(p) {
			good = append(good, p)
		} else {
			state[p] = mountUp // not something we would run a command for; leave it to libvirt
		}
	}
	if len(good) > 0 {
		out, _ := pp.run(h, probeCommand(good))
		got := parseProbe(out)
		for _, p := range good {
			if got[p] != "" {
				state[p] = got[p]
			} else {
				state[p] = mountDown // the node did not answer: treat as unreachable, never as fine
			}
		}
	}
	pp.mu.Lock()
	pp.cache[h] = &probeEntry{at: pp.now(), state: state}
	delete(pp.busy, h)
	pp.mu.Unlock()
	close(ch)
}

func covers(have map[string]mountState, want []string) bool {
	for _, p := range want {
		if _, ok := have[p]; !ok {
			return false
		}
	}
	return true
}

func pick(have map[string]mountState, want []string) map[string]mountState {
	out := make(map[string]mountState, len(want))
	for _, p := range want {
		out[p] = have[p]
	}
	return out
}

// forget drops the last answer for a node: a mount was just added or removed there, and the old answer would be
// served for up to probeStale. The next question waits for a new probe.
func (pp *poolProber) forget(h *hostconn.Host) {
	pp.mu.Lock()
	delete(pp.cache, h)
	pp.mu.Unlock()
}

// unavailableText is the one-line reason shown next to the word "unavailable".
func unavailableText(server string, st mountState) string {
	switch st {
	case mountUnmounted:
		return "the share is not mounted on this node"
	}
	if server == "" {
		server = "the NFS server"
	}
	return server + " does not answer. VMs with a disk on it wait for it and continue when it is back; nothing is lost"
}
