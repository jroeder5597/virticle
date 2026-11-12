package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/domxml"
)

// serialPending compares the serial devices of the running definition with
// the saved one: "add" or "remove" when only the saved one differs in having
// or lacking a pty serial.
func serialPending(live []domxml.Serial, savedXML string) string {
	saved, err := domxml.Serials(savedXML)
	if err != nil {
		return ""
	}
	has := func(l []domxml.Serial) bool {
		for _, s := range l {
			if s.Type == "pty" {
				return true
			}
		}
		return false
	}
	switch {
	case has(saved) && !has(live):
		return "add"
	case !has(saved) && has(live):
		return "remove"
	}
	return ""
}

const restartNote = "applies after the VM is powered off and started"

// vmSerialAdd gives the VM a pty serial console. A running VM is tried live
// first (libvirt decides whether QEMU can take the device); when it refuses,
// the saved definition is edited and the change waits for a power cycle.
func (s *Server) vmSerialAdd(w http.ResponseWriter, r *http.Request) {
	s.serialChange(w, r, "add serial console", true)
}

// vmSerialRemove removes the pty serial console, with the same live-or-saved rule.
func (s *Server) vmSerialRemove(w http.ResponseWriter, r *http.Request) {
	s.serialChange(w, r, "remove serial console", false)
}

func (s *Server) serialChange(w http.ResponseWriter, r *http.Request, kind string, add bool) {
	c, err := s.hwContext(r)
	if err != nil {
		fail(w, err)
		return
	}
	var res hwResult
	err = s.task(r, kind, c.dom.Name, c.h.Name, func(func(float64)) error {
		raw, err := c.h.DomainGetXMLDesc(c.dom, libvirt.DomainXMLInactive)
		if err != nil {
			return err
		}
		var edited string
		if add {
			edited, err = domxml.AddSerial(raw)
		} else {
			var n int
			edited, n, err = domxml.RemoveSerial(raw)
			if err == nil && n == 0 {
				err = errors.New("the VM has no pty serial console to remove")
			}
		}
		if err != nil {
			return badRequest(err.Error())
		}
		live := false
		if c.running {
			if err := c.liveSerial(add); err != nil {
				*c.why = err.Error()
			} else {
				live = true
			}
		}
		if add && live {
			// attached with the config flag too: the saved definition has it already
			res = c.result("live+config")
			return nil
		}
		if _, err := c.h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate); err != nil {
			if live {
				return fmt.Errorf("removed from the running VM, but saving the definition failed: %w", err)
			}
			return err
		}
		if live {
			res = c.result("live+config")
			return nil
		}
		res = c.result("config")
		if res.NeedsRestart {
			res.endNote()
			res.addNote("The serial console change " + restartNote)
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// liveSerial attaches (to the running VM and the saved definition) or
// detaches (from the running VM only) the serial device. An error is
// libvirt's refusal and means nothing was changed.
func (c hwCtx) liveSerial(add bool) error {
	if add {
		return c.h.DomainAttachDeviceFlags(c.dom, domxml.SerialXML, affLive|affConfig)
	}
	live, _, err := c.docs()
	if err != nil {
		return err
	}
	for _, n := range live.Devices("serial") {
		if n.Attrs["type"] == "pty" {
			return c.h.DomainDetachDeviceFlags(c.dom, live.Raw(n), affLive)
		}
	}
	return errors.New("the running VM has no pty serial device")
}
