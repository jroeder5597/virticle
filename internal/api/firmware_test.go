package api

import (
	"encoding/xml"
	"strings"
	"testing"
)

const capsSample = `<domainCapabilities>
  <os supported='yes'>
    <enum name='firmware'><value>efi</value></enum>
    <loader supported='yes'>
      <enum name='secure'><value>yes</value><value>no</value></enum>
    </loader>
  </os>
  <devices>
    <tpm supported='yes'>
      <enum name='model'><value>tpm-tis</value><value>tpm-crb</value></enum>
      <enum name='backendModel'><value>passthrough</value><value>emulator</value></enum>
    </tpm>
  </devices>
</domainCapabilities>`

func TestParseFwCaps(t *testing.T) {
	c, err := parseFwCaps(capsSample)
	if err != nil {
		t.Fatal(err)
	}
	if !c.UEFI || !c.SecureBoot || !c.TPM || c.tpmModel() != "tpm-crb" {
		t.Fatalf("%+v", c)
	}
	none, _ := parseFwCaps(`<domainCapabilities><os supported='yes'><enum name='firmware'/></os><devices><tpm supported='no'/></devices></domainCapabilities>`)
	if none.UEFI || none.TPM || none.SecureBoot {
		t.Fatalf("%+v", none)
	}
	q := createVMRequest{Firmware: "uefi", SecureBoot: true, TPM: true}
	if err := q.checkFirmware(none); err == nil || !strings.Contains(err.Error(), "UEFI") {
		t.Fatalf("want a UEFI error, got %v", err)
	}
	if err := (createVMRequest{Firmware: "bios", SecureBoot: true}).checkFirmware(c); err == nil {
		t.Fatal("secure boot on BIOS must be refused")
	}
	if err := (createVMRequest{Firmware: "uefi", TPM: true}).checkFirmware(c); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, q createVMRequest) string {
	t.Helper()
	if err := q.normalize(); err != nil {
		t.Fatal(err)
	}
	out := q.domainXML("/p/d.qcow2")
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, out)
	}
	return out
}

func TestDomainXMLFirmware(t *testing.T) {
	base := createVMRequest{Name: "x", VCPUs: 1, MemoryMiB: 128, Network: "default", ISO: "/i.iso"}
	bios := build(t, base)
	for _, no := range []string{"firmware='efi'", "<tpm", "smm", "hyperv"} {
		if strings.Contains(bios, no) {
			t.Errorf("bios VM must not contain %s", no)
		}
	}
	u := base
	u.Firmware, u.SecureBoot, u.TPM = "uefi", true, true
	x := build(t, u)
	for _, want := range []string{"<os firmware='efi'>", "enabled='yes' name='secure-boot'", "enabled='yes' name='enrolled-keys'", "<smm state='on'/>", "<tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>"} {
		if !strings.Contains(x, want) {
			t.Errorf("missing %s in\n%s", want, x)
		}
	}
	plain := base
	plain.Firmware = "uefi"
	if x := build(t, plain); !strings.Contains(x, "enabled='no' name='secure-boot'") || strings.Contains(x, "smm") {
		t.Errorf("plain UEFI:\n%s", x)
	}
}

func TestDomainXMLWindows(t *testing.T) {
	w := build(t, createVMRequest{Name: "w", VCPUs: 2, MemoryMiB: 4096, Network: "default", ISO: "/i.iso", OSType: "windows"})
	for _, want := range []string{"<hyperv mode='custom'>", "offset='localtime'", "name='hypervclock' present='yes'", "<target dev='sda' bus='sata'/>", "<target dev='sdb' bus='sata'/>", "<model type='e1000e'/>"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %s in\n%s", want, w)
		}
	}
	v := build(t, createVMRequest{Name: "w", VCPUs: 2, MemoryMiB: 4096, Network: "default", OSType: "windows", DiskBus: "virtio", NICModel: "virtio"})
	if !strings.Contains(v, "<target dev='vda' bus='virtio'/>") || !strings.Contains(v, "<model type='virtio'/>") {
		t.Errorf("virtio override:\n%s", v)
	}
	if err := (&createVMRequest{OSType: "beos"}).normalize(); err == nil {
		t.Error("unknown osType accepted")
	}
}

func TestFirmwareInfo(t *testing.T) {
	f := firmwareInfo(`<domain><os firmware='efi'><type>hvm</type><firmware><feature enabled='yes' name='secure-boot'/></firmware><loader readonly='yes' secure='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.secboot.fd</loader><nvram template='/t'>/var/lib/libvirt/qemu/nvram/x_VARS.fd</nvram></os><devices><tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm></devices></domain>`)
	if f.Firmware != "uefi" || !f.SecureBoot || f.TPMModel != "tpm-crb" || f.TPMVersion != "2.0" || !strings.HasSuffix(f.NVRAM, "_VARS.fd") {
		t.Fatalf("%+v", f)
	}
	if b := firmwareInfo(`<domain><os><type>hvm</type></os></domain>`); b.Firmware != "bios" || b.SecureBoot || b.TPMModel != "" {
		t.Fatalf("%+v", b)
	}
}

func TestTags(t *testing.T) {
	got, err := cleanTags([]string{" prod ", "web.v2:eu", "prod", "a b"})
	if err != nil || strings.Join(got, ",") != "prod,web.v2:eu,a b" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "-x", "a/b", "<b>", strings.Repeat("a", 32), "tab\tx", "ü"} {
		if _, err := cleanTags([]string{bad}); err == nil {
			t.Errorf("tag %q accepted", bad)
		}
	}
	if _, err := cleanTags([]string{strings.Repeat("a", 31)}); err != nil {
		t.Errorf("31 chars must be fine: %v", err)
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = "t" + string(rune('a'+i))
	}
	if _, err := cleanTags(many); err == nil {
		t.Error("17 tags accepted")
	}
}

func TestTagsCoexist(t *testing.T) {
	// Template marker, snapshot schedule and tags all live under one root.
	meta, err := metaSetChild("", "template", `<template sealed="linux"/>`)
	if err != nil {
		t.Fatal(err)
	}
	meta, err = metaSetChild(meta, "snapshot-schedule", `<snapshot-schedule enabled="yes" mode="daily" at="01:00" keep="3" prefix="auto" memory="no" quiesce="no"/>`)
	if err != nil {
		t.Fatal(err)
	}
	// libvirt hands the children back prefixed.
	back := `<vp:meta xmlns:vp="https://virt-proj.invalid/ns/1">` + strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(meta, "<meta>"), "</meta>"), "<", "<vp:") + `</vp:meta>`
	back = strings.ReplaceAll(back, "<vp:/", "</vp:")
	meta, err = metaSetChild(back, "tags", tagsElem([]string{"a&b", "prod"}))
	if err != nil {
		t.Fatal(err)
	}
	m, tags := parseAppMeta(meta)
	if !m.Template || m.Sealed != "linux" || strings.Join(tags, ",") != "a&b,prod" || !strings.Contains(meta, "snapshot-schedule") {
		t.Fatalf("%+v %v\n%s", m, tags, meta)
	}
	meta, _ = metaSetChild(meta, "tags", "")
	if _, tags := parseAppMeta(meta); len(tags) != 0 || !strings.Contains(meta, "template") {
		t.Fatalf("removing tags: %v %s", tags, meta)
	}
}
