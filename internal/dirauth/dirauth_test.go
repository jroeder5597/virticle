package dirauth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"virticle/internal/dirauth/ldaptest"
)

const (
	base   = "dc=example,dc=org"
	svcDN  = "cn=svc,dc=example,dc=org"
	aliceD = "uid=alice,ou=people,dc=example,dc=org"
	bobD   = "uid=bob,ou=people,dc=example,dc=org"
	carolD = "cn=Carol C,ou=people,dc=example,dc=org"
)

func fixture(t *testing.T, ad bool) (*ldaptest.Server, Config) {
	t.Helper()
	s, err := ldaptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.AD = ad
	s.Entries = []ldaptest.Entry{
		{DN: base, Attrs: map[string][]string{"objectClass": {"domain"}}},
		{DN: svcDN, Attrs: map[string][]string{"cn": {"svc"}}},
		{DN: aliceD, Attrs: map[string][]string{"uid": {"alice"}, "mail": {"alice@example.org"}, "cn": {"Alice A"}, "displayName": {"Alice Anders"},
			"memberOf": {"cn=admins,ou=groups,dc=example,dc=org"}}},
		{DN: bobD, Attrs: map[string][]string{"uid": {"bob"}, "mail": {"bob@example.org"}}},
		{DN: carolD, Attrs: map[string][]string{"sAMAccountName": {"carol"}, "objectClass": {"user"}}},
		{DN: "cn=ops,ou=groups,dc=example,dc=org", Attrs: map[string][]string{"cn": {"ops"}, "objectClass": {"group"}, "member": {bobD}}},
		{DN: "cn=legacy,ou=groups,dc=example,dc=org", Attrs: map[string][]string{"cn": {"legacy"}, "objectClass": {"posixGroup"}, "memberUid": {"bob"}}},
		{DN: "cn=outer,ou=groups,dc=example,dc=org", Attrs: map[string][]string{"cn": {"outer"}, "objectClass": {"group"}, "member": {"cn=inner,ou=groups,dc=example,dc=org"}}},
		{DN: "cn=inner,ou=groups,dc=example,dc=org", Attrs: map[string][]string{"cn": {"inner"}, "objectClass": {"group"}, "member": {carolD}}},
	}
	s.Passwords = map[string]string{svcDN: "svc-pass", aliceD: "alice-pass", bobD: "bob-pass", carolD: "carol-pass"}
	return s, Config{URL: s.TLSURL(), BindDN: svcDN, BindPassword: "svc-pass", BaseDN: base, CACert: s.CAPEM, Timeout: 2 * time.Second}
}

func TestAuthenticateGroups(t *testing.T) {
	s, cfg := fixture(t, false)
	r, err := Authenticate(cfg, "alice", "alice-pass")
	if err != nil || r.DN != aliceD || r.Name != "alice" || r.Display != "Alice Anders" {
		t.Fatalf("%+v %v", r, err)
	}
	if !r.InGroup("admins") || !r.InGroup("CN=Admins,OU=Groups,DC=example,DC=org") || r.InGroup("ops") {
		t.Errorf("memberOf groups: %v", r.Groups)
	}
	// The mail address works as the typed name, and groups come from the group search (member and memberUid).
	r, err = Authenticate(cfg, "bob@example.org", "bob-pass")
	if err != nil || r.Name != "bob" || !r.InGroup("ops") || !r.InGroup("legacy") || r.AD {
		t.Fatalf("%+v %v", r, err)
	}
	// Bound as the service account, then as the user, then as the service account again.
	if len(s.Binds) < 4 || s.Binds[0] != svcDN || s.Binds[1] != aliceD {
		t.Errorf("binds %v", s.Binds)
	}
}

func TestAuthenticateRefusals(t *testing.T) {
	s, cfg := fixture(t, false)
	for name, c := range map[string]struct{ u, p string }{
		"wrong password": {"alice", "nope"},
		"unknown user":   {"zed", "x"},
		"empty password": {"alice", ""},
	} {
		if _, err := Authenticate(cfg, c.u, c.p); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A filter character in the name must not widen the search.
	if _, err := Authenticate(cfg, "*", "alice-pass"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wildcard: %v", err)
	}
	if _, err := Authenticate(cfg, "alice)(uid=*", "alice-pass"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("injection: %v", err)
	}
	s.Entries = append(s.Entries, ldaptest.Entry{DN: "uid=alice2,dc=example,dc=org", Attrs: map[string][]string{"uid": {"alice"}}})
	if _, err := Authenticate(cfg, "alice", "alice-pass"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("two matches: %v", err)
	}
	bad := cfg
	bad.BindPassword = "wrong"
	var ue *UnreachableError
	if _, err := Authenticate(bad, "alice", "alice-pass"); !errors.As(err, &ue) || !strings.Contains(err.Error(), "bind user") {
		t.Errorf("bad service password: %v", err)
	}
}

func TestTransport(t *testing.T) {
	s, cfg := fixture(t, false)
	var ue *UnreachableError
	// StartTLS on the plain port.
	st := cfg
	st.URL = s.PlainURL()
	if _, err := Authenticate(st, "alice", "alice-pass"); err != nil {
		t.Errorf("StartTLS: %v", err)
	}
	// The test CA is not a system root: without it the certificate is refused, in plain words.
	noCA := cfg
	noCA.CACert = ""
	if _, err := Authenticate(noCA, "alice", "alice-pass"); !errors.As(err, &ue) || !strings.Contains(err.Error(), "CA certificate") {
		t.Errorf("untrusted LDAPS: %v", err)
	}
	noCA.URL = s.PlainURL()
	if _, err := Authenticate(noCA, "alice", "alice-pass"); !errors.As(err, &ue) || !strings.Contains(err.Error(), "CA certificate") {
		t.Errorf("untrusted StartTLS: %v", err)
	}
	// Plain LDAP only with the explicit allowance.
	plain := cfg
	plain.URL, plain.CACert, plain.AllowPlain = s.PlainURL(), "", true
	if _, err := Authenticate(plain, "alice", "alice-pass"); err != nil {
		t.Errorf("plain allowed: %v", err)
	}
	// Unreachable: a closed port gives an UnreachableError quickly.
	dead := cfg
	dead.URL = "ldaps://127.0.0.1:1"
	start := time.Now()
	if _, err := Authenticate(dead, "alice", "alice-pass"); !errors.As(err, &ue) {
		t.Errorf("unreachable: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	for _, c := range []Config{{URL: "http://x", BaseDN: base}, {URL: "ldaps://x"}, {URL: "ldaps://x", BaseDN: base, UserFilter: "(uid=x)"}, {URL: "ldaps://x", BaseDN: base, CACert: "junk"}} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestActiveDirectoryNestedGroups(t *testing.T) {
	_, cfg := fixture(t, true)
	r, err := Authenticate(cfg, "carol", "carol-pass")
	if err != nil || !r.AD {
		t.Fatalf("%+v %v", r, err)
	}
	if !r.InGroup("inner") || !r.InGroup("outer") {
		t.Errorf("nested groups missing: %v", r.Groups)
	}
	// A plain directory does not follow nesting: only the direct group.
	_, cfg = fixture(t, false)
	cfg.UserFilter = "(cn=Carol C)"
	cfg.UserFilter = "(sAMAccountName={user})"
	r, err = Authenticate(cfg, "carol", "carol-pass")
	if err != nil || r.AD || !r.InGroup("inner") || r.InGroup("outer") {
		t.Errorf("%+v %v", r, err)
	}
}

func TestTestReport(t *testing.T) {
	_, cfg := fixture(t, false)
	steps := Test(cfg, "alice", "")
	var text []string
	for _, s := range steps {
		if !s.OK {
			t.Errorf("failed step: %s", s.Text)
		}
		text = append(text, s.Text)
	}
	all := strings.Join(text, "\n")
	for _, w := range []string{"LDAPS", "Bound as the bind user", "User found as " + aliceD, "1 group(s): admins"} {
		if !strings.Contains(all, w) {
			t.Errorf("report lacks %q:\n%s", w, all)
		}
	}
	steps = Test(cfg, "nobody", "")
	if last := steps[len(steps)-1]; last.OK || !strings.Contains(last.Text, "not found") {
		t.Errorf("%+v", steps)
	}
	steps = Test(cfg, "alice", "wrong")
	if last := steps[len(steps)-1]; last.OK || !strings.Contains(last.Text, "wrong username or password") {
		t.Errorf("%+v", steps)
	}
	bad := cfg
	bad.URL = "ldaps://127.0.0.1:1"
	steps = Test(bad, "", "")
	if len(steps) != 1 || steps[0].OK {
		t.Errorf("%+v", steps)
	}
}
