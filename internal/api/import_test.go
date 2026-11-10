package api

import (
	"strings"
	"testing"
)

func TestParseImgInfo(t *testing.T) {
	ok := `{"format":"qcow2","virtual-size":1048576,"actual-size":200704,"format-specific":{"type":"qcow2","data":{"compat":"1.1"}}}`
	if in, err := parseImgInfo(ok, "/p/a.img"); err != nil || in.Format != "qcow2" || in.VirtualSize != 1048576 {
		t.Fatalf("%+v %v", in, err)
	}
	cases := map[string]string{
		"backing":   `{"format":"qcow2","virtual-size":1,"backing-filename":"/etc/passwd","full-backing-filename":"/etc/passwd"}`,
		"data file": `{"format":"qcow2","virtual-size":1,"format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`,
		"extent":    `{"format":"vmdk","virtual-size":1,"format-specific":{"type":"vmdk","data":{"extents":[{"filename":"/etc/shadow","format":"FLAT"}]}}}`,
		"format":    `{"format":"qed","virtual-size":1}`,
		"encrypted": `{"format":"qcow2","virtual-size":1,"encrypted":true}`,
		"garbage":   `qemu-img: Could not open`,
	}
	for name, js := range cases {
		if _, err := parseImgInfo(js, "/p/a.img"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	self := `{"format":"vmdk","virtual-size":1,"format-specific":{"type":"vmdk","data":{"extents":[{"filename":"/w/d.vmdk","format":"SPARSE"}]}}}`
	if _, err := parseImgInfo(self, "/w/d.vmdk"); err != nil {
		t.Errorf("self-contained vmdk refused: %v", err)
	}
}

func TestParseTarListing(t *testing.T) {
	good := "-rw-r--r-- root/root      2048 2026-01-02 03:04 vm.ovf\n-rw-r--r-- root/root  10485760 2026-01-02 03:04 vm-disk1.vmdk\n"
	es, err := parseTarListing(good)
	if err != nil || len(es) != 2 || es[1].Size != 10485760 {
		t.Fatalf("%v %v", es, err)
	}
	bad := map[string]string{
		"traversal": "-rw-r--r-- root/root 5 2026-01-02 03:04 ../evil.vmdk",
		"subdir":    "-rw-r--r-- root/root 5 2026-01-02 03:04 a/b.vmdk",
		"absolute":  "-rw-r--r-- root/root 5 2026-01-02 03:04 /etc/x",
		"dir":       "drwxr-xr-x root/root 0 2026-01-02 03:04 d/",
		"symlink":   "lrwxrwxrwx root/root 0 2026-01-02 03:04 vm.vmdk -> /etc/passwd",
		"hardlink":  "hrw-r--r-- root/root 0 2026-01-02 03:04 vm.vmdk link to x",
		"newline":   "-rw-r--r-- root/root 5 2026-01-02 03:04 a.ovf\nb.vmdk",
		"escape":    "-rw-r--r-- root/root 5 2026-01-02 03:04 a\\nb.vmdk",
		"dup":       "-rw-r--r-- root/root 5 2026-01-02 03:04 a.ovf\n-rw-r--r-- root/root 5 2026-01-02 03:04 a.ovf",
		"huge":      "-rw-r--r-- root/root 9999999999999999 2026-01-02 03:04 a.vmdk",
	}
	for name, l := range bad {
		if _, err := parseTarListing(l); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var many []string
	for i := 0; i <= importMaxOVAEntries; i++ {
		many = append(many, "-rw-r--r-- root/root 5 2026-01-02 03:04 f"+strings.Repeat("x", i%10)+string(rune('a'+i%26))+string(rune('a'+i/26))+".vmdk")
	}
	if _, err := parseTarListing(strings.Join(many, "\n")); err == nil {
		t.Error("too many members accepted")
	}
}

const sampleOVF = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1" xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData" xmlns:vmw="http://www.vmware.com/schema/ovf">
  <References>
    <File ovf:id="file1" ovf:href="tiny-disk1.vmdk" ovf:size="1234"/>
    <File ovf:id="file2" ovf:href="tiny-disk2.vmdk" ovf:size="1234"/>
  </References>
  <DiskSection>
    <Info>Virtual disk information</Info>
    <Disk ovf:diskId="vmdisk1" ovf:fileRef="file1" ovf:capacity="64" ovf:capacityAllocationUnits="byte * 2^20"/>
    <Disk ovf:diskId="vmdisk2" ovf:fileRef="file2" ovf:capacity="1073741824"/>
  </DiskSection>
  <VirtualSystem ovf:id="tiny">
    <Info>A tiny VM</Info>
    <Name>tiny-vm</Name>
    <OperatingSystemSection ovf:id="94" vmw:osType="windows9Server64Guest"><Info/><Description>Windows Server 2016</Description></OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware</Info>
      <System><vssd:ElementName>Virtual Hardware Family</vssd:ElementName><vssd:VirtualSystemType>vmx-13</vssd:VirtualSystemType></System>
      <Item><rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits><rasd:ElementName>2 virtual CPU(s)</rasd:ElementName><rasd:InstanceID>1</rasd:InstanceID><rasd:ResourceType>3</rasd:ResourceType><rasd:VirtualQuantity>2</rasd:VirtualQuantity></Item>
      <Item><rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits><rasd:ElementName>512MB of memory</rasd:ElementName><rasd:InstanceID>2</rasd:InstanceID><rasd:ResourceType>4</rasd:ResourceType><rasd:VirtualQuantity>512</rasd:VirtualQuantity></Item>
      <Item><rasd:ElementName>Hard Disk 2</rasd:ElementName><rasd:HostResource>ovf:/disk/vmdisk2</rasd:HostResource><rasd:InstanceID>6</rasd:InstanceID><rasd:ResourceType>17</rasd:ResourceType></Item>
      <Item><rasd:ElementName>Hard Disk 1</rasd:ElementName><rasd:HostResource>ovf:/disk/vmdisk1</rasd:HostResource><rasd:InstanceID>7</rasd:InstanceID><rasd:ResourceType>17</rasd:ResourceType></Item>
      <vmw:Config ovf:required="false" vmw:key="firmware" vmw:value="efi"/>
      <vmw:Config ovf:required="false" vmw:key="bootOptions.efiSecureBootEnabled" vmw:value="true"/>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

func TestParseOVF(t *testing.T) {
	o, err := parseOVF([]byte(sampleOVF))
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "tiny-vm" || o.VCPUs != 2 || o.MemoryMiB != 512 || !o.UEFI || !o.SecureBoot || !o.Windows {
		t.Fatalf("%+v", o)
	}
	// Order follows the hardware items (disk2 is listed first there).
	if len(o.Disks) != 2 || o.Disks[0].File != "tiny-disk2.vmdk" || o.Disks[1].CapacityBytes != 64<<20 {
		t.Fatalf("%+v", o.Disks)
	}
	p := o.proposal("tiny-vm", "default", "tiny-vm-disk1.qcow2")
	if p.Firmware != "uefi" || !p.SecureBoot || p.OSType != "windows" || p.ExistingVolume != "tiny-vm-disk1.qcow2" || p.MemoryMiB != 512 {
		t.Fatalf("%+v", p)
	}
	if _, err := parseOVF([]byte("<nope")); err == nil {
		t.Error("broken XML accepted")
	}
	if _, err := parseOVF([]byte(`<Envelope><VirtualSystem id="x"><Name>n</Name></VirtualSystem></Envelope>`)); err == nil {
		t.Error("OVF without disks accepted")
	}
	if _, err := parseOVF([]byte(`<Envelope><VirtualSystemCollection/></Envelope>`)); err == nil {
		t.Error("collection accepted")
	}
}

func TestParseProgress(t *testing.T) {
	log := "    (0.00/100%)\r    (12.50/100%)\r    (63.33/100%)"
	if p, ok := parseProgress(log); !ok || p < 0.633 || p > 0.634 {
		t.Fatalf("%v %v", p, ok)
	}
	if _, ok := parseProgress("nothing"); ok {
		t.Fatal("no progress expected")
	}
}
