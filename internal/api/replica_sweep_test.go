package api

import (
	"sort"
	"testing"
	"time"
)

func TestOrphanDirs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-13 * time.Hour).Unix()
	young := now.Add(-2 * time.Hour).Unix()
	listed := map[string]int64{
		".virticle-rp.OWNED1": old,   // a replica's directory, old: kept
		".virticle-rp.STAGE1": old,   // staging of a cut short run: removed
		".virticle-rp.STAGE2": young, // maybe a run that is going on: kept
		".virticle-rp.OWNED2": old,   // named by another node's VM as its copy here: kept
	}
	known := map[string]bool{"/p/.virticle-rp.OWNED1": true, "/p/.virticle-rp.OWNED2": true}
	got := orphanDirs("/p", listed, known, now)
	sort.Strings(got)
	if len(got) != 1 || got[0] != "/p/.virticle-rp.STAGE1" {
		t.Errorf("orphans: %v", got)
	}
	if g := orphanDirs("/p", listed, nil, now); len(g) != 3 {
		t.Errorf("with nothing known only age protects: %v", g)
	}
}
