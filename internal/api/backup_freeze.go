package api

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// A live backup is the image of the moment libvirt's backup-begin runs. To make
// that image consistent for the guest (databases, journals), the guest's file
// systems are frozen through the QEMU guest agent for that one call and thawed
// right after it. libvirt's backup-begin has no flag for this.

const (
	freezeInfoWait = 5  // seconds to wait for the agent's guest-info
	freezeWait     = 15 // seconds to wait for the freeze
	thawWait       = 15 // seconds to wait for one thaw attempt
	thawTries      = 4
)

var thawPause = time.Second

// Texts that the task and the manifest carry.
const (
	frozenText = "guest file systems frozen"
	frozenNo   = "not frozen: "
)

// guestFreezer is the guest agent, as far as the freeze needs it.
type guestFreezer interface {
	// Freeze freezes the guest's file systems; it fails when the guest cannot.
	Freeze() error
	// Thaw thaws them; thawing file systems that are not frozen is not an error.
	Thaw() error
}

// freezeOutcome says what happened to the guest during one backup-begin.
type freezeOutcome struct {
	Frozen  bool   // the file systems were frozen while backup-begin ran
	Text    string // the one clause the task shows
	ThawErr error  // set when the guest may still be frozen
}

// freezeNotTried marks a Freeze error that came before anything was sent to the guest to freeze.
type freezeNotTried struct{ err error }

func (e *freezeNotTried) Error() string { return e.err.Error() }
func (e *freezeNotTried) Unwrap() error { return e.err }

// freezeFail is an explicit freeze request that could not be met; the backup is not started.
type freezeFail struct{ err error }

func (e *freezeFail) Error() string { return e.err.Error() }
func (e *freezeFail) Unwrap() error { return e.err }

// withFrozenGuest runs begin (the call that starts the backup job) with the
// guest's file systems frozen, as the request says:
//
//	quiesce nil   automatic: freeze when the agent is connected; if that fails, begin anyway
//	quiesce true  freeze or fail (a *freezeFail, begin is not called)
//	quiesce false never freeze
//
// Once a freeze was tried the guest is thawed whatever happens, with a few
// retries; ThawErr is set only when every try failed.
func withFrozenGuest(quiesce *bool, agent bool, g guestFreezer, begin func() error) (freezeOutcome, error) {
	if quiesce != nil && !*quiesce {
		return freezeOutcome{Text: frozenNo + "turned off for this backup"}, begin()
	}
	explicit := quiesce != nil
	if !agent {
		if explicit {
			return freezeOutcome{}, &freezeFail{errors.New("cannot freeze the guest file systems: the guest agent is not connected. Install and start qemu-guest-agent in the guest, or back up without freezing")}
		}
		return freezeOutcome{Text: frozenNo + "no guest agent"}, begin()
	}
	if ferr := g.Freeze(); ferr != nil {
		// A freeze that timed out may still have taken effect: thaw to be sure.
		out := freezeOutcome{Text: frozenNo + freezeReason(ferr)}
		var nt *freezeNotTried
		if !errors.As(ferr, &nt) {
			out.ThawErr = g.Thaw() // one try: an agent that cannot freeze in time rarely thaws in time
		}
		if explicit {
			return out, &freezeFail{fmt.Errorf("could not freeze the guest file systems: %v. Check that qemu-guest-agent runs in the guest, or back up without freezing", ferr)}
		}
		return out, begin()
	}
	err := begin()
	out := freezeOutcome{Frozen: true, Text: frozenText}
	if out.ThawErr = thawWithRetries(g); out.ThawErr != nil {
		log.Printf("backup: the guest file systems could not be thawed: %v", out.ThawErr)
	}
	return out, err
}

func thawWithRetries(g guestFreezer) (err error) {
	for i := 0; i < thawTries; i++ {
		if i > 0 {
			time.Sleep(thawPause)
		}
		if err = g.Thaw(); err == nil {
			return nil
		}
	}
	return err
}

// thawProblem is the sentence for a guest that may have been left frozen.
func thawProblem(vm string, err error) string {
	return fmt.Sprintf("the guest file systems of %s could not be thawed (%v); the guest may be frozen. Thaw it now with: virsh domfsthaw %s", vm, err, vm)
}

// freezeReason shortens an error to a clause for the task text.
func freezeReason(err error) string {
	s := err.Error()
	for i, c := range s {
		if c == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// agentFreezer freezes through the guest agent of a running domain.
type agentFreezer struct {
	h   *hostconn.Host
	dom libvirt.Domain
}

func (a agentFreezer) Freeze() error {
	raw, err := agentCmdT(a.h, a.dom, "guest-info", nil, freezeInfoWait)
	if err != nil {
		return &freezeNotTried{fmt.Errorf("the guest agent does not answer (%v)", err)}
	}
	if _, can, perr := parseAgentInfo(raw); perr != nil || !can {
		return &freezeNotTried{errors.New("the guest agent cannot freeze file systems")}
	}
	if _, err := agentCmdT(a.h, a.dom, "guest-fsfreeze-freeze", nil, freezeWait); err != nil {
		return err
	}
	return nil
}

func (a agentFreezer) Thaw() error {
	_, err := agentCmdT(a.h, a.dom, "guest-fsfreeze-thaw", nil, thawWait)
	return err
}
