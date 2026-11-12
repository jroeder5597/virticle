package domxml

import "fmt"

// Serial is one <serial> device as the UI lists it.
type Serial struct {
	Type string `json:"type"`
	Port string `json:"port"`
}

// Serials lists the serial devices of a definition.
func Serials(src string) ([]Serial, error) {
	d, err := Parse(src)
	if err != nil {
		return nil, err
	}
	var out []Serial
	for _, n := range d.Devices("serial") {
		s := Serial{Type: n.Attrs["type"]}
		if t := n.Child("target"); t != nil {
			s.Port = t.Attrs["port"]
		}
		out = append(out, s)
	}
	return out, nil
}

// SerialXML is the device AddSerial adds, in the form libvirt accepts for a
// live or persistent attach.
const SerialXML = "<serial type='pty'><target port='0'/></serial>"

// ConsoleXML is the matching console (an alias of serial port 0).
const ConsoleXML = "<console type='pty'><target type='serial' port='0'/></console>"

// serialConsole reports whether a <console> is the alias of a serial port
// (target type serial, or no target at all, which means the same).
func serialConsole(n *Node) bool {
	t := n.Child("target")
	return t == nil || t.Attrs["type"] == "" || t.Attrs["type"] == "serial"
}

func portOf(n *Node) string {
	if t := n.Child("target"); t != nil && t.Attrs["port"] != "" {
		return t.Attrs["port"]
	}
	return "0"
}

// AddSerial adds a pty serial device on port 0 and, when the definition has
// no serial console yet, the matching <console type='pty'>. It refuses when
// any serial device exists: the app's console opens the first one, so a
// second device would not be the one it reaches.
func AddSerial(src string) (string, error) {
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	if s := d.Devices("serial"); len(s) > 0 {
		return "", fmt.Errorf("the VM already has a serial device (type %s)", s[0].Attrs["type"])
	}
	dev := d.Root.Child("devices")
	if dev == nil {
		return "", fmt.Errorf("the definition has no <devices>")
	}
	frag := SerialXML
	for _, c := range d.Devices("console") {
		if serialConsole(c) {
			return Apply(src, []Splice{d.AppendChild(dev, frag)})
		}
	}
	// One joined fragment: two appends would splice at the same offset.
	return Apply(src, []Splice{d.AppendChild(dev, frag+"\n    "+ConsoleXML)})
}

// RemoveSerial removes the pty serial devices and the serial consoles that
// belong to them. Serials of other types (tcp, unix, file) stay. The count is
// the number of serial devices removed.
func RemoveSerial(src string) (string, int, error) {
	d, err := Parse(src)
	if err != nil {
		return "", 0, err
	}
	ports := map[string]bool{}
	var edits []Splice
	for _, n := range d.Devices("serial") {
		if n.Attrs["type"] == "pty" {
			ports[portOf(n)] = true
			edits = append(edits, d.Remove(n))
		}
	}
	removed := len(edits)
	if removed == 0 {
		return src, 0, nil
	}
	for _, c := range d.Devices("console") {
		if serialConsole(c) && ports[portOf(c)] {
			edits = append(edits, d.Remove(c))
		}
	}
	out, err := Apply(src, edits)
	return out, removed, err
}

// ClockOffset returns the offset attribute of <clock> ("" when absent).
func ClockOffset(src string) string {
	d, err := Parse(src)
	if err != nil {
		return ""
	}
	if c := d.Root.Child("clock"); c != nil {
		return c.Attrs["offset"]
	}
	return ""
}

// SetClockOffset sets <clock offset=...>, touching nothing else (timers stay).
// A definition without <clock> gets one.
func SetClockOffset(src, offset string) (string, error) {
	if offset != "utc" && offset != "localtime" {
		return "", fmt.Errorf("offset must be utc or localtime")
	}
	d, err := Parse(src)
	if err != nil {
		return "", err
	}
	c := d.Root.Child("clock")
	if c == nil {
		return Apply(src, []Splice{d.AppendChild(d.Root, "<clock offset='"+offset+"'/>")})
	}
	if o := c.Attrs["offset"]; o != "" && o != "utc" && o != "localtime" {
		return "", fmt.Errorf("the clock offset is %q, which this editor does not change; use the XML tab", o)
	}
	return Apply(src, []Splice{d.SetAttr(c, "offset", offset)})
}
