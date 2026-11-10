package migpki

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestCAPersistsWithPrivateKey(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateCA(dir, t0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "ca-key.pem"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("CA key: %v %v", st, err)
	}
	b, err := LoadOrCreateCA(dir, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("reloading created a different CA")
	}
	if !a.Cert.IsCA || a.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("not a CA certificate")
	}
	// A lone certificate or key must not be silently replaced.
	os.Remove(filepath.Join(dir, "ca-key.pem"))
	if _, err := LoadOrCreateCA(dir, t0); err == nil {
		t.Fatal("expected an error when the CA key is missing")
	}
}

func issue(t *testing.T, ca *CA, n Names, now time.Time) ([]byte, []byte) {
	t.Helper()
	c, k, err := ca.Issue(n, now)
	if err != nil {
		t.Fatal(err)
	}
	return c, k
}

func TestIssuedCertificate(t *testing.T) {
	ca := newTestCA(t)
	certPEM, keyPEM := issue(t, ca, Names{Hosts: []string{"vspike-n1", "vspike-n1.lab", "VSPIKE-N1"}, IPs: []net.IP{net.ParseIP("192.168.150.11"), net.ParseIP("fd00::11")}}, t0)
	crt, info, err := ParseCert(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	// Chain verifies for both roles.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := crt.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{u}}); err != nil {
			t.Errorf("usage %v: %v", u, err)
		}
	}
	// SANs: duplicates (case-insensitive) dropped, IPs kept.
	if got := strings.Join(info.DNSNames, ","); got != "vspike-n1,vspike-n1.lab" {
		t.Errorf("DNS names: %s", got)
	}
	for _, h := range []string{"vspike-n1", "vspike-n1.lab", "192.168.150.11", "fd00::11"} {
		if err := crt.VerifyHostname(h); err != nil {
			t.Errorf("name %s: %v", h, err)
		}
	}
	if crt.VerifyHostname("vspike-n2") == nil || crt.VerifyHostname("192.168.150.12") == nil {
		t.Error("certificate covers names it should not")
	}
	// Key usage as QEMU wants it for ECDSA: digitalSignature; EKU both.
	if crt.KeyUsage&x509.KeyUsageDigitalSignature == 0 || crt.IsCA {
		t.Errorf("key usage %v isCA %v", crt.KeyUsage, crt.IsCA)
	}
	var server, client bool
	for _, u := range crt.ExtKeyUsage {
		server = server || u == x509.ExtKeyUsageServerAuth
		client = client || u == x509.ExtKeyUsageClientAuth
	}
	if !server || !client {
		t.Errorf("EKU serverAuth=%v clientAuth=%v", server, client)
	}
	if crt.Subject.CommonName != "vspike-n1" {
		t.Errorf("CN %q", crt.Subject.CommonName)
	}
	if info.NotAfter.Sub(t0) < 700*24*time.Hour {
		t.Errorf("validity too short: %v", info.NotAfter)
	}
	if !strings.Contains(string(keyPEM), "EC PRIVATE KEY") {
		t.Error("no EC key")
	}
	if _, _, err := ca.Issue(Names{}, t0); err == nil {
		t.Error("a certificate without names must be refused")
	}
}

func TestVerifyRejectsOtherCA(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	certPEM, _ := issue(t, other, Names{Hosts: []string{"n1"}}, t0)
	crt, _, _ := ParseCert(certPEM)
	if err := ca.Verify(crt, "n1", t0); err == nil {
		t.Fatal("a certificate from another CA must not verify")
	}
}

// snapshot builds what a freshly installed node looks like.
func snapshot(t *testing.T, ca *CA, host string, now time.Time) Snapshot {
	t.Helper()
	certPEM, keyPEM := issue(t, ca, Names{Hosts: []string{host}}, now)
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	content := map[string][]byte{FileCA: ca.CertPEM, FileServerCert: certPEM, FileClientCert: certPEM, FileServerKey: keyPEM, FileClientKey: keyPEM}
	sn := Snapshot{Dir: "/etc/pki/qemu", Files: map[string]FileState{}, Hostname: host, CACert: ca.CertPEM, Server: certPEM, Client: certPEM,
		Marker: &Marker{CAFingerprint: ca.Fingerprint(), Installed: map[string]string{}}}
	for f, b := range content {
		sn.Files[f] = FileState{Exists: true, SHA256: sum(b)}
		sn.Marker.Installed[f] = sum(b)
	}
	return sn
}

func TestEvaluate(t *testing.T) {
	ca := newTestCA(t)
	now := t0.Add(24 * time.Hour)
	tests := []struct {
		name  string
		edit  func(*Snapshot)
		at    time.Time
		state string
		want  string // substring of the first problem
	}{
		{"fresh", func(*Snapshot) {}, now, StateOK, ""},
		{"nothing", func(s *Snapshot) { *s = Snapshot{Dir: s.Dir, Files: map[string]FileState{}} }, now, StateNotSetUp, ""},
		{"foreign files", func(s *Snapshot) { s.Marker = nil }, now, StateMismatch, "did not create"},
		{"bad marker", func(s *Snapshot) { s.Marker = nil; s.MarkerBad = true }, now, StateMismatch, "marker"},
		{"other CA in marker", func(s *Snapshot) { s.Marker.CAFingerprint = "abcdef0123456789abcdef" }, now, StateMismatch, "different controller"},
		{"file changed", func(s *Snapshot) {
			f := s.Files[FileServerKey]
			f.SHA256 = "00"
			s.Files[FileServerKey] = f
		}, now, StateMismatch, "changed"},
		{"file missing", func(s *Snapshot) { s.Files[FileClientKey] = FileState{} }, now, StateMismatch, "missing"},
		{"CA replaced on node", func(s *Snapshot) { s.CACert = newTestCA(t).CertPEM }, now, StateMismatch, "not this controller's CA"},
		{"name changed", func(s *Snapshot) { s.Hostname = "renamed" }, now, StateMismatch, "renamed"},
		{"expiring", func(*Snapshot) {}, t0.Add(NodeValidity - 10*24*time.Hour), StateExpiring, "expires in"},
		{"expired", func(*Snapshot) {}, t0.Add(NodeValidity + 24*time.Hour), StateExpired, "expired"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sn := snapshot(t, ca, "vspike-n1", t0)
			tc.edit(&sn)
			st := ca.Evaluate(sn, tc.at)
			if st.State != tc.state {
				t.Fatalf("state %q (%v), want %q", st.State, st.Problems, tc.state)
			}
			if tc.want != "" && (len(st.Problems) == 0 || !strings.Contains(strings.Join(st.Problems, "|"), tc.want)) {
				t.Fatalf("problems %v, want one containing %q", st.Problems, tc.want)
			}
			if tc.state == StateOK && (st.Cert == nil || len(st.Problems) != 0 || !st.Usable()) {
				t.Fatalf("ok status incomplete: %+v", st)
			}
		})
	}
}

func TestUsable(t *testing.T) {
	for state, want := range map[string]bool{StateOK: true, StateExpiring: true, StateExpired: false, StateMismatch: false, StateNotSetUp: false} {
		if (Status{State: state}).Usable() != want {
			t.Errorf("%s usable != %v", state, want)
		}
	}
}
