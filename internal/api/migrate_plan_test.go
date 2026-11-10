package api

import (
	"reflect"
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestShq(t *testing.T) {
	for in, want := range map[string]string{
		"plain":      `'plain'`,
		"it's":       `'it'\''s'`,
		"a b;rm -rf": `'a b;rm -rf'`,
		"":           `''`,
	} {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSafePath(t *testing.T) {
	for p, ok := range map[string]bool{
		"/var/lib/libvirt/images/vm.s1.qcow2": true,
		"/var/lib/libvirt/images/a b":         false,
		"/x/$(id)":                            false,
		"/x/a;b":                              false,
		"relative/path":                       false,
		"/x/it's":                             false,
	} {
		if safePathRE.MatchString(p) != ok {
			t.Errorf("safePathRE(%q) != %v", p, ok)
		}
	}
}

const domXML = `<domain type='kvm'><name>vm</name><uuid>u</uuid>
<os><type arch='x86_64' machine='q35'>hvm</type><nvram>/var/lib/libvirt/qemu/nvram/vm_VARS.fd</nvram></os>
<cpu mode='custom' match='exact'><model>Skylake</model><feature policy='require' name='vmx'/></cpu>
<devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/p/a.qcow2'/><target dev='vda'/></disk>
<disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='/p/ro.img'/><target dev='vdb'/><readonly/></disk>
<disk type='file' device='cdrom'><source file='/iso/x.iso'/><target dev='sda'/><readonly/></disk>
<interface type='network'><source network='default'/></interface>
<interface type='bridge'><source bridge='br0'/></interface>
<hostdev mode='subsystem' type='usb'/>
<tpm model='tpm-crb'/>
</devices></domain>`

func TestParseMigDomain(t *testing.T) {
	d, err := parseMigDomain(domXML)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Disks) != 3 || d.Disks[0].Path != "/p/a.qcow2" || !d.Disks[1].ReadOnly || d.Disks[2].Device != "cdrom" {
		t.Errorf("disks: %+v", d.Disks)
	}
	if len(d.Ifs) != 2 || d.Ifs[0].Network != "default" || d.Ifs[1].Bridge != "br0" {
		t.Errorf("ifs: %+v", d.Ifs)
	}
	if d.Hostdevs != 1 || !d.TPM || d.NVRAM == "" || d.CPUMode != "custom" || d.Arch != "x86_64" {
		t.Errorf("domain: %+v", d)
	}
	if len(d.CPUXML) < 11 || d.CPUXML[:4] != "<cpu" || d.CPUXML[len(d.CPUXML)-6:] != "</cpu>" {
		t.Errorf("cpu xml: %q", d.CPUXML)
	}
}

func TestParseChain(t *testing.T) {
	c, err := parseChain(`[{"filename":"/p/top.qcow2","format":"qcow2","virtual-size":1048576},{"filename":"/p/base.qcow2","format":"qcow2","virtual-size":1048576}]`)
	if err != nil || len(c) != 2 || c[0].Path != "/p/top.qcow2" || c[1].Path != "/p/base.qcow2" || c[0].VirtualSize != 1048576 {
		t.Errorf("%+v %v", c, err)
	}
	if _, err := parseChain("not json"); err == nil {
		t.Error("expected error")
	}
	if _, err := parseChain("[]"); err == nil {
		t.Error("expected error for empty chain")
	}
}

func TestParseSnapshotFiles(t *testing.T) {
	f, err := parseSnapshotFiles(`<domainsnapshot><name>s1</name>
<memory snapshot='external' file='/p/vm.s1.mem'/>
<disks><disk name='vda' snapshot='external'><source file='/p/vm.s1.qcow2'/></disk><disk name='vdb' snapshot='no'/></disks>
<domain><devices>
<disk type='file' device='disk'><source file='/p/vm.qcow2'/></disk>
<disk type='file' device='cdrom'><source file='/iso/x.iso'/></disk>
<disk type='file' device='disk'><source file='/p/ro.img'/><readonly/></disk>
</devices></domain></domainsnapshot>`)
	if err != nil {
		t.Fatal(err)
	}
	want := snapFiles{Memory: "/p/vm.s1.mem", Disks: []string{"/p/vm.s1.qcow2"}, Domain: []string{"/p/vm.qcow2"}}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("got %+v want %+v", f, want)
	}
	f, _ = parseSnapshotFiles(`<domainsnapshot><memory snapshot='internal'/></domainsnapshot>`)
	if f.Memory != "" {
		t.Errorf("memory %q", f.Memory)
	}
}

func TestFileSet(t *testing.T) {
	var s fileSet
	s.add("/a", true, true)
	s.add("/b", false, false)
	s.add("/a", false, false)
	s.add("/b", true, false)
	s.add("", true, false)
	if len(s.list) != 2 || !s.get("/a").Active || !s.get("/a").Owned || !s.get("/b").Owned {
		t.Errorf("%+v %+v", s.get("/a"), s.get("/b"))
	}
}

func TestPlanFlagsAndCopyList(t *testing.T) {
	p := &migPlan{Mode: "live", Disks: []planDisk{{migDisk: migDisk{Target: "vda"}}}}
	if p.mirrorFlag() != libvirt.MigrateNonSharedDisk {
		t.Error("live without snapshots or chain mirrors the whole disk")
	}
	p.Disks[0].Chain = []imgInfo{{Path: "/a"}, {Path: "/b"}}
	if p.mirrorFlag() != libvirt.MigrateNonSharedInc {
		t.Error("an overlay needs NonSharedInc")
	}
	p.Disks[0].Chain = nil
	p.Snaps = []migSnap{{Name: "s"}}
	if p.mirrorFlag() != libvirt.MigrateNonSharedInc {
		t.Error("snapshots need NonSharedInc")
	}
	p.Mode = "offline"
	if p.mirrorFlag() != 0 {
		t.Error("offline mirrors nothing")
	}
	p.Files.add("/top", true, false)
	p.Files.add("/act", true, true)
	p.Files.add("/skip", false, false)
	p.Files.get("/skip").Skip = true
	if l := p.copyList(); len(l) != 1 || l[0].Path != "/top" {
		t.Errorf("copy list: %+v", l)
	}
}

func TestChecksBlocked(t *testing.T) {
	var c checkList
	c.pass("a", "")
	c.warn("b", "")
	if c.blocked() {
		t.Error("warnings must not block")
	}
	c.bad("c", "")
	if !c.blocked() {
		t.Error("errors must block")
	}
}
