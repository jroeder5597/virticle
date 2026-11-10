package api

import (
	"encoding/xml"
	"reflect"
	"testing"
)

func TestDescendants(t *testing.T) {
	list := []snapInfo{
		{Name: "a"}, {Name: "b", Parent: "a"}, {Name: "c", Parent: "b"},
		{Name: "d", Parent: "a"}, {Name: "x"}, {Name: "y", Parent: "x"},
	}
	got := descendants(list, "a")
	want := []string{"d", "c", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := descendants(list, "c"); len(got) != 0 {
		t.Errorf("leaf has descendants %v", got)
	}
}

func TestSnapInfoOf(t *testing.T) {
	const raw = `<domainsnapshot>
  <name>s2</name><description>d</description><state>running</state>
  <parent><name>s1</name></parent><creationTime>42</creationTime>
  <memory snapshot='external' file='/p/vm.s2.mem'/>
  <disks>
    <disk name='vda' snapshot='external' type='file'><source file='/p/vm.s2.qcow2'/></disk>
    <disk name='sda' snapshot='no'/>
  </disks>
  <domain type='kvm'><devices><disk type='file' device='disk'><source file='/ignored'/></disk></devices></domain>
</domainsnapshot>`
	var x snapXML
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		t.Fatal(err)
	}
	si := snapInfoOf(x)
	want := snapInfo{
		Name: "s2", Description: "d", Created: 42, State: "running", Parent: "s1", HasMemory: true,
		Kind: "external", Files: []string{"/p/vm.s2.mem", "/p/vm.s2.qcow2"},
	}
	if !reflect.DeepEqual(si, want) {
		t.Errorf("got %+v want %+v", si, want)
	}
	var in snapXML
	xml.Unmarshal([]byte(`<domainsnapshot><name>i</name><state>shutoff</state><memory snapshot='no'/><disks><disk name='vda' snapshot='internal'/></disks></domainsnapshot>`), &in)
	if k := snapInfoOf(in); k.Kind != "internal" || k.HasMemory || len(k.Files) != 0 {
		t.Errorf("internal: %+v", k)
	}
}

func TestSecModelIsAppArmor(t *testing.T) {
	deb := `<capabilities><host><secmodel><model>apparmor</model><doi>0</doi></secmodel><secmodel><model>dac</model></secmodel></host></capabilities>`
	rhel := `<capabilities><host><secmodel><model>selinux</model></secmodel><secmodel><model>dac</model></secmodel></host></capabilities>`
	if !secModelIsAppArmor(deb) || secModelIsAppArmor(rhel) || secModelIsAppArmor("") {
		t.Fatal("wrong security model detection")
	}
}
