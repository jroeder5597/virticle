package api

import (
	"strings"
	"testing"
	"time"
)

func TestReplicationMetaRoundTrip(t *testing.T) {
	c := replConfig{Enabled: true, TargetNode: 2, TargetName: "vspike-n2", Pool: "default", EveryMinutes: 15, Dir: "/var/lib/libvirt/images/.virticle-rp.abc123",
		ReplaceStale: true, LastOK: 1700000000, LastBytes: 12345, LastKind: kindIncremental, LastSecs: 9}
	meta, err := setMetaChildren("", map[string]string{replChild: c.element()})
	if err != nil {
		t.Fatal(err)
	}
	got := parseReplication(meta)
	if got == nil || *got != c {
		t.Fatalf("round trip: %+v", got)
	}
	// other children are kept, and an older Virticle's children are not disturbed
	meta, err = setMetaChildren(`<meta><backup-schedule enabled="yes" mode="interval" every-minutes="60" target-node="0" pool="p" keep-last="3"/></meta>`, map[string]string{replChild: c.element()})
	if err != nil || !strings.Contains(meta, "backup-schedule") || parseReplication(meta) == nil {
		t.Fatalf("meta %q: %v", meta, err)
	}
	if bs, err := parseBackupSchedule(meta); err != nil || bs == nil {
		t.Errorf("the backup schedule is still readable: %v %v", bs, err)
	}
	// removing
	meta, _ = setMetaChildren(meta, map[string]string{replChild: ""})
	if parseReplication(meta) != nil || !strings.Contains(meta, "backup-schedule") {
		t.Errorf("removal: %q", meta)
	}
	if parseReplication("") != nil || parseReplication("<meta/>") != nil || parseReplication("<broken") != nil {
		t.Error("no replication in these")
	}
	for name, bad := range map[string]replConfig{
		"no node":  {Pool: "p", EveryMinutes: 15},
		"no pool":  {TargetNode: 1, EveryMinutes: 15},
		"interval": {TargetNode: 1, Pool: "p", EveryMinutes: 1},
	} {
		if bad.validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestReplicaAndStaleMeta(t *testing.T) {
	rm := replicaMeta{Dir: "/p/.virticle-rp.x", Pool: "default", SourceNode: 1, SourceName: "vspike-n1", Every: 5, Last: 100, Checked: 200, Points: 2, Bytes: 4096}
	st := staleMeta{ToNode: 2, ToName: "vspike-n2", At: 300}
	fo := failedOverMeta{FromNode: 1, FromName: "vspike-n1", At: 400, Planned: true}
	meta, err := setMetaChildren("", map[string]string{replicaChild: rm.element(), staleChild: st.element(), failedOverChd: fo.element()})
	if err != nil {
		t.Fatal(err)
	}
	if g := parseReplicaMeta(meta); g == nil || *g != rm {
		t.Errorf("replica: %+v", g)
	}
	if g := parseStale(meta); g == nil || *g != st {
		t.Errorf("stale: %+v", g)
	}
	if g := parseFailedOver(meta); g == nil || *g != fo {
		t.Errorf("failed over: %+v", g)
	}
	// names with quotes and markup cannot break out of the attribute
	hostile := replicaMeta{Dir: `/p/"x"<>&`, SourceName: `n"><evil/>`}
	m2, err := setMetaChildren("", map[string]string{replicaChild: hostile.element()})
	if err != nil {
		t.Fatal(err)
	}
	if g := parseReplicaMeta(m2); g == nil || strings.Contains(m2, "<evil/>") {
		t.Errorf("hostile name: %q", m2)
	}
}

func TestReplicaLate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	min := func(n int) int64 { return now.Add(-time.Duration(n) * time.Minute).Unix() }
	for _, tc := range []struct {
		last  int64
		every int
		late  bool
	}{
		{min(10), 15, false},
		{min(30), 15, false}, // exactly twice: not yet
		{min(32), 15, true},
		{min(100), 60, false},
		{min(200), 60, true},
		{0, 15, false}, // the first copy is on its way
		{min(500), 0, false},
	} {
		if got := replicaLate(tc.last, tc.every, now); got != tc.late {
			t.Errorf("last %d min ago, every %d: late = %v, want %v", (now.Unix()-tc.last)/60, tc.every, got, tc.late)
		}
	}
}

func TestSummaryStateWords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rm := replicaMeta{Dir: "/p/d", SourceNode: 1, SourceName: "a", Every: 15, Last: now.Add(-time.Hour).Unix(), Points: 2}
	meta, _ := setMetaChildren("", map[string]string{replicaChild: rm.element()})
	v := vmSummary{State: "shutoff", Autostart: true}
	v.addReplication(meta, now)
	if v.State != "replica" || v.Replica == nil || !v.Replica.Late || v.Autostart {
		t.Errorf("replica summary: %+v", v)
	}
	meta, _ = setMetaChildren("", map[string]string{staleChild: staleMeta{ToNode: 2, ToName: "b", At: 5}.element()})
	v = vmSummary{State: "shutoff"}
	v.addReplication(meta, now)
	if v.State != "stale" || v.Stale == nil {
		t.Errorf("stale summary: %+v", v)
	}
	// a running VM is never relabelled
	v = vmSummary{State: "running"}
	v.addReplication(meta, now)
	if v.State != "running" {
		t.Errorf("running stale VM relabelled: %s", v.State)
	}
	meta, _ = setMetaChildren("", map[string]string{replChild: replConfig{Enabled: true, TargetNode: 2, TargetName: "b", Pool: "p", EveryMinutes: 5}.element()})
	v = vmSummary{State: "running"}
	v.addReplication(meta, now)
	if v.Replication == nil || v.Replication.TargetName != "b" || v.State != "running" {
		t.Errorf("source summary: %+v", v)
	}
}

func TestReplicaDefinition(t *testing.T) {
	src := `<domain type='kvm'><name>web</name><uuid>11111111-2222-3333-4444-555555555555</uuid>
<metadata><meta xmlns="x"><tags><tag>a</tag></tags><backup-schedule enabled="yes"/></meta></metadata>
<os><nvram>/var/lib/libvirt/qemu/nvram/web_VARS.fd</nvram></os>
<devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/var/lib/libvirt/images/web-root.qcow2'><backingStore type='file'><source file='/var/lib/libvirt/images/base.qcow2'/></backingStore></source><target dev='vda' bus='virtio'/></disk>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/data/web-data.qcow2'/><target dev='vdb' bus='virtio'/></disk>
<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='/isos/install.iso'/><target dev='sda' bus='sata'/><readonly/></disk>
<interface type='network'><mac address='52:54:00:aa:bb:cc'/><source network='default'/></interface>
</devices></domain>`
	md, err := parseMigDomain(src)
	if err != nil {
		t.Fatal(err)
	}
	x, vols, err := replicaDefinition(src, md, "/pool/dir")
	if err != nil {
		t.Fatal(err)
	}
	if vols["vda"] != "web.qcow2" || vols["vdb"] != "web-vdb.qcow2" || len(vols) != 2 {
		t.Errorf("volumes %v", vols)
	}
	for _, want := range []string{"/pool/dir/web.qcow2", "/pool/dir/web-vdb.qcow2", "52:54:00:aa:bb:cc", "web_VARS.fd", "<uuid>11111111-2222-3333-4444-555555555555</uuid>"} {
		if !strings.Contains(x, want) {
			t.Errorf("definition lacks %s:\n%s", want, x)
		}
	}
	for _, gone := range []string{"base.qcow2", "install.iso", "<metadata", "backup-schedule", "/var/lib/libvirt/images/web-root.qcow2", "/data/web-data.qcow2"} {
		if strings.Contains(x, gone) {
			t.Errorf("definition still has %s:\n%s", gone, x)
		}
	}
}

func TestSameStamps(t *testing.T) {
	if !sameStamps([]string{"1|2", "3|4"}, []string{"1|2", "3|4"}) {
		t.Error("equal stamps")
	}
	if sameStamps([]string{"1|2"}, []string{"1|3"}) || sameStamps(nil, nil) || sameStamps([]string{"1|2"}, []string{"1|2", "3|4"}) {
		t.Error("different or missing stamps are never the same: they mean a run is needed")
	}
}

func TestReplicaAlerts(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Unix(1_700_000_000, 0)
	in := alertInput{Fresh: map[int64]bool{2: true}, Replicas: []alertReplica{{Node: 2, NodeName: "n2", UUID: "u", Name: "web", Source: "n1", Every: 15, Last: now.Add(-20 * time.Minute)}}}
	raised, _, _ := tr.step(in, now)
	if len(raised) != 0 {
		t.Fatalf("a copy 20 minutes old at a 15 minute interval is fine: %v", raised)
	}
	in.Replicas[0].Last = now.Add(-40 * time.Minute)
	raised, _, all := tr.step(in, now.Add(time.Minute))
	if len(raised) != 1 || raised[0].Kind != "replica-age" || raised[0].Severity != "warning" || !strings.Contains(raised[0].Title, "web") || len(all) != 1 {
		t.Fatalf("older than twice the interval raises: %+v", raised)
	}
	// it clears by itself when a run succeeds
	in.Replicas[0].Last = now.Add(2 * time.Minute)
	_, cleared, all := tr.step(in, now.Add(3*time.Minute))
	if len(cleared) != 1 || len(all) != 0 {
		t.Fatalf("clear: %+v", cleared)
	}
	// no copy yet: no alert
	in.Replicas[0].Last = time.Time{}
	if r, _, _ := tr.step(in, now.Add(4*time.Minute)); len(r) != 0 {
		t.Errorf("a first copy in progress: %v", r)
	}
	in = alertInput{Fresh: map[int64]bool{1: true}, Stale: []alertStale{{Node: 1, NodeName: "n1", UUID: "u", Name: "web", To: "n2"}}}
	r, _, _ := tr.step(in, now.Add(5*time.Minute))
	if len(r) != 1 || r[0].Kind != "stale-copy" {
		t.Errorf("stale copy alert: %+v", r)
	}
}

func TestJobVMOfReplicate(t *testing.T) {
	if jobVM("web \u2192 vspike-n2") != "web" || jobVM("web@snap") != "web" || jobVM("web") != "web" {
		t.Error("job names")
	}
}
