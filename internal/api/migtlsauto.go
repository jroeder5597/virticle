package api

// Encrypted migration without being asked.
//
// A node that has the node command gets a migration certificate from the controller's CA when it is
// added, when the controller starts and whenever the node comes online, and the certificate is renewed
// before it expires. Nothing here may fail a node registration: an error is recorded, shown on the node
// and tried again later, at most once per migAutoRetry for each node. A node whose certificate an
// administrator removed by hand (store.Node.MigTLSOff) is left alone until Fix is pressed.

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"virticle/internal/cluster"
	"virticle/internal/migpki"
)

const (
	migAutoRetry = 10 * time.Minute // between two attempts on the same node, whatever the outcome
	migAutoSweep = time.Hour        // how often every online node is looked at for renewal
)

// errMigAutoSkip means the node cannot take part (offline, or without the node command); it is not an error to show.
var errMigAutoSkip = errors.New("nothing to do on this node")

// migTLSEnsurer is what the automatic setup needs from the controller and its nodes.
type migTLSEnsurer interface {
	// Ensure makes the node's migration certificate valid. It returns errMigAutoSkip for a node it cannot work on.
	Ensure(id int64) error
	// OptedOut is true when encrypted migration was removed from the node by hand.
	OptedOut(id int64) bool
}

type migTLSAuto struct {
	ens   migTLSEnsurer
	now   func() time.Time
	retry time.Duration
	// changed is told when the outcome for a node changed, so that open pages refresh.
	changed func(id int64)

	mu      sync.Mutex
	last    map[int64]time.Time
	running map[int64]bool
	errs    map[int64]string
}

func newMigTLSAuto(e migTLSEnsurer, changed func(int64)) *migTLSAuto {
	return &migTLSAuto{ens: e, now: time.Now, retry: migAutoRetry, changed: changed,
		last: map[int64]time.Time{}, running: map[int64]bool{}, errs: map[int64]string{}}
}

// Try makes sure of one node's certificate unless the node was opted out, an attempt is running, or the
// last attempt is younger than the retry interval. It reports whether it tried. It blocks while it works.
func (a *migTLSAuto) Try(id int64) bool {
	if a.ens.OptedOut(id) {
		return false
	}
	a.mu.Lock()
	if a.running[id] || (!a.last[id].IsZero() && a.now().Sub(a.last[id]) < a.retry) {
		a.mu.Unlock()
		return false
	}
	a.running[id] = true
	a.last[id] = a.now()
	a.mu.Unlock()

	err := a.ens.Ensure(id)

	a.mu.Lock()
	a.running[id] = false
	prev := a.errs[id]
	switch {
	case errors.Is(err, errMigAutoSkip):
		delete(a.last, id) // not tried: the next online event may try at once
		delete(a.errs, id)
	case err != nil:
		a.errs[id] = err.Error()
	default:
		delete(a.errs, id)
	}
	cur := a.errs[id]
	a.mu.Unlock()
	if err != nil && !errors.Is(err, errMigAutoSkip) {
		log.Printf("migration TLS: node %d: %v", id, err)
	}
	if a.changed != nil && cur != prev {
		a.changed(id)
	}
	return !errors.Is(err, errMigAutoSkip)
}

// Error is the last automatic failure on a node, empty when there is none.
func (a *migTLSAuto) Error(id int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.errs[id]
}

// Reset lets the next trigger try at once and drops a recorded failure (after a manual setup or an edit of the node).
func (a *migTLSAuto) Reset(id int64) {
	a.mu.Lock()
	delete(a.last, id)
	delete(a.errs, id)
	a.mu.Unlock()
}

// serverEnsurer is the real thing behind migTLSAuto.
type serverEnsurer struct{ s *Server }

func (e serverEnsurer) OptedOut(id int64) bool {
	cfg, ok := e.s.Nodes.Config(id)
	return !ok || cfg.MigTLSOff
}

func (e serverEnsurer) Ensure(id int64) error {
	s := e.s
	h, err := s.Nodes.Host(id)
	if err != nil || !h.CanRoot() {
		return errMigAutoSkip
	}
	defer lockNodeTLS(id)()
	info, _, _, err := s.readMigTLS(h)
	if err != nil {
		return err
	}
	forced := e.s.tookOver(id)
	install, force, err := migAutoAction(info.State, addressOnlyMismatch(info.Status), forced && fromOtherCA(info.Status))
	if err != nil || !install {
		if err == nil && forced {
			e.s.takeOverDone(id)
		}
		return err
	}
	_, err = s.installMigTLS(id, h, migTLSReq{Force: force}, func(float64) {})
	if err == nil && forced {
		e.s.takeOverDone(id)
	}
	return err
}

// fromOtherCA is true when the node's certificates were set up by a different controller's CA.
func fromOtherCA(st migpki.Status) bool {
	for _, p := range st.Problems {
		if strings.HasPrefix(p, "set up by a different controller or CA") {
			return true
		}
	}
	return false
}

// migAutoAction decides what the automatic setup does with a node's certificate files, given their state. A
// node that was just taken over from another controller has that controller's certificates; they are replaced
// (the old files are kept as *.virticle-orig). Otherwise files that are not ours are never touched.
func migAutoAction(state string, addressOnly, takenOver bool) (install, force bool, err error) {
	switch state {
	case migpki.StateOK:
		return false, false, nil
	case migpki.StateMismatch:
		switch {
		case takenOver:
			return true, true, nil
		case !addressOnly:
			return false, false, errors.New("the certificate files on the node were not installed by this controller or were changed; press Fix to replace them")
		}
	}
	return true, false, nil
}

// addressOnlyMismatch is true when the node's address changed and the certificate does not cover it: the files are ours and
// unmodified, so issuing again is safe.
func addressOnlyMismatch(st migpki.Status) bool {
	if st.State != migpki.StateMismatch || len(st.Problems) == 0 {
		return false
	}
	for _, p := range st.Problems {
		if !strings.Contains(p, "does not cover") {
			return false
		}
	}
	return true
}

// startMigAuto starts the automatic setup: on every node that comes online, and a sweep for renewals.
func (s *Server) startMigAuto() {
	if s.migAuto != nil || s.Nodes == nil || s.Store == nil {
		return
	}
	s.migAuto = newMigTLSAuto(serverEnsurer{s}, func(id int64) { s.Nodes.Publish(cluster.Event{Type: "node", Node: id, Detail: "migration-tls"}) })
	a := s.migAuto
	events, _ := s.Nodes.Subscribe()
	go func() {
		for ev := range events {
			if ev.Type == "node" && ev.Detail == "online" {
				go a.Try(ev.Node)
			}
		}
	}()
	go func() {
		wait := 20 * time.Second
		for {
			time.Sleep(wait)
			wait = migAutoSweep
			for _, n := range s.Nodes.Status() {
				if n.State == "online" {
					go a.Try(n.ID)
				}
			}
		}
	}()
}

// markTakeOver remembers that a node was just taken over from another controller, until its migration
// certificates have been dealt with. It is kept in memory only: after a restart the node simply shows its
// state and the Fix button.
func (s *Server) markTakeOver(id int64)  { s.takeOvers.Store(id, true) }
func (s *Server) tookOver(id int64) bool { _, ok := s.takeOvers.Load(id); return ok }
func (s *Server) takeOverDone(id int64)  { s.takeOvers.Delete(id) }
