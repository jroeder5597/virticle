package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"virticle/internal/ctlbackup"
)

func backupEnv(t *testing.T) *env {
	e := newEnv(t)
	e.srv.DataDir = t.TempDir()
	os.WriteFile(filepath.Join(e.srv.DataDir, "id_ed25519"), []byte("KEY"), 0o600)
	e.user("ann", admin)
	e.user("bob", operator)
	return e
}

func TestControllerBackupDownload(t *testing.T) {
	e := backupEnv(t)
	post := func(user string, body map[string]any, o ...opt) int {
		return e.do("POST", "/api/controller-backup/download", body, append([]opt{cookieOf(e, user)}, o...)...).Code
	}
	if c := post("bob", map[string]any{"password": testPW}); c != 403 {
		t.Errorf("operator: %d", c)
	}
	if c := post("ann", map[string]any{"password": "wrong password!"}); c != 403 {
		t.Errorf("wrong password: %d", c)
	}
	if c := post("ann", map[string]any{"password": testPW, "passphrase": "short"}); c != 400 {
		t.Errorf("short passphrase: %d", c)
	}
	// an API token of an administrator is not enough
	tok := e.do("POST", "/api/account/tokens", map[string]any{"name": "t", "role": "admin"}, cookieOf(e, "ann"))
	var created struct{ Secret string }
	decode(t, tok, &created)
	if w := e.do("POST", "/api/controller-backup/download", map[string]any{"password": testPW}, bearer(created.Secret)); w.Code != 403 {
		t.Errorf("token: %d", w.Code)
	}

	w := e.do("POST", "/api/controller-backup/download", map[string]any{"password": testPW}, cookieOf(e, "ann"))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "virticle-backup-") {
		t.Fatalf("download: %d %v", w.Code, w.Header())
	}
	m, err := ctlbackup.Extract(bytes.NewReader(w.Body.Bytes()), t.TempDir())
	if err != nil || m.Format != ctlbackup.FormatVersion {
		t.Fatalf("archive: %v %+v", err, m)
	}

	w = e.do("POST", "/api/controller-backup/download", map[string]any{"password": testPW, "passphrase": "a long passphrase"}, cookieOf(e, "ann"))
	if w.Code != 200 || !ctlbackup.IsEncrypted(w.Body.Bytes()) || !strings.HasSuffix(w.Header().Get("Content-Disposition"), `.age"`) {
		t.Fatalf("encrypted download: %d", w.Code)
	}

	var st cbStatus
	decode(t, e.do("GET", "/api/controller-backup", nil, cookieOf(e, "ann")), &st)
	if st.Last == nil || st.Last.Via != "download" || !st.Last.Encrypted || st.Last.Size == 0 || st.Daily {
		t.Errorf("status: %+v", st.Last)
	}
	if c := e.do("GET", "/api/controller-backup", nil, cookieOf(e, "bob")).Code; c != 403 {
		t.Errorf("operator status: %d", c)
	}
}

func TestDailyBackupWritesAndPrunes(t *testing.T) {
	e := backupEnv(t)
	dir := filepath.Join(e.srv.DataDir, "backups")
	os.MkdirAll(dir, 0o700)
	for i := 0; i < 9; i++ {
		// names of earlier days
		name := ctlbackup.FileName("h", time.Date(2026, 1, 1+i, 3, 30, 0, 0, time.UTC), false)
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
	if err := e.srv.dailyBackupRun(); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != ctlBackupKeep {
		t.Errorf("kept %d", len(ents))
	}
	tasks, _ := e.srv.Store.TasksBy("scheduler", 0)
	if len(tasks) != 1 || tasks[0].Kind != cbTaskKind || tasks[0].Status != "ok" {
		t.Errorf("tasks %+v", tasks)
	}
	// a failure leaves a failed task, which the alerts turn into an alert
	e.srv.DataDir = filepath.Join(e.srv.DataDir, "id_ed25519", "nope") // not a directory
	if err := e.srv.dailyBackupRun(); err == nil {
		t.Fatal("expected a failure")
	}
	tasks, _ = e.srv.Store.TasksBy("scheduler", 0)
	_, _, all := newAlertTracker(alertConfig{}).step(alertInput{Jobs: tasks}, time.Now())
	if len(all) != 1 || all[0].Kind != "failed-job" || !strings.Contains(all[0].Title, "daily backup of the controller") {
		t.Errorf("alerts: %+v", all)
	}
}

func TestDailyBackupSchedule(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	enabled, runs, fails := false, 0, false
	d := &dailyBackup{now: func() time.Time { return now }, enabled: func() bool { return enabled },
		jitter: func() time.Duration { return 5 * time.Minute },
		run: func() error {
			runs++
			if fails {
				return os.ErrPermission
			}
			return nil
		}}
	if d.tick() || runs != 0 {
		t.Fatal("ran while off")
	}
	enabled = true
	if d.tick() { // first look: only schedules
		t.Fatal("ran at once")
	}
	if want := time.Date(2026, 6, 11, 3, 35, 0, 0, time.UTC); !d.next.Equal(want) {
		t.Fatalf("next %v", d.next)
	}
	now = time.Date(2026, 6, 11, 3, 34, 0, 0, time.UTC)
	if d.tick() {
		t.Fatal("ran early")
	}
	now = now.Add(time.Minute)
	fails = true
	if !d.tick() || runs != 1 {
		t.Fatal("did not run when due")
	}
	if want := now.Add(cbRetry); !d.next.Equal(want) {
		t.Fatalf("retry at %v", d.next)
	}
	now, fails = d.next, false
	if !d.tick() || runs != 2 {
		t.Fatal("no retry")
	}
	if want := time.Date(2026, 6, 12, 3, 35, 0, 0, time.UTC); !d.next.Equal(want) {
		t.Fatalf("next after success %v", d.next)
	}
	enabled = false
	d.tick()
	if !d.next.IsZero() {
		t.Error("schedule kept while off")
	}
}

// With two-step sign-in on, the download also asks for a current code.
func TestControllerBackupDownloadAsksForTheSecondStep(t *testing.T) {
	e := backupEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.srv.TOTPClock = func() time.Time { return now }
	secret, codes := e.turnOnTwoStep(e.cookies["ann"], now)
	post := func(body map[string]any) int {
		return e.do("POST", "/api/controller-backup/download", body, cookieOf(e, "ann")).Code
	}
	if c := post(map[string]any{"password": testPW}); c != 403 {
		t.Errorf("no code: %d", c)
	}
	if c := post(map[string]any{"password": testPW, "code": "000000"}); c != 403 {
		t.Errorf("wrong code: %d", c)
	}
	if c := post(map[string]any{"password": "wrong password!", "code": codeAt(secret, now.Add(30*time.Second))}); c != 403 {
		t.Errorf("wrong password with a good code: %d", c)
	}
	now = now.Add(60 * time.Second)
	if c := post(map[string]any{"password": testPW, "code": codeAt(secret, now)}); c != 200 {
		t.Errorf("good code: %d", c)
	}
	if c := post(map[string]any{"password": testPW, "code": codes[0]}); c != 200 {
		t.Errorf("recovery code: %d", c)
	}
}
