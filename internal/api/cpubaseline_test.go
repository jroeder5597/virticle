package api

import (
	"errors"
	"os"
	"strings"
	"testing"

	"virticle/internal/domxml"
)

func loadDomcaps(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func nodeFrom(t *testing.T, id int64, name, file string) nodeCPU {
	t.Helper()
	dc := loadDomcaps(t, file)
	h, err := parseHostModel(dc)
	if err != nil {
		t.Fatal(err)
	}
	return nodeCPU{ID: id, Name: name, Host: h, Usable: parseUsableModels(dc)}
}

func TestParseHostModelFixtures(t *testing.T) {
	deb := nodeFrom(t, 1, "n1", "domcaps-debian13.xml")
	rok := nodeFrom(t, 3, "n3", "domcaps-rocky10.xml")
	for _, n := range []nodeCPU{deb, rok} {
		if n.Host.Model != "Skylake-Client-IBRS" || n.Host.Vendor != "Intel" {
			t.Errorf("%s: model %q vendor %q", n.Name, n.Host.Model, n.Host.Vendor)
		}
	}
	if deb.Host.has("its-no") || !rok.Host.has("its-no") {
		t.Error("its-no should be on the Rocky host only")
	}
	if !deb.Host.has("hle") || deb.Host.Features[len(deb.Host.Features)-1].Policy != "disable" {
		t.Error("disable policies should be kept")
	}
	if len(deb.Usable) == 0 || len(rok.Usable) == 0 {
		t.Fatalf("usable models: %d and %d", len(deb.Usable), len(rok.Usable))
	}
	if _, ok := deb.Usable["Skylake-Client-noTSX-IBRS"]; !ok {
		t.Error("Skylake-Client-noTSX-IBRS should be usable on the Debian node")
	}
	// usable='no' (hle/rtm blockers): the host-model gets around it by
	// disabling those features, but the custom list does not offer it.
	for _, m := range []string{"Broadwell", "Skylake-Client-IBRS"} {
		if _, ok := deb.Usable[m]; ok {
			t.Errorf("%s is usable='no' on the lab hosts and must not be listed", m)
		}
	}
	if _, err := parseHostModel("<domainCapabilities><cpu><mode name='host-model' supported='no'/></cpu></domainCapabilities>"); err == nil {
		t.Error("an unsupported host-model should be an error")
	}
}

// fakeLibvirt mimics the two calls on top of the fixtures: the baseline is
// the intersection of the inputs, computed by node 0, which rejects features
// it does not know; Compatible accepts a CPU whose features a node knows and
// whose model is its host model or listed as usable; blocked[i] names models
// node i's QEMU does not have.
func fakeProbe(nodes []nodeCPU, calls *int, blocked map[int]string) baselineProbe {
	return baselineProbe{
		Baseline: func(inputs []string) (string, error) {
			*calls++
			var cpus []cpuDef
			for _, in := range inputs {
				d, err := parseCPUDef(in)
				if err != nil {
					return "", err
				}
				for _, f := range d.Features {
					if !nodes[0].Host.has(f.Name) {
						return "", errors.New("internal error: Unknown CPU feature " + f.Name)
					}
				}
				cpus = append(cpus, d)
			}
			out := cpuDef{Model: cpus[0].Model, Vendor: cpus[0].Vendor, Features: commonFeatures(cpus)}
			return compareXML(out), nil
		},
		Compatible: func(i int, d cpuDef) (bool, error) {
			if _, ok := nodes[i].Usable[d.Model]; !ok && d.Model != nodes[i].Host.Model || blocked[i] == d.Model {
				return false, nil
			}
			for _, f := range d.Features {
				if !nodes[i].Host.has(f.Name) && f.Policy == "require" {
					return false, errors.New("Unknown CPU feature " + f.Name)
				}
			}
			return true, nil
		},
	}
}

func TestBaselineAcrossDebianAndRocky(t *testing.T) {
	nodes := []nodeCPU{nodeFrom(t, 1, "n1", "domcaps-debian13.xml"), nodeFrom(t, 3, "n3", "domcaps-rocky10.xml")}
	calls := 0
	res, err := chooseBaseline("x86_64", nodes, fakeProbe(nodes, &calls, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "libvirt" || res.CPU.Model != "Skylake-Client-IBRS" {
		t.Fatalf("got %s %s", res.Source, res.CPU.Model)
	}
	if res.CPU.has("its-no") {
		t.Error("its-no is not on the Debian node and must not be in the baseline")
	}
	if !res.CPU.has("ibrs") || !res.CPU.has("hle") {
		t.Error("shared features and disable policies should stay")
	}
	if calls != 2 {
		t.Errorf("expected one retry after dropping the unknown feature, got %d baseline calls", calls)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "its-no") {
		t.Errorf("notes: %v", res.Notes)
	}
}

func TestBaselineSingleNode(t *testing.T) {
	nodes := []nodeCPU{nodeFrom(t, 1, "n1", "domcaps-debian13.xml")}
	calls := 0
	res, err := chooseBaseline("x86_64", nodes, fakeProbe(nodes, &calls, nil))
	if err != nil || res.CPU.Model != nodes[0].Host.Model || len(res.CPU.Features) != len(nodes[0].Host.Features) {
		t.Fatalf("%v %+v", err, res.CPU)
	}
	if _, err := chooseBaseline("x86_64", nil, baselineProbe{}); err == nil {
		t.Error("no nodes should be an error")
	}
}

func TestBaselineFallsBackToOlderNamedModel(t *testing.T) {
	nodes := []nodeCPU{nodeFrom(t, 1, "n1", "domcaps-debian13.xml"), nodeFrom(t, 3, "n3", "domcaps-rocky10.xml")}
	// The Rocky QEMU does not know the model libvirt's baseline returns.
	calls := 0
	res, err := chooseBaseline("x86_64", nodes, fakeProbe(nodes, &calls, map[int]string{1: "Skylake-Client-IBRS"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "fallback" {
		t.Fatalf("source %s", res.Source)
	}
	if res.CPU.Model == "Skylake-Client-IBRS" || res.CPU.Model == "" {
		t.Fatalf("model %q", res.CPU.Model)
	}
	if _, ok := nodes[0].Usable[res.CPU.Model]; !ok {
		t.Errorf("%s is not usable on node 1", res.CPU.Model)
	}
	if _, ok := nodes[1].Usable[res.CPU.Model]; !ok {
		t.Errorf("%s is not usable on node 3", res.CPU.Model)
	}
	// It must be the newest common one: no common model may rank higher.
	for m := range nodes[0].Usable {
		if _, ok := nodes[1].Usable[m]; ok && modelRank(m) > modelRank(res.CPU.Model) {
			t.Errorf("%s ranks above the chosen %s", m, res.CPU.Model)
		}
	}
	if res.CPU.has("its-no") {
		t.Error("common features only")
	}
}

func TestBaselineNoCommonModel(t *testing.T) {
	nodes := []nodeCPU{nodeFrom(t, 1, "n1", "domcaps-debian13.xml"), nodeFrom(t, 3, "n3", "domcaps-rocky10.xml")}
	nodes[1].Usable = map[string]string{"EPYC": "AMD"}
	calls := 0
	p := fakeProbe(nodes, &calls, nil)
	p.Baseline = func([]string) (string, error) { return "", errors.New("internal error: the CPUs are incompatible") }
	if _, err := chooseBaseline("x86_64", nodes, p); err == nil {
		t.Error("expected an error")
	}
}

func TestUnknownFeature(t *testing.T) {
	err := errors.New("internal error: Unknown CPU feature its-no")
	if unknownFeature(err) != "its-no" || unknownFeature(errors.New("x")) != "" || unknownFeature(nil) != "" {
		t.Error("unknownFeature")
	}
}

func TestModelRank(t *testing.T) {
	if !(modelRank("Skylake-Client-IBRS") > modelRank("Haswell-noTSX") && modelRank("Haswell-noTSX") == modelRank("Haswell") && modelRank("nonsense") == 0) {
		t.Error("ranks")
	}
}

func TestCPURelation(t *testing.T) {
	base := cpuDef{Model: "Skylake-Client-IBRS", Features: []cpuFeat{{"require", "a"}, {"require", "b"}, {"disable", "hle"}}}
	for _, c := range []struct {
		model string
		feats []string
		want  string
	}{
		{"Skylake-Client-IBRS", []string{"b", "a"}, relSame},
		{"Skylake-Client-IBRS", []string{"a", "b", "c"}, relNewer},
		{"Skylake-Client-IBRS", []string{"a"}, relOlder},
		{"Skylake-Client-IBRS", []string{"a", "c"}, relDiffers},
		{"Cascadelake-Server", []string{"a"}, relNewer},
		{"Haswell", []string{"a"}, relOlder},
		{"mystery", nil, relDiffers},
		{"", nil, ""},
	} {
		if got := cpuRelation(c.model, c.feats, base); got != c.want {
			t.Errorf("%s %v: got %q, want %q", c.model, c.feats, got, c.want)
		}
	}
}

const newVMSample = `<domain type='kvm'>
  <name>t</name>
  <memory unit='MiB'>128</memory>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <features><acpi/></features>
  <cpu mode='host-model'/>
  <clock offset='utc'/>
  <devices/>
</domain>`

func TestNewVMCPUXML(t *testing.T) {
	d := cpuDef{Model: "Skylake-Client-IBRS", Vendor: "Intel", Features: []cpuFeat{{"require", "ibrs"}, {"disable", "hle"}}}
	out, err := newVMCPUXML(newVMSample, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<cpu mode='custom' match='exact' check='partial'>",
		"<model fallback='forbid'>Skylake-Client-IBRS</model>",
		"<vendor>Intel</vendor>",
		"<feature policy='require' name='ibrs'/>",
		"<feature policy='disable' name='hle'/>",
		"<virticle:cpu policy='cluster-baseline' model='Skylake-Client-IBRS' features='ibrs'/>",
		"<clock offset='utc'/>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "host-model") {
		t.Error("host-model should be gone")
	}
	if _, err := domxml.Parse(out); err != nil {
		t.Fatal(err)
	}
	tu, err := domxml.ReadTuning(out)
	if err != nil || tu.CPUMode != "custom" || tu.CPUModel != "Skylake-Client-IBRS" {
		t.Errorf("%v %+v", err, tu)
	}
}

func TestCPUMetaRoundTrip(t *testing.T) {
	d := cpuDef{Model: "Skylake-Client-IBRS", Features: []cpuFeat{{"require", "b"}, {"require", "a"}, {"disable", "hle"}}}
	meta, err := metaSetChild("<meta><tags><tag>x</tag></tags></meta>", "cpu", cpuMetaElement("", d))
	if err != nil {
		t.Fatal(err)
	}
	m := parseCPUMeta(meta)
	if m == nil || m.Model != "Skylake-Client-IBRS" || strings.Join(m.features, ",") != "a,b" {
		t.Fatalf("%+v from %s", m, meta)
	}
	if _, tags := parseAppMeta(meta); len(tags) != 1 {
		t.Error("tags must survive")
	}
	gone, _ := metaSetChild(meta, "cpu", "")
	if parseCPUMeta(gone) != nil {
		t.Error("marker should be removed")
	}
	if parseCPUMeta("") != nil || parseCPUMeta("<meta><cpu policy='other'/></meta>") != nil {
		t.Error("only the baseline policy counts")
	}
}

func TestSetCPUWithFeaturesReplacesOld(t *testing.T) {
	src := `<domain type='kvm'>
  <name>t</name>
  <cpu mode='custom' match='exact'>
    <model fallback='allow'>qemu64</model>
    <feature policy='require' name='old'/>
    <topology sockets='1' cores='2' threads='1'/>
    <numa><cell id='0' cpus='0-1' memory='1' unit='KiB'/></numa>
  </cpu>
  <devices/>
</domain>`
	out, err := domxml.SetCPU(src, cpuSpecFromDef(cpuDef{Model: "Haswell", Features: []cpuFeat{{"require", "avx2"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "'old'") || !strings.Contains(out, "name='avx2'") || !strings.Contains(out, "<topology sockets='1' cores='2' threads='1'/>") || !strings.Contains(out, "<numa>") {
		t.Errorf("unexpected result:\n%s", out)
	}
}

func TestBaselineLoweredByOlderNode(t *testing.T) {
	deb := nodeFrom(t, 1, "n1", "domcaps-debian13.xml")
	older := deb
	older.ID, older.Name = 9, "old"
	older.Host = deb.Host.without("rsba").without("gds-no").without("umip")
	calls := 0
	without, err := chooseBaseline("x86_64", []nodeCPU{deb}, fakeProbe([]nodeCPU{deb}, &calls, nil))
	if err != nil {
		t.Fatal(err)
	}
	both := []nodeCPU{deb, older}
	with, err := chooseBaseline("x86_64", both, fakeProbe(both, &calls, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rel := cpuRelation(with.CPU.Model, with.CPU.enabled(), without.CPU); rel != relOlder {
		t.Fatalf("relation %q", rel)
	}
	from, to := describeBaselineChange(without.CPU, with.CPU)
	if from != "Skylake-Client-IBRS" || !strings.HasPrefix(to, "Skylake-Client-IBRS without ") || !strings.Contains(to, "umip") {
		t.Errorf("%q -> %q", from, to)
	}
	if !older.accepts(with.CPU) || older.accepts(without.CPU) {
		t.Error("the older node accepts only the lowered baseline")
	}
}
