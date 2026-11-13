package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A database from before schema 3 gains the sign-in columns and tables, keeps
// its users as local accounts with two-step sign-in off, and is copied first.
func TestMigrateToSchema3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{oldSchema,
		`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin'`,
		`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
		`INSERT INTO users(username, pwhash, created) VALUES('alice','x',1)`,
		`CREATE TABLE audit (id INTEGER PRIMARY KEY, time INTEGER NOT NULL, username TEXT NOT NULL, token TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL)`,
		`INSERT INTO audit(time, username, ip, method, path, status) VALUES(1,'alice','i','POST','/p',200)`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		u, err := s.UserByName("alice")
		if err != nil || u.Source != SourceLocal || u.TwoStep {
			t.Fatalf("alice: %+v %v", u, err)
		}
		if e, err := s.Audit(AuditFilter{}); err != nil || len(e) != 1 || e[0].Detail != "" {
			t.Fatalf("audit: %v %v", e, err)
		}
		if i == 0 {
			if m, _ := filepath.Glob(path + ".bak-2-*"); len(m) != 1 {
				t.Errorf("no pre-upgrade copy: %v", m)
			}
		}
		s.Close()
	}
}

func TestTwoStepState(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser("u", "password one", RoleOperator)
	hashes := []string{HashRecoveryCode(u.ID, "aaaaa-bbbbb"), HashRecoveryCode(u.ID, "ccccc-ddddd")}
	if err := s.EnableTwoStep(u.ID, "sealed", 100, hashes); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTwoStep(u.ID, "sealed2", 100, hashes); !errors.Is(err, ErrConflict) {
		t.Fatalf("second enable: %v", err)
	}
	got, _ := s.User(u.ID)
	if !got.TwoStep {
		t.Fatal("not on")
	}
	// A step is accepted once; the step that confirmed the secret is spent.
	for step, want := range map[int64]bool{100: false, 99: false} {
		if ok, _ := s.UseTwoStepStep(u.ID, step); ok != want {
			t.Errorf("step %d: %v want %v", step, ok, want)
		}
	}
	for i, st := range []int64{101, 101, 102} {
		if ok, _ := s.UseTwoStepStep(u.ID, st); ok != (i != 1) {
			t.Errorf("step %d (try %d): %v", st, i, ok)
		}
	}
	// Recovery codes work once, in any spelling.
	if ok, _ := s.UseRecoveryCode(u.ID, HashRecoveryCode(u.ID, " AAAAA-bbbbb ")); !ok {
		t.Error("recovery code refused")
	}
	if ok, _ := s.UseRecoveryCode(u.ID, HashRecoveryCode(u.ID, "aaaaa-bbbbb")); ok {
		t.Error("recovery code reused")
	}
	if ok, _ := s.UseRecoveryCode(u.ID, HashRecoveryCode(u.ID+1, "ccccc-ddddd")); ok {
		t.Error("another user's hash matched")
	}
	if n, _ := s.RecoveryCodesLeft(u.ID); n != 1 {
		t.Errorf("left %d", n)
	}
	tok, _ := s.NewSession(u.ID, time.Hour, false)
	if err := s.ResetTwoStep(u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.User(u.ID)
	if got.TwoStep {
		t.Error("still on")
	}
	if _, err := s.SessionUser(tok); err == nil {
		t.Error("a reset must end the user's sessions")
	}
	if n, _ := s.RecoveryCodesLeft(u.ID); n != 0 {
		t.Error("codes survive a reset")
	}
	if ok, _ := s.UseTwoStepStep(u.ID, 500); ok {
		t.Error("a step was accepted while off")
	}
}

func TestSetupOnlySession(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser("u", "password one", RoleOperator)
	tok, _ := s.NewSession(u.ID, time.Hour, true)
	if p, err := s.SessionUser(tok); err != nil || !p.SetupOnly {
		t.Fatalf("%+v %v", p, err)
	}
	s.EndSetupOnly(tok)
	if p, _ := s.SessionUser(tok); p.SetupOnly {
		t.Fatal("still limited")
	}
}

func TestUpsertExternal(t *testing.T) {
	s := open(t)
	local, _ := s.CreateUser("localboss", "password one", RoleAdmin)
	u, err := s.UpsertExternal(SourceOIDC, "iss|sub1", "carol", RoleViewer)
	if err != nil || u.Source != SourceOIDC || u.Role != RoleViewer {
		t.Fatalf("%+v %v", u, err)
	}
	// Role follows the provider at the next sign-in.
	u2, err := s.UpsertExternal(SourceOIDC, "iss|sub1", "carol", RoleAdmin)
	if err != nil || u2.ID != u.ID || u2.Role != RoleAdmin {
		t.Fatalf("%+v %v", u2, err)
	}
	// A rename at the provider keeps the account.
	u3, err := s.UpsertExternal(SourceOIDC, "iss|sub1", "carol.new", RoleAdmin)
	if err != nil || u3.ID != u.ID || u3.Username != "carol.new" {
		t.Fatalf("%+v %v", u3, err)
	}
	// Another subject cannot take a name; neither can a provider take a local name.
	if _, err := s.UpsertExternal(SourceOIDC, "iss|sub2", "carol.new", RoleViewer); !errors.Is(err, ErrConflict) {
		t.Fatalf("name taken by another subject: %v", err)
	}
	if _, err := s.UpsertExternal(SourceLDAP, "cn=x", "localboss", RoleAdmin); !errors.Is(err, ErrConflict) {
		t.Fatalf("local name: %v", err)
	}
	if _, err := s.UpsertExternal(SourceLDAP, "cn=x", "carol.new", RoleAdmin); !errors.Is(err, ErrConflict) {
		t.Fatalf("other source: %v", err)
	}
	// External accounts have no password to guess, and an external admin does not count as the last local one.
	if _, err := s.CheckPassword("carol.new", "!"); err == nil {
		t.Fatal("external account signed in with a password")
	}
	if err := s.UpdateUser(local.ID, UserUpdate{Disabled: ptr(true)}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("an external admin must not stand in for the local one: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
