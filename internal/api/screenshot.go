package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Screenshots show whatever is on the guest's screen, so they need the same
// role as the console. They are deliberately NOT in auditedGets: a preview
// refreshes every few seconds and would drown the audit log. Opening the
// interactive console (vnc, serial) is still recorded.

const (
	shotReuse   = 2 * time.Second  // a frame this young is served again
	shotWait    = 4 * time.Second  // how long a second request waits for the first
	shotKeep    = 30 * time.Second // idle frames are dropped after this
	shotMaxSize = 64 << 20
)

// shotSlot serialises captures of one VM and remembers its last frame.
type shotSlot struct {
	sem  chan struct{} // capacity 1: at most one capture in flight
	img  image.Image   // guarded by sem
	at   time.Time     // guarded by sem
	used time.Time     // guarded by shotCache.mu
}

type shotCache struct {
	mu    sync.Mutex
	slots map[string]*shotSlot
}

func (c *shotCache) slot(key string) *shotSlot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, s := range c.slots {
		if len(s.sem) == 0 && now.Sub(s.used) > shotKeep {
			delete(c.slots, k)
		}
	}
	if c.slots == nil {
		c.slots = map[string]*shotSlot{}
	}
	s := c.slots[key]
	if s == nil {
		s = &shotSlot{sem: make(chan struct{}, 1)}
		c.slots[key] = s
	}
	s.used = now
	return s
}

// screenshotCmd is the node-side command: virsh writes the image to a
// temporary file, which is printed as one line of base64 and removed.
// The URI is explicit: for a node user other than root, plain virsh would talk
// to that user's own session and not find the VM.
func screenshotCmd(uuid string) string {
	return "f=$(mktemp) && timeout 20 virsh -c qemu:///system screenshot " + shellQuote(uuid) + ` "$f" >/dev/null; rc=$?; ` +
		`[ $rc -eq 0 ] && base64 -w0 "$f"; rm -f "$f"; exit $rc`
}

// vmScreenshot returns the VM's current screen as PNG (default) or JPEG,
// optionally shrunk to ?w= pixels wide.
func (s *Server) vmScreenshot(w http.ResponseWriter, r *http.Request) {
	width := 0
	if v := r.URL.Query().Get("w"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 16 || n > 8192 {
			fail(w, badRequest("w must be between 16 and 8192"))
			return
		}
		width = n
	}
	format := r.URL.Query().Get("fmt")
	if format != "" && format != "png" && format != "jpeg" {
		fail(w, badRequest("fmt must be png or jpeg"))
		return
	}
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
	if st := stateName(state); st != "running" && st != "paused" {
		conflict(w, "the VM is not running, so it has no screen")
		return
	}
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
	if len(x.Devices.Graphics) == 0 {
		conflict(w, "this VM has no graphics device, so there is nothing to take a screenshot of")
		return
	}

	slot := s.shots.slot(r.PathValue("node") + "/" + r.PathValue("uuid"))
	select {
	case slot.sem <- struct{}{}:
		defer func() { <-slot.sem }()
		if slot.img == nil || time.Since(slot.at) > shotReuse {
			// Not DomainScreenshot: go-libvirt's stream reader can return early
			// on an empty packet and then deadlock the node's whole libvirt
			// connection when the rest of the stream arrives (seen when the
			// guest resets mid-capture). virsh over SSH cannot wedge it.
			out, err := h.Run(screenshotCmd(r.PathValue("uuid")))
			if err != nil {
				fail(w, fmt.Errorf("screenshot failed: %s", truncate(out, 200)))
				return
			}
			raw, err := base64.StdEncoding.DecodeString(out)
			if err != nil || len(raw) > shotMaxSize {
				fail(w, errors.New("screenshot failed: unexpected output from the node"))
				return
			}
			img, err := decodeScreenshot(raw)
			if err != nil {
				fail(w, err)
				return
			}
			slot.img, slot.at = img, time.Now()
		}
	case <-time.After(shotWait):
		// Another capture of this VM is still running; settle for the last frame.
		select {
		case slot.sem <- struct{}{}:
			defer func() { <-slot.sem }()
		default:
			conflict(w, "the screenshot is taking too long")
			return
		}
		if slot.img == nil {
			conflict(w, "the screenshot is taking too long")
			return
		}
	case <-r.Context().Done():
		return
	}

	out := boxScale(slot.img, width)
	var buf bytes.Buffer
	if format == "jpeg" {
		w.Header().Set("Content-Type", "image/jpeg")
		err = jpeg.Encode(&buf, out, &jpeg.Options{Quality: 72})
	} else {
		w.Header().Set("Content-Type", "image/png")
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, out)
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// conflict answers 409 with a message for the user.
func conflict(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
