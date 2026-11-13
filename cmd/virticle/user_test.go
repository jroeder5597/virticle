package main

import (
	"bytes"
	"strings"
	"testing"

	"virticle/internal/store"
)

func userEnv(t *testing.T) (dir string, st *store.Store) {
	t.Helper()
	dir = t.TempDir()
	p, err := store.DBPath(dir, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return dir, st
}

func run(args []string, in string, terminal bool, typed ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	n := 0
	code = runUserCommand(args, userIO{stdin: strings.NewReader(in), stdout: &o, stderr: &e, isTerminal: terminal,
		readPassword: func() (string, error) { n++; return typed[(n-1)%len(typed)], nil }})
	return code, o.String(), e.String()
}

func TestResetTwoStepAndPassword(t *testing.T) {
	dir, st := userEnv(t)
	u, _ := st.CreateUser("alice", "old password 123", store.RoleAdmin)
	st.CreateUser("bob", "bobs password 123", store.RoleAdmin)
	if err := st.EnableTwoStep(u.ID, "sealed", 1, []string{"h"}); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := st.Login("alice", "old password 123")
	st.Close()

	if code, out, _ := run([]string{"reset-2fa", "alice", "-data", dir}, "", false); code != 0 || !strings.Contains(out, "off for alice") {
		t.Fatalf("reset-2fa: %d %s", code, out)
	}
	if code, out, _ := run([]string{"reset-2fa", "alice", "-data", dir}, "", false); code != 0 || !strings.Contains(out, "does not have") {
		t.Fatalf("second reset-2fa: %d %s", code, out)
	}
	// On a terminal the password is asked twice, and a mismatch changes nothing.
	if code, _, e := run([]string{"reset-password", "alice", "-data", dir}, "", true, "first password 1", "other password 2"); code == 0 || !strings.Contains(e, "differ") {
		t.Fatalf("mismatch: %d %s", code, e)
	}
	if code, _, e := run([]string{"reset-password", "alice", "-data", dir}, "", true, "short"); code == 0 || !strings.Contains(e, "at least") {
		t.Fatalf("policy: %d %s", code, e)
	}
	if code, out, e := run([]string{"reset-password", "alice", "-data", dir}, "", true, "brand new password"); code != 0 {
		t.Fatalf("terminal reset: %d %s %s", code, out, e)
	}
	// Without a terminal it refuses unless the password comes on stdin.
	if code, _, e := run([]string{"reset-password", "bob", "-data", dir}, "pw from a pipe 1\n", false); code == 0 || !strings.Contains(e, "not a terminal") {
		t.Fatalf("pipe without flag: %d %s", code, e)
	}
	if code, out, e := run([]string{"reset-password", "bob", "--password-stdin", "-data", dir}, "pw from a pipe 1\n", false); code != 0 {
		t.Fatalf("pipe with flag: %d %s %s", code, out, e)
	}
	if code, _, e := run([]string{"reset-password", "nobody", "-data", dir}, "", true, "x"); code == 0 || !strings.Contains(e, "no user") {
		t.Fatalf("unknown user: %d %s", code, e)
	}
	if code, _, _ := run([]string{"reset-password", "alice", "-data", t.TempDir()}, "", true, "x"); code == 0 {
		t.Fatal("an empty data directory must be refused, not created")
	}

	_, st = userEnv2(t, dir)
	defer st.Close()
	if _, _, err := st.Login("alice", "brand new password"); err != nil {
		t.Errorf("new password: %v", err)
	}
	if _, _, err := st.Login("alice", "old password 123"); err == nil {
		t.Error("old password still works")
	}
	if _, _, err := st.Login("bob", "pw from a pipe 1"); err != nil {
		t.Errorf("bob: %v", err)
	}
	if _, err := st.SessionUser(tok); err == nil {
		t.Error("sessions survive the resets")
	}
	if a, _ := st.UserByName("alice"); a.TwoStep {
		t.Error("two-step still on")
	}
}

func TestResetPasswordRefusesExternalAccounts(t *testing.T) {
	dir, st := userEnv(t)
	st.CreateUser("root", "root password 12", store.RoleAdmin)
	st.UpsertExternal(store.SourceLDAP, "cn=x", "xavier", store.RoleViewer)
	st.Close()
	if code, _, e := run([]string{"reset-password", "xavier", "-data", dir}, "", true, "a long password 1"); code == 0 || !strings.Contains(e, "no password here") {
		t.Fatalf("%d %s", code, e)
	}
}

func userEnv2(t *testing.T, dir string) (string, *store.Store) {
	t.Helper()
	p, _ := store.DBPath(dir, func(string, ...any) {})
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return dir, st
}
