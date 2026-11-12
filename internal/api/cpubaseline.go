package api

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The cluster CPU baseline: the best named CPU model plus feature list that
// every online node can provide. A VM that uses it can live-migrate between
// those nodes without exposing a host-specific CPU. This file holds the pure
// parts (parsing, choosing, fallback); cpubaseline_cluster.go talks to nodes.

type cpuFeat struct {
	Policy string `json:"policy"` // require | disable
	Name   string `json:"name"`
}

// cpuDef is a named CPU model with its feature list.
type cpuDef struct {
	Model    string    `json:"model"`
	Vendor   string    `json:"vendor,omitempty"`
	Features []cpuFeat `json:"features"`
}

type cpuXMLDoc struct {
	Model    string `xml:"model"`
	Vendor   string `xml:"vendor"`
	Features []struct {
		Policy string `xml:"policy,attr"`
		Name   string `xml:"name,attr"`
	} `xml:"feature"`
}

func (c cpuXMLDoc) def() cpuDef {
	d := cpuDef{Model: strings.TrimSpace(c.Model), Vendor: strings.TrimSpace(c.Vendor), Features: []cpuFeat{}}
	for _, f := range c.Features {
		p := f.Policy
		if p == "" {
			p = "require"
		}
		d.Features = append(d.Features, cpuFeat{p, f.Name})
	}
	return d
}

// parseHostModel reads the host-model CPU of a domain capabilities document.
func parseHostModel(domcaps string) (cpuDef, error) {
	var x struct {
		Modes []struct {
			Name      string `xml:"name,attr"`
			Supported string `xml:"supported,attr"`
			cpuXMLDoc
		} `xml:"cpu>mode"`
	}
	if err := xml.Unmarshal([]byte(domcaps), &x); err != nil {
		return cpuDef{}, fmt.Errorf("domain capabilities: %w", err)
	}
	for _, m := range x.Modes {
		if m.Name != "host-model" {
			continue
		}
		if m.Supported == "no" || strings.TrimSpace(m.Model) == "" {
			return cpuDef{}, fmt.Errorf("the hypervisor offers no host-model CPU")
		}
		return m.def(), nil
	}
	return cpuDef{}, fmt.Errorf("the hypervisor reports no host-model CPU")
}

// parseUsableModels lists the named models the hypervisor marks usable='yes'
// in custom mode, with their vendor.
func parseUsableModels(domcaps string) map[string]string {
	var x struct {
		Modes []struct {
			Name   string `xml:"name,attr"`
			Models []struct {
				Usable string `xml:"usable,attr"`
				Vendor string `xml:"vendor,attr"`
				Name   string `xml:",chardata"`
			} `xml:"model"`
		} `xml:"cpu>mode"`
	}
	out := map[string]string{}
	if xml.Unmarshal([]byte(domcaps), &x) != nil {
		return out
	}
	for _, m := range x.Modes {
		if m.Name != "custom" {
			continue
		}
		for _, mo := range m.Models {
			if n := strings.TrimSpace(mo.Name); mo.Usable == "yes" && n != "" {
				out[n] = mo.Vendor
			}
		}
	}
	return out
}

// parseCPUDef reads a <cpu> element as libvirt returns it.
func parseCPUDef(cpuXML string) (cpuDef, error) {
	var c cpuXMLDoc
	if err := xml.Unmarshal([]byte(cpuXML), &c); err != nil {
		return cpuDef{}, fmt.Errorf("CPU definition: %w", err)
	}
	d := c.def()
	if d.Model == "" {
		return cpuDef{}, fmt.Errorf("the CPU definition has no model")
	}
	return d, nil
}

// baselineInputXML is the form ConnectBaselineHypervisorCPU takes: a bare
// <cpu> with the architecture, model and features (no maxphysaddr).
func baselineInputXML(arch string, d cpuDef) string {
	var b strings.Builder
	b.WriteString("<cpu>\n<arch>" + esc(arch) + "</arch>\n<model>" + esc(d.Model) + "</model>\n")
	if d.Vendor != "" {
		b.WriteString("<vendor>" + esc(d.Vendor) + "</vendor>\n")
	}
	for _, f := range d.Features {
		b.WriteString("<feature policy='" + esc(f.Policy) + "' name='" + esc(f.Name) + "'/>\n")
	}
	b.WriteString("</cpu>")
	return b.String()
}

// compareXML is the CPU as a domain would carry it, for
// ConnectCompareHypervisorCPU.
func compareXML(d cpuDef) string {
	var b strings.Builder
	b.WriteString("<cpu mode='custom' match='exact' check='partial'>\n<model fallback='forbid'>" + esc(d.Model) + "</model>\n")
	if d.Vendor != "" {
		b.WriteString("<vendor>" + esc(d.Vendor) + "</vendor>\n")
	}
	for _, f := range d.Features {
		b.WriteString("<feature policy='" + esc(f.Policy) + "' name='" + esc(f.Name) + "'/>\n")
	}
	b.WriteString("</cpu>")
	return b.String()
}

var unknownFeatureRE = regexp.MustCompile(`Unknown CPU feature ([A-Za-z0-9._-]+)`)

// unknownFeature extracts the feature name from libvirt's complaint that the
// node asked to compute the baseline does not know a feature another node
// reported (a newer QEMU, as on RHEL next to Debian).
func unknownFeature(err error) string {
	if err == nil {
		return ""
	}
	if m := unknownFeatureRE.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

// without returns d minus the named feature.
func (d cpuDef) without(name string) cpuDef {
	out := cpuDef{Model: d.Model, Vendor: d.Vendor, Features: []cpuFeat{}}
	for _, f := range d.Features {
		if f.Name != name {
			out.Features = append(out.Features, f)
		}
	}
	return out
}

// requires reports whether d lists the feature with policy "require".
func (d cpuDef) requires(name string) bool {
	for _, f := range d.Features {
		if f.Name == name && f.Policy == "require" {
			return true
		}
	}
	return false
}

func (d cpuDef) has(name string) bool {
	for _, f := range d.Features {
		if f.Name == name {
			return true
		}
	}
	return false
}

// enabled is the sorted list of features the CPU requires.
func (d cpuDef) enabled() []string {
	out := []string{}
	for _, f := range d.Features {
		if f.Policy == "require" || f.Policy == "force" {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// commonFeatures keeps the features with the same policy on every CPU.
func commonFeatures(cpus []cpuDef) []cpuFeat {
	out := []cpuFeat{}
	if len(cpus) == 0 {
		return out
	}
	for _, f := range cpus[0].Features {
		all := true
		for _, c := range cpus[1:] {
			found := false
			for _, g := range c.Features {
				found = found || g == f
			}
			all = all && found
		}
		if all {
			out = append(out, f)
		}
	}
	return out
}

// modelRank orders named models by generation, so that the fallback can pick
// the newest one every node offers. Variants (-IBRS, -noTSX, -v2) rank with
// their base. Unknown names rank 0 and are only used when nothing else is
// common.
var modelRanks = map[string]int{
	"qemu64": 1, "kvm64": 1, "Conroe": 3, "Penryn": 5, "Nehalem": 10, "Westmere": 11,
	"Opteron_G1": 2, "Opteron_G2": 4, "Opteron_G3": 8, "Opteron_G4": 25, "Opteron_G5": 26,
	"SandyBridge": 20, "IvyBridge": 30, "Haswell": 40, "Broadwell": 50,
	"Skylake-Client": 60, "Skylake-Server": 61, "Cascadelake-Server": 70, "Cooperlake": 71,
	"Icelake-Client": 79, "Icelake-Server": 80, "SapphireRapids": 90, "GraniteRapids": 100,
	"Snowridge": 62, "Denverton": 45, "KnightsMill": 45,
	"x86-64-v2": 15, "x86-64-v3": 45, "x86-64-v4": 75,
	"EPYC": 55, "Dhyana": 55, "EPYC-Rome": 65, "EPYC-Milan": 75, "EPYC-Genoa": 95,
}

var modelSuffixRE = regexp.MustCompile(`(-IBRS|-noTSX|-v\d+)+$`)

func modelRank(name string) int {
	if r, ok := modelRanks[name]; ok {
		return r
	}
	return modelRanks[modelSuffixRE.ReplaceAllString(name, "")]
}

// Relation says how a VM's stored CPU compares with the current baseline.
const (
	relSame    = "same"
	relNewer   = "newer"   // the VM's CPU is newer than the baseline
	relOlder   = "older"   // the baseline moved up
	relDiffers = "differs" // not comparable
)

// cpuRelation compares a VM's recorded CPU (model plus enabled features)
// with the baseline.
func cpuRelation(vmModel string, vmFeatures []string, base cpuDef) string {
	if vmModel == "" {
		return ""
	}
	bf := base.enabled()
	if vmModel == base.Model {
		vs, bs := toSet(vmFeatures), toSet(bf)
		sup, sub := true, true
		for f := range bs {
			sup = sup && vs[f]
		}
		for f := range vs {
			sub = sub && bs[f]
		}
		switch {
		case sup && sub:
			return relSame
		case sup:
			return relNewer
		case sub:
			return relOlder
		}
		return relDiffers
	}
	a, b := modelRank(vmModel), modelRank(base.Model)
	switch {
	case a == 0 || b == 0 || a == b:
		return relDiffers
	case a > b:
		return relNewer
	}
	return relOlder
}

func toSet(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, s := range l {
		m[s] = true
	}
	return m
}

// nodeCPU is what one node tells about its CPU.
type nodeCPU struct {
	ID     int64
	Name   string
	Host   cpuDef
	Usable map[string]string // model -> vendor, from the custom mode list
}

// baselineProbe is what choosing needs from libvirt, so tests can fake it.
type baselineProbe struct {
	// Baseline computes the migratable baseline of the CPU definitions
	// (baselineInputXML form) and returns a <cpu> element.
	Baseline func(inputs []string) (string, error)
	// Compatible says whether node idx (into the nodes slice) can run the CPU.
	Compatible func(idx int, d cpuDef) (bool, error)
}

type baselineResult struct {
	CPU    cpuDef
	Source string   // libvirt | fallback
	Notes  []string // what happened on the way, for the log
}

// chooseBaseline computes the baseline for the nodes: libvirt's migratable
// baseline of all host-model CPUs, checked on every node; when some node
// cannot run it (the model is missing from its QEMU) the newest named model
// every node lists as usable, with the features all of them share.
func chooseBaseline(arch string, nodes []nodeCPU, p baselineProbe) (baselineResult, error) {
	res, err := computeBaseline(arch, nodes, p)
	if err != nil {
		return res, err
	}
	for _, f := range pmuFeatures {
		if res.CPU.requires(f) {
			res.CPU = res.CPU.without(f)
			res.Notes = append(res.Notes, "left out "+f+": QEMU takes it away from a running VM unless the virtual PMU is on")
		}
	}
	return res, nil
}

// pmuFeatures are CPU features that depend on the virtual PMU. The PMU is off
// for a VM with a named CPU model, and QEMU on RHEL-family nodes then drops
// these features at start, so the running VM would differ from the baseline
// it was given.
var pmuFeatures = []string{"pdcm"}

func computeBaseline(arch string, nodes []nodeCPU, p baselineProbe) (baselineResult, error) {
	if len(nodes) == 0 {
		return baselineResult{}, fmt.Errorf("no online node")
	}
	var res baselineResult
	hosts := make([]cpuDef, len(nodes))
	for i, n := range nodes {
		hosts[i] = n.Host
	}
	// Step 1: libvirt's baseline. A feature the executing node does not know
	// belongs to another node's newer QEMU and cannot be in the baseline of
	// all nodes, so it is dropped from the inputs and the call repeated.
	inputs := append([]cpuDef(nil), hosts...)
	var baseErr error
	for try := 0; try < 64; try++ {
		xmls := make([]string, len(inputs))
		for i, c := range inputs {
			xmls[i] = baselineInputXML(arch, c)
		}
		var out string
		out, baseErr = p.Baseline(xmls)
		if baseErr == nil {
			d, err := parseCPUDef(out)
			if err != nil {
				baseErr = err
				break
			}
			ok, why := allCompatible(nodes, d, p)
			if ok {
				res.CPU, res.Source = d, "libvirt"
				return res, nil
			}
			baseErr = fmt.Errorf("the baseline %s is not usable on %s", d.Model, why)
			break
		}
		f := unknownFeature(baseErr)
		if f == "" {
			break
		}
		res.Notes = append(res.Notes, "dropped feature "+f+" that the baseline node does not know")
		for i := range inputs {
			inputs[i] = inputs[i].without(f)
		}
	}
	res.Notes = append(res.Notes, "libvirt baseline failed: "+baseErr.Error())

	// Step 2: newest named model usable on every node.
	var common []string
	for m := range nodes[0].Usable {
		all := true
		for _, n := range nodes[1:] {
			_, ok := n.Usable[m]
			all = all && ok
		}
		if all {
			common = append(common, m)
		}
	}
	vendor := hosts[0].Vendor
	for _, h := range hosts {
		if h.Vendor != vendor {
			vendor = ""
		}
	}
	sort.Slice(common, func(i, j int) bool {
		if ri, rj := modelRank(common[i]), modelRank(common[j]); ri != rj {
			return ri > rj
		}
		return common[i] < common[j]
	})
	feats := commonFeatures(hosts)
	for _, m := range common {
		if v := nodes[0].Usable[m]; vendor != "" && v != "" && v != vendor {
			continue // an AMD model on Intel hosts is never usable, skip the probe
		}
		for _, withFeats := range []bool{true, false} {
			d := cpuDef{Model: m, Vendor: vendor, Features: []cpuFeat{}}
			if withFeats {
				d.Features = feats
			}
			if ok, _ := allCompatible(nodes, d, p); ok {
				res.CPU, res.Source = d, "fallback"
				res.Notes = append(res.Notes, "using the newest model every node lists as usable: "+m)
				return res, nil
			}
		}
	}
	return res, fmt.Errorf("no CPU model is usable on every node (%s)", baseErr)
}

// allCompatible checks d on every node; on failure why names the first node.
func allCompatible(nodes []nodeCPU, d cpuDef, p baselineProbe) (bool, string) {
	for i, n := range nodes {
		ok, err := p.Compatible(i, d)
		if err != nil {
			return false, n.Name + " (" + err.Error() + ")"
		}
		if !ok {
			return false, n.Name
		}
	}
	return true, ""
}
