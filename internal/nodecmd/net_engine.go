package nodecmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"virticle/internal/nodeproto"
)

// The engine of the host network verbs: it saves the state, arms the rollback
// timer, applies the steps a stack backend planned, and restores. A change is
// a list of steps that the backend computed at plan time and that is stored
// with the change, so a rollback replays what was recorded and does not
// depend on recomputing anything.
//
// State directory (root only), below Config.Prefix:
//
//	var/lib/virticle-node/net/pending            id of the change that waits for a confirmation
//	var/lib/virticle-node/net/lock               held while a change is applied or rolled back
//	var/lib/virticle-node/net/managed/BRIDGE.json  the bridges this feature created
//	var/lib/virticle-node/net/changes/ID/        meta.json, files/ (copies), captures/, runtime.json, log

const keepChanges = 5

// netStep is one thing to do. Kinds:
//
//	write      write Content to Path (atomic, mode Mode)
//	remove     remove Path
//	run        run Argv
//	run        with Secs set: tried again every second for that long
//	wait-link  wait until the interface Path exists (Secs seconds)
//	wait-addr  wait until the interface Path has every address in Argv: "dhcp4" (any global IPv4 address),
//	           "any" (any global address) or an address with prefix (Secs seconds)
//
// Ignore: a failure of this step is noted and the sequence goes on.
type netStep struct {
	Kind    string   `json:"kind"`
	Path    string   `json:"path,omitempty"`
	Content string   `json:"content,omitempty"`
	Mode    uint32   `json:"mode,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Secs    int      `json:"secs,omitempty"`
	Ignore  bool     `json:"ignore,omitempty"`
}

func isWait(k string) bool { return k == "wait-link" || k == "wait-addr" }

func stepRun(argv ...string) netStep { return netStep{Kind: "run", Argv: argv} }
func stepRunIgnore(argv ...string) netStep {
	return netStep{Kind: "run", Argv: argv, Ignore: true}
}

func (s netStep) String() string {
	switch s.Kind {
	case "write":
		return "write " + s.Path
	case "remove":
		return "remove " + s.Path
	case "run":
		return shellWords(s.Argv)
	case "wait-link":
		return fmt.Sprintf("wait up to %d s for the interface %s", s.Secs, s.Path)
	case "wait-addr":
		return fmt.Sprintf("wait up to %d s until %s has %s", s.Secs, s.Path, strings.Join(s.Argv, ", "))
	}
	return s.Kind
}

// shellWords shows an argv for people. It is never run through a shell.
func shellWords(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \t\"'$,;&|<>()*?\\") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// bridgeSpec describes a bridge this feature created. It is the marker: a
// bridge is managed when it has one.
type bridgeSpec struct {
	Bridge        string            `json:"bridge"`
	Uplink        string            `json:"uplink,omitempty"`
	MoveAddresses bool              `json:"moveAddresses,omitempty"`
	VLANAware     bool              `json:"vlanAware,omitempty"`
	VLANs         string            `json:"vlans,omitempty"`
	MAC           string            `json:"mac,omitempty"`
	MTU           int               `json:"mtu,omitempty"`
	Stack         string            `json:"stack"`
	Created       string            `json:"created"`
	Moved         string            `json:"moved,omitempty"`        // the configuration that moved, in the stack's own format
	UplinkExpect  []string          `json:"uplinkExpect,omitempty"` // what the uplink must have again after a delete
	Data          map[string]string `json:"data,omitempty"`         // stack private
}

func (s bridgeSpec) clone() *bridgeSpec {
	d := s
	d.Data = map[string]string{}
	for k, v := range s.Data {
		d.Data[k] = v
	}
	d.UplinkExpect = append([]string(nil), s.UplinkExpect...)
	return &d
}

type netSave struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

// netPlanned is what a backend computes for a request.
type netPlanned struct {
	Steps   []netStep // the change
	Restore []netStep // run after the saved files are put back
	Saves   []netSave // tool output to keep for manual recovery
	Marker  string    // put | remove
	Spec    *bridgeSpec
	Summary []string
	Move    *nodeproto.NetAddressMove
	Warn    []string
}

type savedFile struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Name    string `json:"name,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
}

type netMeta struct {
	ID       string      `json:"id"`
	Action   string      `json:"action"`
	Bridge   string      `json:"bridge"`
	Stack    string      `json:"stack"`
	State    string      `json:"state"`
	Reason   string      `json:"reason,omitempty"`
	Message  string      `json:"message,omitempty"`
	Started  time.Time   `json:"started"`
	Deadline time.Time   `json:"deadline"`
	Finished time.Time   `json:"finished,omitempty"`
	Timeout  int         `json:"timeout"`
	Steps    []netStep   `json:"steps"`
	Restore  []netStep   `json:"restore"`
	Saved    []savedFile `json:"saved"`
	Marker   string      `json:"marker,omitempty"`
	Spec     *bridgeSpec `json:"spec,omitempty"`
}

func (m *netMeta) pending() bool {
	return m.State == nodeproto.NetArmed || m.State == nodeproto.NetApplying || m.State == nodeproto.NetApplied
}

var changeIDRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[a-z0-9]{4}$`)

func (c *Ctx) netDir() string             { return c.hostPath("var/lib/virticle-node/net") }
func (c *Ctx) changeDir(id string) string { return filepath.Join(c.netDir(), "changes", id) }

func newChangeID(now time.Time) string {
	b := []byte(strings.ToLower(randName(4)))
	return now.UTC().Format("20060102-150405") + "-" + string(b)
}

// --- files ---

// atomicWrite replaces path with data: a part file in the same directory is
// written, synced and renamed.
func (c *Ctx) atomicWrite(path string, data []byte, mode uint32) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".virticle-part.")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cancel := c.Cleanup(func() { _ = os.Remove(tmp) })
	defer cancel()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// hostPath is p below Config.Prefix. The prefix is empty in production, so the root directory is
// put in front: the path must never be relative to the working directory.
func (c *Ctx) hostPath(p string) string { return filepath.Join("/", c.Cfg.Prefix, p) }

func (c *Ctx) writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return c.atomicWrite(path, append(b, '\n'), 0o600)
}

func (c *Ctx) loadMeta(id string) (*netMeta, error) {
	if !changeIDRE.MatchString(id) {
		return nil, Die("%q is not a change id", id)
	}
	b, err := os.ReadFile(filepath.Join(c.changeDir(id), "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Fail("there is no saved change %s", id)
		}
		return nil, Fail("cannot read the change %s: %v", id, err)
	}
	var m netMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, Fail("the saved change %s is damaged: %v", id, err)
	}
	return &m, nil
}

func (c *Ctx) saveMeta(m *netMeta) error {
	return c.writeJSON(filepath.Join(c.changeDir(m.ID), "meta.json"), m)
}

// netLog writes to the change's own log and the journal (through logger, when
// it is installed).
func (c *Ctx) netLog(id, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	line := fmt.Sprintf("%s %s\n", c.netNow().UTC().Format(time.RFC3339), msg)
	if f, err := os.OpenFile(filepath.Join(c.changeDir(id), "log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		_, _ = f.WriteString(line)
		f.Close()
	}
	_, _ = c.netRun("logger", "-t", "virticle-node", "-p", "daemon.notice", "--", "network change "+id+": "+msg)
}

// lock takes the one lock of the host network verbs and returns the release function.
func (c *Ctx) netLock() (func(), error) {
	if err := os.MkdirAll(c.netDir(), 0o700); err != nil {
		return nil, Fail("cannot create the state directory: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(c.netDir(), "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, Fail("cannot open the lock file: %v", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, Fail("cannot lock: %v", err)
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

// --- managed bridges ---

func (c *Ctx) managedPath(br string) string {
	return filepath.Join(c.netDir(), "managed", br+".json")
}

func (c *Ctx) loadManaged() map[string]*bridgeSpec {
	res := map[string]*bridgeSpec{}
	ents, _ := os.ReadDir(filepath.Join(c.netDir(), "managed"))
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(c.netDir(), "managed", e.Name()))
		if err != nil {
			continue
		}
		var s bridgeSpec
		if json.Unmarshal(b, &s) == nil && s.Bridge != "" {
			res[s.Bridge] = &s
		}
	}
	return res
}

// --- pending change and status ---

func (c *Ctx) pendingMeta() *netMeta {
	b, err := os.ReadFile(filepath.Join(c.netDir(), "pending"))
	if err != nil {
		return nil
	}
	m, err := c.loadMeta(strings.TrimSpace(string(b)))
	if err != nil || !m.pending() {
		return nil
	}
	return m
}

func (c *Ctx) changeView(m *netMeta) nodeproto.NetChange {
	v := nodeproto.NetChange{ID: m.ID, Action: m.Action, Bridge: m.Bridge, State: m.State, Reason: m.Reason, Message: m.Message,
		Started: m.Started.UTC().Format(time.RFC3339)}
	if !m.Finished.IsZero() {
		v.Finished = m.Finished.UTC().Format(time.RFC3339)
	}
	if m.pending() {
		if left := int(m.Deadline.Sub(c.netNow()).Round(time.Second) / time.Second); left > 0 {
			v.SecondsLeft = left
		}
	}
	return v
}

func (c *Ctx) netStatus() nodeproto.NetStatus {
	st := nodeproto.NetStatus{Recent: []nodeproto.NetChange{}}
	ents, _ := os.ReadDir(filepath.Join(c.netDir(), "changes"))
	var ids []string
	for _, e := range ents {
		if changeIDRE.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for _, id := range ids {
		m, err := c.loadMeta(id)
		if err != nil {
			continue
		}
		v := c.changeView(m)
		if len(st.Recent) < keepChanges {
			st.Recent = append(st.Recent, v)
		}
		if st.Pending == nil && m.pending() {
			p := v
			st.Pending = &p
		}
	}
	return st
}

func (c *Ctx) prune() {
	ents, _ := os.ReadDir(filepath.Join(c.netDir(), "changes"))
	var ids []string
	for _, e := range ents {
		if changeIDRE.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	kept := 0
	for _, id := range ids {
		m, err := c.loadMeta(id)
		if err != nil || m.pending() {
			continue
		}
		if kept++; kept > keepChanges {
			_ = os.RemoveAll(c.changeDir(id))
		}
	}
}

// --- saving ---

// touchedPaths are the files a list of steps writes or removes.
func touchedPaths(steps []netStep) []string {
	seen := map[string]bool{}
	var r []string
	for _, s := range steps {
		if (s.Kind == "write" || s.Kind == "remove") && !seen[s.Path] {
			seen[s.Path] = true
			r = append(r, s.Path)
		}
	}
	return r
}

// saveState creates the change directory and stores everything a rollback
// needs: copies of the files the change touches, the runtime addresses and
// routes, and the tool output the backend asked for. It changes nothing on the system.
func (c *Ctx) saveState(m *netMeta, pl *netPlanned, snap *netSnap) error {
	dir := c.changeDir(m.ID)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "captures"), 0o700); err != nil {
		return err
	}
	for i, p := range touchedPaths(pl.Steps) {
		sf := savedFile{Path: p}
		b, err := os.ReadFile(c.hostPath(p))
		switch {
		case err == nil:
			sf.Existed, sf.Name = true, fmt.Sprintf("f%d", i)
			sf.Mode = 0o644
			if fi, e := os.Stat(c.hostPath(p)); e == nil {
				sf.Mode = uint32(fi.Mode().Perm())
			}
			if err := c.atomicWrite(filepath.Join(dir, "files", sf.Name), b, 0o600); err != nil {
				return err
			}
		case os.IsNotExist(err):
		default:
			return fmt.Errorf("cannot read %s: %w", p, err)
		}
		m.Saved = append(m.Saved, sf)
	}
	if err := c.writeJSON(filepath.Join(dir, "runtime.json"), map[string]any{"links": snap.Links, "routes": snap.Routes, "portVlans": snap.PortVLANs}); err != nil {
		return err
	}
	for _, s := range pl.Saves {
		if out, err := c.netRun(s.Argv...); err == nil {
			_ = c.atomicWrite(filepath.Join(dir, "captures", s.Name), []byte(out), 0o600)
		}
	}
	return c.saveMeta(m)
}

// --- running steps ---

func (c *Ctx) runStep(s netStep) error {
	switch s.Kind {
	case "write":
		mode := s.Mode
		if mode == 0 {
			mode = 0o644
		}
		return c.atomicWrite(c.hostPath(s.Path), []byte(s.Content), mode)
	case "remove":
		if err := os.Remove(c.hostPath(s.Path)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "run":
		if s.Secs > 0 { // tried again every second for Secs seconds (networkd may not know a link yet)
			return c.waitFor(s.Secs, func() (bool, string) {
				_, err := c.netRun(s.Argv...)
				if err != nil {
					return false, err.Error()
				}
				return true, ""
			})
		}
		_, err := c.netRun(s.Argv...)
		return err
	case "wait-link":
		return c.waitFor(s.Secs, func() (bool, string) {
			out, err := c.netRun("ip", "-j", "-d", "addr", "show", "dev", s.Path)
			if err != nil {
				return false, fmt.Sprintf("the interface %s did not appear", s.Path)
			}
			l, _ := parseIPLinks(out)
			return len(l) > 0, ""
		})
	case "wait-addr":
		return c.waitFor(s.Secs, func() (bool, string) {
			out, err := c.netRun("ip", "-j", "-d", "addr", "show", "dev", s.Path)
			if err != nil {
				return false, fmt.Sprintf("%s: %v", s.Path, err)
			}
			links, _ := parseIPLinks(out)
			if len(links) == 0 {
				return false, fmt.Sprintf("the interface %s is missing", s.Path)
			}
			missing := missingAddrs(links[0], s.Argv)
			if len(missing) == 0 {
				return true, ""
			}
			return false, fmt.Sprintf("%s did not get %s", s.Path, strings.Join(missing, ", "))
		})
	}
	return fmt.Errorf("unknown step %q", s.Kind)
}

func missingAddrs(l netLink, want []string) []string {
	var miss []string
	for _, w := range want {
		ok := false
		for _, a := range l.globalAddrs() {
			switch {
			case w == "any":
				ok = true
			case w == "dhcp4":
				ok = ok || a.Family == "inet"
			default:
				ok = ok || a.CIDR() == w
			}
		}
		if !ok {
			miss = append(miss, w)
		}
	}
	return miss
}

func (c *Ctx) waitFor(secs int, ok func() (bool, string)) error {
	end := c.netNow().Add(time.Duration(secs) * time.Second)
	for {
		done, why := ok()
		if done {
			return nil
		}
		if !c.netNow().Before(end) || c.Context.Err() != nil {
			if why == "" {
				why = "timed out"
			}
			return errors.New(why)
		}
		c.netSleep(time.Second)
	}
}

// --- apply, rollback, confirm ---

// runChange applies a saved change (the detached half of net-apply).
func (c *Ctx) runChange(id string) error {
	unlock, err := c.netLock()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := c.loadMeta(id)
	if err != nil {
		return err
	}
	if m.State != nodeproto.NetArmed {
		return Fail("the change %s is %s and is not applied again", id, m.State)
	}
	// Let net-apply's answer reach the controller before the network changes.
	c.netSleep(2 * time.Second)
	m.State = nodeproto.NetApplying
	if err := c.saveMeta(m); err != nil {
		return Fail("%v", err)
	}
	c.netLog(id, "applying %s of bridge %s", m.Action, m.Bridge)
	for i, s := range m.Steps {
		c.netLog(id, "step %d: %s", i+1, s)
		if err := c.runStep(s); err != nil {
			if s.Ignore {
				c.netLog(id, "step %d failed, going on: %v", i+1, err)
				continue
			}
			reason := "apply failed"
			if isWait(s.Kind) {
				reason = "check failed"
			}
			c.netLog(id, "step %d failed: %v", i+1, err)
			c.rollbackLocked(m, reason, fmt.Sprintf("step %d (%s): %v", i+1, s, err), false)
			return nil
		}
	}
	m.State = nodeproto.NetApplied
	if err := c.saveMeta(m); err != nil {
		return Fail("%v", err)
	}
	c.netLog(id, "applied; waiting for the confirmation until %s", m.Deadline.UTC().Format(time.RFC3339))
	return nil
}

// rollbackLocked restores the saved state. The lock is held. It is safe to
// call again: a change that is not pending any more is left alone.
func (c *Ctx) rollbackLocked(m *netMeta, reason, message string, fromTimer bool) {
	if !m.pending() {
		return
	}
	if !fromTimer {
		if err := c.netTimer().Cancel(m.ID); err != nil {
			c.netLog(m.ID, "the rollback timer could not be cancelled (it finds the change finished): %v", err)
		}
	}
	c.netLog(m.ID, "rolling back (%s)", reason)
	var problems []string
	failed := false
	for _, sf := range m.Saved {
		var err error
		if sf.Existed {
			var b []byte
			if b, err = os.ReadFile(filepath.Join(c.changeDir(m.ID), "files", sf.Name)); err == nil {
				err = c.atomicWrite(c.hostPath(sf.Path), b, sf.Mode)
			}
		} else if e := os.Remove(c.hostPath(sf.Path)); e != nil && !os.IsNotExist(e) {
			err = e
		}
		if err != nil {
			failed = true
			problems = append(problems, fmt.Sprintf("%s: %v", sf.Path, err))
			c.netLog(m.ID, "restoring %s failed: %v", sf.Path, err)
		}
	}
	for _, s := range m.Restore {
		if err := c.runStep(s); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", s, err))
			c.netLog(m.ID, "restore step %q: %v", s, err)
		}
	}
	m.Reason = reason
	m.Message = message
	if len(problems) > 0 {
		m.Message = strings.TrimSpace(message + " Problems while restoring: " + strings.Join(problems, "; "))
	}
	m.State = nodeproto.NetRolledBack
	if failed {
		m.State = nodeproto.NetFailed
	}
	m.Finished = c.netNow()
	_ = c.saveMeta(m)
	_ = os.Remove(filepath.Join(c.netDir(), "pending"))
	c.netLog(m.ID, "%s", m.State)
	c.prune()
}

// confirmLocked makes a change permanent.
func (c *Ctx) confirmLocked(m *netMeta) error {
	switch m.State {
	case nodeproto.NetConfirmed:
		return nil
	case nodeproto.NetRolledBack, nodeproto.NetFailed:
		return Fail("the change %s was rolled back (%s)", m.ID, m.Reason)
	case nodeproto.NetArmed, nodeproto.NetApplying:
		return Fail("the change %s is still being applied; try again in a moment", m.ID)
	}
	// The timer is cancelled first. If that fails the change is still confirmed:
	// a timer that fires later finds a confirmed change and does nothing.
	if err := c.netTimer().Cancel(m.ID); err != nil {
		c.netLog(m.ID, "the rollback timer could not be cancelled: %v", err)
	}
	switch m.Marker {
	case "put":
		if m.Spec != nil {
			if err := c.writeJSON(c.managedPath(m.Spec.Bridge), m.Spec); err != nil {
				return Fail("cannot record the bridge as managed: %v", err)
			}
		}
	case "remove":
		_ = os.Remove(c.managedPath(m.Bridge))
	}
	m.State, m.Finished = nodeproto.NetConfirmed, c.netNow()
	if err := c.saveMeta(m); err != nil {
		return Fail("%v", err)
	}
	_ = os.Remove(filepath.Join(c.netDir(), "pending"))
	c.netLog(m.ID, "confirmed")
	c.prune()
	return nil
}
