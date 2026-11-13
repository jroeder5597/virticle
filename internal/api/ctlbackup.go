package api

// Backup of the controller itself (see internal/ctlbackup): the Backup page's
// status and the download, and the optional daily copy on this server.
//
// The download hands out the controller's private keys, so it needs a web
// session (not an API token) and the administrator's password again; a user
// with two-step sign-in on also gives a current code (or a recovery code).

import (
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"virticle/internal/buildinfo"
	"virticle/internal/ctlbackup"
)

const (
	cbSettingKey = "controller-backup"
	// CtlBackupKeep is how many daily copies stay on the server.
	ctlBackupKeep = 7
	cbTaskKind    = "controller backup"
)

type cbSettings struct {
	Daily bool `json:"daily"`
}

func (s *Server) cbSettings() cbSettings {
	var c cbSettings
	if v, ok, err := s.Store.WebSetting(cbSettingKey); err == nil && ok {
		json.Unmarshal([]byte(v), &c)
	}
	return c
}

func (s *Server) cbSource() (ctlbackup.Source, error) {
	if s.DataDir == "" {
		return ctlbackup.Source{}, errors.New("the controller has no data directory configured")
	}
	host, _ := os.Hostname()
	return ctlbackup.Source{DataDir: s.DataDir, Snapshot: s.Store.SnapshotTo, App: buildinfo.Version, Host: host}, nil
}

type cbRecordView struct {
	Time      int64  `json:"time"`
	Size      int64  `json:"size,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
	Via       string `json:"via"` // cli | download | daily
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

type cbStatus struct {
	Daily     bool          `json:"daily"`
	Keep      int           `json:"keep"`
	Dir       string        `json:"dir"`
	Last      *cbRecordView `json:"last,omitempty"`      // newest successful backup of any kind
	LastDaily *cbRecordView `json:"lastDaily,omitempty"` // newest daily attempt, also when it failed
	Command   string        `json:"restoreCommand"`
}

func recView(kind string, r ctlbackup.Record) *cbRecordView {
	return &cbRecordView{Time: r.Time.Unix(), Size: r.Size, Encrypted: r.Encrypted, Via: kind, OK: r.OK, Error: r.Error}
}

func (s *Server) ctlBackupStatus(w http.ResponseWriter, r *http.Request) {
	st := cbStatus{Daily: s.cbSettings().Daily, Keep: ctlBackupKeep, Dir: filepath.Join(s.DataDir, "backups"),
		Command: "sudo systemctl stop virticle && sudo virticle restore FILE"}
	recs := ctlbackup.ReadRecords(s.DataDir)
	if k, rec, ok := recs.Newest(); ok {
		st.Last = recView(k, rec)
	}
	if d, ok := recs["daily"]; ok {
		st.LastDaily = recView("daily", d)
	}
	writeJSON(w, st)
}

func (s *Server) putCtlBackup(w http.ResponseWriter, r *http.Request) {
	var req cbSettings
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if s.DataDir == "" {
		fail(w, badRequest("the controller has no data directory configured"))
		return
	}
	was := s.cbSettings().Daily
	b, _ := json.Marshal(req)
	if err := s.Store.SetWebSetting(cbSettingKey, string(b)); err != nil {
		fail(w, err)
		return
	}
	if req.Daily && !was {
		go s.dailyBackupRun() // the first copy at once, so that "on" can be seen to work
	}
	s.ctlBackupStatus(w, r)
}

type cbDownloadReq struct {
	Password   string `json:"password"`
	Code       string `json:"code"` // current two-step code or recovery code, when the user has two-step sign-in on
	Passphrase string `json:"passphrase"`
}

func (s *Server) ctlBackupDownload(w http.ResponseWriter, r *http.Request) {
	p, _, ok := accountSession(w, r)
	if !ok {
		return
	}
	var req cbDownloadReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	src, err := s.cbSource()
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if req.Passphrase != "" {
		if err := ctlbackup.CheckPassphrase(req.Passphrase); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
	}
	key := "pw:" + p.User.Username
	if wait := s.limiter.locked(key); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		deny(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if !s.verifyPassword(p.User, req.Password) {
		s.limiter.fail(key, limPair)
		deny(w, http.StatusForbidden, "the password is not right")
		return
	}
	if p.User.TwoStep {
		good, _, err := s.checkSecondStep(p.User.ID, req.Code)
		if err != nil {
			fail(w, err)
			return
		}
		if !good {
			s.limiter.fail(key, limPair)
			deny(w, http.StatusForbidden, "the two-step code is not right")
			return
		}
	}
	s.limiter.reset(key)

	src.Now = time.Now
	name := ctlbackup.FileName(src.Host, src.Now(), req.Passphrase != "")
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Cache-Control", "no-store")
	cw := &countWriter{w: w}
	var out io.Writer = cw
	var enc io.Closer
	if req.Passphrase != "" {
		e, err := ctlbackup.Encrypt(cw, req.Passphrase)
		if err != nil {
			fail(w, err)
			return
		}
		out, enc = e, e
	}
	_, err = ctlbackup.Write(out, src)
	if err == nil && enc != nil {
		err = enc.Close()
	}
	if err != nil {
		logf("controller backup download failed: %v", err)
		if cw.n == 0 {
			h.Del("Content-Disposition")
			fail(w, err)
			return
		}
		panic(http.ErrAbortHandler) // part of the file is out: cut the connection so that it cannot pass as complete
	}
	rec := ctlbackup.Record{Time: time.Now(), Size: cw.n, Encrypted: req.Passphrase != "", OK: true}
	if err := ctlbackup.Remember(s.DataDir, "download", rec, nil, nil); err != nil {
		logf("controller backup: recording the download: %v", err)
	}
}

type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// --- the daily copy ---

var cbOnce sync.Once

// startControllerBackup runs the daily copy loop (once per process).
func (s *Server) startControllerBackup() {
	if s.DataDir == "" || s.Store == nil {
		return
	}
	cbOnce.Do(func() {
		d := &dailyBackup{now: time.Now, enabled: func() bool { return s.cbSettings().Daily }, run: s.dailyBackupRun,
			jitter: func() time.Duration { return randomJitter(20 * time.Minute) }}
		go func() {
			for range time.Tick(time.Minute) {
				d.tick()
			}
		}()
	})
}

// dailyBackup decides when the daily copy runs. tick is called every minute.
type dailyBackup struct {
	now     func() time.Time
	enabled func() bool
	run     func() error
	jitter  func() time.Duration
	next    time.Time
}

// retryAfter is the wait after a failed copy.
const cbRetry = 30 * time.Minute

// tick runs the copy when it is due and reports whether it did.
func (d *dailyBackup) tick() bool {
	now := d.now()
	if !d.enabled() {
		d.next = time.Time{}
		return false
	}
	if d.next.IsZero() {
		d.next = ctlbackup.NextDaily(now, d.jitter())
		return false
	}
	if now.Before(d.next) {
		return false
	}
	if err := d.run(); err != nil {
		d.next = now.Add(cbRetry)
	} else {
		d.next = ctlbackup.NextDaily(now, d.jitter())
	}
	return true
}

var cbRunning sync.Mutex

// dailyBackupRun writes one copy into <data>/backups, keeps the newest seven,
// records the result and leaves a task the alerts page turns into an alert
// when the copy failed.
func (s *Server) dailyBackupRun() error {
	cbRunning.Lock()
	defer cbRunning.Unlock()
	t, terr := s.Store.StartTask(cbTaskKind, "controller", "", "scheduler")
	size, err := s.writeDailyBackup()
	if terr == nil {
		s.Store.FinishTask(&t, err)
	}
	rec := ctlbackup.Record{Time: time.Now(), OK: err == nil, Size: size}
	if err != nil {
		rec.Error = err.Error()
		logf("daily controller backup failed: %v", err)
	}
	if e := ctlbackup.Remember(s.DataDir, "daily", rec, nil, nil); e != nil {
		logf("controller backup: recording the result: %v", e)
	}
	return err
}

func (s *Server) writeDailyBackup() (int64, error) {
	src, err := s.cbSource()
	if err != nil {
		return 0, err
	}
	src.Now = time.Now
	dir := filepath.Join(s.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	_, size, err := ctlbackup.WriteFile(filepath.Join(dir, ctlbackup.FileName(src.Host, src.Now(), false)), src, "")
	if err != nil {
		return 0, err
	}
	_, err = ctlbackup.Prune(dir, ctlBackupKeep)
	return size, err
}

// randomJitter is a random wait between 0 and max.
func randomJitter(max time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(max))) }
