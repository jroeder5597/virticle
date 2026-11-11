package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/hostconn"
)

// guestAgentWait bounds every agent call made for a page view. A guest whose
// agent is stopped or wedged costs one such wait, not the usual 15 s.
const guestAgentWait = 3

// guestOptionalWait bounds each of the optional agent commands of the Guest
// details card, and guestBudget the whole request: a guest that stops
// answering midway costs one wait, then the rest is skipped.
const (
	guestOptionalWait = 2
	guestBudget       = 8 * time.Second
	// guestClockSkew is the offset between guest and host clock that is worth showing.
	guestClockSkew = 2 * time.Second
)

type guestFS struct {
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type,omitempty"`
	Device     string `json:"device,omitempty"`
	UsedBytes  uint64 `json:"usedBytes"`
	TotalBytes uint64 `json:"totalBytes"`
}

type guestOS struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	PrettyName string `json:"prettyName,omitempty"`
	Version    string `json:"version,omitempty"`
	Kernel     string `json:"kernel,omitempty"`
	Machine    string `json:"machine,omitempty"`
}

type guestInfo struct {
	Available   bool      `json:"available"`
	Error       string    `json:"error,omitempty"`
	OS          *guestOS  `json:"os,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	Timezone    string    `json:"timezone,omitempty"`
	Filesystems []guestFS `json:"filesystems"`

	// Guest details card; every field is omitted when the agent lacks the command.
	AgentVersion   string      `json:"agentVersion,omitempty"`
	Quiesce        *bool       `json:"quiesce,omitempty"` // the agent supports filesystem freeze
	TimezoneOffset *int        `json:"timezoneOffset,omitempty"`
	ClockOffsetSec *float64    `json:"clockOffsetSec,omitempty"` // guest minus host, only when it is above guestClockSkew
	Users          []guestUser `json:"users,omitempty"`
	VCPUsOnline    *int        `json:"vcpusOnline,omitempty"`
	VCPUsTotal     *int        `json:"vcpusTotal,omitempty"`
	Interfaces     []guestNIC  `json:"interfaces,omitempty"`
}

type guestUser struct {
	User      string  `json:"user"`
	Domain    string  `json:"domain,omitempty"`
	LoginTime float64 `json:"loginTime"` // Unix seconds
}

type guestNIC struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"` // CIDR form, IPv4 and IPv6, link-local included
}

// parseAgentInfo reads guest-info: the agent version and whether it can
// freeze filesystems (what quiesced snapshots need).
func parseAgentInfo(raw json.RawMessage) (version string, quiesce bool, err error) {
	var in struct {
		Version  string `json:"version"`
		Commands []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"supported_commands"`
	}
	if err = json.Unmarshal(raw, &in); err != nil {
		return "", false, err
	}
	for _, c := range in.Commands {
		if c.Name == "guest-fsfreeze-freeze" && c.Enabled {
			quiesce = true
		}
	}
	return in.Version, quiesce, nil
}

// parseGuestTime reads guest-get-time (nanoseconds since the epoch) and
// returns the guest clock minus ref.
func parseGuestTime(raw json.RawMessage, ref time.Time) (time.Duration, error) {
	var ns int64
	if err := json.Unmarshal(raw, &ns); err != nil {
		return 0, err
	}
	return time.Unix(0, ns).Sub(ref), nil
}

func parseGuestUsers(raw json.RawMessage) ([]guestUser, error) {
	var in []struct {
		User      string  `json:"user"`
		Domain    string  `json:"domain"`
		LoginTime float64 `json:"login-time"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := []guestUser{}
	for _, u := range in {
		out = append(out, guestUser{User: u.User, Domain: u.Domain, LoginTime: u.LoginTime})
	}
	return out, nil
}

func parseGuestVCPUs(raw json.RawMessage) (online, total int, err error) {
	var in []struct {
		Online bool `json:"online"`
	}
	if err = json.Unmarshal(raw, &in); err != nil {
		return 0, 0, err
	}
	for _, c := range in {
		if c.Online {
			online++
		}
	}
	return online, len(in), nil
}

// parseGuestNICs lists every interface the agent reports with all its
// addresses, IPv4 first. The loopback interface is left out.
func parseGuestNICs(raw json.RawMessage) ([]guestNIC, error) {
	var in []struct {
		Name  string `json:"name"`
		MAC   string `json:"hardware-address"`
		Addrs []struct {
			IP     string `json:"ip-address"`
			Type   string `json:"ip-address-type"`
			Prefix int    `json:"prefix"`
		} `json:"ip-addresses"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := []guestNIC{}
	for _, i := range in {
		if i.Name == "lo" || i.Name == "" {
			continue
		}
		n := guestNIC{Name: i.Name, MAC: strings.ToLower(i.MAC), Addresses: []string{}}
		var v6 []string
		for _, a := range i.Addrs {
			s := a.IP
			if a.Prefix > 0 {
				s = fmt.Sprintf("%s/%d", a.IP, a.Prefix)
			}
			if a.Type == "ipv6" || strings.Contains(a.IP, ":") {
				v6 = append(v6, s)
			} else {
				n.Addresses = append(n.Addresses, s)
			}
		}
		n.Addresses = append(n.Addresses, v6...)
		out = append(out, n)
	}
	return out, nil
}

func parseOSInfo(raw json.RawMessage) (*guestOS, error) {
	var o struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		PrettyName    string `json:"pretty-name"`
		Version       string `json:"version"`
		VersionID     string `json:"version-id"`
		KernelRelease string `json:"kernel-release"`
		Machine       string `json:"machine"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	v := o.Version
	if v == "" {
		v = o.VersionID
	}
	return &guestOS{ID: o.ID, Name: o.Name, PrettyName: o.PrettyName, Version: v, Kernel: o.KernelRelease, Machine: o.Machine}, nil
}

// parseFSInfo keeps the filesystems that have a size (the agent lists
// others without byte counts, such as an unmounted or unsupported volume),
// once per mount point, in mount point order.
func parseFSInfo(raw json.RawMessage) ([]guestFS, error) {
	var in []struct {
		Name       string  `json:"name"`
		Mountpoint string  `json:"mountpoint"`
		Type       string  `json:"type"`
		Used       *uint64 `json:"used-bytes"`
		Total      *uint64 `json:"total-bytes"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := []guestFS{}
	seen := map[string]bool{}
	for _, f := range in {
		if f.Used == nil || f.Total == nil || *f.Total == 0 || f.Mountpoint == "" || seen[f.Mountpoint] {
			continue
		}
		seen[f.Mountpoint] = true
		out = append(out, guestFS{Mountpoint: f.Mountpoint, Type: f.Type, Device: f.Name, UsedBytes: *f.Used, TotalBytes: *f.Total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out, nil
}

// vmGuest reports what the guest agent knows about the guest. It is a
// separate request from the VM detail so a missing or stuck agent never
// slows the detail page; the UI asks for it only when the agent is connected.
func (s *Server) vmGuest(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	g := guestInfo{Filesystems: []guestFS{}}
	raw, err := agentCmdT(h, dom, "guest-get-osinfo", nil, guestAgentWait)
	if err != nil {
		g.Error = err.Error()
		writeJSON(w, g)
		return
	}
	g.Available = true
	if g.OS, err = parseOSInfo(raw); err != nil {
		g.Error = err.Error()
	}
	// The rest is optional; an old agent may lack a command.
	if raw, err := agentCmdT(h, dom, "guest-get-host-name", nil, guestAgentWait); err == nil {
		var n struct {
			Name string `json:"host-name"`
		}
		if json.Unmarshal(raw, &n) == nil {
			g.Hostname = n.Name
		}
	}
	if raw, err := agentCmdT(h, dom, "guest-get-timezone", nil, guestAgentWait); err == nil {
		var z struct {
			Zone   string `json:"zone"`
			Offset int    `json:"offset"`
		}
		if json.Unmarshal(raw, &z) == nil {
			g.Timezone = z.Zone
		}
	}
	if raw, err := agentCmdT(h, dom, "guest-get-fsinfo", nil, guestAgentWait); err == nil {
		if fs, err := parseFSInfo(raw); err == nil {
			g.Filesystems = fs
		}
	}
	s.guestDetails(h, dom, &g)
	writeJSON(w, g)
}

// guestDetails adds the optional rows of the Guest details card. Every
// command is read-only, optional and bounded; none of them runs anything in
// the guest.
func (s *Server) guestDetails(h *hostconn.Host, dom libvirt.Domain, g *guestInfo) {
	deadline := time.Now().Add(guestBudget)
	// try runs one command unless the time budget is used up.
	try := func(cmd string, use func(json.RawMessage)) {
		if time.Now().After(deadline) {
			return
		}
		if raw, err := agentCmdT(h, dom, cmd, nil, guestOptionalWait); err == nil {
			use(raw)
		}
	}
	try("guest-info", func(raw json.RawMessage) {
		if v, q, err := parseAgentInfo(raw); err == nil {
			g.AgentVersion, g.Quiesce = v, &q
		}
	})
	asked := time.Now()
	try("guest-get-time", func(raw json.RawMessage) {
		// Half the round trip is the best estimate of when the guest read its clock.
		if off, err := parseGuestTime(raw, asked.Add(time.Since(asked)/2)); err == nil && (off > guestClockSkew || off < -guestClockSkew) {
			sec := off.Seconds()
			g.ClockOffsetSec = &sec
		}
	})
	try("guest-get-users", func(raw json.RawMessage) {
		if u, err := parseGuestUsers(raw); err == nil {
			g.Users = u
		}
	})
	try("guest-get-vcpus", func(raw json.RawMessage) {
		if on, total, err := parseGuestVCPUs(raw); err == nil {
			g.VCPUsOnline, g.VCPUsTotal = &on, &total
		}
	})
	try("guest-network-get-interfaces", func(raw json.RawMessage) {
		if n, err := parseGuestNICs(raw); err == nil {
			g.Interfaces = n
		}
	})
}

// --- guest agent prompt: OS detection and the virtio-win ISO ---

const virtioWinURL = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso"

// virtioWinPattern is the volume name that counts as the virtio-win ISO
// (matched case-insensitively). VIRT_VIRTIO_WIN_PATTERN overrides it, for
// sites that name the file differently.
var virtioWinPattern = sync.OnceValue(func() string {
	if p := os.Getenv("VIRT_VIRTIO_WIN_PATTERN"); p != "" {
		return strings.ToLower(p)
	}
	return "virtio-win*.iso"
})

func isVirtioWinISO(name, pattern string) bool {
	ok, err := path.Match(strings.ToLower(pattern), strings.ToLower(name))
	return err == nil && ok
}

var osinfoIDRE = regexp.MustCompile(`<(?:\w+:)?os\s+id=["']([^"']+)["']`)

// guessOS names the likely guest family from a domain definition: the
// libosinfo id that virt-install and virt-manager record, else the
// Windows-only hints (Hyper-V enlightenments, a local-time clock).
func guessOS(domXML string) (family, id, source string) {
	if i := strings.Index(domXML, "libosinfo.org/xmlns/libvirt/domain"); i >= 0 {
		if m := osinfoIDRE.FindStringSubmatch(domXML[i:]); m != nil {
			id = m[1]
			if strings.Contains(id, "microsoft.com/") {
				return "windows", id, "libosinfo"
			}
			return "linux", id, "libosinfo" // every other id is Unix-like; the agent steps are the same
		}
	}
	var x struct {
		Features struct {
			Hyperv *struct{} `xml:"hyperv"`
		} `xml:"features"`
		Clock struct {
			Offset string `xml:"offset,attr"`
		} `xml:"clock"`
	}
	if xml.Unmarshal([]byte(domXML), &x) == nil && (x.Features.Hyperv != nil || x.Clock.Offset == "localtime") {
		return "windows", "", "guess"
	}
	return "", "", ""
}

type virtioWinVolume struct {
	Pool   string `json:"pool"`
	Volume string `json:"volume"`
}

type agentHelp struct {
	OS     string `json:"os,omitempty"`
	OSID   string `json:"osId,omitempty"`
	Source string `json:"source,omitempty"` // libosinfo | guess
	// For Windows guests:
	VirtioWin *virtioWinVolume `json:"virtioWin,omitempty"` // an ISO found on the node
	Attached  string           `json:"attached,omitempty"`  // CD-ROM target already holding it
	CDROM     string           `json:"cdrom,omitempty"`     // an empty CD-ROM drive that can take it
	// CDROMInUse is set when the only drives hold other media: inserting
	// would replace that medium.
	CDROMInUse string `json:"cdromInUse,omitempty"`
	// Pools a download can go to (directory-backed and running).
	DownloadPools []string `json:"downloadPools"`
	DownloadURL   string   `json:"downloadUrl"`
	DownloadName  string   `json:"downloadName"`
}

func (s *Server) vmAgentHelp(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		fail(w, err)
		return
	}
	out := agentHelp{DownloadPools: []string{}, DownloadURL: virtioWinURL, DownloadName: "virtio-win.iso"}
	out.OS, out.OSID, out.Source = guessOS(raw)
	if out.OS == "windows" {
		var x domainXML
		if err := xml.Unmarshal([]byte(raw), &x); err != nil {
			fail(w, err)
			return
		}
		pattern := virtioWinPattern()
		pools, _, err := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive)
		if err != nil {
			fail(w, err)
			return
		}
		for _, p := range pools {
			if px, err := h.StoragePoolGetXMLDesc(p, 0); err == nil {
				var t struct {
					Type string `xml:"type,attr"`
				}
				if xml.Unmarshal([]byte(px), &t) == nil && (t.Type == "dir" || t.Type == "netfs" || t.Type == "fs") {
					out.DownloadPools = append(out.DownloadPools, p.Name)
				}
			}
			vols, _, err := h.StoragePoolListAllVolumes(p, 1, 0)
			if err != nil {
				continue
			}
			for _, v := range vols {
				if out.VirtioWin == nil && isVirtioWinISO(v.Name, pattern) {
					out.VirtioWin = &virtioWinVolume{Pool: p.Name, Volume: v.Name}
				}
			}
		}
		sort.Strings(out.DownloadPools)
		for _, k := range x.Devices.Disks {
			if k.Device != "cdrom" {
				continue
			}
			if k.Source.File != "" && isVirtioWinISO(path.Base(k.Source.File), pattern) {
				out.Attached = k.Target.Dev
			}
			if k.Source.File == "" && out.CDROM == "" {
				out.CDROM = k.Target.Dev
			}
			if k.Source.File != "" && out.CDROMInUse == "" {
				out.CDROMInUse = k.Target.Dev
			}
		}
	}
	writeJSON(w, out)
}

// parseAgentInterfaces turns guest-network-get-interfaces into addresses by
// lower-case MAC, leaving out loopback and link-local ones.
func parseAgentInterfaces(raw json.RawMessage) (map[string][]string, error) {
	var in []struct {
		MAC   string `json:"hardware-address"`
		Addrs []struct {
			IP string `json:"ip-address"`
		} `json:"ip-addresses"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, i := range in {
		mac := strings.ToLower(i.MAC)
		if mac == "" {
			continue
		}
		for _, a := range i.Addrs {
			if !strings.HasPrefix(a.IP, "fe80:") && a.IP != "127.0.0.1" && a.IP != "::1" {
				out[mac] = append(out[mac], a.IP)
			}
		}
	}
	return out, nil
}

// agentIPs asks the agent for addresses with a short timeout. libvirt's own
// DomainInterfaceAddresses(agent) waits forever on a stuck agent and holds
// the domain's agent lock meanwhile, which would hang the VM detail page.
func agentIPs(h *hostconn.Host, dom libvirt.Domain) (map[string][]string, error) {
	raw, err := agentCmdT(h, dom, "guest-network-get-interfaces", nil, guestAgentWait)
	if err != nil {
		return nil, err
	}
	return parseAgentInterfaces(raw)
}
