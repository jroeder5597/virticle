package domxml

import "testing"

// The Advanced card no longer edits the CPU topology or the NUMA policy. Saving its CPU mode (without a topology
// in the request), huge pages or pinning must leave an existing explicit topology and NUMA policy alone.
func TestSavingCardKeepsTopologyAndNuma(t *testing.T) {
	src, err := SetCPU(tuneSample, CPUSpec{Mode: "custom", Model: "Skylake-Client", Topology: &CPUTopology{Sockets: 1, Cores: 2, Threads: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if src, err = SetNumatune(src, "strict", "0"); err != nil {
		t.Fatal(err)
	}
	steps := map[string]func(string) (string, error){
		"mode":      func(x string) (string, error) { return SetCPU(x, CPUSpec{Mode: "host-model"}) },
		"model":     func(x string) (string, error) { return SetCPU(x, CPUSpec{Mode: "custom", Model: "Skylake-Client"}) },
		"hugepages": func(x string) (string, error) { return SetHugepages(x, 2048) },
		"pinning":   func(x string) (string, error) { return ClearPins(x) },
	}
	for name, step := range steps {
		out, err := step(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		tn, err := ReadTuning(out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tn.Topology == nil || tn.Topology.Sockets != 1 || tn.Topology.Cores != 2 || tn.Topology.Threads != 2 {
			t.Errorf("%s dropped the topology: %+v", name, tn.Topology)
		}
		if tn.NumaMode != "strict" || tn.NumaNodeset != "0" {
			t.Errorf("%s dropped the NUMA policy: %q %q", name, tn.NumaMode, tn.NumaNodeset)
		}
	}
}
