package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// Node evacuation: move the VMs of one node to the others. The plan (which VM
// goes where) comes from the pure planner in evacplan.go. Each VM is then
// moved by the ordinary migration code (launchMigration), so every move is a
// normal "migrate" task with the usual pre-flight, progress and rollback.
// A parent "evacuate" task tracks how many VMs are done.

type evacReq struct {
	Targets        []int64  `json:"targets"`
	VMs            []string `json:"vms"`
	IncludeShutOff *bool    `json:"includeShutOff"` // absent: shut-off VMs are included
	Concurrency    int      `json:"concurrency"`
	// Encrypt is passed to every move (see migReq.Encrypt); nil picks
	// encryption where both nodes are set up for it.
	Encrypt *bool `json:"encrypt"`
}

type evacPreview struct {
	Source      string           `json:"source"`
	Targets     []evacTarget     `json:"targets"`
	Assignments []evacAssignment `json:"assignments"`
	Blocked     []evacBlocked    `json:"blocked"`
}

// userRequest builds the request context s.task and launchMigration want for
// work that runs after the HTTP request ended.
func userRequest(user string) *http.Request {
	r, _ := http.NewRequestWithContext(context.WithValue(context.Background(), ctxKey{}, user), http.MethodPost, "/", nil)
	return r
}

func (s *Server) buildEvacPlan(srcID int64, src *hostconn.Host, req evacReq) (evacPreview, map[string]libvirt.Domain, error) {
	out := evacPreview{Source: src.Name, Targets: []evacTarget{}, Assignments: []evacAssignment{}, Blocked: []evacBlocked{}}
	status := s.Nodes.Status()
	byID := map[int64]bool{}
	for _, n := range status {
		byID[n.ID] = true
	}
	want := map[int64]bool{}
	for _, id := range req.Targets {
		if id == srcID {
			return out, nil, badRequest("the node cannot be its own evacuation target")
		}
		if !byID[id] {
			return out, nil, badRequest(fmt.Sprintf("no such node %d", id))
		}
		want[id] = true
	}
	for _, n := range status {
		if n.ID == srcID || (len(want) > 0 && !want[n.ID]) {
			continue
		}
		t := evacTarget{ID: n.ID, Name: n.Hostname, Online: n.State == "online", Maintenance: n.Maintenance}
		if t.Name == "" {
			t.Name = n.Addr
		}
		if t.Online {
			if h, err := s.Nodes.Host(n.ID); err == nil {
				if free, err := h.NodeGetFreeMemory(); err == nil {
					t.FreeKiB = free / 1024
				} else {
					t.Online = false
				}
			} else {
				t.Online = false
			}
		}
		out.Targets = append(out.Targets, t)
	}

	doms, _, err := src.ConnectListAllDomains(1, 0)
	if err != nil {
		return out, nil, err
	}
	domByUUID := map[string]libvirt.Domain{}
	var all []evacVM
	for _, d := range doms {
		uuid := cluster.FormatUUID(d.UUID)
		state, maxMem, _, _, _, err := src.DomainGetInfo(d)
		if err != nil {
			return out, nil, err
		}
		domByUUID[uuid] = d
		all = append(all, evacVM{UUID: uuid, Name: d.Name, State: stateName(state), MemoryKiB: maxMem})
	}
	vms, err := selectEvacVMs(all, req.VMs, req.IncludeShutOff == nil || *req.IncludeShutOff)
	if err != nil {
		return out, nil, badRequest(err.Error())
	}

	check := func(vm evacVM, t evacTarget) []string {
		_, c := s.preflight(srcID, src, domByUUID[vm.UUID], migReq{Target: t.ID, Encrypt: req.Encrypt})
		return blockingReasons(c)
	}
	plan, blocked := planEvacuation(vms, out.Targets, check)
	if plan != nil {
		out.Assignments = plan
	}
	if blocked != nil {
		out.Blocked = blocked
	}
	return out, domByUUID, nil
}

func (s *Server) evacuateCheck(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req evacReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	plan, _, err := s.buildEvacPlan(id, h, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, plan)
}

type evacResult struct {
	UUID       string   `json:"uuid"`
	Name       string   `json:"name"`
	State      string   `json:"state"`
	Target     int64    `json:"target,omitempty"`
	TargetName string   `json:"targetName,omitempty"`
	Status     string   `json:"status"` // pending | running | ok | failed | blocked | skipped
	Error      string   `json:"error,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	Task       int64    `json:"task,omitempty"`
}

type evacRun struct {
	mu        sync.Mutex
	Node      int64         `json:"node"`
	NodeName  string        `json:"nodeName"`
	Task      int64         `json:"task"`
	Started   int64         `json:"started"`
	Finished  int64         `json:"finished,omitempty"`
	Running   bool          `json:"running"`
	Cancelled bool          `json:"cancelled"`
	Results   []*evacResult `json:"results"`
	cancel    atomic.Bool
	encrypt   *bool
}

func (e *evacRun) snapshot() *evacRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := &evacRun{Node: e.Node, NodeName: e.NodeName, Task: e.Task, Started: e.Started, Finished: e.Finished,
		Running: e.Running, Cancelled: e.cancel.Load()}
	for _, r := range e.Results {
		cp := *r
		c.Results = append(c.Results, &cp)
	}
	return c
}

func (e *evacRun) set(r *evacResult, fn func(*evacResult)) {
	e.mu.Lock()
	fn(r)
	e.mu.Unlock()
}

var evacRuns = struct {
	sync.Mutex
	m map[int64]*evacRun
}{m: map[int64]*evacRun{}}

func (s *Server) evacuate(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req evacReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.Concurrency == 0 {
		req.Concurrency = 1
	}
	if req.Concurrency < 1 || req.Concurrency > 4 {
		fail(w, badRequest("concurrency must be between 1 and 4"))
		return
	}
	evacRuns.Lock()
	if old := evacRuns.m[id]; old != nil && old.snapshot().Running {
		evacRuns.Unlock()
		fail(w, badRequest("an evacuation of this node is already running"))
		return
	}
	// Reserve the slot before the (slow) planning so that two requests cannot both start.
	run := &evacRun{Node: id, NodeName: h.Name, Started: time.Now().Unix(), Running: true, encrypt: req.Encrypt}
	evacRuns.m[id] = run
	evacRuns.Unlock()
	release := func() {
		run.mu.Lock()
		run.Running = false
		run.mu.Unlock()
	}

	plan, doms, err := s.buildEvacPlan(id, h, req)
	if err != nil {
		release()
		fail(w, err)
		return
	}
	if len(plan.Assignments) == 0 {
		release()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"error": "no VM can be moved", "plan": plan})
		return
	}
	run.mu.Lock()
	for _, a := range plan.Assignments {
		run.Results = append(run.Results, &evacResult{UUID: a.VM.UUID, Name: a.VM.Name, State: a.VM.State,
			Target: a.Target, TargetName: a.TargetName, Status: "pending"})
	}
	for _, b := range plan.Blocked {
		run.Results = append(run.Results, &evacResult{UUID: b.VM.UUID, Name: b.VM.Name, State: b.VM.State,
			Status: "blocked", Reasons: b.Reasons})
	}
	run.mu.Unlock()

	user := userOf(r)
	total := len(plan.Assignments)
	taskID, _, err := s.startBackgroundWait(r, "evacuate", h.Name, h.Name, func(progress func(float64)) error {
		defer release()
		return s.runEvacuation(user, id, h, doms, run, total, req.Concurrency, progress)
	})
	if err != nil {
		release()
		fail(w, err)
		return
	}
	run.mu.Lock()
	run.Task = taskID
	run.mu.Unlock()
	writeJSON(w, map[string]any{"task": taskID, "plan": plan})
}

func (s *Server) runEvacuation(user string, srcID int64, src *hostconn.Host, doms map[string]libvirt.Domain, run *evacRun, total, conc int, progress func(float64)) error {
	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, conc)
		progMu   sync.Mutex
		finished int
	)
	done := func() {
		progMu.Lock()
		finished++
		progress(float64(finished) / float64(total))
		progMu.Unlock()
	}
	var queue []*evacResult
	run.mu.Lock()
	for _, res := range run.Results {
		if res.Status == "pending" {
			queue = append(queue, res)
		}
	}
	run.mu.Unlock()

	for _, res := range queue {
		sem <- struct{}{} // wait for a free slot
		if run.cancel.Load() {
			<-sem
			break
		}
		wg.Add(1)
		go func(res *evacResult) {
			defer wg.Done()
			defer func() { <-sem }()
			defer done()
			run.set(res, func(x *evacResult) { x.Status = "running" })
			err := s.moveOne(user, srcID, src, doms[res.UUID], res, run)
			run.set(res, func(x *evacResult) {
				if err != nil {
					x.Status, x.Error = "failed", err.Error()
				} else {
					x.Status = "ok"
				}
			})
		}(res)
	}
	wg.Wait()

	run.mu.Lock()
	defer run.mu.Unlock()
	run.Finished = time.Now().Unix()
	var moved, failed, blocked, skipped int
	var problems []string
	for _, res := range run.Results {
		switch res.Status {
		case "ok":
			moved++
		case "failed":
			failed++
			problems = append(problems, res.Name+": "+res.Error)
		case "blocked":
			blocked++
			problems = append(problems, res.Name+" blocked: "+strings.Join(res.Reasons, "; "))
		case "pending":
			res.Status = "skipped"
			skipped++
		}
	}
	if run.cancel.Load() {
		run.Cancelled = true
	}
	if failed == 0 && blocked == 0 && skipped == 0 {
		return nil
	}
	msg := fmt.Sprintf("%d moved, %d failed, %d blocked, %d not started", moved, failed, blocked, skipped)
	if skipped > 0 {
		msg = "cancelled: " + msg
	}
	if len(problems) > 0 {
		msg += " (" + strings.Join(problems, " | ") + ")"
	}
	return errors.New(msg)
}

func (s *Server) moveOne(user string, srcID int64, src *hostconn.Host, dom libvirt.Domain, res *evacResult, run *evacRun) error {
	l, pre, err := s.launchMigration(userRequest(user), srcID, src, dom, migReq{Target: res.Target, Encrypt: run.encrypt})
	if errors.Is(err, errPreflight) {
		reasons := blockingReasons(pre.Checks)
		return errors.New("pre-flight failed: " + strings.Join(reasons, "; "))
	}
	if err != nil {
		return err
	}
	run.set(res, func(x *evacResult) { x.Task = l.Task })
	return <-l.Done
}

func (s *Server) evacuateStatus(w http.ResponseWriter, r *http.Request) {
	id, _, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	evacRuns.Lock()
	run := evacRuns.m[id]
	evacRuns.Unlock()
	if run == nil {
		writeJSON(w, map[string]any{"run": nil})
		return
	}
	writeJSON(w, map[string]any{"run": run.snapshot()})
}

func (s *Server) evacuateCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		fail(w, badRequest("invalid node"))
		return
	}
	evacRuns.Lock()
	run := evacRuns.m[id]
	evacRuns.Unlock()
	if run == nil || !run.snapshot().Running {
		fail(w, badRequest("no evacuation of this node is running"))
		return
	}
	run.cancel.Store(true)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) putMaintenance(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		fail(w, badRequest("invalid node"))
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.SetMaintenance(id, req.On); err != nil {
		fail(w, err)
		return
	}
	s.Nodes.SetMaintenance(id, req.On)
	s.Nodes.Publish(cluster.Event{Type: "node", Node: id, Detail: "maintenance"})
	writeJSON(w, map[string]bool{"maintenance": req.On})
}

// selectEvacVMs applies the request's VM filter. With a filter only the listed
// VMs are returned (an unknown UUID is an error, never silently ignored), and
// they are taken at their word whatever their state. Without a filter every
// VM of the node is returned, shut-off ones only when includeShutOff is set.
func selectEvacVMs(all []evacVM, filter []string, includeShutOff bool) ([]evacVM, error) {
	if len(filter) == 0 {
		var out []evacVM
		for _, v := range all {
			if v.State == "shutoff" && !includeShutOff {
				continue
			}
			out = append(out, v)
		}
		return out, nil
	}
	want := map[string]bool{}
	for _, u := range filter {
		want[strings.ToLower(strings.TrimSpace(u))] = true
	}
	var out []evacVM
	for _, v := range all {
		if want[v.UUID] {
			out = append(out, v)
			delete(want, v.UUID) // what is left in want afterwards is unknown
		}
	}
	for u := range want {
		return nil, errors.New("no VM " + u + " on this node")
	}
	return out, nil
}
