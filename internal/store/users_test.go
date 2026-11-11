package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// oldSchema is the schema of the first release (commit 43713d5): users had no
// role or disabled column, and there were no token or audit tables.
const oldSchema = `
CREATE TABLE users (
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, pwhash TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE sessions (
  token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires INTEGER NOT NULL);
CREATE TABLE nodes (
  id INTEGER PRIMARY KEY, addr TEXT UNIQUE NOT NULL, user TEXT NOT NULL, jump TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
CREATE TABLE tasks (
  id INTEGER PRIMARY KEY, kind TEXT NOT NULL, target TEXT NOT NULL, node TEXT NOT NULL, username TEXT NOT NULL,
  status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', progress REAL NOT NULL DEFAULT 0,
  started INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0);
`

func TestMigrateOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := bcrypt.GenerateFromPassword([]byte("old password 1"), bcrypt.MinCost)
	for _, q := range []string{oldSchema,
		`INSERT INTO users(username, pwhash, created) VALUES('alice','` + string(h) + `',1), ('bob','` + string(h) + `',2)`,
		`INSERT INTO nodes(addr, user, created) VALUES('n1','root',1)`,
		`INSERT INTO sessions(token, user_id, expires) VALUES('tok',1,` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	// Opening twice proves the migration is idempotent.
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		users, err := s.Users()
		if err != nil || len(users) != 2 {
			t.Fatalf("users: %v %v", users, err)
		}
		for _, u := range users {
			if u.Role != RoleAdmin || u.Disabled {
				t.Errorf("existing user %s became %s disabled=%v, want an enabled admin", u.Username, u.Role, u.Disabled)
			}
		}
		if _, _, err := s.Login("alice", "old password 1"); err != nil {
			t.Errorf("existing password no longer works: %v", err)
		}
		if p, err := s.SessionUser("tok"); err != nil || p.Role != RoleAdmin {
			t.Errorf("existing session: %v %v", p, err)
		}
		if n, _ := s.Nodes(); len(n) != 1 {
			t.Errorf("nodes lost: %v", n)
		}
		if _, _, err := s.CreateToken(users[0].ID, "t", RoleViewer, 0); err != nil {
			t.Errorf("tokens table: %v", err)
		}
		if err := s.AddAudit(AuditEntry{Time: 1, User: "x", IP: "i", Method: "POST", Path: "/p", Status: 200}); err != nil {
			t.Errorf("audit table: %v", err)
		}
		s.Close()
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLastAdminRule(t *testing.T) {
	s := open(t)
	a, _ := s.CreateUser("a", "password one", RoleAdmin)
	b, _ := s.CreateUser("b", "password two", RoleAdmin)
	viewer := RoleViewer
	if err := s.UpdateUser(a.ID, UserUpdate{Role: &viewer}); err != nil {
		t.Fatalf("demote with a second admin: %v", err)
	}
	if err := s.UpdateUser(b.ID, UserUpdate{Role: &viewer}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := s.DeleteUser(b.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("delete last admin: %v", err)
	}
	if err := s.DeleteUser(a.ID); err != nil {
		t.Fatalf("delete a non-admin: %v", err)
	}
}

func TestDisableEndsSessions(t *testing.T) {
	s := open(t)
	s.CreateUser("root", "password one", RoleAdmin)
	u, _ := s.CreateUser("u", "password two", RoleOperator)
	tok, _, err := s.Login("u", "password two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(tok); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := s.UpdateUser(u.ID, UserUpdate{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(tok); err == nil {
		t.Error("session survived disabling")
	}
}

func TestAuditPrune(t *testing.T) {
	s := open(t)
	old := time.Now().Add(-auditMaxAge - time.Hour).Unix()
	for i := 0; i < 5; i++ {
		s.AddAudit(AuditEntry{Time: old, User: "o", IP: "i", Method: "POST", Path: "/old", Status: 200})
	}
	s.AddAudit(AuditEntry{Time: time.Now().Unix(), User: "n", IP: "i", Method: "POST", Path: "/new", Status: 200})
	if err := s.PruneAudit(); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Audit(AuditFilter{})
	if len(e) != 1 || e[0].Path != "/new" {
		t.Fatalf("after prune: %+v", e)
	}
	// The row cap keeps only the newest entries.
	tx, _ := s.db.Begin()
	for i := 0; i < auditMaxRows+10; i++ {
		tx.Exec(`INSERT INTO audit(time, username, ip, method, path, status) VALUES(?,?,?,?,?,?)`, time.Now().Unix(), "u", "i", "POST", "/x", 200)
	}
	tx.Commit()
	s.PruneAudit()
	var n int
	s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n)
	if n != auditMaxRows {
		t.Errorf("rows after cap = %d, want %d", n, auditMaxRows)
	}
}

func TestTokenHashedAtRest(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser("u", "password one", RoleAdmin)
	_, secret, _ := s.CreateToken(u.ID, "t", RoleAdmin, 0)
	var stored string
	s.db.QueryRow(`SELECT hash FROM api_tokens`).Scan(&stored)
	if stored == "" || stored == secret || len(stored) != 64 {
		t.Errorf("stored token form %q", stored)
	}
	if _, err := s.TokenUser(secret); err != nil {
		t.Error(err)
	}
	if _, err := s.TokenUser(stored); err == nil {
		t.Error("the stored hash works as a token")
	}
}

// Tokens issued before the rename start with "vp_"; they are matched by hash
// only, so they keep working.
func TestOldTokenPrefixStillWorks(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser("u", "password one", RoleAdmin)
	const secret = "vp_oldtokenvalue"
	if _, err := s.db.Exec(`INSERT INTO api_tokens(user_id, name, hash, role, created, expires) VALUES(?,?,?,?,?,?)`,
		u.ID, "old", hashToken(secret), RoleAdmin, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenUser(secret); err != nil {
		t.Error(err)
	}
	_, fresh, _ := s.CreateToken(u.ID, "new", RoleAdmin, 0)
	if !strings.HasPrefix(fresh, "virticle_") {
		t.Errorf("new token %q", fresh)
	}
}
