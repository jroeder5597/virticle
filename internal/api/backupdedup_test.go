package api

import "testing"

// A backup on a share that two nodes mount is listed once; a backup of the same name on local pools is not merged.
func TestDedupShared(t *testing.T) {
	in := []backupEntry{
		{Node: 2, Pool: "nas", Dir: "vm-1.vmbackup", share: "10.0.0.5:/e@/mnt/nas"},
		{Node: 1, Pool: "nas", Dir: "vm-1.vmbackup", share: "10.0.0.5:/e@/mnt/nas"},
		{Node: 1, Pool: "default", Dir: "vm-1.vmbackup"},
		{Node: 2, Pool: "default", Dir: "vm-1.vmbackup"},
		{Node: 2, Pool: "nas", Dir: "vm-2.vmbackup", share: "10.0.0.5:/e@/mnt/nas"},
	}
	out := dedupShared(in)
	if len(out) != 4 {
		t.Fatalf("%d entries: %+v", len(out), out)
	}
	for _, e := range out {
		if e.share != "" && e.Dir == "vm-1.vmbackup" && e.Node != 1 {
			t.Errorf("the node with the lowest id keeps the shared backup: %+v", e)
		}
	}
}
