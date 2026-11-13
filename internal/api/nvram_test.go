package api

import (
	"strings"
	"testing"
)

const nvramRawXML = `<domain><os firmware='efi'><type>hvm</type><loader readonly='yes' type='pflash' format='raw'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>` +
	`<nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd' templateFormat='raw' format='raw'>/var/lib/libvirt/qemu/nvram/a_VARS.fd</nvram></os></domain>`

func TestParseNvramEl(t *testing.T) {
	n, ok := parseNvramEl(nvramRawXML)
	want := nvramEl{Template: "/usr/share/OVMF/OVMF_VARS_4M.fd", TemplateFormat: "raw", Format: "raw", Path: "/var/lib/libvirt/qemu/nvram/a_VARS.fd"}
	if !ok || n != want {
		t.Fatalf("got %+v %v", n, ok)
	}
	if nvramIsQcow2(nvramRawXML) {
		t.Error("raw store reported as qcow2")
	}
	if _, ok := parseNvramEl("<domain><os/></domain>"); ok {
		t.Error("no nvram element")
	}
}

func TestWithQcow2Nvram(t *testing.T) {
	n, _ := parseNvramEl(nvramRawXML)
	out := withQcow2Nvram(nvramRawXML, n, "/var/lib/libvirt/qemu/nvram/a_VARS.qcow2")
	if !nvramIsQcow2(out) || !strings.Contains(out, ">/var/lib/libvirt/qemu/nvram/a_VARS.qcow2</nvram>") ||
		!strings.Contains(out, "template='/var/lib/libvirt/qemu/nvram/a_VARS.qcow2' templateFormat='qcow2'") || strings.Contains(out, "a_VARS.fd") {
		t.Errorf("rewritten: %s", out)
	}
	if !strings.Contains(out, "OVMF_CODE_4M.fd") {
		t.Error("the rest of the definition must stay")
	}
	if strings.Contains(out, "firmware=") {
		t.Errorf("libvirt must not pick the firmware again at start: %s", out)
	}
}

func TestWithoutFirmwareSelection(t *testing.T) {
	in := "<domain><os firmware='efi'>\n    <type>hvm</type>\n    <firmware>\n      <feature enabled='yes' name='secure-boot'/>\n    </firmware>\n    <loader readonly='yes' secure='yes' type='pflash' format='raw'>/c.fd</loader>\n  </os></domain>"
	out := withoutFirmwareSelection(in)
	for _, bad := range []string{"firmware", "<feature"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q left in %s", bad, out)
		}
	}
	if !strings.Contains(out, "<os>") || !strings.Contains(out, "secure='yes'") || !strings.Contains(out, "<type>hvm</type>") {
		t.Errorf("rest must stay: %s", out)
	}
}

func TestUEFIMemoryWarning(t *testing.T) {
	for _, c := range []struct {
		fw   string
		mem  int
		warn bool
	}{{"uefi", 256, true}, {"uefi", 511, true}, {"uefi", 512, false}, {"bios", 128, false}, {"uefi", 2048, false}} {
		q := createVMRequest{Firmware: c.fw, MemoryMiB: c.mem}
		if got := q.uefiMemoryWarning() != ""; got != c.warn {
			t.Errorf("%s %d MiB: warning %v, want %v", c.fw, c.mem, got, c.warn)
		}
	}
	if w := (createVMRequest{Firmware: "uefi", MemoryMiB: 128}).uefiMemoryWarning(); w != "UEFI guests usually need at least 512 MiB to boot." {
		t.Errorf("text: %q", w)
	}
}

func TestParseMigDomainNVRAMFormat(t *testing.T) {
	d, err := parseMigDomain(`<domain><name>a</name><uuid>x</uuid><os><type arch='x86_64' machine='q35'>hvm</type><nvram format='qcow2'>/n/a.qcow2</nvram></os></domain>`)
	if err != nil || d.NVRAM != "/n/a.qcow2" || d.NVRAMFormat != "qcow2" {
		t.Errorf("%+v %v", d, err)
	}
	d, _ = parseMigDomain(`<domain><name>a</name><uuid>x</uuid><os><type arch='x86_64' machine='q35'>hvm</type><nvram>/n/a.fd</nvram></os></domain>`)
	if d.NVRAMFormat != "raw" {
		t.Errorf("default format %q", d.NVRAMFormat)
	}
}

func TestRawNvramTemplate(t *testing.T) {
	n, _ := parseNvramEl(nvramRawXML)
	if got := rawNvramTemplate(nvramRawXML, n); got != "/usr/share/OVMF/OVMF_VARS_4M.fd" {
		t.Errorf("raw store: %q", got)
	}
	out := withQcow2Nvram(nvramRawXML, n, "/var/lib/libvirt/qemu/nvram/a_VARS.qcow2")
	n2, _ := parseNvramEl(out)
	if got := rawNvramTemplate(out, n2); got != "" {
		t.Errorf("a self-named template without metadata must give nothing, got %q", got)
	}
	withMeta := strings.Replace(out, "<os", "<metadata><virticle:meta xmlns:virticle='x'><virticle:nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd'/></virticle:meta></metadata><os", 1)
	if got := rawNvramTemplate(withMeta, n2); got != "/usr/share/OVMF/OVMF_VARS_4M.fd" {
		t.Errorf("from metadata: %q", got)
	}
	back := withRawNvramTemplate(strings.Replace(out, ">/var/lib/libvirt/qemu/nvram/a_VARS.qcow2</nvram>", "></nvram>", 1), "/usr/share/OVMF/OVMF_VARS_4M.fd")
	if !strings.Contains(back, "<nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd' templateFormat='raw' format='qcow2'></nvram>") {
		t.Errorf("clone: %s", back)
	}
}

func TestWithNvramTemplate(t *testing.T) {
	n, _ := parseNvramEl(nvramRawXML)
	self := withQcow2Nvram(nvramRawXML, n, "/n/a_VARS.qcow2")
	got := withNvramTemplate(self, "/n/b_VARS.qcow2")
	if !strings.Contains(got, "template='/n/b_VARS.qcow2' templateFormat='qcow2'") || !strings.Contains(got, ">/n/a_VARS.qcow2</nvram>") {
		t.Errorf("%s", got)
	}
}
