package api

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/domxml"
	"virticle/internal/hostconn"
	"virticle/internal/snapsched"
)

// A VM whose CPU came from the cluster baseline carries one child element in
// the application metadata, <cpu policy='cluster-baseline' model= features=>,
// with the model and enabled features it was given. The domain XML holds the
// resolved custom model; this element only says where it came from, so the UI
// can show it and tell when the baseline has since moved.

const cpuPolicyBaseline = "cluster-baseline"

// vmCPU is what the VM list says about a VM's CPU policy.
type vmCPU struct {
	Policy   string `json:"policy"`
	Model    string `json:"model"`
	Relation string `json:"relation,omitempty"` // same | newer | older | differs, against the current baseline
	// features is kept for the comparison only.
	features []string
}

func cpuMetaElement(prefix string, d cpuDef) string {
	p := ""
	if prefix != "" {
		p = prefix + ":"
	}
	return fmt.Sprintf("<%scpu policy='%s' model='%s' features='%s'/>", p, cpuPolicyBaseline, esc(d.Model), esc(strings.Join(d.enabled(), " ")))
}

// parseCPUMeta reads the CPU element of the metadata root; nil when the VM
// has none.
func parseCPUMeta(raw string) *vmCPU {
	if raw == "" {
		return nil
	}
	var root struct {
		CPU *struct {
			Policy   string `xml:"policy,attr"`
			Model    string `xml:"model,attr"`
			Features string `xml:"features,attr"`
		} `xml:"cpu"`
	}
	if xml.Unmarshal([]byte(raw), &root) != nil || root.CPU == nil || root.CPU.Policy != cpuPolicyBaseline {
		return nil
	}
	return &vmCPU{Policy: root.CPU.Policy, Model: root.CPU.Model, features: strings.Fields(root.CPU.Features)}
}

// cpuSpecFromDef is the SetCPU request for a baseline: a custom model with
// exact matching, partial checking (libvirt verifies the host can provide the
// CPU before the VM starts) and fallback forbid, so a node that lacks the
// model refuses to start the VM instead of silently running another one.
func cpuSpecFromDef(d cpuDef) domxml.CPUSpec {
	spec := domxml.CPUSpec{Mode: "custom", Model: d.Model, Vendor: d.Vendor, Fallback: "forbid", Check: "partial", Features: []domxml.CPUFeature{}}
	for _, f := range d.Features {
		spec.Features = append(spec.Features, domxml.CPUFeature{Policy: f.Policy, Name: f.Name})
	}
	return spec
}

// newVMCPUXML edits the XML generated for a new VM: the baseline as its CPU
// and the marker in its metadata.
func newVMCPUXML(src string, d cpuDef) (string, error) {
	out, err := domxml.SetCPU(src, cpuSpecFromDef(d))
	if err != nil {
		return "", err
	}
	doc, err := domxml.Parse(out)
	if err != nil {
		return "", err
	}
	meta := fmt.Sprintf("<metadata><%s:meta xmlns:%s='%s'>%s</%s:meta></metadata>", snapsched.Prefix, snapsched.Prefix, snapsched.NS, cpuMetaElement(snapsched.Prefix, d), snapsched.Prefix)
	return domxml.Apply(out, []domxml.Splice{doc.AppendChild(doc.Root, meta)})
}

// setCPUMeta records (d non-nil) or clears (nil) the marker of a defined VM.
func setCPUMeta(h *hostconn.Host, dom libvirt.Domain, d *cpuDef) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	if d == nil && parseCPUMeta(old) == nil {
		return nil
	}
	el := ""
	if d != nil {
		el = cpuMetaElement("", *d)
	}
	meta, err := metaSetChild(old, "cpu", el)
	if err != nil {
		return err
	}
	return setDomainMeta(h, dom, meta)
}

// cpuRelationFor fills in the comparison with the current baseline, when
// there is one.
func (s *Server) cpuRelationFor(c *vmCPU) {
	s.cpuBase.mu.Lock()
	cur := s.cpuBase.cur
	s.cpuBase.mu.Unlock()
	if c == nil || cur == nil || cur.Error != "" || cur.Model == "" {
		return
	}
	c.Relation = cpuRelation(c.Model, c.features, cur.def())
}

// newVMCPU is the CPU a new VM gets.
type newVMCPU struct {
	base *cpuDef // the cluster baseline; nil for the other modes
	mode string  // host-model | host-passthrough when base is nil
}

// resolveNewVMCPU turns the request's cpu field into a choice. The default is
// the cluster baseline; when there is none (no common model, no online node)
// the default falls back to host-model, but an explicit "baseline" fails.
func (s *Server) resolveNewVMCPU(node int64, want string) (newVMCPU, error) {
	switch want {
	case "host-model", "host-passthrough":
		return newVMCPU{mode: want}, nil
	case "", "baseline":
	default:
		return newVMCPU{}, badRequest("cpu must be baseline, host-model or host-passthrough")
	}
	b := s.clusterCPUBaseline()
	if !b.includes(node) {
		b = s.refreshBaseline() // the node came online after the last computation
	}
	if b.Error == "" && b.Model != "" && b.includes(node) {
		d := b.def()
		return newVMCPU{base: &d}, nil
	}
	if want == "baseline" {
		why := b.Error
		if why == "" {
			why = "this node is not part of it"
		}
		return newVMCPU{}, badRequest("there is no cluster CPU baseline: " + why)
	}
	return newVMCPU{mode: "host-model"}, nil
}

// apply edits the generated XML of a new VM.
func (c newVMCPU) apply(src string) (string, error) {
	switch {
	case c.base != nil:
		return newVMCPUXML(src, *c.base)
	case c.mode == "host-passthrough":
		return domxml.SetCPU(src, domxml.CPUSpec{Mode: "host-passthrough"})
	}
	return src, nil // the generated XML already says host-model
}

func (b *clusterBaseline) includes(node int64) bool {
	for _, n := range b.Nodes {
		if n.ID == node {
			return true
		}
	}
	return false
}
