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
	probeLimit = 4 * time.Second // per mount, in seconds for the remote `timeout`
	probeTTL   = 6 * time.Second // how long an answer is reused
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

// poolProber caches probe results per node. One probe per node runs at a time; callers that arrive during a
// probe wait for it and share its answer.
type poolProber struct {
	mu    sync.Mutex
	cache map[*hostconn.Host]*probeEntry
	busy  map[*hostconn.Host]chan struct{}
	run   func(h *hostconn.Host, cmd string) (string, error) // replaced in tests
	now   func() time.Time
}

var nfsProber = &poolProber{
	cache: map[*hostconn.Host]*probeEntry{},
	busy:  map[*hostconn.Host]chan struct{}{},
	run:   func(h *hostconn.Host, cmd string) (string, error) { return h.Run(cmd) },
	now:   time.Now,
}

// probe returns the state of each path on the node. A path the node did not answer for is reported as down.
func (pp *poolProber) probe(h *hostconn.Host, paths []string) map[string]mountState {
	if len(paths) == 0 {
		return nil
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for {
		pp.mu.Lock()
		if e := pp.cache[h]; e != nil && pp.now().Sub(e.at) < probeTTL && covers(e.state, sorted) {
			pp.mu.Unlock()
			return pick(e.state, sorted)
		}
		if ch, running := pp.busy[h]; running {
			pp.mu.Unlock()
			<-ch
			continue
		}
		ch := make(chan struct{})
		pp.busy[h] = ch
		pp.mu.Unlock()

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
			out, err := pp.run(h, probeCommand(good))
			got := parseProbe(out)
			for _, p := range good {
				switch {
				case got[p] != "":
					state[p] = got[p]
				case err != nil:
					state[p] = mountDown // the node did not answer: treat as unreachable, never as fine
				default:
					state[p] = mountDown
				}
			}
		}
		pp.mu.Lock()
		pp.cache[h] = &probeEntry{at: pp.now(), state: state}
		delete(pp.busy, h)
		pp.mu.Unlock()
		close(ch)
		return pick(state, sorted)
	}
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

// forget drops what is known about a node (after its connection was replaced).
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
	return server + " does not answer. VMs using this storage pause until it is back; nothing is lost"
}
