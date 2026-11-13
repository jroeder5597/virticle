package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

func layoutOf(virt uint64) []diskLayout {
	return []diskLayout{{Target: "vda", Source: "/p/vm.qcow2", Virt: virt}, {Target: "vdb", Source: "/p/vm-vdb.qcow2", Virt: virt}}
}

// bk makes a backup of u1 taken day days after t0.
func bk(dir string, day int, kind, parent, ck string, virt uint64) chainBackup {
	m := &backupManifest{Format: 1, VM: "vm", UUID: "6f1a2b3c-0000-4000-8000-000000000001", Kind: kind, Parent: parent, Checkpoint: ck,
		Created: t0.AddDate(0, 0, day).Format(time.RFC3339)}
	if kind == kindIncremental {
		m.Format = 2
	}
	for _, l := range layoutOf(virt) {
		m.Disks = append(m.Disks, backupDiskRec{Target: l.Target, File: l.Target + ".qcow2", Format: "qcow2", VirtualSize: l.Virt, Source: l.Source})
	}
	return chainBackup{Dir: dir, M: m}
}

func baseInput() incrInput {
	return incrInput{Live: true, AllQcow2: true, Supported: true, Layout: layoutOf(1 << 30), Target: "n1:default", Checkpoints: map[string]bool{}}
}

func cks(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// a chain of n backups: the first full, the rest incremental, each with its checkpoint.
func chainN(n int) []chainBackup {
	var out []chainBackup
	for i := 0; i < n; i++ {
		kind, parent := kindFull, ""
		if i > 0 {
			kind, parent = kindIncremental, out[i-1].Dir
		}
		out = append(out, bk("vm-"+string(rune('a'+i))+".vmbackup", i, kind, parent, "virticle-"+string(rune('a'+i)), 1<<30))
	}
	return out
}

func TestDecideEveryFallback(t *testing.T) {
	good := chainN(2)
	cases := []struct {
		name   string
		edit   func(in *incrInput)
		kind   string
		why    string
		ck     bool
		parent string
	}{
		{"incremental on the newest backup", func(in *incrInput) { in.Backups, in.Checkpoints = good, cks("virticle-a", "virticle-b") }, kindIncremental, "", true, "vm-b.vmbackup"},
		{"VM shut off", func(in *incrInput) { in.Live = false; in.Backups, in.Checkpoints = good, cks("virticle-b") }, kindFull, whyShutOff, false, ""},
		{"a raw disk", func(in *incrInput) { in.AllQcow2 = false; in.Backups, in.Checkpoints = good, cks("virticle-b") }, kindFull, whyFormat, false, ""},
		{"old libvirt", func(in *incrInput) { in.Supported = false; in.Backups, in.Checkpoints = good, cks("virticle-b") }, kindFull, whyOldVirt, false, ""},
		{"first backup", func(in *incrInput) {}, kindFull, whyFirst, true, ""},
		{"backups from before this feature (no checkpoint)", func(in *incrInput) {
			in.Backups = []chainBackup{bk("vm-old.vmbackup", 0, "", "", "", 1<<30)}
		}, kindFull, whyNoBase, true, ""},
		{"checkpoint gone (VM restarted uncleanly, moved, reverted)", func(in *incrInput) { in.Backups = good; in.Checkpoints = cks() }, kindFull, whyNoBase, true, ""},
		{"only an older checkpoint is left: build on that backup", func(in *incrInput) { in.Backups, in.Checkpoints = good, cks("virticle-a") }, kindIncremental, "", true, "vm-a.vmbackup"},
		{"disk resized", func(in *incrInput) {
			in.Backups, in.Checkpoints = good, cks("virticle-b")
			in.Layout = layoutOf(2 << 30)
		}, kindFull, whyLayout, true, ""},
		{"disk added", func(in *incrInput) {
			in.Backups, in.Checkpoints = good, cks("virticle-b")
			in.Layout = append(layoutOf(1<<30), diskLayout{Target: "vdc", Source: "/p/c.qcow2", Virt: 1 << 30})
		}, kindFull, whyLayout, true, ""},
		{"disk removed", func(in *incrInput) {
			in.Backups, in.Checkpoints = good, cks("virticle-b")
			in.Layout = layoutOf(1 << 30)[:1]
		}, kindFull, whyLayout, true, ""},
		{"disk moved to another file", func(in *incrInput) {
			in.Backups, in.Checkpoints = good, cks("virticle-b")
			in.Layout = layoutOf(1 << 30)
			in.Layout[0].Source = "/q/vm.qcow2"
		}, kindFull, whyLayout, true, ""},
		{"parent without recorded layout (written by a version that did not record it)", func(in *incrInput) {
			b := chainN(1)
			for i := range b[0].M.Disks {
				b[0].M.Disks[i].Source = ""
			}
			in.Backups, in.Checkpoints = b, cks("virticle-a")
		}, kindFull, whyLayout, true, ""},
		{"a link of the chain is damaged", func(in *incrInput) {
			in.Backups, in.Checkpoints = good, cks("virticle-b")
			in.Intact = func(d string) bool { return d != "vm-a.vmbackup" }
		}, kindFull, whyDamaged, true, ""},
		{"a link of the chain is missing", func(in *incrInput) {
			in.Backups, in.Checkpoints = good[1:], cks("virticle-b") // the full backup was deleted
		}, kindFull, whyDamaged, true, ""},
		{"chain is full: every 7th backup is a full one", func(in *incrInput) {
			in.Backups = chainN(chainBackups)
			in.Checkpoints = cks("virticle-" + string(rune('a'+chainBackups-1)))
		}, kindFull, whyRotation, true, ""},
		{"chain one short of full still grows", func(in *incrInput) {
			in.Backups = chainN(chainBackups - 1)
			in.Checkpoints = cks("virticle-" + string(rune('a'+chainBackups-2)))
		}, kindIncremental, "", true, "vm-" + string(rune('a'+chainBackups-2)) + ".vmbackup"},
		{"another VM's backup in the pool is not a base", func(in *incrInput) {
			b := chainN(1)
			b[0].M.UUID = "6f1a2b3c-0000-4000-8000-000000000002"
			in.Backups, in.Checkpoints = nil, cks("virticle-a") // the caller filters by VM; nothing left
		}, kindFull, whyFirst, true, ""},
	}
	for _, c := range cases {
		in := baseInput()
		c.edit(&in)
		got := decideBackup(in)
		if got.Kind != c.kind || got.Reason != c.why || got.Checkpoint != c.ck || got.Parent != c.parent {
			t.Errorf("%s: got %+v, want kind %s reason %q checkpoint %v parent %q", c.name, got, c.kind, c.why, c.ck, c.parent)
		}
		if got.Kind == kindIncremental && got.ParentCheckpoint == "" {
			t.Errorf("%s: no parent checkpoint named", c.name)
		}
	}
}

func TestDecideNeverBuildsOnABackupWithoutItsCheckpointInLibvirt(t *testing.T) {
	// The newest backup lost its checkpoint, an older one still has its own: the
	// newest cannot be a base (its changes would be counted from the wrong point).
	b := chainN(3)
	in := baseInput()
	in.Backups, in.Checkpoints = b, cks("virticle-a")
	got := decideBackup(in)
	if got.Parent != "vm-a.vmbackup" {
		t.Fatalf("%+v", got)
	}
}

func TestChainOf(t *testing.T) {
	b := chainN(4)
	by := map[string]*backupManifest{}
	for _, x := range b {
		by[x.Dir] = x.M
	}
	got, err := chainOf(by, "vm-d.vmbackup")
	if err != nil || !reflect.DeepEqual(got, []string{"vm-a.vmbackup", "vm-b.vmbackup", "vm-c.vmbackup", "vm-d.vmbackup"}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := chainOf(by, "vm-a.vmbackup"); err != nil || len(got) != 1 {
		t.Fatalf("a full backup is its own chain: %v %v", got, err)
	}
	// a missing middle link
	delete(by, "vm-b.vmbackup")
	if _, err := chainOf(by, "vm-d.vmbackup"); err == nil || !strings.Contains(err.Error(), "vm-b.vmbackup is missing") {
		t.Fatalf("missing link: %v", err)
	}
	// a loop
	by["vm-b.vmbackup"] = b[1].M
	by["vm-a.vmbackup"] = &backupManifest{Kind: kindIncremental, Parent: "vm-d.vmbackup", UUID: b[0].M.UUID, Created: b[0].M.Created}
	if _, err := chainOf(by, "vm-d.vmbackup"); err == nil {
		t.Fatal("a loop was followed")
	}
	// a parent of another VM, and a parent that is not older
	by["vm-a.vmbackup"] = b[0].M
	other := *b[0].M
	other.UUID = "6f1a2b3c-0000-4000-8000-000000000002"
	by["vm-a.vmbackup"] = &other
	if _, err := chainOf(by, "vm-d.vmbackup"); err == nil {
		t.Fatal("a parent of another VM was accepted")
	}
	by["vm-a.vmbackup"] = b[0].M
	younger := *b[1].M
	younger.Created = t0.AddDate(0, 0, 9).Format(time.RFC3339)
	by["vm-b.vmbackup"] = &younger
	if _, err := chainOf(by, "vm-d.vmbackup"); err == nil {
		t.Fatal("a parent that is not older was accepted")
	}
	// an incremental backup that names no parent, and an unknown backup
	by["vm-b.vmbackup"] = &backupManifest{Kind: kindIncremental, UUID: b[0].M.UUID, Created: b[1].M.Created}
	if _, err := chainOf(by, "vm-d.vmbackup"); err == nil {
		t.Fatal("no parent")
	}
	if _, err := chainOf(by, "nope.vmbackup"); err == nil {
		t.Fatal("unknown backup")
	}
	if chainProblem(by, "vm-a.vmbackup") != "" || chainProblem(by, "vm-d.vmbackup") == "" {
		t.Error("chainProblem")
	}
	// an endless chain is cut off
	long := map[string]*backupManifest{}
	prev := ""
	for i := 0; i < maxChainWalk+5; i++ {
		d := "l" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + ".vmbackup"
		kind := kindIncremental
		if i == 0 {
			kind = kindFull
		}
		long[d] = &backupManifest{Kind: kind, Parent: prev, UUID: "u", Created: t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)}
		prev = d
	}
	if _, err := chainOf(long, prev); err == nil {
		t.Fatal("a chain beyond the limit was followed")
	}
}

func rb(key string, day int, parent string) retained {
	return retained{Key: key, UUID: "u1", Created: t0.AddDate(0, 0, day), Parent: parent}
}

func TestPruneKeepsWhatKeptBackupsNeed(t *testing.T) {
	// two weeks of daily backups, a full one every 7th: F1 i i i i i i | F2 i i i i i i
	var all []retained
	for d := 0; d < 14; d++ {
		parent := "b" + string(rune('a'+d-1))
		if d%7 == 0 {
			parent = ""
		}
		all = append(all, rb("b"+string(rune('a'+d)), d, parent))
	}
	// keep 3: the newest three are bl, bm, bn; their chain reaches back to bh (the second full backup)
	got := pruneChains(all, "u1", 3, "")
	want := []string{"bg", "bf", "be", "bd", "bc", "bb", "ba"} // newest first; the first chain goes whole
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keep 3: got %v want %v", got, want)
	}
	// keep 9: the 9th newest is bf (day 5), which needs bg? no: bf needs be..ba. Nothing of the first chain can go.
	got = pruneChains(all, "u1", 9, "")
	if len(got) != 0 {
		t.Fatalf("keep 9: %v", got)
	}
	// keep 7: the newest 7 are days 7..13, all of the second chain; the whole first chain goes
	if got = pruneChains(all, "u1", 7, ""); len(got) != 7 || got[0] != "bg" || got[6] != "ba" {
		t.Fatalf("keep 7: %v", got)
	}
	// keep 8: day 6 (bg) is kept and needs the first chain back to ba
	if got = pruneChains(all, "u1", 8, ""); len(got) != 0 {
		t.Fatalf("keep 8: %v", got)
	}
	// no kept backup is ever left without its parent
	for keep := 1; keep <= 14; keep++ {
		del := map[string]bool{}
		for _, k := range pruneChains(all, "u1", keep, "") {
			del[k] = true
		}
		for _, b := range all {
			if !del[b.Key] && b.Parent != "" && del[b.Parent] {
				t.Errorf("keep %d: %s survives without its parent %s", keep, b.Key, b.Parent)
			}
		}
		if keep <= 14 && del["b"+string(rune('a'+14-keep))] {
			t.Errorf("keep %d: one of the newest was deleted", keep)
		}
	}
	// deletion order: newest first
	got = pruneChains(all, "u1", 3, "")
	for i := 1; i < len(got); i++ {
		if got[i-1] < got[i] {
			t.Errorf("not newest first: %v", got)
		}
	}
	// the backup just written is protected with its chain even if it is old
	if got = pruneChains(all, "u1", 3, "bb"); hasName(got, "bb") || hasName(got, "ba") {
		t.Errorf("just: %v", got)
	}
	// another VM's backups are not touched, a broken parent reference does not loop
	all = append(all, retained{Key: "x", UUID: "u2", Created: t0, Parent: ""}, rb("loop1", 20, "loop2"), rb("loop2", 21, "loop1"))
	if got = pruneChains(all, "u1", 1, ""); hasName(got, "x") {
		t.Errorf("touched another VM: %v", got)
	}
}

func hasName(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestDependentsOf(t *testing.T) {
	all := []retained{rb("a", 0, ""), rb("b", 1, "a"), rb("c", 2, "b"), rb("d", 3, "a"), rb("e", 4, ""), rb("f", 5, "e")}
	if got := dependentsOf(all, "a"); !reflect.DeepEqual(got, []string{"d", "c", "b"}) {
		t.Errorf("a: %v", got)
	}
	if got := dependentsOf(all, "b"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("b: %v", got)
	}
	if got := dependentsOf(all, "c"); len(got) != 0 {
		t.Errorf("c: %v", got)
	}
	if got := dependentsOf(all, "e"); !reflect.DeepEqual(got, []string{"f"}) {
		t.Errorf("e: %v", got)
	}
}

func TestStaleCheckpoints(t *testing.T) {
	ck := func(n, target string, at int64) ckInfo {
		return ckInfo{Name: "virticle-" + n, Target: target, Created: at}
	}
	// the new checkpoint replaces the older ones of its own target and leaves the others
	all := []ckInfo{ck("new", "n1:a", 50), ck("old", "n1:a", 40), ck("other", "n2:b", 45), ck("older", "n1:a", 10), {Name: "mine-not", Target: "n1:a", Created: 5}}
	got := staleCheckpoints(all, "n1:a", "virticle-new")
	if !reflect.DeepEqual(got, []string{"virticle-old", "virticle-older"}) {
		t.Errorf("%v", got)
	}
	// never more than checkpointsKept
	all = nil
	for i := 0; i < 8; i++ {
		all = append(all, ck(string(rune('a'+i)), "t"+string(rune('a'+i)), int64(i)))
	}
	got = staleCheckpoints(all, "tnew", "virticle-h")
	if len(got) != 8-checkpointsKept || hasName(got, "virticle-h") || hasName(got, "virticle-g") {
		t.Errorf("%v", got)
	}
}

func TestCheckpointXMLAndBackupXML(t *testing.T) {
	x := checkpointXML("virticle-1-ab", "n1:default", []string{"vda", "vdb"}, []string{"sda"})
	for _, want := range []string{"<name>virticle-1-ab</name>", "<description>n1:default</description>", "<disk name='vda' checkpoint='bitmap'/>", "<disk name='sda' checkpoint='no'/>"} {
		if !strings.Contains(x, want) {
			t.Errorf("missing %q in %s", want, x)
		}
	}
	// a hostile target name stays inside the element
	if x := checkpointXML("virticle-1", "<x>&", nil, nil); strings.Contains(x, "<x>") {
		t.Errorf("not escaped: %s", x)
	}
	d := []migDisk{{Target: "vda"}}
	b := backupXML("/p/.w", d, map[string]string{"vda": "vda.qcow2"}, nil, "virticle-a")
	if !strings.Contains(b, "<domainbackup mode='push'><incremental>virticle-a</incremental><disks>") {
		t.Errorf("%s", b)
	}
}

func TestNewCheckpointNameIsOurs(t *testing.T) {
	n := newCheckpointName(t0, "ab12cd")
	if !checkpointRE.MatchString(n) || n != "virticle-20260301T030000Z-ab12cd" {
		t.Errorf("%s", n)
	}
	if !reflect.DeepEqual(oursOnly([]string{"virticle-x", "other", "snap"}), []string{"virticle-x"}) {
		t.Error("oursOnly")
	}
}

func TestManifestKinds(t *testing.T) {
	mk := func(edit func(m map[string]any)) error {
		m := map[string]any{"format": 1, "vm": "vm", "uuid": "6f1a2b3c-0000-4000-8000-000000000001", "created": "2026-03-01T03:00:00Z",
			"disks": []map[string]any{{"target": "vda", "file": "vda.qcow2", "format": "qcow2", "virtualSize": 1, "bytes": 1}}}
		edit(m)
		raw, _ := json.Marshal(m)
		_, err := parseBackupManifest(raw)
		return err
	}
	ok := []struct {
		name string
		edit func(m map[string]any)
	}{
		{"old full backup", func(m map[string]any) {}},
		{"full with a checkpoint", func(m map[string]any) { m["kind"] = "full"; m["checkpoint"] = "virticle-2026-ab" }},
		{"incremental", func(m map[string]any) {
			m["format"] = 2
			m["kind"] = "incremental"
			m["parent"] = "vm-20260301T030000Z.vmbackup"
		}},
	}
	for _, c := range ok {
		if err := mk(c.edit); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	bad := []struct {
		name string
		edit func(m map[string]any)
	}{
		{"incremental in format 1 (an old version would restore a partial disk)", func(m map[string]any) { m["kind"] = "incremental"; m["parent"] = "vm-1.vmbackup" }},
		{"format 2 that says full", func(m map[string]any) { m["format"] = 2 }},
		{"incremental without parent", func(m map[string]any) { m["format"] = 2; m["kind"] = "incremental" }},
		{"incremental with a path as parent", func(m map[string]any) { m["format"] = 2; m["kind"] = "incremental"; m["parent"] = "../x.vmbackup" }},
		{"full with a parent", func(m map[string]any) { m["kind"] = "full"; m["parent"] = "vm-1.vmbackup" }},
		{"unknown kind", func(m map[string]any) { m["kind"] = "differential" }},
		{"bad checkpoint name", func(m map[string]any) { m["checkpoint"] = "x y" }},
		{"format 3", func(m map[string]any) { m["format"] = 3 }},
	}
	for _, c := range bad {
		if err := mk(c.edit); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
}

func TestAnnotateChains(t *testing.T) {
	b := chainN(3)
	var list []backupEntry
	for _, x := range b {
		list = append(list, backupEntry{Node: 1, Pool: "p", Dir: x.Dir, Manifest: x.M})
	}
	// the same names in another pool are another chain
	list = append(list, backupEntry{Node: 2, Pool: "p", Dir: b[2].Dir, Manifest: b[2].M})
	list = append(list, backupEntry{Node: 1, Pool: "p", Dir: "broken.vmbackup", Err: "bad"})
	annotateChains(list)
	if list[0].Chain != 1 || list[0].Dependents != 2 || list[1].Chain != 2 || list[1].Dependents != 1 || list[2].Chain != 3 || list[2].Dependents != 0 {
		t.Errorf("%+v", list[:3])
	}
	if list[3].ChainError == "" || list[3].Chain != 0 {
		t.Errorf("a backup whose parents are in another pool: %+v", list[3])
	}
}
