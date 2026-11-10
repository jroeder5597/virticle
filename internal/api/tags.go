package api

import (
	"encoding/xml"
	"net/http"
	"regexp"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/hostconn"
)

// Tags are short free-form labels kept as <tags><tag>x</tag>...</tags> in the
// application's shared metadata root, next to the template marker and the
// snapshot schedule.

const maxTags = 16

var tagRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:-]{0,30}$`)

// cleanTags trims, validates and de-duplicates (case-sensitively) a tag list.
func cleanTags(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if !tagRE.MatchString(t) {
			return nil, badRequest("invalid tag " + quoteTag(t) + ": 1-31 characters, starting with a letter or digit; letters, digits, space and . _ : - are allowed")
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) > maxTags {
		return nil, badRequest("at most 16 tags per VM")
	}
	return out, nil
}

func quoteTag(t string) string {
	if len(t) > 40 {
		t = t[:40] + "..."
	}
	return `"` + t + `"`
}

func tagsElem(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<tags>")
	for _, t := range tags {
		b.WriteString("<tag>" + esc(t) + "</tag>")
	}
	b.WriteString("</tags>")
	return b.String()
}

// parseAppMeta reads the template marker and the tags from the metadata root.
func parseAppMeta(raw string) (templateMark, []string) {
	tags := []string{}
	if raw == "" {
		return templateMark{}, tags
	}
	var root struct {
		Template *struct {
			Sealed string `xml:"sealed,attr"`
		} `xml:"template"`
		Tags []string `xml:"tags>tag"`
	}
	if xml.Unmarshal([]byte(raw), &root) != nil {
		return templateMark{}, tags
	}
	var m templateMark
	if root.Template != nil {
		m = templateMark{Template: true, Sealed: root.Template.Sealed}
	}
	for _, t := range root.Tags {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return m, tags
}

// getAppMeta is getMark plus the tags, from a single metadata fetch: the VM
// list calls it once per domain.
func getAppMeta(h *hostconn.Host, dom libvirt.Domain) (templateMark, []string, error) {
	raw, err := domainMeta(h, dom)
	if err != nil {
		return templateMark{}, nil, err
	}
	m, tags := parseAppMeta(raw)
	return m, tags, nil
}

func setTags(h *hostconn.Host, dom libvirt.Domain, tags []string) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	meta, err := metaSetChild(old, "tags", tagsElem(tags))
	if err != nil {
		return err
	}
	return setDomainMeta(h, dom, meta)
}

func (s *Server) vmTags(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Tags []string `json:"tags"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	tags, err := cleanTags(req.Tags)
	if err != nil {
		fail(w, err)
		return
	}
	err = s.task(r, "set tags", dom.Name, h.Name, func(func(float64)) error { return setTags(h, dom, tags) })
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "tags": tags})
}
