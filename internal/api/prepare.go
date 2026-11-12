package api

// "Prepare this node" in the web UI: the same steps as "virticle node prepare"
// (internal/prepare), run as a background task. The administrator's password
// or key arrives in the request body, lives in the closure of the task and is
// dropped when it ends. It is never stored, logged, audited (the audit log
// records no bodies) or written into the task record or the result rows.

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"virticle/internal/prepare"
)

type prepareReq struct {
	Addr       string `json:"addr"`
	Jump       string `json:"jump"`
	AdminUser  string `json:"adminUser"`
	Password   string `json:"password"`   // secret: used for this run only
	PrivateKey string `json:"privateKey"` // secret: used for this run only
	CheckOnly  bool   `json:"checkOnly"`

	User        string   `json:"user"`
	UseExisting bool     `json:"useExisting"`
	Confirm     bool     `json:"confirm"`
	AllowRoot   bool     `json:"allowRoot"`
	From        string   `json:"from"`
	Extras      []string `json:"extras"`
}

// prepJob is what the UI reads while and after a run: rows only.
type prepJob struct {
	mu    sync.Mutex
	rows  []prepare.Row
	done  bool
	ok    bool
	check bool
	user  string // the account the controller will log in as
	addr  string
	jump  string
}

type prepJobView struct {
	Rows  []prepare.Row `json:"rows"`
	Done  bool          `json:"done"`
	OK    bool          `json:"ok"`
	Check bool          `json:"checkOnly"`
	User  string        `json:"user"`
	Addr  string        `json:"addr"`
	Jump  string        `json:"jump,omitempty"`
}

func (j *prepJob) set(rows []prepare.Row) {
	j.mu.Lock()
	j.rows = rows
	j.mu.Unlock()
}

var prepJobs = struct {
	sync.Mutex
	m     map[int64]*prepJob
	order []int64
}{m: map[int64]*prepJob{}}

// keepPrepJob remembers a job under its task id; the oldest are forgotten.
func keepPrepJob(id int64, j *prepJob) {
	prepJobs.Lock()
	defer prepJobs.Unlock()
	prepJobs.m[id] = j
	prepJobs.order = append(prepJobs.order, id)
	for len(prepJobs.order) > 20 {
		delete(prepJobs.m, prepJobs.order[0])
		prepJobs.order = prepJobs.order[1:]
	}
}

func (s *Server) prepareStart(w http.ResponseWriter, r *http.Request) {
	var req prepareReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	req.Addr, req.Jump, req.AdminUser = strings.TrimSpace(req.Addr), strings.TrimSpace(req.Jump), strings.TrimSpace(req.AdminUser)
	if req.Addr == "" {
		fail(w, badRequest("address is required"))
		return
	}
	if req.AdminUser == "" {
		req.AdminUser = "root"
	}
	if req.Password == "" && strings.TrimSpace(req.PrivateKey) == "" {
		fail(w, badRequest("give the administrator's password or private key; it is used for this run only and not stored"))
		return
	}
	o := prepare.Options{User: req.User, UseExisting: req.UseExisting, Confirm: req.Confirm, AllowRoot: req.AllowRoot,
		From: strings.TrimSpace(req.From), Extras: req.Extras, ControllerKey: s.PubKey, InstanceID: s.InstanceID}
	if err := o.Validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	admin := prepare.Admin{Addr: req.Addr, User: req.AdminUser, Jump: req.Jump, Password: req.Password}
	if k := strings.TrimSpace(req.PrivateKey); k != "" {
		admin.KeyPEM = []byte(k + "\n")
	}
	// The secrets now live in admin only; drop the request's copies.
	req.Password, req.PrivateKey = "", ""

	job := &prepJob{check: req.CheckOnly, user: o.ServiceUser(), addr: req.Addr, jump: req.Jump}
	kind := "prepare node"
	if req.CheckOnly {
		kind = "check node"
	}
	id, err := s.startBackground(r, kind, req.Addr, "", func(progress func(float64)) error {
		defer func() {
			admin = prepare.Admin{}
			job.mu.Lock()
			job.done = true
			job.mu.Unlock()
		}()
		conn, err := prepare.Dial(s.Nodes, admin)
		if err != nil {
			job.set([]prepare.Row{{ID: "admin", Title: "Administrator access", Status: prepare.Error, Detail: "could not sign in: " + err.Error()}})
			return err
		}
		defer conn.Close()
		run := &prepare.Runner{N: conn, SudoPassword: admin.Password}
		var rows []prepare.Row
		if req.CheckOnly {
			var err error
			rows, _, err = run.Check(o)
			if err != nil && len(rows) == 0 {
				rows = []prepare.Row{{ID: "check", Title: "Check", Status: prepare.Error, Detail: err.Error()}}
			}
		} else {
			rows = run.Prepare(o, func(rs []prepare.Row) {
				job.set(rs)
				progress(progressOf(rs))
			}, func() []prepare.Row { return prepare.Verify(s.Nodes, req.Addr, o.ServiceUser(), req.Jump) })
		}
		job.set(rows)
		job.mu.Lock()
		job.ok = !prepare.HasError(rows)
		ok := job.ok
		job.mu.Unlock()
		if !ok {
			return badRequest("the node was not prepared; see the rows for why")
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	keepPrepJob(id, job)
	fail(w, accepted{Task: id})
}

// progressOf is the share of finished rows among the rows still planned.
func progressOf(rows []prepare.Row) float64 {
	done, total := 0, 0
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "do-") {
			total++
			if r.Status != prepare.Skipped {
				done++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(done) / float64(total+1)
}

func (s *Server) prepareStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("task"), 10, 64)
	if err != nil {
		fail(w, badRequest("bad task id"))
		return
	}
	prepJobs.Lock()
	j := prepJobs.m[id]
	prepJobs.Unlock()
	if j == nil {
		fail(w, badRequest("no such preparation, or the controller was restarted since"))
		return
	}
	j.mu.Lock()
	v := prepJobView{Rows: append([]prepare.Row{}, j.rows...), Done: j.done, OK: j.ok, Check: j.check, User: j.user, Addr: j.addr, Jump: j.jump}
	j.mu.Unlock()
	writeJSON(w, v)
}
