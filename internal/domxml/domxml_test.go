package domxml

import (
	"strings"
	"testing"
)

const sample = `<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>t</name>
  <!-- keep me -->
  <metadata>
    <vp:tag xmlns:vp="https://virt-proj.invalid/ns/1">  odd   spacing </vp:tag>
  </metadata>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
    <boot dev='cdrom'/>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/a/b.qcow2'/>
      <backingStore/>
      <target dev='vda' bus='virtio'/>
      <boot order='9'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/iso/x.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <interface type='network'>
      <mac address='52:54:00:AA:BB:CC'/>
      <source network='default' portid='1' bridge='virbr0'/>
      <model type='virtio'/>
    </interface>
    <interface type='bridge'><mac address='52:54:00:00:00:02'/><source bridge='br0'/></interface>
  </devices>
  <qemu:commandline>
    <qemu:arg value='-foo'/>
  </qemu:commandline>
</domain>`

func TestBootOrder(t *testing.T) {
	out, err := SetBootOrder(sample, []string{"52:54:00:aa:bb:cc", "sda", "vda"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<!-- keep me -->", `<vp:tag xmlns:vp="https://virt-proj.invalid/ns/1">  odd   spacing </vp:tag>`,
		"<qemu:commandline>\n    <qemu:arg value='-foo'/>\n  </qemu:commandline>", "xmlns:qemu=",
		"<boot order='3'/>\n    </disk>", "<boot order='2'/>\n    </disk>",
		"<model type='virtio'/>\n      <boot order='1'/>\n    </interface>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "<boot dev") || strings.Contains(out, "order='9'") {
		t.Errorf("stale boot entries remain:\n%s", out)
	}
	if _, err := Parse(out); err != nil {
		t.Fatal(err)
	}
	// Everything outside the touched elements is identical.
	if !strings.HasPrefix(out, sample[:strings.Index(sample, "<os>")]) {
		t.Error("prefix changed")
	}
	if !strings.HasSuffix(out, sample[strings.Index(sample, "  <qemu:commandline>"):]) {
		t.Error("suffix changed")
	}
	// The second, one-line interface is not listed and stays as it was.
	if !strings.Contains(out, "<interface type='bridge'><mac address='52:54:00:00:00:02'/><source bridge='br0'/></interface>") {
		t.Error("unlisted interface changed")
	}
}

func TestBootOrderSelfClosingAndErrors(t *testing.T) {
	src := "<domain><devices><disk device='disk'><target dev='vda'/></disk><interface><mac address='52:54:00:00:00:01'/></interface></devices></domain>"
	out, err := SetBootOrder(src, []string{"vda"})
	if err != nil || !strings.Contains(out, "<target dev='vda'/><boot order='1'/></disk>") {
		t.Fatalf("%v %s", err, out)
	}
	for _, bad := range [][]string{nil, {"vdz"}, {"vda", "VDA"}} {
		if _, err := SetBootOrder(src, bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := SetBootOrder("<domain><devices>", []string{"vda"}); err == nil {
		t.Error("malformed accepted")
	}
}

func TestDiskWithSource(t *testing.T) {
	d, _ := Parse(sample)
	cd := d.Devices("disk")[1]
	out, err := d.DiskWithSource(cd, "<source file='/iso/y.iso'/>")
	if err != nil {
		t.Fatal(err)
	}
	want := "<disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='/iso/y.iso'/>\n      <target dev='sda' bus='sata'/>\n      <readonly/>\n    </disk>"
	if out != want {
		t.Errorf("swap:\n%s", out)
	}
	out, _ = d.DiskWithSource(cd, "")
	if strings.Contains(out, "source") || !strings.Contains(out, "<readonly/>") {
		t.Errorf("eject:\n%s", out)
	}
	// backingStore is dropped, boot order kept.
	out, _ = d.DiskWithSource(d.Devices("disk")[0], "<source file='/n.qcow2'/>")
	if strings.Contains(out, "backingStore") || !strings.Contains(out, "<boot order='9'/>") || !strings.Contains(out, "/n.qcow2") {
		t.Errorf("disk:\n%s", out)
	}
	// An empty drive gets a source appended.
	e, _ := Parse("<domain><devices><disk device='cdrom'>\n  <target dev='sda'/>\n</disk></devices></domain>")
	out, _ = e.DiskWithSource(e.Devices("disk")[0], "<source file='/x'/>")
	if out != "<disk device='cdrom'>\n  <target dev='sda'/>\n  <source file='/x'/>\n</disk>" {
		t.Errorf("append:\n%q", out)
	}
}

func TestSetNICSource(t *testing.T) {
	d, _ := Parse(sample)
	nic := d.Devices("interface")[0]
	out, err := d.SetNICSource(nic, "", "br0", "down")
	if err != nil {
		t.Fatal(err)
	}
	want := "<interface type='bridge'>\n      <mac address='52:54:00:AA:BB:CC'/>\n      <source bridge='br0'/>\n      <model type='virtio'/>\n      <link state='down'/>\n    </interface>"
	if out != want {
		t.Errorf("got\n%s", out)
	}
	out, _ = d.SetNICSource(d.Devices("interface")[1], "default", "", "up")
	if !strings.Contains(out, "type='network'") || !strings.Contains(out, "<source network='default'/>") || !strings.Contains(out, "<link state='up'/>") {
		t.Errorf("got\n%s", out)
	}
	out, _ = d.SetNICSource(nic, "", "", "down")
	if !strings.Contains(out, "portid='1'") || !strings.Contains(out, "<link state='down'/>") {
		t.Errorf("link only changed source:\n%s", out)
	}
}

func TestNextTarget(t *testing.T) {
	if got := NextTarget("vd", []string{"vda", "vdb", "sda"}); got != "vdc" {
		t.Error(got)
	}
	if got := NextTarget("sd", []string{"sda", "sdc"}); got != "sdb" {
		t.Error(got)
	}
	used := []string{}
	for i := 0; i < 26; i++ {
		used = append(used, "vd"+string(rune('a'+i)))
	}
	if got := NextTarget("vd", used); got != "vdaa" {
		t.Error(got)
	}
}
