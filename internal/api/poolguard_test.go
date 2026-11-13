package api

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"virticle/internal/hostconn"
)

func TestDownMounts(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	reply := "up /mnt/a\ndown /mnt/b\nunmounted /mnt/c\n"
	var rerr error
	pp, _ := newTestProber(&now, &calls, &reply, &rerr)
	h := &hostconn.Host{}
	if got := pp.downMounts(h); got != nil {
		t.Errorf("nothing probed yet: %v", got)
	}
	pp.probe(h, []string{"/mnt/a", "/mnt/b", "/mnt/c"})
	if got := pp.downMounts(h); !reflect.DeepEqual(got, []string{"/mnt/b", "/mnt/c"}) {
		t.Errorf("%v", got)
	}
	now = now.Add(probeStale + time.Second)
	if got := pp.downMounts(h); got != nil {
		t.Errorf("an old answer is not used to leave things out: %v", got)
	}
}

func TestOnMountAndStalledRecords(t *testing.T) {
	if m, ok := onMount("/mnt/nf-share/vm.qcow2", []string{"/mnt/other", "/mnt/nf-share/"}); !ok || m != "/mnt/nf-share/" {
		t.Errorf("%q %v", m, ok)
	}
	if _, ok := onMount("/mnt/nf-share2/vm.qcow2", []string{"/mnt/nf-share"}); ok {
		t.Error("a mount whose name only starts the same is a different mount")
	}
	if got := stalledRecords(nil); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

func TestNFSHintSourceConflict(t *testing.T) {
	got := nfsHint("operation failed: Storage source conflict with pool: 'nf-share'", "n3", "10.0.0.5", "/export")
	if !strings.Contains(got, "already added on n3 as the storage nf-share") || strings.Contains(got, "conflict") {
		t.Errorf("%q", got)
	}
}
