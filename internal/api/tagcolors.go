package api

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"virt-proj/internal/cluster"
)

// A tag's colour belongs to its name and so applies on every VM that carries it. The names stay in libvirt
// domain metadata (tags.go); the colours are controller settings in SQLite. A colour whose tag no VM carries
// any more is left in place (harmless, and it returns if the tag is reused); it is not listed.

var colorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type tagInfo struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	Count int    `json:"count"`
}

// tagList merges the tags found on VMs with the stored colours, sorted by name.
func tagList(vms []vmSummary, colors map[string]string) []tagInfo {
	count := map[string]int{}
	for _, v := range vms {
		for _, t := range v.Tags {
			count[t]++
		}
	}
	out := make([]tagInfo, 0, len(count))
	for n, c := range count {
		out = append(out, tagInfo{Name: n, Color: colors[n], Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	colors, err := s.Store.TagColors()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, tagList(s.collectVMs(), colors))
}

func (s *Server) putTagColor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !tagRE.MatchString(name) {
		fail(w, badRequest("invalid tag name"))
		return
	}
	var req struct {
		Color string `json:"color"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if !colorRE.MatchString(req.Color) {
		fail(w, badRequest(`colour must look like "#1a2b3c" (six hex digits)`))
		return
	}
	c := strings.ToLower(req.Color)
	if err := s.Store.SetTagColor(name, c); err != nil {
		fail(w, err)
		return
	}
	s.Nodes.Publish(cluster.Event{Type: "tags", Name: name})
	writeJSON(w, map[string]any{"ok": true, "name": name, "color": c})
}

func (s *Server) deleteTagColor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !tagRE.MatchString(name) {
		fail(w, badRequest("invalid tag name"))
		return
	}
	if err := s.Store.DeleteTagColor(name); err != nil {
		fail(w, err)
		return
	}
	s.Nodes.Publish(cluster.Event{Type: "tags", Name: name})
	writeJSON(w, map[string]any{"ok": true})
}
