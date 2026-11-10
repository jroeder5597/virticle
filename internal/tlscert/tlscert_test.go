package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"regexp"
	"testing"
	"time"
)

func TestEnsureGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	n := Names{Hosts: []string{"vhost.example"}, IPs: []net.IP{net.ParseIP("192.0.2.9")}}
	cert, key, gen, err := Ensure(dir, n, now)
	if err != nil || !gen {
		t.Fatalf("first: gen=%v err=%v", gen, err)
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", st.Mode())
	}
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(c.Certificate[0])
	for _, name := range []string{"vhost.example", "localhost", "192.0.2.9", "127.0.0.1"} {
		if err := x.VerifyHostname(name); err != nil {
			t.Errorf("certificate does not cover %s: %v", name, err)
		}
	}
	if x.PublicKeyAlgorithm != x509.ECDSA || x.NotAfter.Sub(now) < 300*24*time.Hour {
		t.Errorf("unexpected key algorithm or validity: %v %v", x.PublicKeyAlgorithm, x.NotAfter)
	}
	fp1, _ := Fingerprint(cert)
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(fp1) {
		t.Errorf("fingerprint %q", fp1)
	}

	if _, _, gen, _ := Ensure(dir, n, now); gen {
		t.Error("regenerated a valid certificate")
	}
	if fp2, _ := Fingerprint(cert); fp1 != fp2 {
		t.Error("fingerprint changed")
	}
	n.IPs = append(n.IPs, net.ParseIP("192.0.2.10"))
	if _, _, gen, _ := Ensure(dir, n, now); !gen {
		t.Error("did not regenerate for a new required address")
	}
	if _, _, gen, _ := Ensure(dir, n, now.Add(validity-time.Hour)); !gen {
		t.Error("did not renew a certificate that is about to expire")
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:80": true, "127.0.0.2:1": true,
		"0.0.0.0:8080": false, ":8080": false, "192.168.1.5:80": false, "[::]:80": false, "host.example:80": false, "bad": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v", addr, got)
		}
	}
}
