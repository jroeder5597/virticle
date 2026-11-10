package api

import (
	"errors"
	"fmt"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/hostconn"
)

const maxShutdownTimeoutSec = 3600

// shutdownOutcome says how a graceful shutdown ended.
type shutdownOutcome struct {
	Method string `json:"method"` // agent | acpi
	Forced bool   `json:"forced"`
}

// shutdownWithTimeout asks the guest to shut down (through the guest agent
// when it answers, else by ACPI power button), waits up to timeout for the
// domain to stop and, if force is set, powers it off when the time is up.
func shutdownWithTimeout(h *hostconn.Host, dom libvirt.Domain, timeout time.Duration, force bool, progress func(float64)) (shutdownOutcome, error) {
	var out shutdownOutcome
	if stopped, err := isStopped(h, dom); err != nil || stopped {
		return out, err
	}
	out.Method = "agent"
	if err := h.DomainShutdownFlags(dom, libvirt.DomainShutdownGuestAgent); err != nil {
		// No agent channel, or nothing answering in the guest.
		out.Method = "acpi"
		if err := h.DomainShutdownFlags(dom, libvirt.DomainShutdownAcpiPowerBtn); err != nil {
			return out, err
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		stopped, err := isStopped(h, dom)
		if err != nil {
			return out, err
		}
		if stopped {
			return out, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		progress(1 - float64(left)/float64(timeout))
		time.Sleep(min(time.Second, left))
	}
	if !force {
		return out, fmt.Errorf("%s did not shut down within %s", dom.Name, timeout)
	}
	if err := h.DomainDestroy(dom); err != nil {
		// It may have stopped just now.
		if stopped, serr := isStopped(h, dom); serr != nil || !stopped {
			return out, err
		}
		return out, nil
	}
	out.Forced = true
	return out, nil
}

func isStopped(h *hostconn.Host, dom libvirt.Domain) (bool, error) {
	state, _, err := h.DomainGetState(dom, 0)
	var lv libvirt.Error
	if errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomain) {
		return true, nil // a transient domain disappears when it stops
	}
	if err != nil {
		return false, err
	}
	return libvirt.DomainState(state) == libvirt.DomainShutoff, nil
}
