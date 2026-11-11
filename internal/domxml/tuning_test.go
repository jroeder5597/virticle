package domxml

import (
	"reflect"
	"strings"
	"testing"
)

const tuneSample = `<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>t1</name>
  <uuid>11111111-2222-3333-4444-555555555555</uuid>
  <!-- keep this comment -->
  <metadata>
    <virticle:meta xmlns:virticle='https://virticle.invalid/ns/1'><virticle:tags>a,b</virticle:tags></virticle:meta>
  </metadata>
  <memory unit='KiB'>2097152</memory>
  <vcpu placement='static'>4</vcpu>
  <cputune>
    <vcpupin vcpu='0' cpuset='0'/>
    <shares>2048</shares>
  </cputune>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <cpu mode='custom' match='exact' check='partial'>
    <model fallback='allow'>Skylake-Client</model>
    <feature policy='require' name='vmx'/>
    <numa>
      <cell id='0' cpus='0-3' memory='2097152' unit='KiB'/>
    </numa>
  </cpu>
  <devices>
    <disk type='file' device='disk'><source file='/x.qcow2'/><target dev='vda'/></disk>
  </devices>
  <qemu:commandline><qemu:arg value='-foo'/></qemu:commandline>
</domain>
`

// survives checks that the parts of the document a tuning edit does not own
// are byte-for-byte present afterwards.
func survives(t *testing.T, out string) {
	t.Helper()
	for _, frag := range []string{
		"<!-- keep this comment -->",
		"<virticle:meta xmlns:virticle='https://virticle.invalid/ns/1'><virticle:tags>a,b</virticle:tags></virticle:meta>",
		"<uuid>11111111-2222-3333-4444-555555555555</uuid>",
		"<disk type='file' device='disk'><source file='/x.qcow2'/><target dev='vda'/></disk>",
		"<qemu:commandline><qemu:arg value='-foo'/></qemu:commandline>",
		"<memory unit='KiB'>2097152</memory>",
		"<shares>2048</shares>",
		"<vcpupin vcpu='0' cpuset='0'/>",
		"<cell id='0' cpus='0-3' memory='2097152' unit='KiB'/>",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("lost %q in:\n%s", frag, out)
		}
	}
	if _, err := Parse(out); err != nil {
		t.Errorf("result is not well-formed: %v", err)
	}
}

func TestSetCPUTopologyKeepsFeaturesAndNuma(t *testing.T) {
	out, err := SetCPU(tuneSample, CPUSpec{Mode: "custom", Model: "Skylake-Client", Topology: &CPUTopology{Sockets: 1, Cores: 2, Threads: 2}})
	if err != nil {
		t.Fatal(err)
	}
	survives(t, out)
	if !strings.Contains(out, "<feature policy='require' name='vmx'/>") {
		t.Errorf("same model: features must stay:\n%s", out)
	}
	tn, _ := ReadTuning(out)
	if tn.Topology == nil || tn.Topology.Total() != 4 || tn.CPUModel != "Skylake-Client" || tn.NumaCells != 1 {
		t.Errorf("read back %+v", tn)
	}
	if strings.Contains(out, "check='partial'") {
		t.Errorf("stale check attribute kept")
	}
}

func TestSetCPUModeChangeDropsModelFeatures(t *testing.T) {
	out, err := SetCPU(tuneSample, CPUSpec{Mode: "host-passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	survives(t, strings.Replace(out, "<vcpupin", "<vcpupin", 1))
	if strings.Contains(out, "Skylake-Client") || strings.Contains(out, "name='vmx'") {
		t.Errorf("model and features belong to the old mode:\n%s", out)
	}
	if !strings.Contains(out, "<cpu mode='host-passthrough'>") {
		t.Errorf("mode not set:\n%s", out)
	}
}

func TestSetCPUOnSelfClosingAndMissing(t *testing.T) {
	src := strings.Replace(tuneSample, tuneSample[strings.Index(tuneSample, "  <cpu "):strings.Index(tuneSample, "  <devices>")], "  <cpu mode='host-model' check='none'/>\n", 1)
	out, err := SetCPU(src, CPUSpec{Mode: "host-model", Topology: &CPUTopology{Sockets: 2, Cores: 2, Threads: 1}})
	if err != nil {
		t.Fatal(err)
	}
	tn, _ := ReadTuning(out)
	if tn.CPUMode != "host-model" || tn.Topology == nil || tn.Topology.Sockets != 2 {
		t.Errorf("%+v\n%s", tn, out)
	}
	none := strings.Replace(src, "  <cpu mode='host-model' check='none'/>\n", "", 1)
	out, err = SetCPU(none, CPUSpec{Mode: "custom", Model: "qemu64"})
	if err != nil {
		t.Fatal(err)
	}
	if tn, _ = ReadTuning(out); tn.CPUModel != "qemu64" {
		t.Errorf("%+v", tn)
	}
	if _, err := SetCPU(none, CPUSpec{Mode: "custom"}); err == nil {
		t.Error("custom without a model must fail")
	}
	if _, err := SetCPU(none, CPUSpec{Mode: "bogus"}); err == nil {
		t.Error("bad mode must fail")
	}
}

func TestSetCPUClearTopology(t *testing.T) {
	withTopo, _ := SetCPU(tuneSample, CPUSpec{Mode: "custom", Model: "Skylake-Client", Topology: &CPUTopology{Sockets: 1, Cores: 4, Threads: 1}})
	out, err := SetCPU(withTopo, CPUSpec{Mode: "custom", Model: "Skylake-Client", ClearTopology: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<topology") {
		t.Errorf("topology not removed:\n%s", out)
	}
	// Neither Topology nor ClearTopology: left alone.
	kept, _ := SetCPU(withTopo, CPUSpec{Mode: "custom", Model: "Skylake-Client"})
	if !strings.Contains(kept, "<topology") {
		t.Errorf("topology dropped without being asked:\n%s", kept)
	}
}

func TestHugepages(t *testing.T) {
	out, err := SetHugepages(tuneSample, 2048)
	if err != nil {
		t.Fatal(err)
	}
	survives(t, out)
	tn, _ := ReadTuning(out)
	if !tn.Hugepages || tn.HugepageKiB != 2048 {
		t.Fatalf("%+v\n%s", tn, out)
	}
	// Change the size inside an existing memoryBacking that has another child.
	mb := strings.Replace(tuneSample, "<memory unit='KiB'>2097152</memory>", "<memory unit='KiB'>2097152</memory>\n  <memoryBacking>\n    <nosharepages/>\n  </memoryBacking>", 1)
	a, _ := SetHugepages(mb, 2048)
	b, err := SetHugepages(a, 1048576)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, "<nosharepages/>") || strings.Contains(b, "size='2048'") || !strings.Contains(b, "size='1048576'") {
		t.Errorf("%s", b)
	}
	// Removing leaves nosharepages, and removes an otherwise empty memoryBacking.
	c, _ := SetHugepages(b, 0)
	if !strings.Contains(c, "<nosharepages/>") || strings.Contains(c, "hugepages") {
		t.Errorf("%s", c)
	}
	d, _ := SetHugepages(out, 0)
	if d != tuneSample {
		t.Errorf("add then remove must restore the original:\n%s", d)
	}
}

func TestNumatune(t *testing.T) {
	out, err := SetNumatune(tuneSample, "strict", "0")
	if err != nil {
		t.Fatal(err)
	}
	survives(t, out)
	tn, _ := ReadTuning(out)
	if tn.NumaMode != "strict" || tn.NumaNodeset != "0" {
		t.Fatalf("%+v", tn)
	}
	out2, _ := SetNumatune(out, "preferred", "1")
	if tn, _ = ReadTuning(out2); tn.NumaMode != "preferred" || tn.NumaNodeset != "1" || strings.Count(out2, "<memory mode") != 1 {
		t.Errorf("%s", out2)
	}
	back, _ := SetNumatune(out, "", "")
	if back != tuneSample {
		t.Errorf("add then remove must restore the original:\n%s", back)
	}
}

func TestClearPins(t *testing.T) {
	out, err := ClearPins(tuneSample)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "vcpupin") || !strings.Contains(out, "<shares>2048</shares>") {
		t.Errorf("only the pins go; shares stay:\n%s", out)
	}
	only := strings.Replace(tuneSample, "    <shares>2048</shares>\n", "    <emulatorpin cpuset='1'/>\n", 1)
	out, _ = ClearPins(only)
	if strings.Contains(out, "cputune") {
		t.Errorf("empty cputune must go:\n%s", out)
	}
}

func TestReadTuning(t *testing.T) {
	tn, err := ReadTuning(tuneSample)
	if err != nil {
		t.Fatal(err)
	}
	want := []Pin{{VCPU: 0, CPUSet: "0"}}
	if tn.VCPUs != 4 || !reflect.DeepEqual(tn.Pins, want) || tn.CPUMode != "custom" || tn.CPUModel != "Skylake-Client" || !tn.Any() {
		t.Errorf("%+v", tn)
	}
	if !reflect.DeepEqual(tn.Pinned(), []int{0}) {
		t.Errorf("pinned %v", tn.Pinned())
	}
}

func TestCPUSet(t *testing.T) {
	got, err := ParseCPUSet("0-3,^2,8")
	if err != nil || !reflect.DeepEqual(got, []int{0, 1, 3, 8}) {
		t.Fatalf("%v %v", got, err)
	}
	if FormatCPUSet(got) != "0-1,3,8" {
		t.Errorf("format %s", FormatCPUSet(got))
	}
	for _, bad := range []string{"", "a", "3-1", "-1", "0,,1", "0-9999", "^0"} {
		if _, err := ParseCPUSet(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSetCPUDropsMigratableOutsidePassthrough(t *testing.T) {
	src := "<domain type='kvm'>\n  <name>t</name>\n  <cpu mode='host-passthrough' check='none' migratable='on'/>\n  <devices/>\n</domain>"
	out, err := SetCPU(src, CPUSpec{Mode: "custom", Model: "Haswell", Check: "partial", Fallback: "forbid", Vendor: "Intel", Features: []CPUFeature{{"require", "avx2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "migratable") || strings.Contains(out, "check='none'") {
		t.Errorf("stale attributes kept:\n%s", out)
	}
	for _, w := range []string{"<cpu mode='custom' match='exact' check='partial'>", "<model fallback='forbid'>Haswell</model>", "<vendor>Intel</vendor>", "<feature policy='require' name='avx2'/>"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	back, err := SetCPU(out, CPUSpec{Mode: "host-passthrough"})
	if err != nil || strings.Contains(back, "<model") || strings.Contains(back, "<feature") {
		t.Errorf("%v\n%s", err, back)
	}
}
