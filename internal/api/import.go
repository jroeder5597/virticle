package api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// Importing a disk image: the image is already on the node (an uploaded or
// downloaded volume, or a file inside a pool directory). It is inspected with
// qemu-img, which is never pointed at anything it could follow: images that
// reference a backing file, an external data file or foreign VMDK extents are
// refused, because a hostile image could name any file on the node. The
// format is detected from the content (never the extension) and passed to
// qemu-img convert explicitly. The result is a new qcow2 volume.

var importFormats = map[string]bool{"qcow2": true, "raw": true, "vmdk": true, "vdi": true, "vhdx": true}

const (
	importMaxOVAEntries = 64
	importMaxOVFBytes   = 2 << 20
	importMaxOVABytes   = 4 << 40
	importFreeMargin    = 256 << 20
)

type importSource struct {
	Pool   string `json:"pool"`
	Volume string `json:"volume"`
	Path   string `json:"path"`
}

type importRequest struct {
	Source importSource `json:"source"`
	// Pool is where the result goes. Name is the new volume's name for a
	// disk, or the VM name (volumes are "<name>-disk1.qcow2", ...) for an OVA.
	Pool string `json:"pool"`
	Name string `json:"name"`
}

// --- qemu-img info ---

// importImgInfo is the part of `qemu-img info --output=json` we use.
type importImgInfo struct {
	Format         string `json:"format"`
	VirtualSize    int64  `json:"virtual-size"`
	ActualSize     int64  `json:"actual-size"`
	Encrypted      bool   `json:"encrypted"`
	BackingName    string `json:"backing-filename"`
	FullBacking    string `json:"full-backing-filename"`
	FormatSpecific struct {
		Data struct {
			DataFile string `json:"data-file"`
			Extents  []struct {
				Filename string `json:"filename"`
			} `json:"extents"`
		} `json:"data"`
	} `json:"format-specific"`
}

// parseImgInfo extracts and vets the JSON. self is the path qemu-img was
// given. Anything that makes qemu open a second file is refused.
func parseImgInfo(out, self string) (importImgInfo, error) {
	var in importImgInfo
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return in, fmt.Errorf("not a disk image qemu-img understands: %s", impFirstLine(out))
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &in); err != nil {
		return in, fmt.Errorf("cannot read qemu-img output: %w", err)
	}
	switch {
	case !importFormats[in.Format]:
		return in, badRequest("unsupported image format " + quoteTag(in.Format) + ": supported are qcow2, raw, vmdk, vdi and vhdx")
	case in.BackingName != "" || in.FullBacking != "":
		return in, badRequest("refusing image: it has a backing file (" + in.FullBacking + "); a backing reference can point at any file on the node. Flatten it first with qemu-img convert on a machine you trust")
	case in.FormatSpecific.Data.DataFile != "":
		return in, badRequest("refusing image: it uses an external data file (" + in.FormatSpecific.Data.DataFile + ")")
	case in.Encrypted:
		return in, badRequest("encrypted images cannot be imported")
	case in.VirtualSize <= 0:
		return in, badRequest("the image reports no size")
	}
	for _, e := range in.FormatSpecific.Data.Extents {
		if path.Clean(e.Filename) != path.Clean(self) {
			return in, badRequest("refusing VMDK: it refers to another file (" + e.Filename + "); only self-contained images are imported")
		}
	}
	return in, nil
}

func impFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func qemuInfo(h *hostconn.Host, file string) (importImgInfo, error) {
	// -U: do not take locks, so an image that a VM uses can be inspected.
	// No -f: the format is probed from the content, and no --backing-chain:
	// a referenced file is reported, never opened.
	out, err := h.RunRoot("qemu-img info -U --output=json -- " + shQuote(file) + " 2>&1")
	if err != nil {
		return importImgInfo{}, badRequest("qemu-img could not read the image: " + impFirstLine(out))
	}
	return parseImgInfo(out, file)
}

// --- locating the source ---

// poolDir is the directory of a running directory-like pool.
type poolDir struct{ Name, Dir string }

func poolDirs(h *hostconn.Host) ([]poolDir, error) {
	pools, _, err := h.ConnectListAllStoragePools(1, libvirt.ConnectListStoragePoolsActive)
	if err != nil {
		return nil, err
	}
	var out []poolDir
	for _, p := range pools {
		raw, err := h.StoragePoolGetXMLDesc(p, 0)
		if err != nil {
			continue
		}
		var px struct {
			Type   string `xml:"type,attr"`
			Target struct {
				Path string `xml:"path"`
			} `xml:"target"`
		}
		if xml.Unmarshal([]byte(raw), &px) == nil && (px.Type == "dir" || px.Type == "fs" || px.Type == "netfs") && strings.HasPrefix(px.Target.Path, "/") {
			out = append(out, poolDir{p.Name, strings.TrimRight(px.Target.Path, "/")})
		}
	}
	return out, nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// resolveSource turns the request into the real path of a regular file that
// lies inside a pool directory of the node (symlinks resolved on the node, so
// a link inside the pool pointing outside is refused).
func resolveSource(h *hostconn.Host, src importSource) (string, error) {
	dirs, err := poolDirs(h)
	if err != nil {
		return "", err
	}
	var file string
	switch {
	case src.Volume != "":
		pool, err := h.StoragePoolLookupByName(src.Pool)
		if err != nil {
			return "", err
		}
		vol, err := h.StorageVolLookupByName(pool, src.Volume)
		if err != nil {
			return "", err
		}
		if file, err = h.StorageVolGetPath(vol); err != nil {
			return "", err
		}
	case src.Path != "":
		file = src.Path
		if !strings.HasPrefix(file, "/") || path.Clean(file) != file || hasControl(file) {
			return "", badRequest("the path must be absolute, normalised (no .., no //) and free of control characters")
		}
	default:
		return "", badRequest("choose a volume or give a path")
	}
	if len(dirs) == 0 {
		return "", badRequest("the node has no running directory-backed storage pool")
	}
	// The path must be inside a pool dir by name first, then by real path.
	real, err := h.RunRoot("p=$(realpath -e -- " + shQuote(file) + ") && [ -f \"$p\" ] && printf '%s' \"$p\"")
	if err != nil || real == "" || hasControl(real) {
		return "", badRequest("no such regular file on the node: " + file)
	}
	for _, d := range dirs {
		rd, err := h.RunRoot("realpath -e -- " + shQuote(d.Dir))
		if err == nil && strings.HasPrefix(real, strings.TrimRight(rd, "/")+"/") {
			return real, nil
		}
	}
	return "", badRequest("refusing " + real + ": it is not inside a storage pool directory of this node")
}

// destPool returns the directory of the running dir pool the result goes to.
func destPool(h *hostconn.Host, name string) (libvirt.StoragePool, poolDir, error) {
	dirs, err := poolDirs(h)
	if err != nil {
		return libvirt.StoragePool{}, poolDir{}, err
	}
	for _, d := range dirs {
		if d.Name == name {
			p, err := h.StoragePoolLookupByName(name)
			return p, d, err
		}
	}
	return libvirt.StoragePool{}, poolDir{}, badRequest("the destination must be a running directory-backed pool")
}

func freeBytes(h *hostconn.Host, dir string) (int64, error) {
	out, err := h.RunRoot("df -P -B1 -- " + shQuote(dir) + " | awk 'NR==2{print $4}'")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// --- tar (OVA) listing ---

var (
	tarLineRE   = regexp.MustCompile(`^(.)[rwxsStT-]{9} \S+ +(\d+) \d{4}-\d{2}-\d{2} \d{2}:\d{2} (.+)$`)
	plainNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`)
)

type tarEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// parseTarListing vets `tar -tv` output. Only regular files with plain names
// (no directories, no slash, no links) are accepted, so extraction cannot
// write outside the work directory.
func parseTarListing(out string) ([]tarEntry, error) {
	var es []tarEntry
	seen := map[string]bool{}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		m := tarLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			return nil, badRequest("refusing archive: unexpected entry in the listing: " + quoteTag(impFirstLine(line)))
		}
		if m[1] != "-" {
			return nil, badRequest("refusing archive: " + quoteTag(m[3]) + " is not a regular file (directories, links and devices are not allowed in an OVA)")
		}
		if !plainNameRE.MatchString(m[3]) {
			return nil, badRequest("refusing archive: member " + quoteTag(m[3]) + " is not a plain file name (no slashes, no ..)")
		}
		if seen[m[3]] {
			return nil, badRequest("refusing archive: duplicate member " + m[3])
		}
		seen[m[3]] = true
		size, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return nil, badRequest("refusing archive: bad size for " + m[3])
		}
		total += size
		es = append(es, tarEntry{m[3], size})
		if len(es) > importMaxOVAEntries {
			return nil, badRequest("refusing archive: more than 64 members")
		}
	}
	if len(es) == 0 {
		return nil, badRequest("the archive is empty")
	}
	if total > importMaxOVABytes {
		return nil, badRequest("refusing archive: contents total more than 4 TiB")
	}
	return es, nil
}

// listTar returns the vetted members of a tar file, or ok=false when the file
// is not a tar archive at all.
func listTar(h *hostconn.Host, file string) (entries []tarEntry, ok bool, err error) {
	out, runErr := h.RunRoot("LC_ALL=C TZ=UTC timeout 60 tar --quoting-style=escape -tvf " + shQuote(file) + " 2>/dev/null")
	if runErr != nil || strings.TrimSpace(out) == "" {
		return nil, false, nil
	}
	entries, err = parseTarListing(out)
	return entries, true, err
}

func ovfMember(es []tarEntry) (string, error) {
	name := ""
	for _, e := range es {
		if strings.HasSuffix(strings.ToLower(e.Name), ".ovf") {
			if name != "" {
				return "", badRequest("the archive contains more than one .ovf descriptor")
			}
			name = e.Name
			if e.Size > importMaxOVFBytes {
				return "", badRequest("the .ovf descriptor is larger than 2 MiB")
			}
		}
	}
	if name == "" {
		return "", badRequest("this is a tar archive, but it has no .ovf descriptor: not an OVA")
	}
	return name, nil
}

// --- OVF ---

type ovfKV struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
}

type ovfEnvelope struct {
	Files []struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	} `xml:"References>File"`
	Disks []struct {
		ID       string `xml:"diskId,attr"`
		FileRef  string `xml:"fileRef,attr"`
		Capacity string `xml:"capacity,attr"`
		Units    string `xml:"capacityAllocationUnits,attr"`
	} `xml:"DiskSection>Disk"`
	Collection *struct{} `xml:"VirtualSystemCollection"`
	VS         struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"Name"`
		OS   struct {
			OSType string `xml:"osType,attr"`
			Desc   string `xml:"Description"`
		} `xml:"OperatingSystemSection"`
		HW struct {
			Items []struct {
				ResourceType string `xml:"ResourceType"`
				Quantity     string `xml:"VirtualQuantity"`
				Units        string `xml:"AllocationUnits"`
				HostResource string `xml:"HostResource"`
			} `xml:"Item"`
			Config []ovfKV `xml:"Config"`
			Extra  []ovfKV `xml:"ExtraConfig"`
		} `xml:"VirtualHardwareSection"`
	} `xml:"VirtualSystem"`
}

type ovfDisk struct {
	File          string `json:"file"`
	CapacityBytes int64  `json:"capacityBytes,omitempty"`
}

// ovfInfo is what the descriptor says; the controller turns it into a
// proposed createVMRequest.
type ovfInfo struct {
	Name       string    `json:"name"`
	VCPUs      int       `json:"vcpus"`
	MemoryMiB  int       `json:"memoryMiB"`
	UEFI       bool      `json:"uefi"`
	SecureBoot bool      `json:"secureBoot"`
	Windows    bool      `json:"windows"`
	Disks      []ovfDisk `json:"disks"`
}

// ovfBytes converts a quantity with an OVF allocation unit ("byte * 2^20",
// "MegaBytes", ...) to bytes; def is the unit's size when none is given.
func ovfBytes(qty int64, units string, def int64) int64 {
	u := strings.ToLower(strings.TrimSpace(units))
	switch {
	case u == "":
		return qty * def
	case strings.Contains(u, "2^"):
		if n, err := strconv.Atoi(strings.TrimSpace(u[strings.Index(u, "2^")+2:])); err == nil && n >= 0 && n < 50 {
			return qty << n
		}
	case u == "byte" || u == "bytes":
		return qty
	case strings.HasPrefix(u, "kilo") || u == "kb":
		return qty << 10
	case strings.HasPrefix(u, "mega") || u == "mb":
		return qty << 20
	case strings.HasPrefix(u, "giga") || u == "gb":
		return qty << 30
	}
	return qty * def
}

func parseOVF(src []byte) (ovfInfo, error) {
	var env ovfEnvelope
	if err := xml.Unmarshal(src, &env); err != nil {
		return ovfInfo{}, badRequest("the .ovf descriptor is not valid XML: " + err.Error())
	}
	if env.Collection != nil {
		return ovfInfo{}, badRequest("OVAs with several virtual machines are not supported")
	}
	vs := env.VS
	info := ovfInfo{Name: vs.Name, VCPUs: 1, MemoryMiB: 1024, Disks: []ovfDisk{}}
	if info.Name == "" {
		info.Name = vs.ID
	}
	files := map[string]string{}
	for _, f := range env.Files {
		files[f.ID] = f.Href
	}
	diskByID := map[string]ovfDisk{}
	var diskOrder []string
	for _, d := range env.Disks {
		capB, _ := strconv.ParseInt(strings.TrimSpace(d.Capacity), 10, 64)
		diskByID[d.ID] = ovfDisk{File: files[d.FileRef], CapacityBytes: ovfBytes(capB, d.Units, 1)}
		diskOrder = append(diskOrder, d.ID)
	}
	used := map[string]bool{}
	for _, it := range vs.HW.Items {
		qty, _ := strconv.ParseInt(strings.TrimSpace(it.Quantity), 10, 64)
		switch strings.TrimSpace(it.ResourceType) {
		case "3":
			if qty > 0 && qty <= 1024 {
				info.VCPUs = int(qty)
			}
		case "4":
			if b := ovfBytes(qty, it.Units, 1<<20); b >= 64<<20 && b <= 64<<40 {
				info.MemoryMiB = int(b >> 20)
			}
		case "17":
			hr := strings.TrimSpace(it.HostResource)
			id := hr[strings.LastIndex(hr, "/")+1:]
			if d, ok := diskByID[id]; ok && d.File != "" && !used[id] {
				used[id] = true
				info.Disks = append(info.Disks, d)
			}
		}
	}
	if len(info.Disks) == 0 { // no hardware items pointing at disks: take the disk section in order
		for _, id := range diskOrder {
			if d := diskByID[id]; d.File != "" {
				info.Disks = append(info.Disks, d)
			}
		}
	}
	if len(info.Disks) == 0 {
		return ovfInfo{}, badRequest("the .ovf descriptor lists no disks")
	}
	for _, kv := range append(append([]ovfKV{}, vs.HW.Config...), vs.HW.Extra...) {
		switch strings.ToLower(kv.Key) {
		case "firmware":
			info.UEFI = info.UEFI || strings.EqualFold(kv.Value, "efi")
		case "uefi.securebot.enabled", "uefi.secureboot.enabled", "bootoptions.efisecurebootenabled":
			info.SecureBoot = info.SecureBoot || strings.EqualFold(kv.Value, "true")
		}
	}
	if info.SecureBoot {
		info.UEFI = true
	}
	hint := strings.ToLower(vs.OS.OSType + " " + vs.OS.Desc)
	info.Windows = strings.Contains(hint, "windows")
	return info, nil
}

// proposal is the create-VM form an OVA suggests; the first disk becomes the
// VM's disk, the volume is used as it is.
func (o ovfInfo) proposal(name, pool string, firstVol string) createVMRequest {
	q := createVMRequest{
		Name: name, VCPUs: o.VCPUs, MemoryMiB: o.MemoryMiB, Pool: pool, ExistingVolume: firstVol,
		Firmware: "bios", OSType: "linux",
	}
	if o.UEFI {
		q.Firmware, q.SecureBoot = "uefi", o.SecureBoot
	}
	if o.Windows {
		q.OSType = "windows"
	}
	return q
}

// --- conversion with progress ---

var progressRE = regexp.MustCompile(`\((\d+(?:\.\d+)?)/100%\)`)

// parseProgress finds the newest "(12.34/100%)" in qemu-img's -p output.
func parseProgress(log string) (float64, bool) {
	all := progressRE.FindAllStringSubmatch(log, -1)
	if len(all) == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(all[len(all)-1][1], 64)
	return f / 100, err == nil
}

// workDir is a private scratch directory inside the destination pool
// directory, so the final rename never crosses a file system.
type workDir struct {
	h    *hostconn.Host
	path string
	// stop, when set, is polled by convert; once it returns true convert gives
	// up (the caller has killed qemu-img and removed the directory).
	stop func() bool
}

func newWorkDir(h *hostconn.Host, poolDir string) (*workDir, error) {
	out, err := h.RunRoot("umask 077 && mktemp -d -- " + shQuote(poolDir+"/.virticle-import.XXXXXX"))
	if err != nil || !strings.HasPrefix(out, poolDir+"/.virticle-import.") || hasControl(out) {
		return nil, fmt.Errorf("cannot create a work directory in %s: %s", poolDir, impFirstLine(out))
	}
	return &workDir{h: h, path: out}, nil
}

func (w *workDir) file(n string) string { return w.path + "/" + n }

// remove kills a conversion still running from this directory (by the PID it
// recorded, after checking the process really is qemu-img) and deletes the
// directory.
func (w *workDir) remove() error {
	_, err := w.h.RunRoot("p=$(cat " + shQuote(w.file("qpid")) + " 2>/dev/null); if [ -n \"$p\" ] && [ \"$(cat /proc/$p/comm 2>/dev/null)\" = qemu-img ]; then kill $p; sleep 1; fi; rm -rf -- " + shQuote(w.path))
	return err
}

// convert runs qemu-img convert in the background on the node, polls its
// progress file and renames the result to dest when it succeeded. The
// conversion is detached from the SSH session, so no long-lived channel is
// needed; its PID and exit code are files in the work directory.
func (w *workDir) convert(src, srcFmt, dest string, progress func(float64)) error {
	for _, f := range []string{"rc", "qpid", "prog.log", "out.part"} {
		w.h.RunRoot("rm -f -- " + shQuote(w.file(f)))
	}
	inner := fmt.Sprintf("qemu-img convert -p -U -f %s -O qcow2 -- %s %s >%s 2>&1 & p=$!; echo $p >%s; wait $p; echo $? >%s",
		shQuote(srcFmt), shQuote(src), shQuote(w.file("out.part")), shQuote(w.file("prog.log")), shQuote(w.file("qpid")), shQuote(w.file("rc")))
	if out, err := w.h.RunRoot("umask 077; nohup sh -c " + shQuote(inner) + " >/dev/null 2>&1 </dev/null & echo started"); err != nil || out != "started" {
		return fmt.Errorf("cannot start qemu-img: %s %v", out, err)
	}
	deadline := time.Now().Add(6 * time.Hour)
	misses := 0
	for {
		time.Sleep(2 * time.Second)
		if w.stop != nil && w.stop() {
			return errors.New("conversion cancelled")
		}
		out, err := w.h.RunRoot("cat -- " + shQuote(w.file("rc")) + " 2>/dev/null; echo ---; tail -c 400 -- " + shQuote(w.file("prog.log")) + " 2>/dev/null | tr '\\r' '\\n'")
		if err != nil {
			if misses++; misses >= 15 {
				return fmt.Errorf("lost contact with the node; qemu-img may still be running (work directory %s, nothing was deleted)", w.path)
			}
			continue
		}
		misses = 0
		rc, log, _ := strings.Cut(out, "---")
		if p, ok := parseProgress(log); ok {
			progress(p)
		}
		if rc = strings.TrimSpace(rc); rc != "" {
			if rc != "0" {
				return fmt.Errorf("qemu-img convert failed (exit %s): %s", rc, impLastLine(log))
			}
			break
		}
		if time.Now().After(deadline) {
			return errors.New("conversion did not finish within 6 hours; it was stopped")
		}
	}
	// A second writer with this name would have been caught earlier; mv -n
	// plus the existence test keeps a race from overwriting anything.
	out, err := w.h.RunRoot("if [ -e " + shQuote(dest) + " ]; then echo 'a file with this name appeared in the pool directory'; exit 1; fi; mv -n -- " + shQuote(w.file("out.part")) + " " + shQuote(dest))
	if err != nil {
		return fmt.Errorf("cannot move the result into the pool: %s", out)
	}
	return nil
}

func impLastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// --- endpoints ---

func (s *Server) importInspect(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req importRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	file, err := resolveSource(h, req.Source)
	if err != nil {
		fail(w, err)
		return
	}
	info, err := qemuInfo(h, file)
	if err != nil {
		fail(w, err)
		return
	}
	resp := map[string]any{"kind": "disk", "file": file, "format": info.Format, "virtualSize": info.VirtualSize, "actualSize": info.ActualSize}
	if info.Format == "raw" {
		if es, ok, err := listTar(h, file); err != nil {
			fail(w, err)
			return
		} else if ok {
			if _, err := ovfMember(es); err == nil {
				resp["kind"], resp["members"] = "ova", es
			}
		}
	}
	if ext := strings.ToLower(path.Ext(file)); resp["kind"] == "disk" && ext != "" && ext != "."+info.Format && (ext == ".qcow2" || ext == ".vmdk" || ext == ".vdi" || ext == ".vhdx" || ext == ".raw" || ext == ".img") {
		resp["warning"] = "the file extension says " + ext + " but the content is " + info.Format + "; the content is what counts"
	}
	writeJSON(w, resp)
}

// importDisk converts the source into the destination pool, as one task.
func (s *Server) importDisk(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req importRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Name == "" {
		fail(w, badRequest("a name is required"))
		return
	}
	pool, pd, err := destPool(h, req.Pool)
	if err != nil {
		fail(w, err)
		return
	}
	file, err := resolveSource(h, req.Source)
	if err != nil {
		fail(w, err)
		return
	}
	info, err := qemuInfo(h, file)
	if err != nil {
		fail(w, err)
		return
	}
	var entries []tarEntry
	ovfName := ""
	if info.Format == "raw" {
		es, ok, err := listTar(h, file)
		if err == nil && ok {
			entries = es
			ovfName, err = ovfMember(es)
		}
		if err != nil {
			fail(w, err)
			return
		}
	}
	free, err := freeBytes(h, pd.Dir)
	if err != nil {
		fail(w, err)
		return
	}
	need := info.ActualSize + importFreeMargin
	if ovfName != "" {
		need = importFreeMargin
		for _, e := range entries {
			need += e.Size
		}
	}
	if free < need {
		fail(w, badRequest(fmt.Sprintf("not enough free space in %s: %d MiB available, about %d MiB needed", pd.Dir, free>>20, need>>20)))
		return
	}

	target := req.Pool + "/" + req.Name
	var result map[string]any
	if ovfName == "" {
		name := req.Name
		if !strings.HasSuffix(name, ".qcow2") {
			name += ".qcow2"
		}
		if !volNameRE.MatchString(name) {
			fail(w, badRequest("volume name must start with a letter or digit and contain only letters, digits, dot, dash, plus and underscore"))
			return
		}
		if _, err := h.StorageVolLookupByName(pool, name); err == nil {
			fail(w, badRequest("a volume named "+name+" already exists in "+req.Pool))
			return
		}
		err = s.task(r, "import", target, h.Name, func(progress func(float64)) error {
			wd, err := newWorkDir(h, pd.Dir)
			if err != nil {
				return err
			}
			defer wd.remove()
			if err := wd.convert(file, info.Format, pd.Dir+"/"+name, progress); err != nil {
				return err
			}
			return h.StoragePoolRefresh(pool, 0)
		})
		result = map[string]any{"kind": "disk", "pool": req.Pool, "volume": name, "sourceFormat": info.Format}
	} else {
		result, err = s.importOVA(r, h, pool, pd, file, ovfName, entries, req)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) importOVA(r *http.Request, h *hostconn.Host, pool libvirt.StoragePool, pd poolDir, file, ovfName string, entries []tarEntry, req importRequest) (map[string]any, error) {
	if !vmNameRE.MatchString(req.Name) {
		return nil, badRequest("for an OVA the name is the VM name: letters, digits, dot, dash and underscore")
	}
	if _, err := h.DomainLookupByName(req.Name); err == nil {
		return nil, badRequest("a VM named " + req.Name + " already exists on this node")
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		sizes[e.Name] = e.Size
	}
	var result map[string]any
	err := s.task(r, "import OVA", req.Pool+"/"+req.Name, h.Name, func(progress func(float64)) error {
		wd, err := newWorkDir(h, pd.Dir)
		if err != nil {
			return err
		}
		defer wd.remove()
		// Members are extracted by their vetted plain names only, never by pattern.
		untar := func(names ...string) error {
			quoted := make([]string, len(names))
			for i, n := range names {
				quoted[i] = shQuote(n)
			}
			out, err := h.RunRoot("tar --no-same-owner --no-same-permissions -xf " + shQuote(file) + " -C " + shQuote(wd.path) + " -- " + strings.Join(quoted, " ") + " 2>&1")
			if err != nil {
				return fmt.Errorf("extracting the archive: %s", impFirstLine(out))
			}
			return nil
		}
		if err := untar(ovfName); err != nil {
			return err
		}
		raw, err := h.RunRoot("head -c " + strconv.Itoa(importMaxOVFBytes) + " -- " + shQuote(wd.file(ovfName)))
		if err != nil {
			return err
		}
		ovf, err := parseOVF([]byte(raw))
		if err != nil {
			return err
		}
		var members []string
		for _, d := range ovf.Disks {
			if _, ok := sizes[d.File]; !ok {
				return badRequest("the descriptor refers to " + quoteTag(d.File) + ", which is not in the archive (or has no plain name)")
			}
			members = append(members, d.File)
		}
		var vols []string
		for i := range ovf.Disks {
			v := fmt.Sprintf("%s-disk%d.qcow2", req.Name, i+1)
			if _, err := h.StorageVolLookupByName(pool, v); err == nil {
				return badRequest("a volume named " + v + " already exists in " + req.Pool)
			}
			vols = append(vols, v)
		}
		if err := untar(members...); err != nil {
			return err
		}
		progress(0.05)
		created := []string{}
		undo := func() {
			for _, v := range created {
				h.RunRoot("rm -f -- " + shQuote(pd.Dir+"/"+v))
			}
		}
		for i, d := range ovf.Disks {
			local := wd.file(d.File)
			info, err := qemuInfo(h, local)
			if err != nil {
				undo()
				return fmt.Errorf("disk %s: %w", d.File, err)
			}
			base, span := 0.05+0.95*float64(i)/float64(len(ovf.Disks)), 0.95/float64(len(ovf.Disks))
			if err := wd.convert(local, info.Format, pd.Dir+"/"+vols[i], func(p float64) { progress(base + span*p) }); err != nil {
				undo()
				return fmt.Errorf("disk %s: %w", d.File, err)
			}
			created = append(created, vols[i])
			h.RunRoot("rm -f -- " + shQuote(local)) // free the space before the next disk
		}
		if err := h.StoragePoolRefresh(pool, 0); err != nil {
			return err
		}
		warnings := []string{}
		if len(vols) > 1 {
			warnings = append(warnings, fmt.Sprintf("the OVA has %d disks; the first becomes the VM's disk, attach the others from the Hardware tab", len(vols)))
		}
		if ovf.Windows {
			warnings = append(warnings, "Windows guest: disk and NIC default to SATA and e1000e, which Windows boots without extra drivers")
		}
		result = map[string]any{
			"kind": "ova", "pool": req.Pool, "volumes": vols, "ovf": ovf, "warnings": warnings,
			"proposal": ovf.proposal(req.Name, req.Pool, vols[0]),
		}
		return nil
	})
	return result, err
}

// existingDisk looks up the volume a VM is to use as its disk as it is, and
// checks that it is a plain qcow2/raw file that no other VM uses.
func existingDisk(h *hostconn.Host, pool libvirt.StoragePool, q *createVMRequest) (libvirt.StorageVol, error) {
	vol, err := h.StorageVolLookupByName(pool, q.ExistingVolume)
	if err != nil {
		return vol, err
	}
	raw, err := h.StorageVolGetXMLDesc(vol, 0)
	if err != nil {
		return vol, err
	}
	var vx struct {
		Type   string `xml:"type,attr"`
		Target struct {
			Path   string `xml:"path"`
			Format struct {
				Type string `xml:"type,attr"`
			} `xml:"format"`
		} `xml:"target"`
		Backing struct {
			Path string `xml:"path"`
		} `xml:"backingStore"`
	}
	if err := xml.Unmarshal([]byte(raw), &vx); err != nil {
		return vol, err
	}
	switch {
	case vx.Type != "file":
		return vol, badRequest("only file volumes can be used as a VM disk")
	case vx.Target.Format.Type != "qcow2" && vx.Target.Format.Type != "raw":
		return vol, badRequest("volume " + q.ExistingVolume + " is " + quoteTag(vx.Target.Format.Type) + ", not qcow2 or raw: import it first")
	case vx.Backing.Path != "":
		return vol, badRequest("volume " + q.ExistingVolume + " has a backing file; import (flatten) it first")
	}
	q.diskFormat = vx.Target.Format.Type
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return vol, err
	}
	for _, d := range doms {
		dx, err := h.DomainGetXMLDesc(d, libvirt.DomainXMLInactive)
		if err != nil {
			continue
		}
		var x domainXML
		if xml.Unmarshal([]byte(dx), &x) != nil {
			continue
		}
		for _, p := range diskPaths(&x) {
			if path.Clean(p) == path.Clean(vx.Target.Path) {
				return vol, badRequest("volume " + q.ExistingVolume + " is already the disk of VM " + d.Name)
			}
		}
	}
	return vol, nil
}
