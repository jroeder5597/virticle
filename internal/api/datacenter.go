package api

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"virt-proj/internal/cluster"
	"virt-proj/internal/hostconn"
)

// nodeTimeout bounds how long the all-nodes lists wait for one node.
const nodeTimeout = 8 * time.Second

// nodeError says why a node is missing from an all-nodes list.
type nodeError struct {
	Node     int64  `json:"node"`
	NodeName string `json:"nodeName"`
	Error    string `json:"error"`
}

// nodeResult is what one node contributed to an all-nodes list.
type nodeResult[T any] struct {
	Node  int64
	Name  string
	Items []T
	Err   string
}

func nodeLabel(n cluster.NodeStatus) string {
	if n.Hostname != "" {
		return n.Hostname
	}
	return n.Addr
}

// mergeNodes flattens per-node results into one list ordered by node name,
// then node id, then the order each node returned. tag stamps an item with its
// node. A node with an error contributes an entry in the error list instead.
func mergeNodes[T, O any](results []nodeResult[T], tag func(T, int64, string) O) ([]O, []nodeError) {
	sorted := append([]nodeResult[T](nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].Node < sorted[j].Node
	})
	items := []O{}
	errs := []nodeError{}
	for _, r := range sorted {
		if r.Err != "" {
			errs = append(errs, nodeError{Node: r.Node, NodeName: r.Name, Error: r.Err})
			continue
		}
		for _, it := range r.Items {
			items = append(items, tag(it, r.Node, r.Name))
		}
	}
	return items, errs
}

// gatherNodes runs fn against every node at the same time. A node that is not
// online, fails, or takes longer than timeout is reported through Err; it never
// fails the call (listVMs, by comparison, drops such nodes silently).
func gatherNodes[T any](s *Server, timeout time.Duration, fn func(h *hostconn.Host) ([]T, error)) []nodeResult[T] {
	var (
		wg  sync.WaitGroup
		out []nodeResult[T]
		mu  sync.Mutex
	)
	for _, n := range s.Nodes.Status() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := nodeResult[T]{Node: n.ID, Name: nodeLabel(n)}
			h, err := s.Nodes.Host(n.ID)
			if err != nil {
				res.Err = n.Error
				if res.Err == "" {
					res.Err = "node is " + n.State
				}
			} else {
				type reply struct {
					items []T
					err   error
				}
				ch := make(chan reply, 1) // buffered: a late answer is dropped, not leaked
				go func() {
					items, err := fn(h)
					ch <- reply{items, err}
				}()
				select {
				case r := <-ch:
					if r.err != nil {
						res.Err = r.err.Error()
					} else {
						res.Items = r.items
					}
				case <-time.After(timeout):
					res.Err = fmt.Sprintf("no answer within %s", timeout)
				}
			}
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

type nodePool struct {
	poolInfo
	Node     int64  `json:"node"`
	NodeName string `json:"nodeName"`
}

type nodeNetwork struct {
	networkInfo
	Node     int64  `json:"node"`
	NodeName string `json:"nodeName"`
}

// listAllPools is GET /api/pools: every pool of every node in one answer.
func (s *Server) listAllPools(w http.ResponseWriter, r *http.Request) {
	res := gatherNodes(s, nodeTimeout, collectPools)
	pools, errs := mergeNodes(res, func(p poolInfo, id int64, name string) nodePool { return nodePool{p, id, name} })
	writeJSON(w, map[string]any{"pools": pools, "errors": errs, "storage": nodeStorage(pools)})
}

// nodeStore is the storage one node really has: its running pools with pools that share a
// filesystem counted once.
type nodeStore struct {
	Node     int64  `json:"node"`
	Capacity uint64 `json:"capacity"`
	Used     uint64 `json:"used"`
}

// nodeStorage totals running pools per node. Libvirt does not say which directory pools sit on the
// same filesystem, so pools of one node with identical capacity AND identical available bytes are taken
// to be one filesystem and counted once. An estimate: two distinct disks of the same size and fill would
// be merged too.
func nodeStorage(pools []nodePool) []nodeStore {
	type fs struct {
		node       int64
		cap, avail uint64
	}
	seen := map[fs]bool{}
	idx := map[int64]int{}
	out := []nodeStore{}
	for _, p := range pools {
		if p.State != "running" {
			continue
		}
		k := fs{p.Node, p.Capacity, p.Available}
		if seen[k] {
			continue
		}
		seen[k] = true
		i, ok := idx[p.Node]
		if !ok {
			i = len(out)
			idx[p.Node] = i
			out = append(out, nodeStore{Node: p.Node})
		}
		out[i].Capacity += p.Capacity
		out[i].Used += p.Allocation
	}
	return out
}

// listAllNetworks is GET /api/networks: every network of every node.
func (s *Server) listAllNetworks(w http.ResponseWriter, r *http.Request) {
	res := gatherNodes(s, nodeTimeout, collectNetworks)
	nets, errs := mergeNodes(res, func(n networkInfo, id int64, name string) nodeNetwork { return nodeNetwork{n, id, name} })
	writeJSON(w, map[string]any{"networks": nets, "errors": errs})
}
