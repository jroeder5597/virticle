package domxml

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Targeted edits for CPU model and topology, huge pages and NUMA memory
// policy, plus a reader for what a domain currently says about them. Like the
// rest of the package each edit replaces one element of the source text.

// ParseCPUSet reads libvirt's cpuset syntax ("0-3,^2,8") into a sorted list.
func ParseCPUSet(s string) ([]int, error) {
	set := map[int]bool{}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("the CPU list is empty")
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		neg := strings.HasPrefix(part, "^")
		part = strings.TrimPrefix(part, "^")
		lo, hi := part, part
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, hi = a, b
		}
		l, err1 := strconv.Atoi(strings.TrimSpace(lo))
		h, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || l < 0 || h < l || h > 4095 {
			return nil, fmt.Errorf("%q is not a valid CPU list (use for example 0-3,6)", s)
		}
		for i := l; i <= h; i++ {
			if neg {
				delete(set, i)
			} else {
				set[i] = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	sort.Ints(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("%q selects no CPU", s)
	}
	return out, nil
}

// FormatCPUSet renders a sorted CPU list as "0-2,5".
func FormatCPUSet(cpus []int) string {
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(cpus[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// Pin is one <vcpupin>.
type Pin struct {
	VCPU   int    `json:"vcpu"`
	CPUSet string `json:"cpuset"`
}

type CPUTopology struct {
	Sockets  int `json:"sockets"`
	Dies     int `json:"dies,omitempty"`
	Clusters int `json:"clusters,omitempty"`
	Cores    int `json:"cores"`
	Threads  int `json:"threads"`
}

// Total is the number of vCPUs the topology describes.
func (t CPUTopology) Total() int {
	n := func(v int) int {
		if v < 1 {
			return 1
		}
		return v
	}
	return n(t.Sockets) * n(t.Dies) * n(t.Clusters) * n(t.Cores) * n(t.Threads)
}

// Tuning is what a domain definition says about placement and CPU shape.
type Tuning struct {
	VCPUs        int          `json:"vcpus"`
	VCPUCPUSet   string       `json:"vcpuCpuset,omitempty"`
	Pins         []Pin        `json:"pins"`
	EmulatorPin  string       `json:"emulatorPin,omitempty"`
	CPUMode      string       `json:"cpuMode"`
	CPUModel     string       `json:"cpuModel,omitempty"`
	Topology     *CPUTopology `json:"topology,omitempty"`
	HasCPU       bool         `json:"-"`
	Hugepages    bool         `json:"hugepages"`
	HugepageKiB  uint64       `json:"hugepageKiB,omitempty"` // 0 with Hugepages set: the host's default size
	NumaMode     string       `json:"numaMode,omitempty"`
	NumaNodeset  string       `json:"numaNodeset,omitempty"`
	NumaCells    int          `json:"numaCells,omitempty"` // <cpu><numa> cells of the guest
	MemnodeCount int          `json:"-"`
}

// Pinned lists every host CPU the definition pins to.
func (t Tuning) Pinned() []int {
	set := map[int]bool{}
	add := func(s string) {
		if cpus, err := ParseCPUSet(s); err == nil {
			for _, c := range cpus {
				set[c] = true
			}
		}
	}
	for _, p := range t.Pins {
		add(p.CPUSet)
	}
	add2 := func(s string) {
		if s != "" {
			add(s)
		}
	}
	add2(t.EmulatorPin)
	add2(t.VCPUCPUSet)
	out := make([]int, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

// Any reports whether the definition ties the VM to host topology.
func (t Tuning) Any() bool {
	return len(t.Pins) > 0 || t.EmulatorPin != "" || t.VCPUCPUSet != "" || t.Hugepages || t.NumaMode != ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ReadTuning extracts the tuning facts from a domain definition.
func ReadTuning(src string) (Tuning, error) {
	d, err := Parse(src)
	if err != nil {
		return Tuning{}, err
	}
	t := Tuning{Pins: []Pin{}, CPUMode: "custom"}
	if v := d.Root.Child("vcpu"); v != nil {
		t.VCPUs = atoi(strings.TrimSpace(d.Text(v)))
		t.VCPUCPUSet = v.Attrs["cpuset"]
	}
	if ct := d.Root.Child("cputune"); ct != nil {
		for _, p := range ct.ChildrenNamed("vcpupin") {
			t.Pins = append(t.Pins, Pin{VCPU: atoi(p.Attrs["vcpu"]), CPUSet: p.Attrs["cpuset"]})
		}
		if e := ct.Child("emulatorpin"); e != nil {
			t.EmulatorPin = e.Attrs["cpuset"]
		}
	}
	if c := d.Root.Child("cpu"); c != nil {
		t.HasCPU = true
		if m := c.Attrs["mode"]; m != "" {
			t.CPUMode = m
		}
		if m := c.Child("model"); m != nil {
			t.CPUModel = strings.TrimSpace(d.Text(m))
		}
		if tp := c.Child("topology"); tp != nil {
			t.Topology = &CPUTopology{Sockets: atoi(tp.Attrs["sockets"]), Dies: atoi(tp.Attrs["dies"]), Clusters: atoi(tp.Attrs["clusters"]),
				Cores: atoi(tp.Attrs["cores"]), Threads: atoi(tp.Attrs["threads"])}
		}
		if n := c.Child("numa"); n != nil {
			t.NumaCells = len(n.ChildrenNamed("cell"))
		}
	}
	if mb := d.Root.Child("memoryBacking"); mb != nil {
		if hp := mb.Child("hugepages"); hp != nil {
			t.Hugepages = true
			if pg := hp.Child("page"); pg != nil {
				t.HugepageKiB = pageKiB(pg.Attrs["size"], pg.Attrs["unit"])
			}
		}
	}
	if nt := d.Root.Child("numatune"); nt != nil {
		if m := nt.Child("memory"); m != nil {
			t.NumaMode, t.NumaNodeset = m.Attrs["mode"], m.Attrs["nodeset"]
		}
		t.MemnodeCount = len(nt.ChildrenNamed("memnode"))
	}
	return t, nil
}

func pageKiB(size, unit string) uint64 {
	n, _ := strconv.ParseUint(size, 10, 64)
	switch strings.ToLower(unit) {
	case "", "kib", "k":
		return n
	case "mib", "m":
		return n << 10
	case "gib", "g":
		return n << 20
	}
	return n
}

func lineIndent(src string, at int) string {
	ls := strings.LastIndexByte(src[:at], '\n')
	pre := src[ls+1 : at]
	if strings.TrimLeft(pre, " \t") == "" {
		return pre
	}
	return ""
}

// CPUSpec is the wanted CPU model and topology.
type CPUSpec struct {
	Mode          string // host-passthrough | host-model | custom
	Model         string // custom only
	Topology      *CPUTopology
	ClearTopology bool
}

var modelRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// SetCPU rewrites the <cpu> element. Children it does not own (<numa>,
// <cache>, <maxphysaddr>, ...) are kept as they are. Feature lists and the
// vendor belong to a model, so they are dropped when the mode or model
// changes and kept otherwise.
func SetCPU(src string, spec CPUSpec) (string, error) {
	switch spec.Mode {
	case "host-passthrough", "host-model", "custom":
	default:
		return "", fmt.Errorf("CPU mode must be host-passthrough, host-model or custom")
	}
	if spec.Mode == "custom" && !modelRE.MatchString(spec.Model) {
		return "", fmt.Errorf("a custom CPU needs a model name")
	}
	if tp := spec.Topology; tp != nil && (tp.Sockets < 1 || tp.Cores < 1 || tp.Threads < 1) {
		return "", fmt.Errorf("sockets, cores and threads must each be at least 1")
	}
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	old := d.Root.Child("cpu")
	var (
		keepFeatures bool
		keep         []string // raw text of kept children
		attrs        []string
		topo         = spec.Topology
	)
	if old != nil {
		oldMode := old.Attrs["mode"]
		if oldMode == "" {
			oldMode = "custom"
		}
		oldModel := ""
		if m := old.Child("model"); m != nil {
			oldModel = strings.TrimSpace(d.Text(m))
		}
		keepFeatures = oldMode == spec.Mode && (spec.Mode != "custom" || oldModel == spec.Model)
		for _, c := range old.Children {
			switch c.Name {
			case "model":
			case "vendor", "feature":
				if keepFeatures {
					keep = append(keep, d.Raw(c))
				}
			case "topology":
				if topo == nil && !spec.ClearTopology {
					keep = append(keep, d.Raw(c))
				}
			default:
				keep = append(keep, d.Raw(c))
			}
		}
		names := make([]string, 0, len(old.Attrs))
		for k := range old.Attrs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if k == "mode" || k == "match" || k == "check" {
				continue
			}
			attrs = append(attrs, " "+k+"='"+Esc(old.Attrs[k])+"'")
		}
	}
	var b strings.Builder
	b.WriteString("<cpu mode='" + spec.Mode + "'")
	if spec.Mode == "custom" {
		b.WriteString(" match='exact'")
	}
	b.WriteString(strings.Join(attrs, "") + ">")
	indent := "    "
	closeIndent := "  "
	if old != nil {
		closeIndent = lineIndent(src, old.Start)
		indent = closeIndent + "  "
	} else if r := d.Root.Child("devices"); r != nil {
		closeIndent = lineIndent(src, r.Start)
		indent = closeIndent + "  "
	}
	if spec.Mode == "custom" {
		b.WriteString("\n" + indent + "<model fallback='allow'>" + spec.Model + "</model>")
	}
	if topo != nil {
		b.WriteString("\n" + indent + "<topology")
		if topo.Sockets > 0 {
			fmt.Fprintf(&b, " sockets='%d'", topo.Sockets)
		}
		if topo.Dies > 1 {
			fmt.Fprintf(&b, " dies='%d'", topo.Dies)
		}
		if topo.Clusters > 1 {
			fmt.Fprintf(&b, " clusters='%d'", topo.Clusters)
		}
		fmt.Fprintf(&b, " cores='%d' threads='%d'/>", topo.Cores, topo.Threads)
	}
	for _, k := range keep {
		b.WriteString("\n" + indent + strings.TrimSpace(k))
	}
	b.WriteString("\n" + closeIndent + "</cpu>")
	if old != nil {
		return Apply(src, []Splice{d.Replace(old, b.String())})
	}
	return Apply(src, []Splice{d.AppendChild(d.Root, b.String())})
}

// SetHugepages backs the guest memory with huge pages of the given size, or
// with ordinary pages when pageKiB is 0. Other <memoryBacking> children
// (nosharepages, locked, access, ...) stay.
func SetHugepages(src string, pageKiB uint64) (string, error) {
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	mb := d.Root.Child("memoryBacking")
	if pageKiB == 0 {
		if mb == nil || mb.Child("hugepages") == nil {
			return src, nil
		}
		if len(mb.Children) == 1 {
			return Apply(src, []Splice{d.Remove(mb)})
		}
		return Apply(src, []Splice{d.Remove(mb.Child("hugepages"))})
	}
	hp := fmt.Sprintf("<hugepages><page size='%d' unit='KiB'/></hugepages>", pageKiB)
	if mb == nil {
		return Apply(src, []Splice{d.AppendChild(d.Root, "<memoryBacking>"+hp+"</memoryBacking>")})
	}
	if old := mb.Child("hugepages"); old != nil {
		return Apply(src, []Splice{d.Replace(old, hp)})
	}
	return Apply(src, []Splice{d.AppendChild(mb, hp)})
}

// SetNumatune sets the memory policy of the guest on the host's NUMA nodes;
// an empty mode removes it. <memnode> children stay.
func SetNumatune(src, mode, nodeset string) (string, error) {
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	nt := d.Root.Child("numatune")
	if mode == "" {
		if nt == nil || nt.Child("memory") == nil {
			return src, nil
		}
		if len(nt.Children) == 1 {
			return Apply(src, []Splice{d.Remove(nt)})
		}
		return Apply(src, []Splice{d.Remove(nt.Child("memory"))})
	}
	mem := fmt.Sprintf("<memory mode='%s' nodeset='%s'/>", Esc(mode), Esc(nodeset))
	if nt == nil {
		return Apply(src, []Splice{d.AppendChild(d.Root, "<numatune>"+mem+"</numatune>")})
	}
	if old := nt.Child("memory"); old != nil {
		return Apply(src, []Splice{d.Replace(old, mem)})
	}
	return Apply(src, []Splice{d.AppendChild(nt, mem)})
}

// ClearPins removes every <vcpupin> and <emulatorpin> (and <cputune> when
// nothing else is left in it).
func ClearPins(src string) (string, error) {
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	ct := d.Root.Child("cputune")
	if ct == nil {
		return src, nil
	}
	var rm []*Node
	for _, c := range ct.Children {
		if c.Name == "vcpupin" || c.Name == "emulatorpin" {
			rm = append(rm, c)
		}
	}
	if len(rm) == 0 {
		return src, nil
	}
	if len(rm) == len(ct.Children) {
		return Apply(src, []Splice{d.Remove(ct)})
	}
	var edits []Splice
	for _, c := range rm {
		edits = append(edits, d.Remove(c))
	}
	return Apply(src, edits)
}
