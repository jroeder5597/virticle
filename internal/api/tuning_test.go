package api

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseHostTopologyFromRealCapabilities(t *testing.T) {
	topo, err := parseHostTopology(fixture(t, "capabilities_n1.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Cells) != 1 || topo.Cells[0].ID != 0 || !reflect.DeepEqual(topo.cpus(), []int{0, 1}) {
		t.Fatalf("%+v", topo)
	}
	if topo.Cells[0].MemoryKiB < 1<<20 {
		t.Errorf("memory %d", topo.Cells[0].MemoryKiB)
	}
	if !reflect.DeepEqual(topo.hugepageSizes(), []uint64{2048, 1048576}) {
		t.Errorf("sizes %v", topo.hugepageSizes())
	}
}

const twoCellCaps = `<capabilities><host><topology><cells num='2'>
 <cell id='0'><memory unit='KiB'>4194304</memory><pages unit='KiB' size='4'>1048576</pages><pages unit='KiB' size='2048'>512</pages>
  <cpus num='2'><cpu id='0'/><cpu id='1'/></cpus></cell>
 <cell id='1'><memory unit='KiB'>4194304</memory><pages unit='KiB' size='4'>1048576</pages><pages unit='KiB' size='2048'>512</pages>
  <cpus num='2'><cpu id='2'/><cpu id='3'/></cpus></cell>
</cells></topology></host></capabilities>`

func TestTwoCellTopologyAndNodeset(t *testing.T) {
	topo, err := parseHostTopology(twoCellCaps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(topo.cpus(), []int{0, 1, 2, 3}) || !reflect.DeepEqual(topo.cellIDs(), []int{0, 1}) {
		t.Fatalf("%+v", topo)
	}
	if topo.freePages(2048) != 1024 {
		t.Errorf("free %d", topo.freePages(2048))
	}
	if n, err := parseNodeset("0-1", topo.cellIDs()); err != nil || !reflect.DeepEqual(n, []int{0, 1}) {
		t.Errorf("%v %v", n, err)
	}
	for _, bad := range []string{"2", "0-2", "x", "1-0", ""} {
		if _, err := parseNodeset(bad, topo.cellIDs()); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHugepageShortage(t *testing.T) {
	topo, _ := parseHostTopology(fixture(t, "capabilities_n1.xml"))
	// The lab node has no huge pages reserved: a 96 MiB VM needs 48 pages.
	w := hugepageShortage(2048, 96*1024, topo)
	if len(w) != 1 || !strings.Contains(w[0], "needs 48 pages") || !strings.Contains(w[0], "only 0 free") {
		t.Errorf("%v", w)
	}
	topo.Cells[0].Pages[1].Free = 64
	if w := hugepageShortage(2048, 96*1024, topo); len(w) != 0 {
		t.Errorf("enough pages, got %v", w)
	}
	if hugepageShortage(0, 96*1024, topo) != nil {
		t.Error("default size is not checked")
	}
}

func TestParseCPUModelsFromRealDomcaps(t *testing.T) {
	m := parseCPUModels(fixture(t, "domcaps_n1.xml"))
	if len(m) < 10 || !contains(m, "qemu64") || !contains(m, "Skylake-Client-noTSX-IBRS") {
		t.Errorf("%d models: %v", len(m), m)
	}
}
