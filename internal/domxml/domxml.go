// Package domxml makes targeted edits to libvirt domain XML. It never
// re-serialises a document: the source is parsed once to learn where each
// element lives, and edits are splices of the original text. Everything not
// touched (comments, <metadata>, qemu:commandline, attribute order, whitespace)
// stays byte-for-byte identical.
package domxml

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Node is an element with its position in the source text.
type Node struct {
	Name     string
	Attrs    map[string]string
	Start    int // offset of '<'
	OpenEnd  int // offset just past the start tag's '>'
	CloseAt  int // offset of the end tag's '<'; equals End for a self-closing element
	End      int // offset just past the element
	SelfEnd  bool
	Children []*Node
	Parent   *Node
}

// Doc is a parsed document plus its source.
type Doc struct {
	Src  string
	Root *Node
}

// Parse locates every element of src.
func Parse(src string) (*Doc, error) {
	dec := xml.NewDecoder(strings.NewReader(src))
	dec.Strict = true
	var root, cur *Node
	prev := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not well-formed XML: %w", err)
		}
		pos := int(dec.InputOffset())
		switch t := tok.(type) {
		case xml.StartElement:
			n := &Node{Name: t.Name.Local, Attrs: map[string]string{}, Start: prev, OpenEnd: pos, Parent: cur}
			for _, a := range t.Attr {
				n.Attrs[a.Name.Local] = a.Value
			}
			n.SelfEnd = strings.HasSuffix(src[prev:pos], "/>")
			if cur != nil {
				cur.Children = append(cur.Children, n)
			} else if root == nil {
				root = n
			}
			cur = n
		case xml.EndElement:
			if cur == nil {
				return nil, fmt.Errorf("unbalanced end tag")
			}
			if cur.SelfEnd {
				cur.CloseAt, cur.End = pos, pos
			} else {
				cur.CloseAt, cur.End = prev, pos
			}
			cur = cur.Parent
		}
		prev = pos
	}
	if root == nil {
		return nil, fmt.Errorf("no root element")
	}
	return &Doc{Src: src, Root: root}, nil
}

// Child returns the first direct child with the given name, or nil.
func (n *Node) Child(name string) *Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ChildrenNamed returns all direct children with the given name.
func (n *Node) ChildrenNamed(name string) []*Node {
	var out []*Node
	if n == nil {
		return out
	}
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// Path follows direct children by name ("devices", "disk" is not a path: use
// Child/ChildrenNamed for repeated elements).
func (n *Node) Path(names ...string) *Node {
	for _, name := range names {
		n = n.Child(name)
	}
	return n
}

// Raw is the element's source text.
func (d *Doc) Raw(n *Node) string { return d.Src[n.Start:n.End] }

// Text is the element's character data (no child elements expected).
func (d *Doc) Text(n *Node) string {
	if n == nil || n.SelfEnd {
		return ""
	}
	return d.Src[n.OpenEnd:n.CloseAt]
}

// Devices returns the direct children of <devices> named kind.
func (d *Doc) Devices(kind string) []*Node { return d.Root.Child("devices").ChildrenNamed(kind) }

// Splice is a replacement of Src[Start:End] by Text.
type Splice struct {
	Start, End int
	Text       string
}

// Apply performs non-overlapping splices on src.
func Apply(src string, edits []Splice) (string, error) {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
	var b strings.Builder
	at := 0
	for _, e := range edits {
		if e.Start < at || e.End < e.Start || e.End > len(src) {
			return "", fmt.Errorf("overlapping edits")
		}
		b.WriteString(src[at:e.Start])
		b.WriteString(e.Text)
		at = e.End
	}
	b.WriteString(src[at:])
	return b.String(), nil
}

// Remove deletes the element together with the indentation and newline that
// precede it, so no blank line remains.
func (d *Doc) Remove(n *Node) Splice {
	s := n.Start
	for s > 0 && (d.Src[s-1] == ' ' || d.Src[s-1] == '\t') {
		s--
	}
	if s > 0 && d.Src[s-1] == '\n' {
		s--
	} else {
		s = n.Start
	}
	return Splice{s, n.End, ""}
}

// AppendChild inserts frag as the last child of n, copying the indentation of
// n's last child when the document is pretty-printed.
func (d *Doc) AppendChild(n *Node, frag string) Splice {
	if n.SelfEnd {
		// <a x='1'/>  ->  <a x='1'>frag</a>
		open := d.Src[n.Start : n.OpenEnd-2]
		open = strings.TrimRight(open, " \t")
		return Splice{n.Start, n.End, open + ">" + frag + "</" + n.Name + ">"}
	}
	if len(n.Children) == 0 {
		return Splice{n.OpenEnd, n.OpenEnd, frag}
	}
	last := n.Children[len(n.Children)-1]
	ls := strings.LastIndexByte(d.Src[:last.Start], '\n')
	indent, ok := "", false
	if ls >= 0 && strings.TrimLeft(d.Src[ls+1:last.Start], " \t") == "" {
		indent, ok = d.Src[ls+1:last.Start], true
	}
	if ok {
		return Splice{last.End, last.End, "\n" + indent + frag}
	}
	return Splice{last.End, last.End, frag}
}

// SetAttr sets (or adds) an attribute on the start tag of n.
func (d *Doc) SetAttr(n *Node, name, value string) Splice {
	tag := d.Src[n.Start:n.OpenEnd]
	re := regexp.MustCompile(`(\s` + regexp.QuoteMeta(name) + `\s*=\s*)('[^']*'|"[^"]*")`)
	val := "'" + escAttr(value) + "'"
	if loc := re.FindStringSubmatchIndex(tag); loc != nil {
		return Splice{n.Start + loc[4], n.Start + loc[5], val}
	}
	end := len(tag) - 1
	if n.SelfEnd {
		end = len(tag) - 2
	}
	end = len(strings.TrimRight(tag[:end], " \t"))
	return Splice{n.Start + end, n.Start + end, " " + name + "=" + val}
}

// Replace swaps the whole element for new text.
func (d *Doc) Replace(n *Node, text string) Splice { return Splice{n.Start, n.End, text} }

func escAttr(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), "'", "&apos;")
}

// Esc escapes s for use inside a single-quoted attribute value.
func Esc(s string) string { return escAttr(s) }

// DiskTargets lists the <target dev> of every disk in the document.
func (d *Doc) DiskTargets() []string {
	var out []string
	for _, k := range d.Devices("disk") {
		if t := k.Child("target"); t != nil {
			out = append(out, t.Attrs["dev"])
		}
	}
	return out
}

// NextTarget returns the first unused device name with the given prefix
// ("vd", "sd"): vda, vdb, ... vdz, vdaa, ...
func NextTarget(prefix string, used []string) string {
	taken := map[string]bool{}
	for _, u := range used {
		taken[u] = true
	}
	for i := 0; i < 26*27; i++ {
		name := prefix + letters(i)
		if !taken[name] {
			return name
		}
	}
	return ""
}

func letters(i int) string {
	s := ""
	for {
		s = string(rune('a'+i%26)) + s
		i = i/26 - 1
		if i < 0 {
			return s
		}
	}
}

// DiskWithSource returns the source text of the disk element at n with its
// <source> and <backingStore> children replaced: by newSource when it is not
// empty, otherwise removed (an empty drive). All other children keep their
// place and content (driver, target, boot, address, ...).
func (d *Doc) DiskWithSource(n *Node, newSource string) (string, error) {
	var edits []Splice
	placed := false
	for _, c := range n.Children {
		switch c.Name {
		case "source":
			if newSource != "" && !placed {
				edits = append(edits, d.Replace(c, newSource))
				placed = true
			} else {
				edits = append(edits, d.Remove(c))
			}
		case "backingStore":
			edits = append(edits, d.Remove(c))
		}
	}
	if newSource != "" && !placed {
		edits = append(edits, d.AppendChild(n, newSource))
	}
	return applyWithin(d.Src, n, edits)
}

// applyWithin applies edits that all lie inside n and returns n's new text.
func applyWithin(src string, n *Node, edits []Splice) (string, error) {
	out, err := Apply(src, edits)
	if err != nil {
		return "", err
	}
	delta := len(out) - len(src)
	return out[n.Start : n.End+delta], nil
}

// BootRef names a device in a boot order: a disk by its target (vda, sda) or
// a network interface by MAC address.
type BootRef string

// SetBootOrder replaces the boot configuration with per-device <boot order>
// elements, in the given order. libvirt forbids mixing these with the older
// <os><boot dev=.../> style, so those entries are removed. Disks and
// interfaces that are not listed lose any boot order they had.
func SetBootOrder(src string, order []string) (string, error) {
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	if len(order) == 0 {
		return "", fmt.Errorf("boot order is empty")
	}
	if d.Root.Child("devices") == nil {
		return "", fmt.Errorf("domain has no <devices>")
	}
	rank := map[string]int{}
	for i, o := range order {
		k := strings.ToLower(o)
		if _, dup := rank[k]; dup {
			return "", fmt.Errorf("%s is listed twice", o)
		}
		rank[k] = i + 1
	}
	var edits []Splice
	if os := d.Root.Child("os"); os != nil {
		for _, b := range os.ChildrenNamed("boot") {
			edits = append(edits, d.Remove(b))
		}
	}
	used := map[string]bool{}
	for _, kind := range []string{"disk", "interface"} {
		for _, dev := range d.Devices(kind) {
			key := ""
			if kind == "disk" {
				if t := dev.Child("target"); t != nil {
					key = strings.ToLower(t.Attrs["dev"])
				}
			} else if m := dev.Child("mac"); m != nil {
				key = strings.ToLower(m.Attrs["address"])
			}
			for _, b := range dev.ChildrenNamed("boot") {
				edits = append(edits, d.Remove(b))
			}
			if n, ok := rank[key]; ok && key != "" {
				used[key] = true
				edits = append(edits, d.AppendChild(dev, fmt.Sprintf("<boot order='%d'/>", n)))
			}
		}
	}
	for _, o := range order {
		if !used[strings.ToLower(o)] {
			return "", fmt.Errorf("%s is not a disk or network interface of this VM", o)
		}
	}
	return Apply(src, edits)
}

// SetNICSource returns the interface element at n with its <source> pointing at
// a libvirt network or a host bridge and, when linkState is not empty, with
// <link state=.../> set. Other children are kept.
func (d *Doc) SetNICSource(n *Node, network, bridge, linkState string) (string, error) {
	var edits []Splice
	if network != "" || bridge != "" {
		typ, src := "network", "<source network='"+Esc(network)+"'/>"
		if bridge != "" {
			typ, src = "bridge", "<source bridge='"+Esc(bridge)+"'/>"
		}
		edits = append(edits, d.SetAttr(n, "type", typ))
		placed := false
		for _, c := range n.ChildrenNamed("source") {
			if !placed {
				edits = append(edits, d.Replace(c, src))
				placed = true
			} else {
				edits = append(edits, d.Remove(c))
			}
		}
		if !placed {
			edits = append(edits, d.AppendChild(n, src))
		}
		// A virtualport belongs to the old attachment and libvirt would reject it.
		for _, c := range n.ChildrenNamed("virtualport") {
			edits = append(edits, d.Remove(c))
		}
	}
	if linkState != "" {
		link := "<link state='" + Esc(linkState) + "'/>"
		if c := n.Child("link"); c != nil {
			edits = append(edits, d.Replace(c, link))
		} else {
			edits = append(edits, d.AppendChild(n, link))
		}
	}
	return applyWithin(d.Src, n, edits)
}
