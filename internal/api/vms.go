package api

import (
	"encoding/xml"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/cluster"
	"virt-proj/internal/hostconn"
)

var stateNames = []string{"unknown", "running", "blocked", "paused", "shutdown", "shutoff", "crashed", "suspended"}

func stateName(s uint8) string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "unknown"
}

type vmSummary struct {
	Node      int64  `json:"node"`
	NodeName  string `json:"nodeName"`
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	State     string `json:"state"`
	VCPUs     uint16 `json:"vcpus"`
	MemoryKiB uint64 `json:"memoryKiB"`
	Autostart bool   `json:"autostart"`
}

func summarize(node int64, h *hostconn.Host, d libvirt.Domain) (vmSummary, error) {
	state, maxMem, _, vcpus, _, err := h.DomainGetInfo(d)
	if err != nil {
		return vmSummary{}, err
	}
	auto, _ := h.DomainGetAutostart(d)
	return vmSummary{
		Node: node, NodeName: h.Name, UUID: cluster.FormatUUID(d.UUID), Name: d.Name,
		State: stateName(state), VCPUs: vcpus, MemoryKiB: maxMem, Autostart: auto != 0,
	}, nil
}

// listVMs aggregates the domains of every online node, querying nodes in parallel.
func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = []vmSummary{}
	)
	for _, n := range s.Nodes.Status() {
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			doms, _, err := h.ConnectListAllDomains(1, 0)
			if err != nil {
				return
			}
			for _, d := range doms {
				if v, err := summarize(n.ID, h, d); err == nil { // a domain may vanish mid-listing
					mu.Lock()
					out = append(out, v)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	writeJSON(w, out)
}

// domainXML is a read-only view of the parts of a domain definition the UI
// displays. It is never used to write a definition back.
type domainXML struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	OS          struct {
		Firmware string `xml:"firmware,attr"`
		Type     struct {
			Arch    string `xml:"arch,attr"`
			Machine string `xml:"machine,attr"`
		} `xml:"type"`
		Loader string `xml:"loader"`
		Boot   []struct {
			Dev string `xml:"dev,attr"`
		} `xml:"boot"`
	} `xml:"os"`
	CPU struct {
		Mode string `xml:"mode,attr"`
	} `xml:"cpu"`
	Devices struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File   string `xml:"file,attr"`
				Dev    string `xml:"dev,attr"`
				Pool   string `xml:"pool,attr"`
				Volume string `xml:"volume,attr"`
				Name   string `xml:"name,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
				Bus string `xml:"bus,attr"`
			} `xml:"target"`
			ReadOnly *struct{} `xml:"readonly"`
			Boot     struct {
				Order int `xml:"order,attr"`
			} `xml:"boot"`
		} `xml:"disk"`
		Interfaces []struct {
			Type string `xml:"type,attr"`
			MAC  struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Network string `xml:"network,attr"`
				Bridge  string `xml:"bridge,attr"`
				Dev     string `xml:"dev,attr"`
			} `xml:"source"`
			Model struct {
				Type string `xml:"type,attr"`
			} `xml:"model"`
			Link struct {
				State string `xml:"state,attr"`
			} `xml:"link"`
			Boot struct {
				Order int `xml:"order,attr"`
			} `xml:"boot"`
		} `xml:"interface"`
		Graphics []struct {
			Type   string `xml:"type,attr"`
			Port   int    `xml:"port,attr"`
			Listen string `xml:"listen,attr"`
			Socket string `xml:"socket,attr"`
		} `xml:"graphics"`
		Channels []struct {
			Target struct {
				Name  string `xml:"name,attr"`
				State string `xml:"state,attr"`
			} `xml:"target"`
		} `xml:"channel"`
		Consoles []struct{} `xml:"console"`
		Hostdevs []struct{} `xml:"hostdev"`
		TPM      *struct{}  `xml:"tpm"`
	} `xml:"devices"`
}

type vmDisk struct {
	Target   string `json:"target"`
	Bus      string `json:"bus"`
	Device   string `json:"device"`
	Format   string `json:"format,omitempty"`
	Source   string `json:"source,omitempty"`
	ReadOnly bool   `json:"readOnly"`
	// Capacity is the virtual size in bytes; BootOrder is 0 when the disk
	// has no per-device boot order.
	Capacity  uint64 `json:"capacity,omitempty"`
	BootOrder int    `json:"bootOrder,omitempty"`
}

type vmNIC struct {
	MAC    string   `json:"mac"`
	Type   string   `json:"type"`
	Source string   `json:"source"`
	Model  string   `json:"model,omitempty"`
	IPs    []string `json:"ips"`
	// LinkState is "up" or "down"; BootOrder is 0 when not set.
	LinkState string `json:"linkState"`
	BootOrder int    `json:"bootOrder,omitempty"`
}

type vmDetail struct {
	vmSummary
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Arch        string   `json:"arch"`
	Machine     string   `json:"machine"`
	Firmware    string   `json:"firmware"`
	CPUMode     string   `json:"cpuMode,omitempty"`
	Boot        []string `json:"boot"`
	Disks       []vmDisk `json:"disks"`
	NICs        []vmNIC  `json:"nics"`
	HasVNC      bool     `json:"hasVNC"`
	HasSerial   bool     `json:"hasSerial"`
	TPM         bool     `json:"tpm"`
	Passthrough int      `json:"passthroughDevices"`
	// Agent is "connected", "disconnected" (channel present, nothing
	// answering inside the guest) or "absent" (no channel device).
	Agent     string   `json:"agent"`
	Snapshots []string `json:"snapshots"`
	XML       string   `json:"xml"`
	// MaxVCPUs and CurrentMemoryKiB complete the CPU and memory forms;
	// MemoryKiB (from the summary) is the maximum.
	MaxVCPUs         uint16 `json:"maxVcpus"`
	CurrentMemoryKiB uint64 `json:"currentMemoryKiB"`
}

func (s *Server) vmDetail(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	node, _, _ := s.host(r)
	sum, err := summarize(node, h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	// Live XML when running (shows runtime detail such as the VNC port and
	// agent state); the persistent definition otherwise.
	raw, err := h.DomainGetXMLDesc(dom, 0)
	if err != nil {
		fail(w, err)
		return
	}
	var x domainXML
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		fail(w, err)
		return
	}
	d := vmDetail{
		vmSummary: sum, Title: x.Title, Description: x.Description,
		Arch: x.OS.Type.Arch, Machine: x.OS.Type.Machine, CPUMode: x.CPU.Mode,
		Firmware: "bios", Boot: []string{}, Disks: []vmDisk{}, NICs: []vmNIC{},
		HasSerial: len(x.Devices.Consoles) > 0, TPM: x.Devices.TPM != nil,
		Passthrough: len(x.Devices.Hostdevs), Agent: "absent", XML: raw,
	}
	if x.OS.Firmware == "efi" || strings.Contains(strings.ToLower(x.OS.Loader), "ovmf") || strings.Contains(strings.ToLower(x.OS.Loader), "edk2") {
		d.Firmware = "uefi"
	}
	for _, b := range x.OS.Boot {
		d.Boot = append(d.Boot, b.Dev)
	}
	for _, k := range x.Devices.Disks {
		src := k.Source.File
		switch {
		case src != "":
		case k.Source.Dev != "":
			src = k.Source.Dev
		case k.Source.Pool != "":
			src = k.Source.Pool + "/" + k.Source.Volume
		default:
			src = k.Source.Name
		}
		vd := vmDisk{Target: k.Target.Dev, Bus: k.Target.Bus, Device: k.Device, Format: k.Driver.Type, Source: src, ReadOnly: k.ReadOnly != nil, BootOrder: k.Boot.Order}
		if k.Device == "disk" && src != "" {
			if _, capacity, _, err := h.DomainGetBlockInfo(dom, k.Target.Dev, 0); err == nil {
				vd.Capacity = capacity
			}
		}
		d.Disks = append(d.Disks, vd)
	}
	for _, g := range x.Devices.Graphics {
		if g.Type == "vnc" {
			d.HasVNC = true
		}
	}
	for _, c := range x.Devices.Channels {
		if c.Target.Name == "org.qemu.guest_agent.0" {
			d.Agent = "disconnected"
			if c.Target.State == "connected" {
				d.Agent = "connected"
			}
		}
	}

	ips := map[string][]string{}
	if sum.State == "running" {
		src := libvirt.DomainInterfaceAddressesSrcLease
		if d.Agent == "connected" {
			src = libvirt.DomainInterfaceAddressesSrcAgent
		}
		if ifs, err := h.DomainInterfaceAddresses(dom, uint32(src), 0); err == nil {
			for _, i := range ifs {
				if len(i.Hwaddr) == 0 {
					continue
				}
				for _, a := range i.Addrs {
					if !strings.HasPrefix(a.Addr, "fe80:") && a.Addr != "127.0.0.1" && a.Addr != "::1" {
						ips[strings.ToLower(i.Hwaddr[0])] = append(ips[strings.ToLower(i.Hwaddr[0])], a.Addr)
					}
				}
			}
		}
	}
	for _, i := range x.Devices.Interfaces {
		src := i.Source.Network
		if src == "" {
			src = i.Source.Bridge
		}
		if src == "" {
			src = i.Source.Dev
		}
		addrs := ips[strings.ToLower(i.MAC.Address)]
		if addrs == nil {
			addrs = []string{}
		}
		link := i.Link.State
		if link == "" {
			link = "up"
		}
		d.NICs = append(d.NICs, vmNIC{MAC: i.MAC.Address, Type: i.Type, Source: src, Model: i.Model.Type, IPs: addrs, LinkState: link, BootOrder: i.Boot.Order})
	}
	if sum.State != "shutoff" {
		// Boot order is only ever edited in the saved definition, so show that
		// one; the live one stays in force until the next start.
		if raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive); err == nil {
			var c domainXML
			if xml.Unmarshal([]byte(raw), &c) == nil {
				order := map[string]int{}
				for _, k := range c.Devices.Disks {
					order[k.Target.Dev] = k.Boot.Order
				}
				for _, i := range c.Devices.Interfaces {
					order[strings.ToLower(i.MAC.Address)] = i.Boot.Order
				}
				for i := range d.Disks {
					d.Disks[i].BootOrder = order[d.Disks[i].Target]
				}
				for i := range d.NICs {
					d.NICs[i].BootOrder = order[strings.ToLower(d.NICs[i].MAC)]
				}
			}
		}
	}
	d.MaxVCPUs = d.VCPUs
	if n, err := h.DomainGetVcpusFlags(dom, uint32(libvirt.DomainVCPUMaximum)); err == nil && n > 0 {
		d.MaxVCPUs = uint16(n)
	}
	if _, _, cur, _, _, err := h.DomainGetInfo(dom); err == nil {
		d.CurrentMemoryKiB = cur
	}
	d.Snapshots, _ = h.DomainSnapshotListNames(dom, 1024, 0)
	if d.Snapshots == nil {
		d.Snapshots = []string{}
	}
	writeJSON(w, d)
}

func (s *Server) vmAction(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	actions := map[string]func() error{
		"start":    func() error { return h.DomainCreate(dom) },
		"shutdown": func() error { return h.DomainShutdown(dom) },
		"reboot":   func() error { return h.DomainReboot(dom, 0) },
		"poweroff": func() error { return h.DomainDestroy(dom) },
		"reset":    func() error { return h.DomainReset(dom, 0) },
		"pause":    func() error { return h.DomainSuspend(dom) },
		"resume":   func() error { return h.DomainResume(dom) },
	}
	fn, ok := actions[req.Action]
	if !ok {
		fail(w, badRequest("unknown action "+req.Action))
		return
	}
	if err := s.task(r, req.Action, dom.Name, h.Name, func(func(float64)) error { return fn() }); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// vmSetXML replaces the persistent definition with user-edited XML. libvirt
// validates it against its schema; the UUID must stay the same so this can
// only ever edit the addressed VM.
func (s *Server) vmSetXML(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		XML string `json:"xml"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	var id struct {
		UUID string `xml:"uuid"`
	}
	if err := xml.Unmarshal([]byte(req.XML), &id); err != nil {
		fail(w, badRequest("not well-formed XML: "+err.Error()))
		return
	}
	if strings.TrimSpace(id.UUID) != cluster.FormatUUID(dom.UUID) {
		fail(w, badRequest("the <uuid> element must stay "+cluster.FormatUUID(dom.UUID)))
		return
	}
	err = s.task(r, "edit definition", dom.Name, h.Name, func(func(float64)) error {
		_, err := h.DomainDefineXMLFlags(req.XML, libvirt.DomainDefineValidate)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) vmAutostart(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Autostart bool `json:"autostart"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	v := int32(0)
	if req.Autostart {
		v = 1
	}
	if err := h.DomainSetAutostart(dom, v); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// vmDelete removes a VM's definition. With ?disks=1 it also deletes the
// writable disk volumes that live in a storage pool, plus the VM's own
// cloud-init seed volume; other CD-ROMs and read-only or shared media are
// never touched.
func (s *Server) vmDelete(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		fail(w, err)
		return
	}
	if stateName(state) != "shutoff" {
		fail(w, badRequest("power the VM off before deleting it"))
		return
	}
	var paths []string
	if r.URL.Query().Get("disks") == "1" {
		raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
		if err != nil {
			fail(w, err)
			return
		}
		var x domainXML
		if err := xml.Unmarshal([]byte(raw), &x); err != nil {
			fail(w, err)
			return
		}
		for _, k := range x.Devices.Disks {
			if k.Device == "disk" && k.ReadOnly == nil && k.Source.File != "" {
				paths = append(paths, k.Source.File)
			}
			// The cloud-init seed is a read-only CD-ROM, recognised by its
			// name; the pool lookup below makes sure it is a pool volume.
			if k.Device == "cdrom" && k.Source.File != "" && path.Base(k.Source.File) == seedVolumeName(dom.Name) {
				paths = append(paths, k.Source.File)
			}
		}
	}
	left := []string{}
	err = s.task(r, "delete", dom.Name, h.Name, func(func(float64)) error {
		flags := libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata |
			libvirt.DomainUndefineNvram | libvirt.DomainUndefineCheckpointsMetadata | libvirt.DomainUndefineTpm
		if err := h.DomainUndefineFlags(dom, flags); err != nil {
			return err
		}
		refreshed := false
		for _, p := range paths {
			vol, err := h.StorageVolLookupByPath(p)
			if err != nil && !refreshed {
				// The file may have been created outside libvirt; let the
				// pools rescan their directories and look again.
				refreshed = true
				if pools, _, perr := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive); perr == nil {
					for _, pool := range pools {
						h.StoragePoolRefresh(pool, 0)
					}
				}
				vol, err = h.StorageVolLookupByPath(p)
			}
			if err != nil {
				left = append(left, p) // not in any storage pool: not ours to delete
				continue
			}
			if err := h.StorageVolDelete(vol, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	// leftDisks lists disks that are outside every storage pool and were
	// therefore left in place.
	writeJSON(w, map[string]any{"ok": true, "leftDisks": left})
}

const agentChannelXML = `<channel type='unix'><target type='virtio' name='org.qemu.guest_agent.0'/></channel>`

// vmAddAgentChannel adds the virtio channel the QEMU guest agent talks over.
// On a running VM it is hot-plugged when possible; if the VM lacks a
// virtio-serial controller the change is saved and applies at next boot.
func (s *Server) vmAddAgentChannel(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		fail(w, err)
		return
	}
	live := false
	err = s.task(r, "add guest agent channel", dom.Name, h.Name, func(func(float64)) error {
		if stateName(state) == "running" {
			if h.DomainAttachDeviceFlags(dom, agentChannelXML, uint32(libvirt.DomainAffectLive|libvirt.DomainAffectConfig)) == nil {
				live = true
				return nil
			}
		}
		return h.DomainAttachDeviceFlags(dom, agentChannelXML, uint32(libvirt.DomainAffectConfig))
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true, "needsRestart": stateName(state) != "shutoff" && !live})
}
