package api

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

func nfsPool(name, server, export, target string, active bool) poolDesc {
	return poolDesc{Name: name, Type: "netfs", Target: target, Server: server, Export: export, Active: active}
}

func TestSameShare(t *testing.T) {
	a := nfsPool("nas", "nas.lan", "/export/vms", "/mnt/nas", true)
	for name, c := range map[string]struct {
		b    poolDesc
		want bool
	}{
		"identical":         {nfsPool("nas", "nas.lan", "/export/vms", "/mnt/nas", true), true},
		"server case":       {nfsPool("nas", "NAS.lan", "/export/vms", "/mnt/nas", true), true},
		"export trailing /": {nfsPool("nas", "nas.lan", "/export/vms/", "/mnt/nas", true), true},
		"other pool name":   {nfsPool("other", "nas.lan", "/export/vms", "/mnt/nas", true), true}, // the path is what VMs refer to
		"other server":      {nfsPool("nas", "nas2.lan", "/export/vms", "/mnt/nas", true), false},
		"other export":      {nfsPool("nas", "nas.lan", "/export/isos", "/mnt/nas", true), false},
		"other mount path":  {nfsPool("nas", "nas.lan", "/export/vms", "/mnt/nas2", true), false},
		"a directory pool":  {poolDesc{Name: "nas", Type: "dir", Target: "/mnt/nas", Active: true}, false},
		"server by address": {nfsPool("nas", "192.0.2.9", "/export/vms", "/mnt/nas", true), false}, // names are not resolved: copy, never guess
	} {
		if got := sameShare(a, c.b); got != c.want {
			t.Errorf("%s: sameShare = %v, want %v", name, got, c.want)
		}
	}
}

func TestPlaceDir(t *testing.T) {
	local := poolDesc{Name: "default", Type: "dir", Target: "/var/lib/libvirt/images", Active: true}
	nas := nfsPool("nas", "nas.lan", "/export/vms", "/mnt/nas", true)
	src := map[string]poolDesc{local.Target: local, nas.Target: nas}

	t.Run("local directory is left to the copy rules", func(t *testing.T) {
		p := placeDir(local.Target, "n1", "n2", src, map[string]poolDesc{local.Target: local})
		if p.Shared || p.Problem != "" || p.Note != "" {
			t.Errorf("%+v", p)
		}
	})
	t.Run("same share on both nodes is shared", func(t *testing.T) {
		p := placeDir(nas.Target, "n1", "n2", src, map[string]poolDesc{nas.Target: nas})
		if !p.Shared || p.Problem != "" {
			t.Errorf("%+v", p)
		}
	})
	t.Run("share missing on the target blocks and says what to add", func(t *testing.T) {
		p := placeDir(nas.Target, "n1", "n2", src, map[string]poolDesc{local.Target: local})
		if p.Shared || !strings.Contains(p.Problem, "not set up on n2") || !strings.Contains(p.Problem, "nas.lan:/export/vms") {
			t.Errorf("%+v", p)
		}
	})
	t.Run("share not running on the target blocks", func(t *testing.T) {
		down := nas
		down.Active = false
		p := placeDir(nas.Target, "n1", "n2", src, map[string]poolDesc{nas.Target: down})
		if !p.Shared || !strings.Contains(p.Problem, "not running") {
			t.Errorf("%+v", p)
		}
	})
	t.Run("another share at the same path is copied, with a note", func(t *testing.T) {
		other := nfsPool("nas", "nas.lan", "/export/other", "/mnt/nas", true)
		p := placeDir(nas.Target, "n1", "n2", src, map[string]poolDesc{nas.Target: other})
		if p.Shared || p.Problem != "" || !strings.Contains(p.Note, "nas.lan:/export/other") {
			t.Errorf("%+v", p)
		}
	})
	t.Run("a plain directory at the same path is copied, with a note", func(t *testing.T) {
		plain := poolDesc{Name: "nas", Type: "dir", Target: nas.Target, Active: true}
		p := placeDir(nas.Target, "n1", "n2", src, map[string]poolDesc{nas.Target: plain})
		if p.Shared || p.Note == "" {
			t.Errorf("%+v", p)
		}
	})
	t.Run("a directory in no pool is not shared", func(t *testing.T) {
		p := placeDir("/elsewhere", "n1", "n2", src, map[string]poolDesc{})
		if p.Shared || p.Problem != "" {
			t.Errorf("%+v", p)
		}
	})
}

// planWith builds a plan whose files are on shared or local storage as given.
func planWith(mode string, disks map[string]bool, order []string) *migPlan {
	p := &migPlan{Mode: mode}
	for _, tgt := range order {
		path := "/mnt/nas/" + tgt + ".qcow2"
		shared := disks[tgt]
		if !shared {
			path = "/var/lib/libvirt/images/" + tgt + ".qcow2"
		}
		p.Disks = append(p.Disks, planDisk{migDisk: migDisk{Target: tgt, Path: path}, Chain: []imgInfo{{Path: path, Format: "qcow2"}}})
		p.Files.add(path, true, p.mirrored(p.Disks[len(p.Disks)-1]))
		if shared {
			f := p.Files.get(path)
			f.Shared, f.Skip, f.SharedPool = true, true, "nas"
		}
	}
	return p
}

func TestMigrationOfSharedDisks(t *testing.T) {
	t.Run("all disks shared: nothing copied, no disk mirrored, no flag", func(t *testing.T) {
		p := planWith("live", map[string]bool{"vda": true}, []string{"vda"})
		if p.mirrorFlag() != 0 || len(p.mirroredTargets()) != 0 || len(p.copyList()) != 0 {
			t.Errorf("flag %v mirrored %v copy %v", p.mirrorFlag(), p.mirroredTargets(), p.copyList())
		}
	})
	t.Run("all disks shared, offline: nothing copied", func(t *testing.T) {
		p := planWith("offline", map[string]bool{"vda": true, "vdb": true}, []string{"vda", "vdb"})
		if len(p.copyList()) != 0 {
			t.Errorf("copy %v", p.copyList())
		}
	})
	t.Run("mixed: only the local disk is mirrored and named for libvirt", func(t *testing.T) {
		p := planWith("live", map[string]bool{"vda": true}, []string{"vda", "vdb"})
		if p.mirrorFlag() != libvirt.MigrateNonSharedDisk {
			t.Errorf("flag %v", p.mirrorFlag())
		}
		if got := p.mirroredTargets(); !reflect.DeepEqual(got, []string{"vdb"}) {
			t.Errorf("mirrored %v", got)
		}
	})
	t.Run("mixed, offline: only the local file is copied", func(t *testing.T) {
		p := planWith("offline", map[string]bool{"vda": true}, []string{"vda", "vdb"})
		l := p.copyList()
		if len(l) != 1 || l[0].Path != "/var/lib/libvirt/images/vdb.qcow2" {
			t.Errorf("copy %v", l)
		}
	})
	t.Run("a chain on a shared disk does not force the incremental mode", func(t *testing.T) {
		p := planWith("live", map[string]bool{"vda": true}, []string{"vda", "vdb"})
		p.Disks[0].Chain = append(p.Disks[0].Chain, imgInfo{Path: "/mnt/nas/base.qcow2"})
		if p.mirrorFlag() != libvirt.MigrateNonSharedDisk {
			t.Errorf("flag %v", p.mirrorFlag())
		}
		p.Disks[1].Chain = append(p.Disks[1].Chain, imgInfo{Path: "/var/lib/libvirt/images/base.qcow2"})
		if p.mirrorFlag() != libvirt.MigrateNonSharedInc {
			t.Errorf("flag with a local chain %v", p.mirrorFlag())
		}
	})
	t.Run("all local: unchanged behaviour", func(t *testing.T) {
		p := planWith("live", map[string]bool{}, []string{"vda"})
		if p.mirrorFlag() != libvirt.MigrateNonSharedDisk || !reflect.DeepEqual(p.mirroredTargets(), []string{"vda"}) {
			t.Errorf("flag %v %v", p.mirrorFlag(), p.mirroredTargets())
		}
	})
}

func TestDiskSummary(t *testing.T) {
	for _, c := range []struct {
		shared, local []string
		want          string
	}{
		{nil, nil, "no disk"},
		{[]string{"vda on nas"}, nil, "no disk is copied: every disk is on shared storage that both nodes mount (vda on nas)"},
		{nil, []string{"vda", "vdb"}, "all disks are copied to the target: vda, vdb"},
		{[]string{"vda on nas"}, []string{"vdb"}, "not copied, on shared storage: vda on nas. Copied, on local storage: vdb"},
	} {
		if got := diskSummary(c.shared, c.local); !strings.Contains(got, c.want) {
			t.Errorf("%v %v: %q does not contain %q", c.shared, c.local, got, c.want)
		}
	}
}

func TestGroupShared(t *testing.T) {
	all := []sharedMember{{Node: 1, NodeName: "n1"}, {Node: 2, NodeName: "n2"}, {Node: 3, NodeName: "n3"}}
	pools := []nodeNFS{
		{Node: 2, Name: "n2", Pool: "nas", Server: "nas.lan", Export: "/vms", Target: "/mnt/nas", State: "running", Autostart: true},
		{Node: 1, Name: "n1", Pool: "nas", Server: "nas.lan", Export: "/vms/", Target: "/mnt/nas", State: "running", Autostart: true},
		{Node: 3, Name: "n3", Pool: "other", Server: "x", Export: "/x", Target: "/mnt/other", State: "unavailable"},
		{Node: 1, Name: "n1", Pool: "other", Server: "x", Export: "/x", Target: "/mnt/other", State: "running"},
		{Node: 2, Name: "n2", Pool: "odd", Server: "a", Export: "/a", Target: "/mnt/odd", State: "running"},
		{Node: 1, Name: "n1", Pool: "odd", Server: "b", Export: "/a", Target: "/mnt/odd", State: "running"},
	}
	g := groupShared(pools, all)
	if len(g) != 3 || g[0].Name != "nas" || g[1].Name != "odd" || g[2].Name != "other" {
		t.Fatalf("%+v", g)
	}
	nas := g[0]
	if len(nas.Nodes) != 2 || len(nas.Missing) != 1 || nas.Missing[0].NodeName != "n3" || nas.State != "running" {
		t.Errorf("nas: %+v", nas)
	}
	if odd := g[1]; len(odd.Nodes) != 1 || len(odd.Differ) != 1 || odd.Differ[0].NodeName != "n2" || len(odd.Missing) != 1 {
		t.Errorf("odd (first node by id decides): %+v", odd)
	}
	if other := g[2]; other.State != "unavailable" || len(other.Missing) != 1 || other.Missing[0].NodeName != "n2" {
		t.Errorf("other: %+v", other)
	}
}

func TestAddSummary(t *testing.T) {
	msg, ok := addSummary("nas", []nodeOutcome{{NodeName: "n1", OK: true}, {NodeName: "n2", OK: true}})
	if !ok || msg != "nas is mounted on n1, n2" {
		t.Errorf("%v %q", ok, msg)
	}
	// one node fails to mount: the result is not complete, names the node and says what is left in place
	msg, ok = addSummary("nas", []nodeOutcome{{NodeName: "n1", OK: true}, {NodeName: "n2", Error: "has no NFS client tools"}})
	if ok || !strings.Contains(msg, "mounted on n1, but not everywhere") || !strings.Contains(msg, "n2: has no NFS client tools") {
		t.Errorf("%v %q", ok, msg)
	}
	msg, ok = addSummary("nas", []nodeOutcome{{NodeName: "n1", Error: "a"}, {NodeName: "n2", Error: "b"}})
	if ok || !strings.Contains(msg, "could not be added on any node") {
		t.Errorf("%v %q", ok, msg)
	}
	// nodes that fail the same way are named together
	msg, _ = addSummary("nas", []nodeOutcome{{NodeName: "n1", Error: "same"}, {NodeName: "n2", Error: "same"}, {NodeName: "n3", Error: "other"}})
	if !strings.Contains(msg, "n1, n2: same; n3: other") {
		t.Errorf("%q", msg)
	}
}

func TestNFSRequestCheck(t *testing.T) {
	good := nfsRequest{Name: "nas", Server: "nas.lan", Path: "/export/vms/"}
	if err := good.check(); err != nil || good.Path != "/export/vms" {
		t.Errorf("%v %+v", err, good)
	}
	for name, q := range map[string]nfsRequest{
		"no name":         {Server: "s", Path: "/x"},
		"slash in name":   {Name: "a/b", Server: "s", Path: "/x"},
		"dot dot name":    {Name: "..", Server: "s", Path: "/x"},
		"no server":       {Name: "n", Path: "/x"},
		"server option":   {Name: "n", Server: "-o", Path: "/x"},
		"server space":    {Name: "n", Server: "a b", Path: "/x"},
		"relative path":   {Name: "n", Server: "s", Path: "x"},
		"path traversal":  {Name: "n", Server: "s", Path: "/x/../etc"},
		"path with quote": {Name: "n", Server: "s", Path: "/x'y"},
		"no path":         {Name: "n", Server: "s"},
	} {
		if err := q.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNFSHints(t *testing.T) {
	cases := map[string]string{
		"internal error: Child process (mount -o nodev,nosuid,noexec s:/x /mnt/x) unexpected exit status 32: mount.nfs: access denied by server while mounting s:/x": "does not allow n3 to mount /x",
		"mount.nfs: mounting s:/x failed, reason given by server: No such file or directory":                                                                         "has no export /x",
		"mount.nfs: Failed to resolve server s: Name or service not known":                                                                                           "cannot resolve s",
		"mount: /mnt/x: bad option; for several filesystems (e.g. nfs, cifs) you might need a /sbin/mount.<type> helper program":                                     "no NFS client tools",
		"mount.nfs: Connection refused": "cannot reach the NFS server s",
		"something odd\nsecond line":    "mounting s:/x on n3 failed: something odd",
	}
	for msg, want := range cases {
		if got := nfsHint(msg, "n3", "s", "/x"); !strings.Contains(got, want) {
			t.Errorf("%q: got %q, want it to contain %q", msg, got, want)
		}
	}
	w := writeHint("qemu-img: Could not create '/mnt/x/a.img': Permission denied", "s", "/x", 107, 107)
	for _, want := range []string{"root_squash", "no_root_squash", "all_squash,anonuid=107,anongid=107", "owned by 107:107"} {
		if !strings.Contains(w, want) {
			t.Errorf("write hint lacks %q: %s", want, w)
		}
	}
	if w := writeHint("Read-only file system", "s", "/x", 0, 0); !strings.Contains(w, "read-only") {
		t.Errorf("%s", w)
	}
}

func TestParseNFSFacts(t *testing.T) {
	f := parseNFSFacts("client=yes\nselinux=Enforcing\nvirt_use_nfs=off\nqemu=107:107\n")
	if !f.Client || f.SELinux != "enforcing" || f.VirtUseNFS != "off" || f.UID != 107 || f.GID != 107 {
		t.Errorf("%+v", f)
	}
	if f := parseNFSFacts("client=no\n"); f.Client || f.SELinux != "" {
		t.Errorf("%+v", f)
	}
}

func TestRefsDirAndRefusal(t *testing.T) {
	x := `<domain><devices><disk type='file'><source file='/mnt/nas/vm1.qcow2'/></disk></devices></domain>`
	if !refsDir(x, "/mnt/nas") || !refsDir(x, "/mnt/nas/") {
		t.Error("a file below the mount is a use")
	}
	if refsDir(x, "/mnt/na") || refsDir(x, "/mnt/nas2") {
		t.Error("a longer or shorter directory name is not")
	}
	msg := removeRefusal("nas", map[string][]string{"n2": {"b"}, "n1": {"a", "c"}})
	if !strings.Contains(msg, "a, c on n1; b on n2") || !strings.Contains(msg, "Move those disks") {
		t.Errorf("%q", msg)
	}
}

func TestProbeCommandAndParse(t *testing.T) {
	cmd := probeCommand([]string{"/mnt/a", "/mnt/b c"})
	if !strings.Contains(cmd, "timeout -s KILL 4 stat -f") || !strings.HasSuffix(cmd, "wait") || !strings.Contains(cmd, "'/mnt/b c'") {
		t.Errorf("%s", cmd)
	}
	got := parseProbe("up /mnt/a\ndown /mnt/b\nunmounted /mnt/c\nbogus /mnt/d\n\nup\n")
	want := map[string]mountState{"/mnt/a": mountUp, "/mnt/b": mountDown, "/mnt/c": mountUnmounted}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestProberCachesAndFailsClosed(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	reply := "up /mnt/a\n"
	var rerr error
	pp := &poolProber{
		cache: map[*hostconn.Host]*probeEntry{}, busy: map[*hostconn.Host]chan struct{}{},
		now: func() time.Time { return now },
		run: func(h *hostconn.Host, cmd string) (string, error) { calls++; return reply, rerr },
	}
	h := &hostconn.Host{}
	if st := pp.probe(h, []string{"/mnt/a"}); st["/mnt/a"] != mountUp || calls != 1 {
		t.Fatalf("%v %d", st, calls)
	}
	pp.probe(h, []string{"/mnt/a"})
	if calls != 1 {
		t.Errorf("answer not reused: %d calls", calls)
	}
	now = now.Add(probeTTL + time.Second)
	reply = "down /mnt/a\n"
	if st := pp.probe(h, []string{"/mnt/a"}); st["/mnt/a"] != mountDown || calls != 2 {
		t.Errorf("%v %d", st, calls)
	}
	// the node itself not answering is "down", never "fine"
	now = now.Add(probeTTL + time.Second)
	reply, rerr = "", errors.New("ssh: connection lost")
	if st := pp.probe(h, []string{"/mnt/a"}); st["/mnt/a"] != mountDown {
		t.Errorf("%v", st)
	}
	// a path that is not plain is never put in a command
	if st := pp.probe(h, []string{"/mnt/a b"}); st["/mnt/a b"] != mountUp {
		t.Errorf("%v", st)
	}
}

func TestOwnerProblem(t *testing.T) {
	for name, c := range map[string]struct {
		uid, mode, qemu int
		want            bool
	}{
		"no_root_squash: root owns it, libvirt hands it over": {0, 0o600, 107, false},
		"all_squash to the VM user":                           {107, 0o600, 107, false},
		"root_squash: owned by nobody, private":               {65534, 0o600, 107, true},
		"root_squash but world writable":                      {65534, 0o666, 107, false},
		"somebody else, group only":                           {1000, 0o660, 107, true},
		"unknown owner is not a verdict":                      {-1, 0, 107, false},
	} {
		if got := ownerProblem(c.uid, c.mode, c.qemu); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if u, m := parseOwner("65534 600\n"); u != 65534 || m != 0o600 {
		t.Errorf("%d %o", u, m)
	}
	if u, _ := parseOwner("garbage"); u != -1 {
		t.Error("garbage must not parse")
	}
}

func TestDeniedOnNFS(t *testing.T) {
	m := openDeniedRE.FindStringSubmatch(`internal error: process exited while connecting to monitor: qemu-kvm: -blockdev {"driver":"file"}: Could not open '/mnt/nas/vm.qcow2': Permission denied`)
	if m == nil || m[1] != "/mnt/nas/vm.qcow2" {
		t.Fatalf("%v", m)
	}
	got := deniedOnNFS("n3", "/mnt/nas/vm.qcow2", "nas:/vms", nfsFacts{SELinux: "enforcing", VirtUseNFS: "on", UID: 107, GID: 107})
	for _, want := range []string{"root_squash", "no_root_squash", "all_squash,anonuid=107,anongid=107", "nas:/vms"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q: %s", want, got)
		}
	}
	got = deniedOnNFS("n3", "/mnt/nas/vm.qcow2", "nas:/vms", nfsFacts{SELinux: "enforcing", VirtUseNFS: "off"})
	if !strings.Contains(got, "virt_use_nfs") || strings.Contains(got, "root_squash") {
		t.Errorf("selinux first: %s", got)
	}
}
