package api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/nodeproto"
	"virticle/internal/store"
)

// Rolling node updates: put a node in maintenance, move its VMs away, install the package updates, restart the
// node when that is needed, wait until it is healthy, end maintenance, move the VMs back, and go on with the next
// node. The planning is in rollplan.go. This file reads the nodes for the plan, runs it, and keeps the state of the
// run in the database after every step, so that a controller that stops (or restarts together with its own machine)
// carries on where it was.
//
// The run is a list of nodes, each with a list of steps. Every step can be run again after an interruption: it looks at
// what is true now (where the VM is, whether the update ran, whether the node restarted) before it acts. The first
// failure stops the run, and the run says in words what state the datacenter was left in (rollCondition). A stopped
// run can be continued from the failed step, or put back (maintenance ended, VMs started and moved back).

var (
	rollPoll         = 5 * time.Second
	rollUpdateMax    = 100 * time.Minute // longest wait for the package tool
	rollWaitBack     = 12 * time.Minute  // longest wait for a restarted node
	rollOnlineWait   = 4 * time.Minute   // longest wait for a node to answer when it was not restarted
	rollShutdownWait = 3 * time.Minute   // longest wait for a VM to shut down
	rollPoolWait     = 90 * time.Second  // longest wait for the pools of a node to be active again
	rollResumeDelay  = 3 * time.Second
)

var errRollCancelled = errors.New("cancelled")

type rollCheck struct {
	Status string `json:"status"` // ok | fixed | problem
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

type rollMoved struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	To     int64  `json:"to"`
	ToName string `json:"toName"`
	Status string `json:"status"` // moving | moved | back | stay
	Note   string `json:"note,omitempty"`
}

type rollDown struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Started bool   `json:"started"`
}

type rollNode struct {
	Node    int64      `json:"node"`
	Name    string     `json:"name"`
	Local   bool       `json:"local,omitempty"`
	Action  string     `json:"action"` // update | skip | current
	Status  string     `json:"status"` // pending | running | done | skipped | current | failed
	SkipWhy string     `json:"skipWhy,omitempty"`
	Steps   []rollStep `json:"steps"`
	// What the plan said, for the live planning of the node and for display.
	UpdateNames []string    `json:"updateNames,omitempty"`
	Stuck       []evacVM    `json:"stuck,omitempty"`
	Moved       []rollMoved `json:"moved,omitempty"`
	Down        []rollDown  `json:"down,omitempty"`
	PoolsBefore []string    `json:"poolsBefore,omitempty"`
	NetsBefore  []string    `json:"netsBefore,omitempty"`
	MaintSet    bool        `json:"maintSet,omitempty"`
	// The update and the restart.
	UpdateStarted bool        `json:"updateStarted,omitempty"`
	UpdateAt      int64       `json:"updateAt,omitempty"`
	Reboot        bool        `json:"reboot"`
	RebootWhy     string      `json:"rebootWhy,omitempty"`
	BootID        string      `json:"bootId,omitempty"`
	RebootSent    bool        `json:"rebootSent,omitempty"`
	RebootAt      int64       `json:"rebootAt,omitempty"`
	Installed     int         `json:"installed,omitempty"`
	HeldBack      []string    `json:"heldBack,omitempty"`
	Checks        []rollCheck `json:"checks,omitempty"`
}

type rollRun struct {
	ID        int64       `json:"id"`
	Started   int64       `json:"started"`
	Finished  int64       `json:"finished,omitempty"`
	User      string      `json:"user"`
	Status    string      `json:"status"` // running | ok | failed | cancelled
	Choice    string      `json:"choice,omitempty"`
	Task      int64       `json:"task,omitempty"`
	Nodes     []*rollNode `json:"nodes"`
	Cur       int         `json:"cur"`
	Cancel    bool        `json:"cancel,omitempty"`
	Restoring bool        `json:"restoring,omitempty"`
	Ack       bool        `json:"ack,omitempty"`
	Resumed   int         `json:"resumed,omitempty"`
	Error     string      `json:"error,omitempty"`
	Condition string      `json:"condition,omitempty"`
	Summary   string      `json:"summary,omitempty"`
}

// rollExec is the running (or resumed) execution of one run.
type rollExec struct {
	s        *Server
	mu       sync.Mutex
	run      *rollRun
	progress func(float64)
}

var rolling = struct {
	sync.Mutex
	cur *rollExec
}{}

func rollActive() bool {
	rolling.Lock()
	defer rolling.Unlock()
	return rolling.cur != nil
}

// mutate changes the run under the lock and stores it.
func (e *rollExec) mutate(fn func(r *rollRun)) {
	e.mu.Lock()
	fn(e.run)
	e.save()
	e.mu.Unlock()
}

func (e *rollExec) save() {
	b, err := json.Marshal(e.run)
	if err != nil {
		log.Printf("rolling update: %v", err)
		return
	}
	if err := e.s.Store.SaveRoll(e.run.ID, e.run.Finished, e.run.Status, string(b)); err != nil {
		log.Printf("rolling update: storing the state: %v", err)
	}
	e.s.Nodes.Publish(cluster.Event{Type: "rolling"})
}

func (e *rollExec) cancelled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.run.Cancel
}

// --- reading the nodes ---

// rollSnapshot is what a plan was made from.
type rollSnapshot struct {
	Infos []rollNodeInfo
	Doms  map[int64]map[string]libvirt.Domain
	Hosts map[int64]*hostconn.Host
}

// rollGather reads every node. With updates it asks each node for its pending updates (slow, in parallel); vmsOf
// limits the reading of VMs to one node (0: the nodes in sel, or all).
func (s *Server) rollGather(withUpdates bool, sel map[int64]bool, vmsOf int64) rollSnapshot {
	status := s.Nodes.Status()
	snap := rollSnapshot{Doms: map[int64]map[string]libvirt.Domain{}, Hosts: map[int64]*hostconn.Host{}}
	infos := make([]rollNodeInfo, len(status))
	var wg sync.WaitGroup
	probe := realLocalProbe()
	host, _ := os.Hostname()
	for i, n := range status {
		name := n.Hostname
		if name == "" {
			name = n.Addr
		}
		infos[i] = rollNodeInfo{ID: n.ID, Name: name, Online: n.State == "online", Maintenance: n.Maintenance, Local: probe.isThisMachine(n.Addr, host)}
		if !infos[i].Online {
			continue
		}
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			infos[i].Online = false
			continue
		}
		snap.Hosts[n.ID] = h
		infos[i].NoNodeCmd = !h.CanRoot()
		wg.Add(1)
		go func(i int, id int64, h *hostconn.Host) {
			defer wg.Done()
			in := &infos[i]
			if free, err := h.AvailableMemory(); err == nil {
				in.FreeKiB = free / 1024
			}
			if _, mem, _, _, _, _, _, _, err := h.NodeGetInfo(); err == nil {
				in.TotalKiB = mem
			}
			wantVMs := vmsOf == id || (vmsOf == 0 && (len(sel) == 0 || sel[id]))
			if wantVMs {
				doms, _, err := h.ConnectListAllDomains(1, 0)
				if err != nil {
					in.UpdateErr = "the virtual machines cannot be listed: " + err.Error()
				}
				m := map[string]libvirt.Domain{}
				for _, d := range doms {
					state, maxMem, _, _, _, err := h.DomainGetInfo(d)
					if err != nil || stateName(state) == "shutoff" {
						continue
					}
					u := cluster.FormatUUID(d.UUID)
					m[u] = d
					in.VMs = append(in.VMs, evacVM{UUID: u, Name: d.Name, State: stateName(state), MemoryKiB: maxMem})
				}
				snap.Doms[id] = m
			}
			if withUpdates && (len(sel) == 0 || sel[id]) && h.CanRoot() {
				u := collectUpdates(h, time.Now())
				in.Family, in.RebootFlag = u.Family, u.RebootRequired
				if u.Error != "" {
					in.UpdateErr = u.Error
				}
				in.Pending = nodeproto.Names(u.Updates)
			}
		}(i, n.ID, h)
	}
	wg.Wait()
	snap.Infos = infos
	return snap
}

// rollBusy says why no run can start now ("" when none is in the way).
func (s *Server) rollBusy() string {
	if rollActive() {
		return "A rolling update is already running."
	}
	if r, ok := s.latestRoll(); ok && r.Status == "running" {
		return "A rolling update is already running."
	}
	evacRuns.Lock()
	for _, r := range evacRuns.m {
		if r.snapshot().Running {
			evacRuns.Unlock()
			return "A node evacuation is running. Wait until it is finished."
		}
	}
	evacRuns.Unlock()
	migRuns.Lock()
	n := len(migRuns.m)
	migRuns.Unlock()
	if n > 0 {
		return "A virtual machine is being migrated. Wait until it is finished."
	}
	return ""
}

func (s *Server) latestRoll() (*rollRun, bool) {
	row, ok, err := s.Store.LatestRoll()
	if err != nil || !ok {
		return nil, false
	}
	var r rollRun
	if json.Unmarshal([]byte(row.State), &r) != nil {
		return nil, false
	}
	r.ID, r.Started, r.Finished, r.Status = row.ID, row.Started, row.Finished, row.Status
	return &r, true
}

// makeRollPlan reads the nodes and plans.
func (s *Server) makeRollPlan(sel map[int64]bool, choice string) (rollPlan, rollSnapshot) {
	snap := s.rollGather(true, sel, 0)
	in := rollPlanIn{Nodes: snap.Infos, Select: sel, Choice: choice, Busy: s.rollBusy()}
	in.Check = func(vm evacVM, from int64, to evacTarget) []string {
		h, dom := snap.Hosts[from], snap.Doms[from][vm.UUID]
		if h == nil {
			return []string{"the node cannot be read"}
		}
		_, c := s.preflight(from, h, dom, migReq{Target: to.ID})
		return blockingReasons(c)
	}
	return planRolling(in), snap
}

// --- HTTP ---

type rollReq struct {
	Nodes     []int64 `json:"nodes"`
	Unmovable string  `json:"unmovable"` // shutdown | skip
}

func (r rollReq) selection() map[int64]bool {
	m := map[int64]bool{}
	for _, id := range r.Nodes {
		m[id] = true
	}
	return m
}

func (s *Server) rollingPlan(w http.ResponseWriter, r *http.Request) {
	var req rollReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	p, _ := s.makeRollPlan(req.selection(), req.Unmovable)
	writeJSON(w, p)
}

func (s *Server) rollingStart(w http.ResponseWriter, r *http.Request) {
	var req rollReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Unmovable != "" && req.Unmovable != rollChoiceShutdown && req.Unmovable != rollChoiceSkip {
		fail(w, badRequest(`unmovable must be "shutdown" or "skip"`))
		return
	}
	p, _ := s.makeRollPlan(req.selection(), req.Unmovable)
	if len(p.Refusals) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": p.Refusals[0], "plan": p})
		return
	}
	if p.NeedsChoice && req.Unmovable == "" {
		fail(w, badRequest("Some virtual machines cannot be moved. Choose what to do with them before starting."))
		return
	}
	run := &rollRun{Started: time.Now().Unix(), User: userOf(r), Status: "running", Choice: req.Unmovable}
	updating := 0
	for _, np := range p.Nodes {
		n := &rollNode{Node: np.Node, Name: np.Name, Local: np.Local, Action: np.Action, Status: "pending", SkipWhy: np.SkipWhy,
			UpdateNames: np.UpdateNames, Reboot: np.Reboot, RebootWhy: np.RebootWhy}
		switch np.Action {
		case "update":
			updating++
			n.Steps = rollStepsFor(np, req.Unmovable)
			for _, s := range np.Stuck {
				n.Stuck = append(n.Stuck, s.VM)
			}
		case "skip":
			n.Status = "skipped"
		default:
			n.Status = "current"
		}
		run.Nodes = append(run.Nodes, n)
	}
	if updating == 0 {
		fail(w, badRequest("There is nothing to update: every node is up to date or left out."))
		return
	}
	// The first node that is updated is where the run begins.
	for i, n := range run.Nodes {
		if n.Action == "update" {
			run.Cur = i
			break
		}
	}
	e, err := s.startRoll(run)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"task": e.taskID(), "run": e.snapshot()})
}

func (e *rollExec) taskID() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.run.Task
}

func (e *rollExec) snapshot() *rollRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := json.Marshal(e.run)
	var c rollRun
	json.Unmarshal(b, &c)
	return &c
}

func (s *Server) rollingStatus(w http.ResponseWriter, r *http.Request) {
	rolling.Lock()
	cur := rolling.cur
	rolling.Unlock()
	if cur != nil {
		writeJSON(w, map[string]any{"run": cur.snapshot(), "active": true})
		return
	}
	run, ok := s.latestRoll()
	if !ok {
		writeJSON(w, map[string]any{"run": nil, "active": false})
		return
	}
	writeJSON(w, map[string]any{"run": run, "active": false})
}

func (s *Server) rollingCancel(w http.ResponseWriter, r *http.Request) {
	rolling.Lock()
	cur := rolling.cur
	rolling.Unlock()
	if cur == nil {
		fail(w, badRequest("no rolling update is running"))
		return
	}
	cur.mutate(func(r *rollRun) { r.Cancel = true })
	writeJSON(w, map[string]bool{"ok": true})
}

// rollingResume handles continue and restore of a stopped run.
func (s *Server) rollingResume(restore bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rollActive() {
			fail(w, badRequest("a rolling update is running"))
			return
		}
		run, ok := s.latestRoll()
		if !ok || (run.Status != "failed" && run.Status != "cancelled") {
			fail(w, badRequest("there is no stopped rolling update to continue"))
			return
		}
		if busy := s.rollBusy(); busy != "" {
			fail(w, badRequest(busy))
			return
		}
		run.User = userOf(r)
		rollReopen(run, restore)
		e, err := s.startRoll(run)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"task": e.taskID(), "run": e.snapshot()})
	}
}

func (s *Server) rollingDismiss(w http.ResponseWriter, r *http.Request) {
	run, ok := s.latestRoll()
	if !ok || run.Status == "running" {
		fail(w, badRequest("there is no finished rolling update to dismiss"))
		return
	}
	run.Ack = true
	b, _ := json.Marshal(run)
	if err := s.Store.SaveRoll(run.ID, run.Finished, run.Status, string(b)); err != nil {
		fail(w, err)
		return
	}
	s.Nodes.Publish(cluster.Event{Type: "rolling"})
	writeJSON(w, map[string]bool{"ok": true})
}

// --- running ---

// startRoll stores the run if it is new and starts executing it in the background as one task.
func (s *Server) startRoll(run *rollRun) (*rollExec, error) {
	if run.ID == 0 {
		b, _ := json.Marshal(run)
		id, err := s.Store.CreateRoll(run.Started, run.Status, string(b))
		if err != nil {
			return nil, err
		}
		run.ID = id
		s.Store.PruneRolls(20)
	}
	e := &rollExec{s: s, run: run}
	rolling.Lock()
	if rolling.cur != nil {
		rolling.Unlock()
		return nil, badRequest("a rolling update is already running")
	}
	rolling.cur = e
	rolling.Unlock()
	nn := 0
	for _, n := range run.Nodes {
		if n.Action == "update" {
			nn++
		}
	}
	target := fmt.Sprintf("%d nodes", nn)
	if nn == 1 {
		target = "1 node"
	}
	taskID, _, err := s.startBackgroundWait(userRequest(run.User), "rolling-update", target, "", func(progress func(float64)) error {
		e.progress = progress
		return e.execute()
	})
	if err != nil {
		rolling.Lock()
		rolling.cur = nil
		rolling.Unlock()
		return nil, err
	}
	e.mutate(func(r *rollRun) { r.Task = taskID })
	return e, nil
}

// resumeRolling continues a run that was interrupted by a restart of the controller. It runs once at start.
func (s *Server) resumeRolling() {
	run, ok := s.latestRoll()
	if !ok || run.Status != "running" {
		return
	}
	run.Resumed++
	log.Printf("rolling update %d was interrupted by a restart of the controller; continuing", run.ID)
	go func() {
		time.Sleep(rollResumeDelay)
		if _, err := s.startRoll(run); err != nil {
			log.Printf("rolling update: cannot continue: %v", err)
		}
	}()
}

func (e *rollExec) execute() error {
	defer func() {
		rolling.Lock()
		rolling.cur = nil
		rolling.Unlock()
	}()
	var err error
	for {
		e.mu.Lock()
		cur := e.run.Cur
		var n *rollNode
		if cur < len(e.run.Nodes) {
			n = e.run.Nodes[cur]
		}
		restoring := e.run.Restoring
		e.mu.Unlock()
		if n == nil {
			break
		}
		if n.Action != "update" {
			e.mutate(func(r *rollRun) { r.Cur++ })
			continue
		}
		if n.Status == "done" {
			e.mutate(func(r *rollRun) { r.Cur++ })
			continue
		}
		if err = e.runNode(n); err != nil {
			break
		}
		if restoring {
			err = errRollCancelled
			break
		}
		e.mutate(func(r *rollRun) { r.Cur++ })
		if e.cancelled() && e.moreWork() {
			err = errRollCancelled
			break
		}
	}
	e.finish(err)
	if err != nil && !errors.Is(err, errRollCancelled) {
		return err
	}
	if errors.Is(err, errRollCancelled) {
		return errors.New("cancelled: " + e.snapshot().Condition)
	}
	return nil
}

func (e *rollExec) moreWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := e.run.Cur; i < len(e.run.Nodes); i++ {
		if e.run.Nodes[i].Action == "update" && e.run.Nodes[i].Status != "done" {
			return true
		}
	}
	return false
}

// finish records how the run ended and tells the user.
func (e *rollExec) finish(err error) {
	e.mutate(func(r *rollRun) {
		r.Finished = time.Now().Unix()
		switch {
		case err == nil:
			r.Status = "ok"
			r.Condition = ""
		case errors.Is(err, errRollCancelled):
			r.Status = "cancelled"
			r.Condition = rollCondition(r)
			if r.Restoring {
				r.Condition = "Put back. " + rollCondition(r)
			}
		default:
			r.Status, r.Error = "failed", err.Error()
			r.Condition = rollCondition(r)
		}
		r.Cancel = false
		r.Summary = rollSummary(r)
	})
	run := e.snapshot()
	title, sev := "Rolling update finished", "warning"
	switch run.Status {
	case "cancelled":
		title = "Rolling update stopped"
	case "failed":
		title = "Rolling update failed"
		sev = "critical"
	}
	msg := run.Summary
	if run.Status != "ok" {
		msg = strings.TrimSpace(run.Error + " " + run.Condition)
	}
	e.s.dispatchAlerts([]Alert{{ID: fmt.Sprintf("rolling-done:%d", run.ID), Kind: "rolling-update", Severity: sev, Title: title, Message: msg,
		Link: "/datacenter/nodes", Since: time.Now().Unix()}}, nil)
	e.s.Nodes.Publish(cluster.Event{Type: "node", Detail: "rolling"})
}

func rollSummary(r *rollRun) string {
	var done, skipped, current []string
	for _, n := range r.Nodes {
		switch n.Status {
		case "done":
			done = append(done, n.Name)
		case "skipped":
			skipped = append(skipped, n.Name)
		case "current":
			current = append(current, n.Name)
		}
	}
	var parts []string
	if len(done) > 0 {
		parts = append(parts, fmt.Sprintf("Updated: %s.", strings.Join(done, ", ")))
	}
	if len(current) > 0 {
		parts = append(parts, fmt.Sprintf("Already up to date: %s.", strings.Join(current, ", ")))
	}
	if len(skipped) > 0 {
		parts = append(parts, fmt.Sprintf("Left out because virtual machines cannot move: %s.", strings.Join(skipped, ", ")))
	}
	return strings.Join(parts, " ")
}

// rollStepCounts counts the steps that will run or have run on the nodes being updated. A skipped step (it does not
// apply to the node) is in neither number, so "7 of 20" never counts work that was never going to happen.
func rollStepCounts(nodes []*rollNode) (total, done int) {
	for _, n := range nodes {
		if n.Action != "update" {
			continue
		}
		for _, s := range n.Steps {
			if s.Status == "skipped" {
				continue
			}
			total++
			if s.Status == "ok" {
				done++
			}
		}
	}
	return total, done
}

func (e *rollExec) updateProgress() {
	if e.progress == nil {
		return
	}
	e.mu.Lock()
	total, done := rollStepCounts(e.run.Nodes)
	e.mu.Unlock()
	if total > 0 {
		e.progress(float64(done) / float64(total))
	}
}

// runNode runs the open steps of one node.
func (e *rollExec) runNode(n *rollNode) error {
	e.mutate(func(r *rollRun) { n.Status = "running" })
	// A node that is not part of the run must be there before this one is touched.
	if !e.touched(n) {
		for _, o := range e.s.Nodes.Status() {
			if o.ID != n.Node && o.State != "online" {
				name := o.Hostname
				if name == "" {
					name = o.Addr
				}
				return fmt.Errorf("%s is offline, so %s was not touched. Bring %s back and press Continue", name, n.Name, name)
			}
		}
	}
	for {
		e.mu.Lock()
		idx := rollNextStep(n.Steps)
		var step rollStep
		if idx >= 0 {
			step = n.Steps[idx]
		}
		e.mu.Unlock()
		if idx < 0 {
			break
		}
		if e.cancelled() {
			return errRollCancelled
		}
		e.mutate(func(r *rollRun) { n.Steps[idx].Status, n.Steps[idx].Detail = "running", "" })
		detail, skip, err := e.phase(n, step.ID)
		if err != nil {
			if errors.Is(err, errRollCancelled) {
				e.mutate(func(r *rollRun) { n.Steps[idx].Status, n.Steps[idx].Detail = stepStatusPend, "" })
				return err
			}
			e.mutate(func(r *rollRun) {
				n.Steps[idx].Status, n.Steps[idx].Detail = "failed", err.Error()
				n.Status = "failed"
			})
			return fmt.Errorf("%s: %s: %w", n.Name, strings.ToLower(step.Title[:1])+step.Title[1:], err)
		}
		e.mutate(func(r *rollRun) {
			if skip {
				n.Steps[idx].Status = "skipped"
			} else {
				n.Steps[idx].Status = "ok"
			}
			n.Steps[idx].Detail = detail
		})
		e.updateProgress()
	}
	e.mutate(func(r *rollRun) {
		n.Status = "done"
		if r.Restoring {
			// put back before it was updated (or after a part of it): it is not counted as updated
			n.Action, n.Status, n.SkipWhy = "skip", "skipped", "the update was stopped and the node was put back"
		}
	})
	return nil
}

// touched says whether any step of the node has run.
func (e *rollExec) touched(n *rollNode) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range n.Steps {
		if s.Status == "ok" || s.Status == "failed" {
			return true
		}
	}
	return false
}

func (e *rollExec) phase(n *rollNode, id string) (detail string, skip bool, err error) {
	switch id {
	case stepMaint:
		return e.pMaint(n)
	case stepEvacuate:
		return e.pEvacuate(n)
	case stepStop:
		return e.pStop(n)
	case stepUpdate:
		return e.pUpdate(n)
	case stepReboot:
		return e.pReboot(n)
	case stepWait:
		return e.pWait(n)
	case stepVerify:
		return e.pVerify(n)
	case stepMaintOff:
		return e.pMaintOff(n)
	case stepStart:
		return e.pStart(n)
	case stepReturn:
		return e.pReturn(n)
	}
	return "", false, fmt.Errorf("unknown step %q", id)
}

// hostWait returns the connection to a node, waiting for it to be online.
func (e *rollExec) hostWait(id int64, max time.Duration) (*hostconn.Host, error) {
	deadline := time.Now().Add(max)
	for {
		if h, err := e.s.Nodes.Host(id); err == nil {
			return h, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the node does not answer (waited %s)", max.Round(time.Second))
		}
		if e.cancelled() {
			return nil, errRollCancelled
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *Server) setMaintenanceOn(id int64, on bool) error {
	if err := s.Store.SetMaintenance(id, on); err != nil {
		return err
	}
	s.Nodes.SetMaintenance(id, on)
	s.Nodes.Publish(cluster.Event{Type: "node", Node: id, Detail: "maintenance"})
	return nil
}

// activeNames lists the names of the active pools and networks of a node.
func activeNames(h *hostconn.Host) (pools, nets []string) {
	if ps, _, err := h.ConnectListAllStoragePools(1, 0); err == nil {
		for _, p := range ps {
			if a, err := h.StoragePoolIsActive(p); err == nil && a != 0 {
				pools = append(pools, p.Name)
			}
		}
	}
	if ns, _, err := h.ConnectListAllNetworks(1, 0); err == nil {
		for _, nw := range ns {
			if a, err := h.NetworkIsActive(nw); err == nil && a != 0 {
				nets = append(nets, nw.Name)
			}
		}
	}
	sort.Strings(pools)
	sort.Strings(nets)
	return
}

func (e *rollExec) pMaint(n *rollNode) (string, bool, error) {
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	if len(n.PoolsBefore) == 0 && len(n.NetsBefore) == 0 {
		p, nw := activeNames(h)
		e.mutate(func(r *rollRun) { n.PoolsBefore, n.NetsBefore = p, nw })
	}
	if err := e.s.setMaintenanceOn(n.Node, true); err != nil {
		return "", false, err
	}
	e.mutate(func(r *rollRun) { n.MaintSet = true })
	return "no VM is moved onto it from now on", false, nil
}

// livePlan plans the evacuation of the node now. The node counts as having updates so that it is planned.
func (e *rollExec) livePlan(n *rollNode) (rollNodePlan, rollSnapshot) {
	snap := e.s.rollGather(false, map[int64]bool{n.Node: true}, n.Node)
	done := map[int64]bool{}
	e.mu.Lock()
	for _, o := range e.run.Nodes {
		if o.Status == "done" {
			done[o.Node] = true
		}
	}
	names := n.UpdateNames
	e.mu.Unlock()
	for i := range snap.Infos {
		if snap.Infos[i].ID == n.Node {
			snap.Infos[i].Pending = names
			if len(names) == 0 {
				snap.Infos[i].Pending = []string{"-"}
			}
			snap.Infos[i].Maintenance = false // planned as if it were not in maintenance yet; it is the source
		}
	}
	in := rollPlanIn{Nodes: snap.Infos, Select: map[int64]bool{n.Node: true}, Choice: rollChoiceShutdown, Done: done}
	in.Check = func(vm evacVM, from int64, to evacTarget) []string {
		h, dom := snap.Hosts[from], snap.Doms[from][vm.UUID]
		if h == nil {
			return []string{"the node cannot be read"}
		}
		_, c := e.s.preflight(from, h, dom, migReq{Target: to.ID})
		return blockingReasons(c)
	}
	p := planRolling(in)
	for _, np := range p.Nodes {
		if np.Node == n.Node {
			return np, snap
		}
	}
	return rollNodePlan{}, snap
}

func (e *rollExec) migrateVM(srcID int64, src *hostconn.Host, dom libvirt.Domain, target int64) error {
	l, pre, err := e.s.launchMigration(userRequest(e.run.User), srcID, src, dom, migReq{Target: target})
	if errors.Is(err, errPreflight) {
		return errors.New("pre-flight failed: " + strings.Join(blockingReasons(pre.Checks), "; "))
	}
	if err != nil {
		return err
	}
	return <-l.Done
}

func (e *rollExec) pEvacuate(n *rollNode) (string, bool, error) {
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	np, snap := e.livePlan(n)
	known := map[string]bool{}
	for _, v := range n.Stuck {
		known[v.UUID] = true
	}
	for _, st := range np.Stuck {
		if !known[st.VM.UUID] {
			e.mu.Lock()
			interrupted, toName, toID := false, "the target node", int64(0)
			for _, m := range n.Moved {
				if m.UUID == st.VM.UUID && m.Status == "moving" {
					interrupted, toID = true, m.To
					if m.ToName != "" {
						toName = m.ToName
					}
				}
			}
			e.mu.Unlock()
			if interrupted {
				resumed := e.settleInterruptedMove(h, st.VM.UUID, toID)
				return "", false, interruptedMoveError(st.VM.Name, toName, st.Reasons, resumed)
			}
			return "", false, fmt.Errorf("%s can no longer be moved (%s). The node itself was not changed except that it is in maintenance", st.VM.Name, strings.Join(st.Reasons, "; "))
		}
	}
	moved := 0
	for _, mv := range np.Moves {
		if e.cancelled() {
			return "", false, errRollCancelled
		}
		dom, ok := snap.Doms[n.Node][mv.VM.UUID]
		if !ok {
			continue
		}
		e.mutate(func(r *rollRun) {
			for i := range n.Moved {
				if n.Moved[i].UUID == mv.VM.UUID {
					n.Moved[i].To, n.Moved[i].ToName, n.Moved[i].Status = mv.To, mv.ToName, "moving"
					return
				}
			}
			n.Moved = append(n.Moved, rollMoved{UUID: mv.VM.UUID, Name: mv.VM.Name, To: mv.To, ToName: mv.ToName, Status: "moving"})
		})
		if err := e.migrateVM(n.Node, h, dom, mv.To); err != nil {
			return "", false, fmt.Errorf("moving %s to %s failed: %w", mv.VM.Name, mv.ToName, err)
		}
		moved++
		e.mutate(func(r *rollRun) {
			for i := range n.Moved {
				if n.Moved[i].UUID == mv.VM.UUID {
					n.Moved[i].Status = "moved"
				}
			}
		})
	}
	e.mutate(func(r *rollRun) {
		for i := range n.Moved {
			if n.Moved[i].Status == "moving" {
				n.Moved[i].Status = "moved"
			}
		}
	})
	total := 0
	e.mu.Lock()
	total = len(n.Moved)
	e.mu.Unlock()
	if total == 0 {
		return "no virtual machine was running on it", true, nil
	}
	return fmt.Sprintf("%d virtual machine(s) moved away", total), false, nil
}

// interruptedMoveError is the stop message after a move that a controller restart cut short. The pre-flight
// reasons name the exact file ("already exists on NODE at PATH"). It is not removed automatically: nothing
// recorded survives the restart that proves the file is the partial copy and not someone's own file.
func interruptedMoveError(vm, toName string, reasons []string, resumed bool) error {
	here := "The VM still runs here."
	if resumed {
		here = "The VM was paused by the move and runs here again."
	}
	return fmt.Errorf("the move of %s was interrupted (the controller stopped) and left a partly copied disk on %s. %s. %s Remove that file on %s yourself, then press Continue", vm, toName, strings.Join(reasons, "; "), here, toName)
}

// resumeAfterInterruptedMove is true when a VM that is paused only because a live move was cut off (the controller
// stopped at the end of the memory copy) must be resumed on its source: it is paused for the migration and the copy
// on the target does not run.
func resumeAfterInterruptedMove(state libvirt.DomainState, reason int32, targetRunning bool) bool {
	return state == libvirt.DomainPaused && libvirt.DomainPausedReason(reason) == libvirt.DomainPausedMigration && !targetRunning
}

// settleInterruptedMove clears what a move that the controller's stop cut off leaves behind: the paused incoming
// domain on the target (only a paused one that is not defined there, as a failed move does) and a source VM that
// libvirt left paused. It reports whether the source was resumed. A target copy that runs is never touched, and then
// the source is not resumed either.
func (e *rollExec) settleInterruptedMove(src *hostconn.Host, uuid string, to int64) bool {
	u, err := cluster.ParseUUID(uuid)
	if err != nil {
		return false
	}
	sd, err := src.DomainLookupByUUID(u)
	if err != nil {
		return false
	}
	state, reason, err := src.DomainGetState(sd, 0)
	if err != nil {
		return false
	}
	targetRunning := false
	if dst, err := e.s.Nodes.Host(to); err == nil {
		if dd, err := dst.DomainLookupByUUID(u); err == nil {
			ds, _, err := dst.DomainGetState(dd, 0)
			dstate := libvirt.DomainState(ds)
			targetRunning = err != nil || (dstate != libvirt.DomainPaused && dstate != libvirt.DomainShutoff)
			if !targetRunning && dstate == libvirt.DomainPaused {
				if persistent, err := dst.DomainIsPersistent(dd); err == nil && persistent == 0 {
					_ = dst.DomainDestroy(dd)
				}
			}
		}
	}
	if !resumeAfterInterruptedMove(libvirt.DomainState(state), reason, targetRunning) {
		return false
	}
	if err := src.DomainResume(sd); err != nil {
		log.Printf("rolling update: resuming %s after the interrupted move: %v", uuid, err)
		return false
	}
	return true
}

func (e *rollExec) pStop(n *rollNode) (string, bool, error) {
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	e.mu.Lock()
	stuck := append([]evacVM(nil), n.Stuck...)
	e.mu.Unlock()
	stopped := 0
	for _, v := range stuck {
		dom, err := h.DomainLookupByUUID(mustUUID(v.UUID))
		if err != nil {
			continue
		}
		state, _, err := h.DomainGetState(dom, 0)
		if err != nil {
			return "", false, err
		}
		if libvirt.DomainState(state) == libvirt.DomainShutoff {
			continue
		}
		e.mutate(func(r *rollRun) {
			for _, d := range n.Down {
				if d.UUID == v.UUID {
					return
				}
			}
			n.Down = append(n.Down, rollDown{UUID: v.UUID, Name: v.Name})
		})
		if err := h.DomainShutdown(dom); err != nil {
			return "", false, fmt.Errorf("cannot shut down %s: %w", v.Name, err)
		}
		deadline := time.Now().Add(rollShutdownWait)
		for {
			st, _, err := h.DomainGetState(dom, 0)
			if err == nil && libvirt.DomainState(st) == libvirt.DomainShutoff {
				break
			}
			if time.Now().After(deadline) {
				return "", false, fmt.Errorf("%s on %s did not shut down within %s. It is still running; shut it down yourself (a guest agent lets it shut down cleanly) and press Continue", v.Name, n.Name, rollShutdownWait)
			}
			time.Sleep(2 * time.Second)
		}
		stopped++
	}
	return fmt.Sprintf("%d virtual machine(s) shut down; they are started again at the end", len(n.Down)), stopped+len(n.Down) == 0, nil
}

func mustUUID(s string) [16]byte {
	u, _ := cluster.ParseUUID(s)
	return u
}

func (e *rollExec) readStatus(h *hostconn.Host) (nodeproto.UpdateStatus, error) {
	out, err := h.RunNode("update-status")
	var st nodeproto.UpdateStatus
	if err != nil {
		return st, fmt.Errorf("%s", strings.TrimSpace(out+" "+err.Error()))
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return st, fmt.Errorf("unreadable update state: %s", firstLine(out))
	}
	return st, nil
}

func (e *rollExec) pUpdate(n *rollNode) (string, bool, error) {
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	start := func() error {
		// Record before starting: a controller that stops between the two finds the start recorded, and the
		// update on the node running or not started (updateVerdict); recorded after, a restart would try to
		// start a second update ("a package update is already running").
		e.mutate(func(r *rollRun) { n.UpdateStarted, n.UpdateAt = true, time.Now().Unix() })
		if out, err := h.RunNode("update-start"); err != nil {
			e.mutate(func(r *rollRun) { n.UpdateStarted = false })
			return fmt.Errorf("the update could not be started: %s", strings.TrimSpace(out))
		}
		return nil
	}
	e.mu.Lock()
	started, at := n.UpdateStarted, n.UpdateAt
	e.mu.Unlock()
	if !started {
		if err := start(); err != nil {
			return "", false, err
		}
		e.mu.Lock()
		at = n.UpdateAt
		e.mu.Unlock()
	}
	deadline := time.Now().Add(rollUpdateMax)
	errSince := time.Time{}
	for {
		if time.Now().After(deadline) {
			return "", false, fmt.Errorf("the update ran for more than %s; look at /var/lib/virticle-update/update.log on the node", rollUpdateMax)
		}
		if h2, err := e.s.Nodes.Host(n.Node); err == nil {
			h = h2
		}
		st, err := e.readStatus(h)
		if err != nil {
			if errSince.IsZero() {
				errSince = time.Now()
			}
			if time.Since(errSince) > 5*time.Minute {
				return "", false, fmt.Errorf("the node does not answer while it updates: %w", err)
			}
			time.Sleep(rollPoll)
			continue
		}
		errSince = time.Time{}
		switch updateVerdict(st, at) {
		case updateStartAgain:
			// An old result, or none: the start was lost (the controller stopped between recording and starting).
			e.mu.Lock()
			at = time.Now().Unix()
			e.mu.Unlock()
			if err := start(); err != nil {
				return "", false, err
			}
		case updateWait:
			time.Sleep(rollPoll)
		case updateFailed:
			// The next Continue must start the update again: the failed result on the node is this run's own, and
			// reading it again would only report the same failure.
			e.mutate(func(r *rollRun) { n.UpdateStarted = false })
			return "", false, errors.New(st.Error + ". Fix the cause on the node (the log is /var/lib/virticle-update/update.log) and press Continue to try the update again")
		default: // ok
			e.mutate(func(r *rollRun) {
				n.Installed, n.HeldBack = len(st.Updated), st.HeldBack
				n.Reboot, n.RebootWhy = st.RebootNeeded, st.RebootWhy
				n.UpdateStarted = true
			})
			d := fmt.Sprintf("%d package(s) installed", len(st.Updated))
			if len(st.Updated) == 0 {
				d = "nothing to install"
			}
			if len(st.HeldBack) > 0 {
				d += fmt.Sprintf("; %d held back by the package tool and not installed (%s)", len(st.HeldBack), strings.Join(firstN(st.HeldBack, 6), ", "))
			}
			return d, false, nil
		}
	}
}

// What pUpdate does with the update state it reads from the node.
const (
	updateStartAgain = "start"
	updateWait       = "wait"
	updateFailed     = "failed"
	updateDone       = "done"
)

// updateVerdict decides from the node's update state; at is when this run started the update.
func updateVerdict(st nodeproto.UpdateStatus, at int64) string {
	switch {
	case st.State == nodeproto.UpdateNone || (st.State != nodeproto.UpdateRunning && st.Started+30 < at):
		return updateStartAgain
	case st.State == nodeproto.UpdateRunning:
		return updateWait
	case st.State == nodeproto.UpdateFailed:
		return updateFailed
	}
	return updateDone
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(append([]string(nil), s[:n]...), "...")
	}
	return s
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func bootID(h *hostconn.Host) (string, error) {
	out, err := h.Run("cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (e *rollExec) pReboot(n *rollNode) (string, bool, error) {
	e.mu.Lock()
	need, sent, id, why := n.Reboot, n.RebootSent, n.BootID, n.RebootWhy
	e.mu.Unlock()
	if !need && !sent {
		return "the updates need no restart", true, nil
	}
	if sent {
		return "restart was sent", false, nil
	}
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	if id == "" {
		if id, err = bootID(h); err != nil {
			return "", false, fmt.Errorf("cannot read the boot id of the node: %w", err)
		}
		e.mutate(func(r *rollRun) { n.BootID = id })
	}
	// Recorded before the command: if the controller stops now, the next start sees the node restarted or not.
	e.mutate(func(r *rollRun) { n.RebootSent, n.RebootAt = true, time.Now().Unix() })
	if out, err := h.RunNode("node-reboot"); err != nil {
		e.mutate(func(r *rollRun) { n.RebootSent = false })
		return "", false, fmt.Errorf("the restart was refused: %s", strings.TrimSpace(out))
	}
	if n.Local {
		return "the machine of the controller restarts now; this page is back when it is up. " + why, false, nil
	}
	return "restarting (" + why + ")", false, nil
}

func (e *rollExec) pWait(n *rollNode) (string, bool, error) {
	e.mu.Lock()
	sent, id, at := n.RebootSent, n.BootID, n.RebootAt
	e.mu.Unlock()
	if !sent {
		// No restart: the node must still answer after the update (libvirt may have been restarted).
		if _, err := e.hostWait(n.Node, rollOnlineWait); err != nil {
			return "", false, err
		}
		return "the node was not restarted and answers", true, nil
	}
	deadline := time.Unix(at, 0).Add(rollWaitBack)
	if min := time.Now().Add(rollOnlineWait); deadline.Before(min) {
		deadline = min // after a resume the node is given time to connect again (the controller backs off while a node is away)
	}
	for {
		if h, err := e.s.Nodes.Host(n.Node); err == nil {
			if cur, err := bootID(h); err == nil && cur != id {
				return "the node is back (restarted " + time.Since(time.Unix(at, 0)).Round(time.Second).String() + " after the command)", false, nil
			}
		}
		if time.Now().After(deadline) {
			return "", false, fmt.Errorf("%s did not come back within %s after the restart. Look at the machine (console, power). It is in maintenance and nothing was moved onto it. When it is up, press Continue", n.Name, rollWaitBack)
		}
		if e.cancelled() {
			return "", false, errRollCancelled
		}
		time.Sleep(rollPoll)
	}
}

func (e *rollExec) pVerify(n *rollNode) (string, bool, error) {
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	var rows []rollCheck
	add := func(status, id, detail string) { rows = append(rows, rollCheck{status, id, detail}) }
	if v, err := h.ConnectGetLibVersion(); err != nil {
		add("problem", "libvirt-connection", "libvirt does not answer: "+firstLine(err.Error()))
	} else {
		add("ok", "libvirt-connection", fmt.Sprintf("libvirt %d.%d.%d answers", v/1000000, v/1000%1000, v%1000))
	}
	det, note, err := h.DetectPrivilege()
	switch {
	case err != nil:
		add("problem", "node-command", "cannot ask the node command: "+err.Error())
	case det == hostconn.PrivNone:
		add("problem", "node-command", note)
	default:
		add("ok", "node-command", "the node command answers with protocol "+hostconn.NodeCmdVersion)
	}
	if h.CanRoot() {
		out, err := h.RunNode("selfcheck", "repair")
		sc := nodeproto.ParseCheckRows(out)
		if len(sc) == 0 {
			add("problem", "selfcheck", "the node's own check did not answer: "+strings.TrimSpace(out+" "+fmt.Sprint(err)))
		}
		for _, r := range sc {
			add(r.Status, r.ID, r.Detail)
		}
	}
	e.mu.Lock()
	wantPools, wantNets := append([]string(nil), n.PoolsBefore...), append([]string(nil), n.NetsBefore...)
	e.mu.Unlock()
	rows = append(rows, e.checkPoolsNets(h, wantPools, wantNets)...)
	bad := 0
	var badText []string
	for _, r := range rows {
		if r.Status == "problem" {
			bad++
			badText = append(badText, r.ID+": "+r.Detail)
		}
	}
	e.mutate(func(r *rollRun) { n.Checks = rows })
	if bad > 0 {
		return "", false, fmt.Errorf("the node is not healthy: %s. It stays in maintenance. Fix this, then press Continue", strings.Join(badText, "; "))
	}
	fixed := 0
	for _, r := range rows {
		if r.Status == "fixed" {
			fixed++
		}
	}
	d := fmt.Sprintf("%d checks passed", len(rows))
	if fixed > 0 {
		d += fmt.Sprintf("; %d thing(s) the update had changed were put right", fixed)
	}
	return d, false, nil
}

// checkPoolsNets looks at the pools and networks that were active before the update: they must be active again
// (a stopped one is started once). Mounts of shared storage must answer.
func (e *rollExec) checkPoolsNets(h *hostconn.Host, pools, nets []string) []rollCheck {
	var rows []rollCheck
	deadline := time.Now().Add(rollPoolWait)
	for _, name := range pools {
		ok, started := false, false
		for {
			p, err := h.StoragePoolLookupByName(name)
			if err == nil {
				if a, _ := h.StoragePoolIsActive(p); a != 0 {
					ok = true
					break
				}
				if !started {
					started = true
					h.StoragePoolCreate(p, 0)
					continue
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		switch {
		case ok && started:
			rows = append(rows, rollCheck{"fixed", "pool:" + name, "the storage pool " + name + " was stopped after the update and is started again"})
		case ok:
			rows = append(rows, rollCheck{"ok", "pool:" + name, "the storage pool " + name + " is active"})
		default:
			rows = append(rows, rollCheck{"problem", "pool:" + name, "the storage pool " + name + " is not active. Look at its page under Storage"})
		}
	}
	for _, name := range nets {
		nw, err := h.NetworkLookupByName(name)
		active := false
		if err == nil {
			if a, _ := h.NetworkIsActive(nw); a != 0 {
				active = true
			} else if h.NetworkCreate(nw) == nil {
				rows = append(rows, rollCheck{"fixed", "network:" + name, "the network " + name + " was stopped after the update and is started again"})
				continue
			}
		}
		if active {
			rows = append(rows, rollCheck{"ok", "network:" + name, "the network " + name + " is active"})
		} else {
			rows = append(rows, rollCheck{"problem", "network:" + name, "the network " + name + " is not active. Look at its page under Network"})
		}
	}
	// Shared storage: every active NFS pool must answer on its mount.
	var mounts []string
	if ps, _, err := h.ConnectListAllStoragePools(1, 0); err == nil {
		for _, p := range ps {
			if a, _ := h.StoragePoolIsActive(p); a == 0 {
				continue
			}
			var x poolXML
			if raw, err := h.StoragePoolGetXMLDesc(p, 0); err == nil {
				xml.Unmarshal([]byte(raw), &x)
			}
			if x.Type == "netfs" && x.Target.Path != "" {
				mounts = append(mounts, x.Target.Path)
			}
		}
	}
	if len(mounts) > 0 {
		nfsProber.forget(h)
		st := nfsProber.probe(h, mounts)
		for _, m := range mounts {
			if st[m] == mountUp {
				rows = append(rows, rollCheck{"ok", "shared:" + m, "the shared storage at " + m + " is mounted and answers"})
			} else {
				rows = append(rows, rollCheck{"problem", "shared:" + m, "the shared storage at " + m + " does not answer (" + string(st[m]) + "). Check the NFS server and the network"})
			}
		}
	}
	return rows
}

func (e *rollExec) pMaintOff(n *rollNode) (string, bool, error) {
	e.mu.Lock()
	set := n.MaintSet
	e.mu.Unlock()
	if !set {
		return "it was not in maintenance because of this run", true, nil
	}
	if err := e.s.setMaintenanceOn(n.Node, false); err != nil {
		return "", false, err
	}
	e.mutate(func(r *rollRun) { n.MaintSet = false })
	return "the node takes VMs again", false, nil
}

func (e *rollExec) pStart(n *rollNode) (string, bool, error) {
	e.mu.Lock()
	down := append([]rollDown(nil), n.Down...)
	e.mu.Unlock()
	if len(down) == 0 {
		return "no virtual machine was shut down", true, nil
	}
	h, err := e.hostWait(n.Node, rollOnlineWait)
	if err != nil {
		return "", false, err
	}
	for _, d := range down {
		dom, err := h.DomainLookupByUUID(mustUUID(d.UUID))
		if err != nil {
			return "", false, fmt.Errorf("cannot find %s on the node: %w", d.Name, err)
		}
		if st, _, err := h.DomainGetState(dom, 0); err == nil && libvirt.DomainState(st) == libvirt.DomainShutoff {
			if err := nfsStartHint(h, h.DomainCreate(dom)); err != nil {
				return "", false, fmt.Errorf("cannot start %s: %w", d.Name, err)
			}
		}
		name := d.Name
		e.mutate(func(r *rollRun) {
			for i := range n.Down {
				if n.Down[i].Name == name {
					n.Down[i].Started = true
				}
			}
		})
	}
	return fmt.Sprintf("%d virtual machine(s) started", len(down)), false, nil
}

func (e *rollExec) pReturn(n *rollNode) (string, bool, error) {
	e.mu.Lock()
	list := append([]rollMoved(nil), n.Moved...)
	e.mu.Unlock()
	if len(list) == 0 {
		return "nothing was moved", true, nil
	}
	back := 0
	for _, m := range list {
		if m.Status == "back" || m.Status == "stay" {
			continue
		}
		if e.cancelled() {
			return "", false, errRollCancelled
		}
		set := func(status, note string) {
			e.mutate(func(r *rollRun) {
				for i := range n.Moved {
					if n.Moved[i].UUID == m.UUID {
						n.Moved[i].Status, n.Moved[i].Note = status, note
					}
				}
			})
		}
		at, h, dom, ok := e.s.findVM(m.UUID)
		switch {
		case !ok:
			return "", false, fmt.Errorf("%s cannot be found on any online node", m.Name)
		case at == n.Node:
			set("back", "")
			back++
		case at != m.To:
			set("stay", "it was moved by someone else")
		default:
			if err := e.migrateVM(at, h, dom, n.Node); err != nil {
				return "", false, fmt.Errorf("moving %s back to %s failed: %w. It keeps running on %s", m.Name, n.Name, err, m.ToName)
			}
			set("back", "")
			back++
		}
	}
	return fmt.Sprintf("%d virtual machine(s) moved back", back), false, nil
}

// findVM looks for a VM on every online node.
func (s *Server) findVM(uuid string) (int64, *hostconn.Host, libvirt.Domain, bool) {
	u, err := cluster.ParseUUID(uuid)
	if err != nil {
		return 0, nil, libvirt.Domain{}, false
	}
	for _, n := range s.Nodes.Status() {
		if n.State != "online" {
			continue
		}
		h, err := s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		if d, err := h.DomainLookupByUUID(u); err == nil && !isReplicaOrStale(h, d) {
			// A replica or a stale copy has the identity of the VM but is not it: after a move to the standby
			// the old node holds the replica, and "found there" must not count as "back".
			return n.ID, h, d, true
		}
	}
	return 0, nil, libvirt.Domain{}, false
}

// --- alerts ---

// rollingAlert is the alert about the newest run: a run that failed or was stopped and has not been continued,
// put back or dismissed.
func (s *Server) rollingAlert() *Alert {
	r, ok := s.latestRoll()
	if !ok || r.Ack || (r.Status != "failed" && r.Status != "cancelled") {
		return nil
	}
	a := Alert{ID: "rolling-update", Kind: "rolling-update", Severity: "warning", Link: "/datacenter/nodes", Since: r.Finished}
	if r.Status == "failed" {
		a.Severity = "critical"
		a.Title = "The rolling update stopped after a failure"
		a.Message = strings.TrimSpace(r.Error + " " + r.Condition + " Open Datacenter, Nodes to continue or put things back.")
	} else {
		a.Title = "The rolling update was stopped"
		a.Message = strings.TrimSpace(r.Condition + " Open Datacenter, Nodes to continue or put things back.")
	}
	return &a
}

var _ = store.ErrNotFound
