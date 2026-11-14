package api

import (
	"strings"
	"testing"
)

func TestSharedDisksOf(t *testing.T) {
	nfs := poolDesc{Name: "share", Type: "netfs", Target: "/mnt/share", Server: "10.0.0.9", Export: "/exp", Active: true}
	local := poolDesc{Name: "default", Type: "dir", Target: "/var/lib/libvirt/images", Active: true}
	src := map[string]poolDesc{"/mnt/share": nfs, local.Target: local}
	dstSame := map[string]poolDesc{"/mnt/share": nfs, local.Target: local}
	dstOther := map[string]poolDesc{"/mnt/share": {Name: "share", Type: "netfs", Target: "/mnt/share", Server: "10.0.0.8", Export: "/exp", Active: true}, local.Target: local}
	dstNone := map[string]poolDesc{local.Target: local}
	disk := func(target, p string) migDisk { return migDisk{Target: target, Path: p, Device: "disk"} }
	one := migDomain{Name: "vm", Disks: []migDisk{disk("vda", "/mnt/share/vm.qcow2")}}
	two := migDomain{Name: "vm", Disks: []migDisk{disk("vda", "/var/lib/libvirt/images/vm.qcow2"), disk("vdb", "/mnt/share/data.qcow2")}}
	localOnly := migDomain{Name: "vm", Disks: []migDisk{disk("vda", "/var/lib/libvirt/images/vm.qcow2")}}
	media := migDomain{Name: "vm", Disks: []migDisk{disk("vda", "/var/lib/libvirt/images/vm.qcow2"), {Target: "sda", Path: "/mnt/share/x.iso", Device: "cdrom", ReadOnly: true}}}

	cases := []struct {
		name string
		md   migDomain
		dst  map[string]poolDesc
		want int
	}{
		{"every disk on a share both nodes mount", one, dstSame, 1},
		{"one shared, one local: refused as well", two, dstSame, 1},
		{"local only", localOnly, dstSame, 0},
		{"the standby mounts another share at that path: a copy is right", one, dstOther, 0},
		{"the standby has no such share: a copy is right", one, dstNone, 0},
		{"install media on the share is no disk", media, dstSame, 0},
	}
	for _, c := range cases {
		got := sharedDisksOf(c.md, "n1", "n2", src, c.dst)
		if len(got) != c.want {
			t.Errorf("%s: %v, want %d", c.name, got, c.want)
		}
	}
	msg := sharedRefusal("vm", "n2", sharedDisksOf(two, "n1", "n2", src, dstSame))
	for _, w := range []string{"vdb on the shared storage share (10.0.0.9:/exp)", "n2", "Move the disk to local storage", "Migrate"} {
		if !strings.Contains(msg, w) {
			t.Errorf("refusal lacks %q: %s", w, msg)
		}
	}
	if sharedRefusal("vm", "n2", nil) != "" {
		t.Error("no shared disks must give no refusal")
	}
}
