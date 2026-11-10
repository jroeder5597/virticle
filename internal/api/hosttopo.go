package api

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"virt-proj/internal/hostconn"
)

// The NUMA and huge page layout of a node, from ConnectGetCapabilities, plus
// the CPU models its hypervisor offers, from ConnectGetDomainCapabilities.

type hostPages struct {
	SizeKiB uint64 `json:"sizeKiB"`
	Total   uint64 `json:"total"`
	Free    uint64 `json:"free"`
}

type hostCell struct {
	ID        int         `json:"id"`
	MemoryKiB uint64      `json:"memoryKiB"`
	CPUs      []int       `json:"cpus"`
	Pages     []hostPages `json:"pages"`
}

type hostTopo struct {
	Cells []hostCell `json:"cells"`
}

func (t hostTopo) cpus() []int {
	set := map[int]bool{}
	for _, c := range t.Cells {
		for _, id := range c.CPUs {
			set[id] = true
		}
	}
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func (t hostTopo) cellIDs() []int {
	out := make([]int, len(t.Cells))
	for i, c := range t.Cells {
		out[i] = c.ID
	}
	return out
}

// hugepageSizes lists the huge page sizes the node offers (4 KiB pages are the
// ordinary ones), smallest first.
func (t hostTopo) hugepageSizes() []uint64 {
	set := map[uint64]bool{}
	for _, c := range t.Cells {
		for _, p := range c.Pages {
			if p.SizeKiB > 4 {
				set[p.SizeKiB] = true
			}
		}
	}
	out := make([]uint64, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (t hostTopo) freePages(size uint64) (free uint64) {
	for _, c := range t.Cells {
		for _, p := range c.Pages {
			if p.SizeKiB == size {
				free += p.Free
			}
		}
	}
	return
}

func parseHostTopology(capsXML string) (hostTopo, error) {
	var x struct {
		Cells []struct {
			ID     int `xml:"id,attr"`
			Memory struct {
				Unit  string `xml:"unit,attr"`
				Value uint64 `xml:",chardata"`
			} `xml:"memory"`
			Pages []struct {
				Unit  string `xml:"unit,attr"`
				Size  uint64 `xml:"size,attr"`
				Count uint64 `xml:",chardata"`
			} `xml:"pages"`
			CPUs struct {
				CPU []struct {
					ID int `xml:"id,attr"`
				} `xml:"cpu"`
			} `xml:"cpus"`
		} `xml:"host>topology>cells>cell"`
	}
	if err := xml.Unmarshal([]byte(capsXML), &x); err != nil {
		return hostTopo{}, fmt.Errorf("cannot read the node's capabilities: %w", err)
	}
	var t hostTopo
	for _, c := range x.Cells {
		hc := hostCell{ID: c.ID, MemoryKiB: kibFromUnit(c.Memory.Value, c.Memory.Unit), CPUs: []int{}}
		for _, p := range c.CPUs.CPU {
			hc.CPUs = append(hc.CPUs, p.ID)
		}
		sort.Ints(hc.CPUs)
		for _, p := range c.Pages {
			size := kibFromUnit(p.Size, p.Unit)
			hc.Pages = append(hc.Pages, hostPages{SizeKiB: size, Total: p.Count, Free: p.Count})
		}
		t.Cells = append(t.Cells, hc)
	}
	if len(t.Cells) == 0 {
		return t, fmt.Errorf("the node reports no NUMA topology")
	}
	return t, nil
}

func kibFromUnit(v uint64, unit string) uint64 {
	switch strings.ToLower(unit) {
	case "", "kib", "k":
		return v
	case "b", "bytes":
		return v / 1024
	case "mib", "m":
		return v << 10
	case "gib", "g":
		return v << 20
	}
	return v
}

// nodeTopology reads the topology of a live node and fills in free huge pages.
func nodeTopology(h *hostconn.Host) (hostTopo, error) {
	caps, err := h.ConnectGetCapabilities()
	if err != nil {
		return hostTopo{}, err
	}
	t, err := parseHostTopology(caps)
	if err != nil {
		return t, err
	}
	for ci := range t.Cells {
		c := &t.Cells[ci]
		var sizes []uint32
		for _, p := range c.Pages {
			sizes = append(sizes, uint32(p.SizeKiB))
		}
		counts, err := h.NodeGetFreePages(sizes, int32(c.ID), 1, 0)
		if err != nil || len(counts) != len(sizes) {
			continue // free counts are advisory; the totals stay
		}
		for i := range c.Pages {
			c.Pages[i].Free = counts[i]
		}
	}
	return t, nil
}

// parseCPUModels lists the named CPU models a hypervisor can use, from
// domain capabilities XML.
func parseCPUModels(domcaps string) []string {
	var x struct {
		Modes []struct {
			Name   string `xml:"name,attr"`
			Models []struct {
				Usable string `xml:"usable,attr"`
				Name   string `xml:",chardata"`
			} `xml:"model"`
		} `xml:"cpu>mode"`
	}
	if xml.Unmarshal([]byte(domcaps), &x) != nil {
		return nil
	}
	var out []string
	for _, m := range x.Modes {
		if m.Name != "custom" {
			continue
		}
		for _, mo := range m.Models {
			if mo.Usable != "no" && strings.TrimSpace(mo.Name) != "" {
				out = append(out, strings.TrimSpace(mo.Name))
			}
		}
	}
	return out
}

// nodeSet parses a NUMA node list ("0", "0-1", "0,2") against the cell IDs.
func parseNodeset(s string, cells []int) ([]int, error) {
	set := map[int]bool{}
	for _, c := range cells {
		set[c] = true
	}
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		lo, hi := part, part
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, hi = a, b
		}
		l, e1 := strconv.Atoi(strings.TrimSpace(lo))
		h, e2 := strconv.Atoi(strings.TrimSpace(hi))
		if e1 != nil || e2 != nil || l < 0 || h < l {
			return nil, fmt.Errorf("%q is not a valid NUMA node list (use for example 0 or 0-1)", s)
		}
		for i := l; i <= h; i++ {
			if !set[i] {
				return nil, fmt.Errorf("this host has no NUMA node %d (it has %v)", i, cells)
			}
			out = append(out, i)
		}
	}
	return out, nil
}
