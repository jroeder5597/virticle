package api

import (
	"reflect"
	"strings"
	"testing"

	"virt-proj/internal/domxml"
)

func mustDev(t *testing.T, file string) hostDevice {
	t.Helper()
	d, ok, err := parseNodeDev(fixture(t, file))
	if err != nil || !ok {
		t.Fatalf("%s: ok=%v err=%v", file, ok, err)
	}
	return d
}

func TestParseRealNodedevs(t *testing.T) {
	nic := mustDev(t, "nodedev_pci_0000_01_00_0.xml")
	if nic.Kind != "pci" || nic.ID != "0000:01:00.0" || nic.Category != "network" || nic.Driver != "virtio-pci" ||
		nic.VendorID != "1af4" || nic.ProductID != "1041" || nic.Vendor != "Red Hat, Inc." || nic.IOMMUGroup != nil {
		t.Errorf("%+v", nic)
	}
	hub := mustDev(t, "nodedev_usb_usb1.xml")
	if hub.Kind != "usb" || hub.ID != "usb:1:1" || hub.VendorID != "1d6b" || hub.ProductID != "0002" || hub.Product != "2.0 root hub" {
		t.Errorf("%+v", hub)
	}
	host := mustDev(t, "nodedev_pci_0000_00_00_0.xml")
	if host.Category != "bridge" || host.Class != "Host bridge" {
		t.Errorf("%+v", host)
	}
	if d, ok, _ := parseNodeDev("<device><name>net_lo</name><capability type='net'><interface>lo</interface></capability></device>"); ok || d.Kind != "" {
		t.Error("net devices are not host devices")
	}
}

func TestIOMMUGroupFixtureWithSeveralFunctions(t *testing.T) {
	gpu := mustDev(t, "nodedev_handwritten_gpu_0a_00_0.xml")
	aud := mustDev(t, "nodedev_handwritten_gpu_0a_00_1.xml")
	if gpu.IOMMUGroup == nil || *gpu.IOMMUGroup != 14 || gpu.Category != "gpu" || gpu.ID != "0000:0a:00.0" {
		t.Fatalf("%+v", gpu)
	}
	if !reflect.DeepEqual(gpu.groupAddr, []string{"0000:0a:00.1"}) || !reflect.DeepEqual(aud.groupAddr, []string{"0000:0a:00.0"}) {
		t.Errorf("group members: %v / %v", gpu.groupAddr, aud.groupAddr)
	}
	devs := []hostDevice{gpu, aud}
	markSafety(devs, hostFacts{Known: true, NIC: map[string]string{}, Storage: map[string]string{}, BootVGA: "0000:0a:00.0"})
	if devs[0].Unsafe || devs[1].Unsafe {
		t.Errorf("a plain GPU and its audio function are passable: %+v", devs)
	}
	if len(devs[0].GroupPeers) != 1 || devs[0].GroupPeers[0].ID != "0000:0a:00.1" || !strings.Contains(devs[0].GroupPeers[0].Name, "Audio") {
		t.Errorf("peers: %+v", devs[0].GroupPeers)
	}
	if len(devs[0].Warnings) != 1 || !strings.Contains(devs[0].Warnings[0], "booted with") {
		t.Errorf("boot GPU warning missing: %v", devs[0].Warnings)
	}
	// Attach: a note about the group, and the XML names the function.
	_, x, note, err := pickHostdev(devs, true, "0000:0a:00.0", "vm-1")
	if err != nil || !strings.Contains(note, "IOMMU group 14 also holds 0000:0a:00.1") {
		t.Fatalf("%v %q", err, note)
	}
	want := "<hostdev mode='subsystem' type='pci' managed='yes'><source><address domain='0x0000' bus='0x0a' slot='0x00' function='0x0'/></source></hostdev>"
	if x != want {
		t.Errorf("xml:\n%s\nwant\n%s", x, want)
	}
	// The other function is used by another VM: the group cannot be given away.
	devs[1].UsedBy = []vmRef{{UUID: "vm-2", Name: "other"}}
	if _, _, _, err := pickHostdev(devs, true, "0000:0a:00.0", "vm-1"); err == nil || !strings.Contains(err.Error(), "other") {
		t.Errorf("group peer in use elsewhere must refuse: %v", err)
	}
}

func TestSafetyRefusals(t *testing.T) {
	var devs []hostDevice
	for _, f := range []string{"nodedev_pci_0000_00_00_0.xml", "nodedev_pci_0000_00_1f_0.xml", "nodedev_pci_0000_01_00_0.xml",
		"nodedev_pci_0000_04_00_0.xml", "nodedev_pci_0000_02_00_0.xml", "nodedev_pci_0000_00_1f_2.xml", "nodedev_usb_usb1.xml"} {
		devs = append(devs, mustDev(t, f))
	}
	// What the lab node n1 reports: default route over 01:00.0, root on 04:00.0 (virtio-blk).
	facts := parseFacts("nic enp1s0 0000:01:00.0\nblk vda 0000:04:00.0\nok\n")
	markSafety(devs, facts)
	got := map[string]string{}
	for _, d := range devs {
		if d.Unsafe {
			got[d.ID] = d.UnsafeReason
		}
	}
	for id, frag := range map[string]string{
		"0000:00:00.0": "Host bridge",
		"0000:00:1f.0": "ISA bridge",
		"0000:01:00.0": "default route (enp1s0)",
		"0000:04:00.0": "mounted filesystem or swap of the host (vda)",
		"usb:1:1":      "root hub",
	} {
		if !strings.Contains(got[id], frag) {
			t.Errorf("%s should be refused for %q, got %q", id, frag, got[id])
		}
	}
	// The xHCI controller and the AHCI controller have nothing mounted: passable.
	for _, id := range []string{"0000:02:00.0", "0000:00:1f.2"} {
		if _, bad := got[id]; bad {
			t.Errorf("%s should not be refused: %s", id, got[id])
		}
	}
	// Refusal reaches the attach path, with the reason.
	_, _, _, err := pickHostdev(devs, true, "0000:01:00.0", "vm")
	if err == nil || !strings.HasPrefix(err.Error(), "refused: ") || !strings.Contains(err.Error(), "default route") {
		t.Errorf("attach of the NIC: %v", err)
	}
}

func TestSharedGroupWithUnsafeDevice(t *testing.T) {
	a, b := mustDev(t, "nodedev_handwritten_nic_03_00_0.xml"), mustDev(t, "nodedev_handwritten_nic_04_00_0.xml")
	devs := []hostDevice{a, b}
	markSafety(devs, parseFacts("nic eno1 0000:03:00.0\nok\n"))
	if !devs[0].Unsafe || devs[1].Unsafe == false || !strings.Contains(devs[1].UnsafeReason, "shares IOMMU group 7 with 0000:03:00.0") {
		t.Errorf("%+v", devs)
	}
}

func TestUnknownFactsRefuseNetworkAndStorage(t *testing.T) {
	devs := []hostDevice{mustDev(t, "nodedev_pci_0000_01_00_0.xml"), mustDev(t, "nodedev_pci_0000_02_00_0.xml")}
	markSafety(devs, parseFacts("")) // script failed: Known=false
	if !devs[0].Unsafe || devs[1].Unsafe {
		t.Errorf("without facts only network/storage devices are refused: %+v", devs)
	}
}

func TestPickHostdevRules(t *testing.T) {
	mouse := mustDev(t, "nodedev_handwritten_usb_mouse.xml")
	mouse2 := mouse
	mouse2.ID, mouse2.usbDev = "usb:3:3", 3
	_, x, _, err := pickHostdev([]hostDevice{mouse}, false, "usb:3:2", "vm")
	if err != nil || x != "<hostdev mode='subsystem' type='usb' managed='yes'><source><vendor id='0x046d'/><product id='0xc077'/></source></hostdev>" {
		t.Fatalf("%v\n%s", err, x)
	}
	_, x, _, _ = pickHostdev([]hostDevice{mouse, mouse2}, false, "usb:3:3", "vm")
	if !strings.Contains(x, "<address bus='3' device='3'/>") {
		t.Errorf("identical devices need the address: %s", x)
	}
	if _, _, _, err := pickHostdev([]hostDevice{mouse}, false, "usb:9:9", "vm"); err == nil {
		t.Error("unknown device must fail")
	}
	mouse.UsedBy = []vmRef{{UUID: "vm", Name: "me"}}
	if _, _, _, err := pickHostdev([]hostDevice{mouse}, false, "usb:3:2", "vm"); err == nil || !strings.Contains(err.Error(), "already attached to this VM") {
		t.Errorf("%v", err)
	}
	mouse.UsedBy = []vmRef{{UUID: "other", Name: "them"}}
	if _, _, _, err := pickHostdev([]hostDevice{mouse}, false, "usb:3:2", "vm"); err == nil || !strings.Contains(err.Error(), "them") {
		t.Errorf("%v", err)
	}
	// PCI without any IOMMU on the node.
	nic := mustDev(t, "nodedev_pci_0000_02_00_0.xml")
	if _, _, _, err := pickHostdev([]hostDevice{nic}, false, "0000:02:00.0", "vm"); err == nil || !strings.Contains(err.Error(), "no active IOMMU") {
		t.Errorf("%v", err)
	}
}

const vmWithHostdevs = `<domain type='kvm'><name>v</name><devices>
  <hostdev mode='subsystem' type='pci' managed='yes'><source><address domain='0x0000' bus='0x0a' slot='0x00' function='0x0'/></source><address type='pci' domain='0x0000' bus='0x05' slot='0x00' function='0x0'/></hostdev>
  <hostdev mode='subsystem' type='usb' managed='yes'><source><vendor id='0x046d'/><product id='0xc077'/><address bus='3' device='2'/></source></hostdev>
  <hostdev mode='subsystem' type='usb'><source><vendor id='0x1d6b'/><product id='0x0002'/></source></hostdev>
  <hostdev mode='capabilities' type='misc'><source><char>/dev/x</char></source></hostdev>
</devices></domain>`

func TestVMHostdevsAndMatching(t *testing.T) {
	hs, err := parseVMHostdevs(vmWithHostdevs)
	if err != nil || len(hs) != 3 {
		t.Fatalf("%v %+v", err, hs)
	}
	gpu := mustDev(t, "nodedev_handwritten_gpu_0a_00_0.xml")
	mouse := mustDev(t, "nodedev_handwritten_usb_mouse.xml")
	hub := mustDev(t, "nodedev_usb_usb1.xml")
	if !hs[0].uses(gpu) || hs[0].uses(mouse) || !hs[1].uses(mouse) || !hs[2].uses(hub) || hs[2].uses(mouse) {
		t.Errorf("uses() wrong: %+v", hs)
	}
	other := mouse
	other.usbDev = 9
	if hs[1].uses(other) {
		t.Error("address-pinned usb hostdev must not match another device with the same ids")
	}
	if k := attachedKey(hs[0]); k != "pci:0000:0a:00.0" {
		t.Error(k)
	}
	if k := attachedKey(hs[1]); k != "usb:046d:c077:3:2" {
		t.Error(k)
	}
	if !hostdevKeyRE.MatchString("usb:046d:c077:3:2") || !hostdevKeyRE.MatchString("pci:0000:0a:00.0") || hostdevKeyRE.MatchString("usb:1; rm -rf /") {
		t.Error("key syntax")
	}
	doc, err := domxml.Parse(vmWithHostdevs)
	if err != nil {
		t.Fatal(err)
	}
	find := func(key string) int {
		n := 0
		for _, h := range doc.Devices("hostdev") {
			if hostdevMatch(key)(h) {
				n++
			}
		}
		return n
	}
	if find("pci:0000:0a:00.0") != 1 || find("usb:046d:c077:3:2") != 1 || find("usb:046d:c077") != 1 || find("usb:1d6b:0002") != 1 || find("usb:046d:c077:3:9") != 0 || find("pci:0000:0b:00.0") != 0 {
		t.Error("detach matching wrong")
	}
}

// Seen on real hardware: a graphics card in the CPU's slot shares its
// IOMMU group with the PCIe root port above it. The port stays with the host.
func TestGPUBehindRootPortIsAllowed(t *testing.T) {
	g := 2
	devs := []hostDevice{
		{ID: "0000:00:01.0", Kind: "pci", Class: "PCI bridge", Category: "bridge", Driver: "pcieport", IOMMUGroup: &g, groupAddr: []string{"0000:01:00.0", "0000:01:00.1"}},
		{ID: "0000:01:00.0", Kind: "pci", Class: "VGA controller", Category: "gpu", Driver: "nvidia", IOMMUGroup: &g, groupAddr: []string{"0000:00:01.0", "0000:01:00.1"}},
		{ID: "0000:01:00.1", Kind: "pci", Class: "Audio device", Category: "other", Driver: "snd_hda_intel", IOMMUGroup: &g, groupAddr: []string{"0000:00:01.0", "0000:01:00.0"}},
	}
	markSafety(devs, hostFacts{Known: true, NIC: map[string]string{}, Storage: map[string]string{}})
	if !devs[0].Unsafe {
		t.Error("the root port itself must not be offered")
	}
	if devs[1].Unsafe || devs[2].Unsafe {
		t.Errorf("GPU and its audio function must be allowed: %q / %q", devs[1].UnsafeReason, devs[2].UnsafeReason)
	}
	_, xml, note, err := pickHostdev(devs, true, "0000:01:00.0", "vm")
	if err != nil || xml == "" {
		t.Fatalf("attach refused: %v", err)
	}
	if !strings.Contains(note, "0000:01:00.1") || strings.Contains(note, "0000:00:01.0") {
		t.Errorf("note should name the audio function, not the root port: %q", note)
	}
	// A bridge of another kind (ISA, host) in the group still blocks.
	devs[0].Class, devs[0].Driver = "ISA bridge", ""
	for i := range devs {
		devs[i].Unsafe, devs[i].UnsafeReason, devs[i].GroupPeers = false, "", nil
	}
	markSafety(devs, hostFacts{Known: true, NIC: map[string]string{}, Storage: map[string]string{}})
	if !devs[1].Unsafe {
		t.Error("a device sharing its group with an ISA bridge must stay refused")
	}
}
