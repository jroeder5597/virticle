package api

import (
	"strings"
	"testing"

	"virticle/internal/domxml"
)

const moveDom = `<domain type='kvm'>
  <name>t</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/a.qcow2'>
        <seclabel model='dac' relabel='no'/>
      </source>
      <backingStore type='file'>
        <format type='qcow2'/>
        <source file='/var/lib/libvirt/images/base.qcow2'/>
      </backingStore>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x04' slot='0x00' function='0x0'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/x.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='block' device='disk'>
      <driver name='qemu' type='raw'/>
      <source dev='/dev/sdb'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
  </devices>
</domain>
`

func TestDiskMoveXML(t *testing.T) {
	out, err := diskMoveXML(moveDom, "vda", "/var/lib/libvirt/dm-pool/a.raw", "raw")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<source file='/var/lib/libvirt/dm-pool/a.raw'>",
		"<seclabel model='dac' relabel='no'/>",
		"<driver name='qemu' type='raw'/>",
		"<address type='pci' domain='0x0000' bus='0x04' slot='0x00' function='0x0'/>",
		"<source dev='/dev/sdb'/>", "<source file='/x.iso'/>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "backingStore") || strings.Contains(out, "a.qcow2") {
		t.Errorf("backing chain or old path left in\n%s", out)
	}
	// Everything outside the disk is byte for byte the same.
	i := strings.Index(moveDom, "<disk")
	j := strings.Index(moveDom, "<disk type='file' device='cdrom'>")
	if !strings.HasPrefix(out, moveDom[:i]) || !strings.HasSuffix(out, moveDom[j:]) {
		t.Errorf("text outside the disk changed:\n%s", out)
	}
	if _, err := domxml.Parse(out); err != nil {
		t.Error(err)
	}
	if _, err := diskMoveXML(moveDom, "vdz", "/p", ""); err == nil {
		t.Error("missing disk accepted")
	}
	// Same format: the driver is left alone.
	out, _ = diskMoveXML(moveDom, "vda", "/p/a.qcow2", "")
	if !strings.Contains(out, "<driver name='qemu' type='qcow2'/>") {
		t.Errorf("driver changed:\n%s", out)
	}
}

func TestMoveDiskOf(t *testing.T) {
	doc, _ := domxml.Parse(moveDom)
	_, a, _ := findMoveDisk(doc, "vda")
	if a.Type != "file" || a.Format != "qcow2" || !a.Backing || a.File != "/var/lib/libvirt/images/a.qcow2" {
		t.Errorf("%+v", a)
	}
	_, c, _ := findMoveDisk(doc, "sda")
	if c.Device != "cdrom" || !c.ReadOnly {
		t.Errorf("%+v", c)
	}
	_, b, _ := findMoveDisk(doc, "vdb")
	if b.Type != "block" || b.File != "" {
		t.Errorf("%+v", b)
	}
}

func TestSnapRefs(t *testing.T) {
	mk := func(name string, disks ...[2]string) snapXML {
		var s snapXML
		s.Name = name
		for _, d := range disks {
			s.Disks = append(s.Disks, struct {
				Name     string `xml:"name,attr"`
				Snapshot string `xml:"snapshot,attr"`
				Source   struct {
					File string `xml:"file,attr"`
				} `xml:"source"`
			}{Name: d[0], Snapshot: d[1]})
		}
		return s
	}
	in, ex, oth := snapRefs([]snapXML{
		mk("i", [2]string{"vda", "internal"}),
		mk("e", [2]string{"vda", "external"}),
		mk("n", [2]string{"vda", "no"}),
		mk("o", [2]string{"vdb", "internal"}),
		mk("bare"),
	}, "vda")
	eq := func(got []string, want string) {
		if strings.Join(got, ",") != want {
			t.Errorf("got %v want %s", got, want)
		}
	}
	eq(in, "i,bare")
	eq(ex, "e")
	eq(oth, "n,o")
}

func TestMoveSpaceAndPools(t *testing.T) {
	need, worst := moveSpace("qcow2", 10<<30, 1<<30, false)
	if need <= 1<<30 || need >= 2<<30 || worst != need {
		t.Errorf("qcow2: %d %d", need, worst)
	}
	need, worst = moveSpace("raw", 10<<30, 1<<30, false)
	if worst != 10<<30 || need >= worst {
		t.Errorf("raw: %d %d", need, worst)
	}
	if need, worst = moveSpace("qcow2", 10<<30, 1<<20, true); need != 10<<30 || worst != need {
		t.Errorf("chain: %d %d", need, worst)
	}
	for _, ok := range []string{"dir", "fs", "netfs"} {
		if v, _ := poolSupported(ok); !v {
			t.Error(ok)
		}
	}
	for _, bad := range []string{"logical", "iscsi", "rbd", "weird"} {
		if v, msg := poolSupported(bad); v || msg == "" {
			t.Error(bad)
		}
	}
	for n, want := range map[string]bool{"a.qcow2": true, "dm-a_1+x.raw": true, "": false, "../x": false, "-x": false, "a b": false, "a/b": false} {
		if moveNameRE.MatchString(n) != want {
			t.Errorf("name %q", n)
		}
	}
}

func TestParseVolDesc(t *testing.T) {
	v, err := parseVolDesc(`<volume><name>a.qcow2</name><capacity unit='bytes'>1000</capacity><allocation unit='bytes'>200</allocation>
<target><path>/p/a.qcow2</path><format type='qcow2'/></target><backingStore><path>/p/base.qcow2</path><format type='qcow2'/></backingStore></volume>`)
	if err != nil || v.Path != "/p/a.qcow2" || v.Format != "qcow2" || v.Capacity != 1000 || v.Allocation != 200 || v.Backing != "/p/base.qcow2" {
		t.Errorf("%+v %v", v, err)
	}
}

func TestBlockCopyDestXML(t *testing.T) {
	got := blockCopyDestXML("/p/a'b.qcow2", "qcow2")
	if !strings.Contains(got, "type='qcow2'") || strings.Contains(got, "a'b") {
		t.Error(got)
	}
}
