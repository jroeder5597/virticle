package api

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

var poolStates = []string{"inactive", "building", "running", "degraded", "inaccessible"}

type poolInfo struct {
	Name       string `json:"name"`
	UUID       string `json:"uuid"`
	Type       string `json:"type"`
	Target     string `json:"target,omitempty"`
	State      string `json:"state"`
	Autostart  bool   `json:"autostart"`
	Capacity   uint64 `json:"capacity"`
	Allocation uint64 `json:"allocation"`
	Available  uint64 `json:"available"`
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := collectPools(h)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, out)
}

// collectPools lists the storage pools of one node, sorted by name.
func collectPools(h *hostconn.Host) ([]poolInfo, error) {
	pools, _, err := h.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil, err
	}
	out := []poolInfo{}
	for _, p := range pools {
		state, capacity, alloc, avail, err := h.StoragePoolGetInfo(p)
		if err != nil {
			continue
		}
		info := poolInfo{Name: p.Name, UUID: cluster.FormatUUID(p.UUID), State: "unknown", Capacity: capacity, Allocation: alloc, Available: avail}
		if int(state) < len(poolStates) {
			info.State = poolStates[state]
		}
		if a, err := h.StoragePoolGetAutostart(p); err == nil {
			info.Autostart = a != 0
		}
		if raw, err := h.StoragePoolGetXMLDesc(p, 0); err == nil {
			var x struct {
				Type   string `xml:"type,attr"`
				Target struct {
					Path string `xml:"path"`
				} `xml:"target"`
			}
			if xml.Unmarshal([]byte(raw), &x) == nil {
				info.Type, info.Target = x.Type, x.Target.Path
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type poolRequest struct {
	Name string `json:"name"`
	Type string `json:"type"` // dir | netfs | logical | iscsi
	// Target is the directory (dir), the local mount point (netfs); unused otherwise.
	Target string `json:"target"`
	// SourceHost and SourcePath: NFS server and export, or iSCSI portal and IQN.
	SourceHost string `json:"sourceHost"`
	SourcePath string `json:"sourcePath"`
	// SourceName is the volume group name (logical).
	SourceName string `json:"sourceName"`
	// XML, when set, is used verbatim instead of the fields above.
	XML string `json:"xml"`
}

func (p poolRequest) xml() (string, error) {
	if p.XML != "" {
		return p.XML, nil
	}
	if p.Name == "" {
		return "", badRequest("name is required")
	}
	need := func(v, what string) error {
		if v == "" {
			return badRequest(what + " is required for a " + p.Type + " pool")
		}
		return nil
	}
	switch p.Type {
	case "dir":
		if err := need(p.Target, "a directory"); err != nil {
			return "", err
		}
		return fmt.Sprintf(`<pool type='dir'><name>%s</name><target><path>%s</path></target></pool>`, esc(p.Name), esc(p.Target)), nil
	case "netfs":
		for _, c := range [][2]string{{p.SourceHost, "a server"}, {p.SourcePath, "an export path"}, {p.Target, "a mount point"}} {
			if err := need(c[0], c[1]); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf(`<pool type='netfs'><name>%s</name><source><host name='%s'/><dir path='%s'/><format type='auto'/></source><target><path>%s</path></target></pool>`,
			esc(p.Name), esc(p.SourceHost), esc(p.SourcePath), esc(p.Target)), nil
	case "logical":
		if err := need(p.SourceName, "a volume group name"); err != nil {
			return "", err
		}
		return fmt.Sprintf(`<pool type='logical'><name>%s</name><source><name>%s</name></source><target><path>/dev/%s</path></target></pool>`,
			esc(p.Name), esc(p.SourceName), esc(p.SourceName)), nil
	case "iscsi":
		for _, c := range [][2]string{{p.SourceHost, "a portal address"}, {p.SourcePath, "a target IQN"}} {
			if err := need(c[0], c[1]); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf(`<pool type='iscsi'><name>%s</name><source><host name='%s'/><device path='%s'/></source><target><path>/dev/disk/by-path</path></target></pool>`,
			esc(p.Name), esc(p.SourceHost), esc(p.SourcePath)), nil
	}
	return "", badRequest("unsupported pool type " + p.Type + "; use the XML form for other types")
}

// createPool defines, builds, starts and autostarts a pool. If starting
// fails the definition is removed again so no half-made pool is left behind.
func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req poolRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	doc, err := req.xml()
	if err != nil {
		fail(w, err)
		return
	}
	var name string
	err = s.task(r, "create pool", req.Name, h.Name, func(func(float64)) error {
		pool, err := h.StoragePoolDefineXML(doc, 0)
		if err != nil {
			return err
		}
		name = pool.Name
		// Build creates the directory or mount point; pools that already
		// exist (a volume group, an iSCSI target) report an error we can skip.
		h.StoragePoolBuild(pool, libvirt.StoragePoolBuildNew)
		if err := h.StoragePoolCreate(pool, 0); err != nil {
			h.StoragePoolUndefine(pool)
			return err
		}
		return h.StoragePoolSetAutostart(pool, 1)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"name": name})
}

func (s *Server) pool(r *http.Request) (*hostconn.Host, libvirt.StoragePool, error) {
	_, h, err := s.host(r)
	if err != nil {
		return nil, libvirt.StoragePool{}, err
	}
	p, err := h.StoragePoolLookupByName(r.PathValue("pool"))
	return h, p, err
}

func (s *Server) poolAction(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
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
		"start":     func() error { return h.StoragePoolCreate(pool, 0) },
		"stop":      func() error { return h.StoragePoolDestroy(pool) },
		"refresh":   func() error { return h.StoragePoolRefresh(pool, 0) },
		"autostart": func() error { return h.StoragePoolSetAutostart(pool, 1) },
		"manual":    func() error { return h.StoragePoolSetAutostart(pool, 0) },
		// "remove" forgets the pool; the data in it is left untouched.
		"remove": func() error {
			if state, _, _, _, err := h.StoragePoolGetInfo(pool); err == nil && state != 0 {
				if err := h.StoragePoolDestroy(pool); err != nil {
					return err
				}
			}
			return h.StoragePoolUndefine(pool)
		},
	}
	fn, ok := actions[req.Action]
	if !ok {
		fail(w, badRequest("unknown action "+req.Action))
		return
	}
	if err := s.task(r, req.Action+" pool", pool.Name, h.Name, func(func(float64)) error { return fn() }); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type volumeInfo struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Format     string `json:"format,omitempty"`
	Capacity   uint64 `json:"capacity"`
	Allocation uint64 `json:"allocation"`
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	// Pick up files created outside libvirt; a failure here (pool busy with
	// another job) is not a reason to refuse the listing.
	h.StoragePoolRefresh(pool, 0)
	vols, _, err := h.StoragePoolListAllVolumes(pool, 1, 0)
	if err != nil {
		fail(w, err)
		return
	}
	out := []volumeInfo{}
	for _, v := range vols {
		_, capacity, alloc, err := h.StorageVolGetInfo(v)
		if err != nil {
			continue
		}
		info := volumeInfo{Name: v.Name, Path: v.Key, Capacity: capacity, Allocation: alloc}
		if raw, err := h.StorageVolGetXMLDesc(v, 0); err == nil {
			var x struct {
				Target struct {
					Path   string `xml:"path"`
					Format struct {
						Type string `xml:"type,attr"`
					} `xml:"format"`
				} `xml:"target"`
			}
			if xml.Unmarshal([]byte(raw), &x) == nil {
				info.Format = x.Target.Format.Type
				if x.Target.Path != "" {
					info.Path = x.Target.Path
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

func volumeXML(name, format string, capacity uint64) string {
	f := ""
	if format != "" {
		f = fmt.Sprintf("<target><format type='%s'/></target>", esc(format))
	}
	return fmt.Sprintf(`<volume><name>%s</name><capacity unit='bytes'>%d</capacity>%s</volume>`, esc(name), capacity, f)
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Format   string `json:"format"`
		Capacity uint64 `json:"capacity"` // bytes
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Name == "" || req.Capacity == 0 {
		fail(w, badRequest("name and capacity are required"))
		return
	}
	err = s.task(r, "create volume", pool.Name+"/"+req.Name, h.Name, func(func(float64)) error {
		_, err := h.StorageVolCreateXML(pool, volumeXML(req.Name, req.Format, req.Capacity), 0)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type countingReader struct {
	r     io.Reader
	n     int64
	total int64
	tick  func(float64)
}

// Read fills up to 1 MiB per call. Each Read becomes one libvirt stream
// packet, and an HTTP body otherwise arrives in small pieces that make the
// upload many times slower.
func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) > 1<<20 {
		p = p[:1<<20]
	}
	n, err := io.ReadFull(c.r, p)
	if err == io.ErrUnexpectedEOF {
		err = nil // short final chunk; the next call reports EOF
	}
	c.n += int64(n)
	if c.total > 0 {
		c.tick(float64(c.n) / float64(c.total))
	}
	return n, err
}

// uploadVolume streams the request body into a new volume through libvirt's
// storage stream, so it needs nothing on the node beyond libvirt itself.
func (s *Server) uploadVolume(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	name := r.PathValue("vol")
	if r.ContentLength <= 0 {
		fail(w, badRequest("a Content-Length header is required"))
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "raw"
		if strings.HasSuffix(strings.ToLower(name), ".qcow2") {
			format = "qcow2"
		}
	}
	err = s.task(r, "upload", pool.Name+"/"+name, h.Name, func(progress func(float64)) error {
		vol, err := h.StorageVolCreateXML(pool, volumeXML(name, format, uint64(r.ContentLength)), 0)
		if err != nil {
			return err
		}
		body := &countingReader{r: r.Body, total: r.ContentLength, tick: progress}
		if err := h.StorageVolUpload(vol, body, 0, 0, 0); err != nil {
			h.StorageVolDelete(vol, 0)
			return err
		}
		if body.n != r.ContentLength {
			h.StorageVolDelete(vol, 0)
			return fmt.Errorf("upload was cut short: got %d of %d bytes", body.n, r.ContentLength)
		}
		return h.StoragePoolRefresh(pool, 0)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) volume(r *http.Request) (*hostconn.Host, libvirt.StoragePool, libvirt.StorageVol, error) {
	h, pool, err := s.pool(r)
	if err != nil {
		return nil, pool, libvirt.StorageVol{}, err
	}
	v, err := h.StorageVolLookupByName(pool, r.PathValue("vol"))
	return h, pool, v, err
}

func (s *Server) volumeAction(w http.ResponseWriter, r *http.Request) {
	h, pool, vol, err := s.volume(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Action   string `json:"action"`   // resize | clone
		Capacity uint64 `json:"capacity"` // resize: new size in bytes
		Name     string `json:"name"`     // clone: new volume name
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	var fn func() error
	switch req.Action {
	case "resize":
		if req.Capacity == 0 {
			fail(w, badRequest("capacity is required"))
			return
		}
		fn = func() error { return h.StorageVolResize(vol, req.Capacity, 0) }
	case "clone":
		if req.Name == "" {
			fail(w, badRequest("name is required"))
			return
		}
		fn = func() error {
			raw, err := h.StorageVolGetXMLDesc(vol, 0)
			if err != nil {
				return err
			}
			var x struct {
				Capacity uint64 `xml:"capacity"`
				Target   struct {
					Format struct {
						Type string `xml:"type,attr"`
					} `xml:"format"`
				} `xml:"target"`
			}
			if err := xml.Unmarshal([]byte(raw), &x); err != nil {
				return err
			}
			_, err = h.StorageVolCreateXMLFrom(pool, volumeXML(req.Name, x.Target.Format.Type, x.Capacity), vol, 0)
			return err
		}
	default:
		fail(w, badRequest("unknown action "+req.Action))
		return
	}
	if err := s.task(r, req.Action+" volume", pool.Name+"/"+vol.Name, h.Name, func(func(float64)) error { return fn() }); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) {
	h, pool, vol, err := s.volume(r)
	if err != nil {
		fail(w, err)
		return
	}
	err = s.task(r, "delete volume", pool.Name+"/"+vol.Name, h.Name, func(func(float64)) error {
		return h.StorageVolDelete(vol, 0)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
