package api

import "testing"

// A share mounted by two nodes is one storage in the datacenter totals.
func TestNodeStorageCountsAShareOnce(t *testing.T) {
	nfs := func(node int64) nodePool {
		return nodePool{poolInfo: poolInfo{Name: "nas", Type: "netfs", Server: "10.0.0.5", Export: "/e", Target: "/mnt/nas", State: "running", Capacity: 1000, Allocation: 100, Available: 900}, Node: node}
	}
	local := func(node int64) nodePool {
		return nodePool{poolInfo: poolInfo{Name: "default", Type: "dir", State: "running", Capacity: 50 + uint64(node), Allocation: 5, Available: 40}, Node: node}
	}
	got := nodeStorage([]nodePool{local(1), nfs(1), local(2), nfs(2)})
	var total uint64
	for _, s := range got {
		total += s.Capacity
	}
	if want := uint64(51 + 52 + 1000); total != want {
		t.Errorf("total capacity %d, want %d (%+v)", total, want, got)
	}
}
