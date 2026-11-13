package domxml

import (
	"fmt"
	"strings"
)

// Rate is one direction of a <bandwidth> element, in libvirt's units: average
// kilobytes per second and burst kilobytes. Average 0 means no limit.
type Rate struct {
	Average, Burst uint64
}

// BandwidthXML is the <bandwidth> element for an interface, or "" when neither
// direction is limited. In is the traffic towards the guest (the guest's
// download), Out the traffic from the guest (its upload). No peak is written.
func BandwidthXML(in, out Rate) string {
	var b strings.Builder
	for _, d := range []struct {
		tag string
		r   Rate
	}{{"inbound", in}, {"outbound", out}} {
		if d.r.Average == 0 {
			continue
		}
		fmt.Fprintf(&b, "<%s average='%d'", d.tag, d.r.Average)
		if d.r.Burst > 0 {
			fmt.Fprintf(&b, " burst='%d'", d.r.Burst)
		}
		b.WriteString("/>")
	}
	if b.Len() == 0 {
		return ""
	}
	return "<bandwidth>" + b.String() + "</bandwidth>"
}

// SetNICBandwidth returns the interface element at n with its <bandwidth>
// replaced, added or (when neither direction has an average) removed. Other
// children are kept.
func (d *Doc) SetNICBandwidth(n *Node, in, out Rate) (string, error) {
	frag := BandwidthXML(in, out)
	old := n.ChildrenNamed("bandwidth")
	var edits []Splice
	if len(old) == 0 && frag != "" {
		edits = append(edits, d.AppendChild(n, frag))
	}
	for i, c := range old {
		if i == 0 && frag != "" {
			edits = append(edits, d.Replace(c, frag))
		} else {
			edits = append(edits, d.Remove(c))
		}
	}
	return applyWithin(d.Src, n, edits)
}
