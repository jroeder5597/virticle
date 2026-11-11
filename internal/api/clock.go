package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/domxml"
	"virticle/internal/hostconn"
)

// Guest clock. The definition's <clock> is edited surgically (offset only);
// the "sync after resume, revert and migration" switch lives in the app
// metadata as <clock sync='off'/> (absent means on). Time is set through the
// guest agent (libvirt's DomainSetTime) from the node's own clock.

// nodeClock returns a function that reads the node's current time. It
// measures the offset between the node and this controller once (halving the
// round trip), so later reads cost nothing. When the node cannot be asked
// the controller's clock is used.
func nodeClock(h *hostconn.Host) func() time.Time {
	t0 := time.Now()
	out, err := h.Run("date +%s%N")
	t1 := time.Now()
	ns, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || perr != nil || ns < 1e18 {
		return time.Now
	}
	off := time.Unix(0, ns).Sub(t0.Add(t1.Sub(t0) / 2))
	return func() time.Time { return time.Now().Add(off) }
}

// guestTime asks the agent for the guest's clock.
func guestTime(h *hostconn.Host, dom libvirt.Domain) (time.Time, error) {
	raw, err := agentCmdT(h, dom, "guest-get-time", nil, 5)
	if err != nil {
		return time.Time{}, err
	}
	var ns int64
	if err := json.Unmarshal(raw, &ns); err != nil {
		return time.Time{}, fmt.Errorf("unreadable guest time: %w", err)
	}
	return time.Unix(0, ns), nil
}

// syncGuestClock sets the guest's clock to the node's. It returns the guest's
// offset (guest minus node) found just before, when the agent could tell.
// The agent sets the system time first and then writes the hardware clock;
// a guest without hwclock (a minimal Debian 13) fails that second step, which
// is reported in note and is not an error: the time is right.
func syncGuestClock(h *hostconn.Host, dom libvirt.Domain) (before time.Duration, known bool, note string, err error) {
	now := nodeClock(h)
	if g, gerr := guestTime(h, dom); gerr == nil {
		before, known = g.Sub(now()), true
	}
	t := now()
	err = h.DomainSetTime(dom, t.Unix(), uint32(t.Nanosecond()), 0)
	if err != nil && strings.Contains(err.Error(), "hardware clock") {
		note, err = "The guest set its system time but could not update its hardware clock (hwclock is missing in the guest).", nil
	}
	return before, known, note, err
}

// agentConnected reports whether the VM runs and its agent channel is open.
func agentConnected(h *hostconn.Host, dom libvirt.Domain) bool {
	st, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil || stateName(st) != "running" {
		return false
	}
	raw, err := h.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return false
	}
	var x domainXML
	if xml.Unmarshal([]byte(raw), &x) != nil {
		return false
	}
	for _, c := range x.Devices.Channels {
		if c.Target.Name == "org.qemu.guest_agent.0" && c.Target.State == "connected" {
			return true
		}
	}
	return false
}

// clockSyncOn reads the per-VM switch; on unless the metadata says off.
func clockSyncOn(h *hostconn.Host, dom libvirt.Domain) bool {
	raw, err := domainMeta(h, dom)
	if err != nil || raw == "" {
		return true
	}
	var root struct {
		Clock *struct {
			Sync string `xml:"sync,attr"`
		} `xml:"clock"`
	}
	if xml.Unmarshal([]byte(raw), &root) != nil || root.Clock == nil {
		return true
	}
	return root.Clock.Sync != "off"
}

func setClockSync(h *hostconn.Host, dom libvirt.Domain, on bool) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	el := ""
	if !on {
		el = "<clock sync='off'/>"
	}
	meta, err := metaSetChild(old, "clock", el)
	if err != nil {
		return err
	}
	return setDomainMeta(h, dom, meta)
}

// autoSyncClock runs in the background after an event that makes a guest's
// clock jump (resume, revert with memory, live migration). It waits a short
// while for the agent, does nothing when the VM has none or the switch is
// off, and never reports to the caller: a failure is logged and shows up as
// a failed task of its own.
func (s *Server) autoSyncClock(user, why string, h *hostconn.Host, dom libvirt.Domain) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("clock sync after %s: %v", why, p)
			}
		}()
		if !clockSyncOn(h, dom) {
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for !agentConnected(h, dom) {
			if time.Now().After(deadline) {
				return // no agent (or not running): nothing to do
			}
			time.Sleep(time.Second)
		}
		t, err := s.Store.StartTask("sync guest clock", dom.Name, h.Name, user)
		if err != nil {
			log.Printf("clock sync after %s: %v", why, err)
			return
		}
		s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
		_, _, _, err = syncGuestClock(h, dom)
		if err != nil {
			err = fmt.Errorf("after %s: %w", why, err)
			log.Printf("sync guest clock %s: %v", dom.Name, err)
		}
		s.Store.FinishTask(&t, err)
		s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
	}()
}

type clockInfo struct {
	// Offset is the running definition's, Saved the persistent one.
	Offset string `json:"offset"`
	Saved  string `json:"saved"`
	// Sync is the per-VM "sync after resume, revert and migration" switch.
	Sync bool `json:"sync"`
	// Guest is set when the agent answered: its time and its offset from the
	// node in milliseconds (negative: the guest is behind).
	Guest       string `json:"guest,omitempty"`
	SkewMs      *int64 `json:"skewMs,omitempty"`
	GuestStatus string `json:"guestStatus,omitempty"`
}

func (s *Server) vmClock(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	out := clockInfo{Sync: clockSyncOn(h, dom)}
	if raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive); err == nil {
		out.Saved = domxml.ClockOffset(raw)
	}
	out.Offset = out.Saved
	st, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		fail(w, err)
		return
	}
	if stateName(st) == "shutoff" {
		out.GuestStatus = "The VM is shut off"
		writeJSON(w, out)
		return
	}
	if raw, err := h.DomainGetXMLDesc(dom, 0); err == nil {
		if o := domxml.ClockOffset(raw); o != "" {
			out.Offset = o
		}
	}
	if !agentConnected(h, dom) {
		out.GuestStatus = "The guest agent is not connected"
	} else {
		now := nodeClock(h)
		if g, err := guestTime(h, dom); err != nil {
			out.GuestStatus = err.Error()
		} else {
			ms := g.Sub(now()).Milliseconds()
			out.Guest, out.SkewMs = g.UTC().Format(time.RFC3339), &ms
		}
	}
	writeJSON(w, out)
}

func (s *Server) hwClock(w http.ResponseWriter, r *http.Request) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Offset string `json:"offset"`
		Sync   *bool  `json:"sync"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Offset == "" && req.Sync == nil {
		fail(w, badRequest("nothing to change"))
		return
	}
	if req.Offset != "" && req.Offset != "utc" && req.Offset != "localtime" {
		fail(w, badRequest(`offset must be "utc" or "localtime"`))
		return
	}
	res := hwResult{Applied: "live+config"}
	err = s.task(r, "change clock", c.dom.Name, c.h.Name, func(func(float64)) error {
		if req.Sync != nil {
			if err := setClockSync(c.h, c.dom, *req.Sync); err != nil {
				return err
			}
		}
		if req.Offset == "" {
			return nil
		}
		raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive)
		if err != nil {
			return err
		}
		if domxml.ClockOffset(raw) == req.Offset {
			return nil
		}
		edited, err := domxml.SetClockOffset(raw, req.Offset)
		if err != nil {
			return badRequest(err.Error())
		}
		if _, err := c.h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate); err != nil {
			return err
		}
		res = c.result("config")
		if res.NeedsRestart {
			res.endNote()
			res.addNote("The clock offset " + restartNote)
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) vmClockSync(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	if !agentConnected(h, dom) {
		fail(w, badRequest("the VM must be running with the guest agent connected"))
		return
	}
	var before time.Duration
	var known bool
	var note string
	err = s.task(r, "sync guest clock", dom.Name, h.Name, func(func(float64)) (err error) {
		before, known, note, err = syncGuestClock(h, dom)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	res := map[string]any{"ok": true}
	if note != "" {
		res["note"] = note
	}
	if known {
		res["beforeMs"] = before.Milliseconds()
	}
	writeJSON(w, res)
}
