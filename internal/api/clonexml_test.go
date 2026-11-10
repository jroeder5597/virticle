package api

import (
	"strings"
	"testing"
)

const srcXML = `<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>tmpl</name>
  <uuid>11111111-2222-3333-4444-555555555555</uuid>
  <metadata>
    <vp:meta xmlns:vp="https://virt-proj.invalid/ns/1"><vp:template sealed="linux"/><vp:snapshot-schedule enabled="yes"/></vp:meta>
    <other:thing xmlns:other="urn:x"><a>keep</a></other:thing>
  </metadata>
  <memory unit='KiB'>524288</memory>
  <os firmware='efi'>
    <type arch='x86_64' machine='pc-q35-10.0'>hvm</type>
    <nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd'>/var/lib/libvirt/qemu/nvram/tmpl_VARS.fd</nvram>
  </os>
  <futureElement foo='bar'><nested/></futureElement>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/pool/tmpl.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/pool/tmpl-seed.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='file' device='cdrom'>
      <source file='/pool/shared &amp; ok.iso'/>
      <target dev='sdb' bus='sata'/>
    </disk>
    <interface type='network'>
      <mac address='52:54:00:aa:bb:cc'/>
      <source network='default'/>
    </interface>
  </devices>
  <qemu:commandline><qemu:arg value='-foo'/></qemu:commandline>
</domain>`

func TestCloneDomainXML(t *testing.T) {
	out, err := cloneDomainXML(srcXML, cloneEdit{
		Name:     "c<1>",
		Disks:    map[string]string{"/pool/tmpl.qcow2": "/pool/c1.qcow2", "/pool/tmpl-seed.iso": ""},
		AddCDROM: "/pool/c1-seed.iso",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"<uuid>", "<mac", "tmpl.qcow2", "tmpl-seed.iso", "vp:template", "vp:meta", "snapshot-schedule", "VARS.fd</nvram>", "<name>tmpl"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q should be gone:\n%s", gone, out)
		}
	}
	for _, kept := range []string{"<name>c&lt;1&gt;</name>", "xmlns:qemu=", "<qemu:arg value='-foo'/>", "<futureElement foo='bar'><nested/></futureElement>",
		"<other:thing xmlns:other=\"urn:x\"><a>keep</a></other:thing>", "<metadata>", "<nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd'></nvram>",
		"/pool/c1.qcow2", "shared &amp; ok.iso", "<source network='default'/>", "<target dev='sdb' bus='sata'/>"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q should survive:\n%s", kept, out)
		}
	}
	// The emptied seed CD-ROM takes the new seed; no extra drive is added.
	if strings.Count(out, "c1-seed.iso") != 1 || strings.Count(out, "device='cdrom'") != 2 {
		t.Errorf("seed not attached in place:\n%s", out)
	}
}

func TestCloneDomainXMLAddCDROMAndEmptyMetadata(t *testing.T) {
	in := strings.Replace(srcXML, `    <other:thing xmlns:other="urn:x"><a>keep</a></other:thing>`+"\n", "", 1)
	out, err := cloneDomainXML(in, cloneEdit{Name: "c2", Disks: map[string]string{"/pool/tmpl.qcow2": "/pool/c2.qcow2"}, AddCDROM: "/pool/c2-seed.iso"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<metadata") {
		t.Errorf("empty metadata should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "c2-seed.iso") || !strings.Contains(out, "target dev='sdc' bus='sata'") {
		t.Errorf("new CD-ROM missing:\n%s", out)
	}
	// Without media the seed drive is just emptied.
	out, _ = cloneDomainXML(srcXML, cloneEdit{Name: "c3", Disks: map[string]string{"/pool/tmpl-seed.iso": ""}})
	if strings.Contains(out, "tmpl-seed.iso") || !strings.Contains(out, "<target dev='sda' bus='sata'/>") {
		t.Errorf("seed drive not emptied:\n%s", out)
	}
}

func TestCloneDomainXMLErrors(t *testing.T) {
	if _, err := cloneDomainXML("<foo/>", cloneEdit{Name: "x"}); err == nil {
		t.Error("expected error for non-domain")
	}
}
