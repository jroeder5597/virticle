package api

import (
	"net/http"
	"strings"
)

// linuxKeys are the key names the send-key endpoint accepts, mapped to Linux
// input event codes (libvirt keycode set 0).
var linuxKeys = map[string]uint32{
	"esc": 1, "tab": 15, "enter": 28, "ctrl": 29, "shift": 42, "alt": 56,
	"space": 57, "delete": 111, "backspace": 14, "up": 103, "down": 108,
	"left": 105, "right": 106, "f1": 59, "f2": 60, "f3": 61, "f4": 62,
	"f5": 63, "f6": 64, "f7": 65, "f8": 66, "f9": 67, "f10": 68,
}

// sendKeys presses the named keys together (a chord such as ctrl, alt,
// delete) on the guest's keyboard. It is how a script gets past "Press any
// key to boot from CD" without a console session.
func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Keys   []string `json:"keys"`
		HoldMs uint32   `json:"holdMs"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if len(req.Keys) == 0 || len(req.Keys) > 8 {
		fail(w, badRequest("keys needs between 1 and 8 key names"))
		return
	}
	var codes []uint32
	for _, k := range req.Keys {
		c, ok := linuxKeys[strings.ToLower(k)]
		if !ok {
			// letters and digits are not mapped on purpose: this is for boot prompts and chords
			fail(w, badRequest("unknown key "+k))
			return
		}
		codes = append(codes, c)
	}
	if req.HoldMs == 0 || req.HoldMs > 2000 {
		req.HoldMs = 100
	}
	if err := h.DomainSendKey(dom, 0, req.HoldMs, codes, 0); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
