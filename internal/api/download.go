package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var volNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}$`)

// shQuote quotes s for a POSIX shell as one single-quoted word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func validDownloadURL(raw string) error {
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return badRequest("the URL must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return badRequest("only absolute http:// and https:// URLs are supported")
	}
	return nil
}

// downloadVolume has the node itself fetch a URL into a directory-backed
// pool: curl writes "<name>.part", which is renamed when complete and then
// picked up by a pool refresh. The image never passes through this server.
func (s *Server) downloadVolume(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	switch {
	case !volNameRE.MatchString(req.Name):
		err = badRequest("volume name must start with a letter or digit and contain only letters, digits, dot, dash, plus and underscore")
	default:
		err = validDownloadURL(req.URL)
	}
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := h.StoragePoolGetXMLDesc(pool, 0)
	if err != nil {
		fail(w, err)
		return
	}
	var px struct {
		Type   string `xml:"type,attr"`
		Target struct {
			Path string `xml:"path"`
		} `xml:"target"`
	}
	if err := xml.Unmarshal([]byte(raw), &px); err != nil {
		fail(w, err)
		return
	}
	if px.Type != "dir" && px.Type != "netfs" && px.Type != "fs" || !strings.HasPrefix(px.Target.Path, "/") {
		fail(w, badRequest("downloads need a pool backed by a directory (dir, netfs or fs), not a "+px.Type+" pool"))
		return
	}
	if state, _, _, _, err := h.StoragePoolGetInfo(pool); err != nil || state != 2 {
		fail(w, badRequest("the pool is not running"))
		return
	}
	if _, err := h.StorageVolLookupByName(pool, req.Name); err == nil {
		fail(w, badRequest("a volume named "+req.Name+" already exists in this pool"))
		return
	}
	dest := strings.TrimRight(px.Target.Path, "/") + "/" + req.Name
	part := dest + ".part"

	err = s.task(r, "download", pool.Name+"/"+req.Name, h.Name, func(progress func(float64)) error {
		// Best effort: the size, for the progress bar.
		var total int64
		if out, err := h.Run("curl -q -sIL --max-time 20 --proto '=http,https' --proto-redir '=http,https' -- " + shQuote(req.URL)); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
					if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
						total = n // the last response wins, after redirects
					}
				}
			}
		}
		done := make(chan struct{})
		defer close(done)
		if total > 0 {
			go func() {
				tick := time.NewTicker(2 * time.Second)
				defer tick.Stop()
				for {
					select {
					case <-done:
						return
					case <-tick.C:
						if out, err := h.Run("stat -c %s -- " + shQuote(part)); err == nil {
							if n, err := strconv.ParseInt(out, 10, 64); err == nil {
								progress(float64(n) / float64(total))
							}
						}
					}
				}
			}()
		}
		// -f: an HTTP error status is a failure, not a saved error page.
		cmd := fmt.Sprintf("umask 077; if [ -e %[3]s ]; then echo 'a file with this name already exists in the pool directory'; exit 1; fi; if curl -q -fsSL --proto '=http,https' --proto-redir '=http,https' --max-redirs 10 --connect-timeout 20 -o %[1]s -- %[2]s && mv -n -- %[1]s %[3]s; then :; else rc=$?; rm -f -- %[1]s; exit $rc; fi",
			shQuote(part), shQuote(req.URL), shQuote(dest))
		if out, err := h.Run(cmd); err != nil {
			if out != "" {
				return fmt.Errorf("download failed: %s", out)
			}
			return fmt.Errorf("download failed: %w", err)
		}
		return h.StoragePoolRefresh(pool, 0)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
