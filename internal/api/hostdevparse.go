package api

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Host devices that can be passed to a VM: parsing libvirt's node device XML,
// deciding which are unsafe to hand over, and building the <hostdev> XML.
// Pure functions only; hostdev.go does the talking to libvirt.

type vmRef struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type groupPeer struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"` // "Vendor Product" when the peer is listed
}

type hostDevice struct {
	Kind      string `json:"kind"` // pci | usb
	ID        string `json:"id"`   // pci: 0000:01:00.0   usb: usb:BUS:DEVICE
	NodeDev   string `json:"nodedev"`
	Class     string `json:"class,omitempty"`    // human readable
	Category  string `json:"category,omitempty"` // gpu | network | storage | usb-controller | bridge | other
	Driver    string `json:"driver,omitempty"`   // host driver bound now
	Vendor    string `json:"vendor"`
	Product   string `json:"product"`
	VendorID  string `json:"vendorId"`
	ProductID string `json:"productId"`

	IOMMUGroup *int        `json:"iommuGroup,omitempty"`
	GroupPeers []groupPeer `json:"groupPeers,omitempty"` // the other devices of the IOMMU group

	UsedBy       []vmRef  `json:"usedBy"`
	Unsafe       bool     `json:"unsafe"`
	UnsafeReason string   `json:"unsafeReason,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`

	classCode uint32
	addr      pciAddr
	usbBus    int
	usbDev    int
	groupAddr []string
}

type pciAddr struct{ Domain, Bus, Slot, Function int }

func (a pciAddr) String() string {
	return fmt.Sprintf("%04x:%02x:%02x.%x", a.Domain, a.Bus, a.Slot, a.Function)
}

var pciIDRE = regexp.MustCompile(`^([0-9a-fA-F]{4}):([0-9a-fA-F]{2}):([0-9a-fA-F]{2})\.([0-7])$`)

func parsePCIID(s string) (pciAddr, bool) {
	m := pciIDRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return pciAddr{}, false
	}
	h := func(x string) int { n, _ := strconv.ParseInt(x, 16, 32); return int(n) }
	return pciAddr{h(m[1]), h(m[2]), h(m[3]), h(m[4])}, true
}

func hexNum(s string) int {
	n, _ := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "0x"), 16, 64)
	return int(n)
}

// parseNodeDev reads `nodedev-dumpxml` of a PCI or USB device. Other device
// kinds give (_, false, nil).
func parseNodeDev(raw string) (hostDevice, bool, error) {
	var x struct {
		Name   string `xml:"name"`
		Parent string `xml:"parent"`
		Driver struct {
			Name string `xml:"name"`
		} `xml:"driver"`
		Cap struct {
			Type     string `xml:"type,attr"`
			Class    string `xml:"class"`
			Domain   string `xml:"domain"`
			Bus      string `xml:"bus"`
			Slot     string `xml:"slot"`
			Function string `xml:"function"`
			Device   string `xml:"device"`
			Product  struct {
				ID   string `xml:"id,attr"`
				Name string `xml:",chardata"`
			} `xml:"product"`
			Vendor struct {
				ID   string `xml:"id,attr"`
				Name string `xml:",chardata"`
			} `xml:"vendor"`
			IOMMU *struct {
				Number int `xml:"number,attr"`
				Addrs  []struct {
					Domain   string `xml:"domain,attr"`
					Bus      string `xml:"bus,attr"`
					Slot     string `xml:"slot,attr"`
					Function string `xml:"function,attr"`
				} `xml:"address"`
			} `xml:"iommuGroup"`
		} `xml:"capability"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return hostDevice{}, false, fmt.Errorf("cannot read node device XML: %w", err)
	}
	c := x.Cap
	d := hostDevice{NodeDev: x.Name, Driver: x.Driver.Name, UsedBy: []vmRef{},
		Vendor: strings.TrimSpace(c.Vendor.Name), Product: strings.TrimSpace(c.Product.Name),
		VendorID: strings.TrimPrefix(c.Vendor.ID, "0x"), ProductID: strings.TrimPrefix(c.Product.ID, "0x")}
	switch c.Type {
	case "pci":
		d.Kind = "pci"
		d.addr = pciAddr{hexNumOrDec(c.Domain), hexNumOrDec(c.Bus), hexNumOrDec(c.Slot), hexNumOrDec(c.Function)}
		d.ID = d.addr.String()
		d.classCode = uint32(hexNum(c.Class))
		d.Class, d.Category = pciClass(d.classCode)
		if c.IOMMU != nil {
			n := c.IOMMU.Number
			d.IOMMUGroup = &n
			for _, a := range c.IOMMU.Addrs {
				id := pciAddr{hexNum(a.Domain), hexNum(a.Bus), hexNum(a.Slot), hexNum(a.Function)}.String()
				if id != d.ID {
					d.groupAddr = append(d.groupAddr, id)
				}
			}
			sort.Strings(d.groupAddr)
		}
		return d, true, nil
	case "usb_device":
		d.Kind = "usb"
		d.usbBus, d.usbDev = hexNumOrDec(c.Bus), hexNumOrDec(c.Device)
		d.ID = fmt.Sprintf("usb:%d:%d", d.usbBus, d.usbDev)
		d.Class, d.Category = "USB device", "usb"
		return d, true, nil
	}
	return hostDevice{}, false, nil
}

// hexNumOrDec reads libvirt's mixed notation: decimal in the capability
// children (<bus>1</bus>) and 0x-prefixed hex in <address> attributes.
func hexNumOrDec(s string) int {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") {
		return hexNum(s)
	}
	n, _ := strconv.Atoi(s)
	return n
}

// pciClass names a 24-bit PCI class code.
func pciClass(code uint32) (name, category string) {
	base, sub := code>>16, (code>>8)&0xff
	switch code >> 8 {
	case 0x0100:
		return "SCSI storage controller", "storage"
	case 0x0101:
		return "IDE controller", "storage"
	case 0x0106:
		return "SATA controller", "storage"
	case 0x0107:
		return "SAS controller", "storage"
	case 0x0108:
		return "NVMe controller", "storage"
	case 0x0200:
		return "Ethernet controller", "network"
	case 0x0280:
		return "Network controller", "network"
	case 0x0300:
		return "VGA controller", "gpu"
	case 0x0302:
		return "3D controller", "gpu"
	case 0x0380:
		return "Display controller", "gpu"
	case 0x0403:
		return "Audio device", "other"
	case 0x0600:
		return "Host bridge", "bridge"
	case 0x0601:
		return "ISA bridge", "bridge"
	case 0x0604:
		return "PCI bridge", "bridge"
	case 0x0c03:
		return "USB controller", "usb-controller"
	case 0x0c05:
		return "SMBus", "other"
	}
	switch base {
	case 0x01:
		return "Storage controller", "storage"
	case 0x02:
		return "Network controller", "network"
	case 0x03:
		return "Display controller", "gpu"
	case 0x06:
		return fmt.Sprintf("Bridge (%02x)", sub), "bridge"
	}
	return fmt.Sprintf("PCI device (class %06x)", code), "other"
}

// hostFacts is what the node itself says about what it needs; it is read over
// SSH with fixed commands (see hostdev.go).
type hostFacts struct {
	// NIC maps the PCI address of a NIC that carries the default route to the
	// interface name.
	NIC map[string]string
	// Storage maps the PCI address of a controller that holds a mounted
	// filesystem or swap to the block device.
	Storage map[string]string
	// BootVGA is the PCI address of the GPU the firmware booted with.
	BootVGA string
	// Known is false when the facts could not be read; then every PCI device
	// that could matter is refused rather than guessed about.
	Known bool
}

// markSafety decides which devices must not be passed through and why. A
// device is also unsafe when it shares its IOMMU group with an unsafe one,
// since the whole group is taken away from the host together.
func markSafety(devs []hostDevice, f hostFacts) {
	byID := map[string]*hostDevice{}
	for i := range devs {
		byID[devs[i].ID] = &devs[i]
	}
	own := func(d *hostDevice) string {
		switch d.Kind {
		case "usb":
			if d.VendorID == "1d6b" { // Linux Foundation: the root hubs of the host's controllers
				return "a USB root hub of the host; the controller it belongs to would stop working"
			}
			return ""
		case "pci":
			switch {
			case d.Category == "bridge":
				return d.Class + ": other devices sit behind it and the host needs it"
			case f.NIC[d.ID] != "":
				return "it carries the node's default route (" + f.NIC[d.ID] + "); passing it through would cut the node off from this server"
			case f.Storage[d.ID] != "":
				return "it holds a mounted filesystem or swap of the host (" + f.Storage[d.ID] + ")"
			case !f.Known && (d.Category == "network" || d.Category == "storage"):
				return "the node's network and storage layout could not be read, so this device cannot be shown to be unused by the host"
			}
		}
		return ""
	}
	for i := range devs {
		d := &devs[i]
		if r := own(d); r != "" {
			d.Unsafe, d.UnsafeReason = true, r
		}
		if d.Kind == "pci" && d.ID == f.BootVGA {
			d.Warnings = append(d.Warnings, "this is the GPU the node booted with; passing it through may blank the node's console")
		}
	}
	for i := range devs {
		d := &devs[i]
		for _, pid := range d.groupAddr {
			d.GroupPeers = append(d.GroupPeers, groupPeer{ID: pid})
			if p := byID[pid]; p != nil {
				d.GroupPeers[len(d.GroupPeers)-1].Name = strings.TrimSpace(p.Vendor + " " + p.Product)
			}
		}
	}
	for i := range devs {
		d := &devs[i]
		if d.Unsafe {
			continue
		}
		for _, pid := range d.groupAddr {
			if p := byID[pid]; p != nil && p.Unsafe && !staysWithHost(p) {
				d.Unsafe = true
				d.UnsafeReason = "it shares IOMMU group " + strconv.Itoa(*d.IOMMUGroup) + " with " + p.ID + " (" + p.Class + "), which must stay with the host: " + p.UnsafeReason
				break
			}
		}
	}
}

// staysWithHost reports a group member that the kernel leaves on the host
// while the rest of its IOMMU group is handed to a VM: a PCIe port bound to
// pcieport, which VFIO explicitly tolerates. A graphics card in the CPU's
// slot always shares its group with such a root port (seen on real
// hardware: Quadro P1000 behind 0000:00:01.0), so it must not block the card.
func staysWithHost(d *hostDevice) bool {
	return d.Kind == "pci" && d.Class == "PCI bridge" && d.Driver == "pcieport"
}

// parseFacts reads the output of the fixed script in hostdev.go: lines of
// "nic <iface> <pci>", "blk <dev> <pci>", "bootvga <pci>" and "ok".
func parseFacts(out string) hostFacts {
	f := hostFacts{NIC: map[string]string{}, Storage: map[string]string{}}
	for _, l := range strings.Split(out, "\n") {
		w := strings.Fields(l)
		switch {
		case len(w) == 3 && w[0] == "nic":
			f.NIC[w[2]] = w[1]
		case len(w) == 3 && w[0] == "blk":
			f.Storage[w[2]] = w[1]
		case len(w) == 2 && w[0] == "bootvga":
			f.BootVGA = w[1]
		case len(w) == 1 && w[0] == "ok":
			f.Known = true
		}
	}
	return f
}

// hostdevXML builds the <hostdev> element for a device. USB devices are
// selected by vendor and product; when several identical devices are present
// the bus and device number pin down the one chosen.
func hostdevXML(d hostDevice, identicalUSB int) string {
	switch d.Kind {
	case "pci":
		return fmt.Sprintf("<hostdev mode='subsystem' type='pci' managed='yes'><source><address domain='0x%04x' bus='0x%02x' slot='0x%02x' function='0x%x'/></source></hostdev>",
			d.addr.Domain, d.addr.Bus, d.addr.Slot, d.addr.Function)
	case "usb":
		s := fmt.Sprintf("<hostdev mode='subsystem' type='usb' managed='yes'><source><vendor id='0x%s'/><product id='0x%s'/>", d.VendorID, d.ProductID)
		if identicalUSB > 1 {
			s += fmt.Sprintf("<address bus='%d' device='%d'/>", d.usbBus, d.usbDev)
		}
		return s + "</source></hostdev>"
	}
	return ""
}

// vmHostdev is one <hostdev> found in a domain definition.
type vmHostdev struct {
	Kind              string
	PCI               pciAddr
	VendorID, Product string
	Bus, Device       int
}

// parseVMHostdevs lists the subsystem host devices of a domain definition.
func parseVMHostdevs(raw string) ([]vmHostdev, error) {
	var x struct {
		Devs []struct {
			Mode   string `xml:"mode,attr"`
			Type   string `xml:"type,attr"`
			Source struct {
				Vendor struct {
					ID string `xml:"id,attr"`
				} `xml:"vendor"`
				Product struct {
					ID string `xml:"id,attr"`
				} `xml:"product"`
				Address struct {
					Domain   string `xml:"domain,attr"`
					Bus      string `xml:"bus,attr"`
					Slot     string `xml:"slot,attr"`
					Function string `xml:"function,attr"`
					Device   string `xml:"device,attr"`
				} `xml:"address"`
			} `xml:"source"`
		} `xml:"devices>hostdev"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return nil, err
	}
	var out []vmHostdev
	for _, h := range x.Devs {
		if h.Mode != "subsystem" {
			continue
		}
		a := h.Source.Address
		switch h.Type {
		case "pci":
			out = append(out, vmHostdev{Kind: "pci", PCI: pciAddr{hexNum(a.Domain), hexNum(a.Bus), hexNum(a.Slot), hexNum(a.Function)}})
		case "usb":
			out = append(out, vmHostdev{Kind: "usb", VendorID: strings.TrimPrefix(h.Source.Vendor.ID, "0x"), Product: strings.TrimPrefix(h.Source.Product.ID, "0x"),
				Bus: hexNumOrDec(a.Bus), Device: hexNumOrDec(a.Device)})
		}
	}
	return out, nil
}

// uses reports whether a VM's host device refers to the node device.
func (v vmHostdev) uses(d hostDevice) bool {
	switch {
	case v.Kind == "pci" && d.Kind == "pci":
		return v.PCI == d.addr
	case v.Kind == "usb" && d.Kind == "usb":
		if v.VendorID != d.VendorID || v.Product != d.ProductID {
			return false
		}
		return v.Bus == 0 || (v.Bus == d.usbBus && v.Device == d.usbDev)
	}
	return false
}
