package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"virticle/internal/migpki"
)

func TestParseQemuConf(t *testing.T) {
	conf := parseQemuConf([]string{
		`#default_tls_x509_cert_dir = "/etc/pki/qemu"`,
		`migrate_tls_x509_cert_dir = "/etc/pki/libvirt-migrate"   # mine`,
		`  user = "libvirt-qemu"`,
		`migrate_tls_x509_verify = 0`,
		`migrate_tls_force=1`,
	})
	want := map[string]string{"migrate_tls_x509_cert_dir": "/etc/pki/libvirt-migrate", "user": "libvirt-qemu", "migrate_tls_x509_verify": "0", "migrate_tls_force": "1"}
	if len(conf) != len(want) {
		t.Fatalf("got %v", conf)
	}
	for k, v := range want {
		if conf[k] != v {
			t.Errorf("%s = %q, want %q", k, conf[k], v)
		}
	}
}

const envOut = `@@uid
0
@@conf
user = "qemu"
@@fqdn
kvm1.example.lan
@@addr
2: eth0    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0\       valid_lft forever preferred_lft forever
2: eth0    inet6 fd00::5/64 scope global \       valid_lft forever preferred_lft forever
3: virbr0    inet 192.168.122.1/24 brd 192.168.122.255 scope global virbr0\       valid_lft forever
4: eth1    inet6 fe80::1/64 scope link \       valid_lft forever
@@pw
qemu:x:107:107:qemu user:/:/sbin/nologin
@@se
/usr/sbin/restorecon
@@conffile
yes
`

func TestParseNodeEnv(t *testing.T) {
	e := parseNodeEnv(envOut)
	if e.UID != "0" || e.FQDN != "kvm1.example.lan" || !e.Restore || !e.ConfRead {
		t.Fatalf("%+v", e)
	}
	var ips []string
	for _, ip := range e.Addrs {
		ips = append(ips, ip.String())
	}
	if strings.Join(ips, ",") != "10.0.0.5,fd00::5" {
		t.Errorf("addresses %v (bridges and link-local must be skipped)", ips)
	}
	if u, err := e.qemuUser(); err != nil || u != "qemu" {
		t.Errorf("user %q %v", u, err)
	}
	if d, err := e.certDir(); err != nil || d != "/etc/pki/qemu" {
		t.Errorf("dir %q %v", d, err)
	}
}

func TestQemuUserAndDir(t *testing.T) {
	deb := nodeEnv{Users: map[string]bool{"libvirt-qemu": true}, Conf: map[string]string{}}
	if u, _ := deb.qemuUser(); u != "libvirt-qemu" {
		t.Errorf("debian user %q", u)
	}
	if _, err := (nodeEnv{Users: map[string]bool{}, Conf: map[string]string{}}).qemuUser(); err == nil {
		t.Error("no account must be an error")
	}
	if u, _ := (nodeEnv{Conf: map[string]string{"user": "+0"}}).qemuUser(); u != "0" {
		t.Errorf("numeric user %q", u)
	}
	if _, err := (nodeEnv{Conf: map[string]string{"user": "a b;rm"}}).qemuUser(); err == nil {
		t.Error("unsafe user accepted")
	}
	e := nodeEnv{Conf: map[string]string{"default_tls_x509_cert_dir": "/srv/pki", "migrate_tls_x509_cert_dir": "/srv/mig"}}
	if d, _ := e.certDir(); d != "/srv/mig" {
		t.Errorf("migrate dir takes precedence, got %q", d)
	}
	e = nodeEnv{Conf: map[string]string{"default_tls_x509_cert_dir": "/srv/pki"}}
	if d, _ := e.certDir(); d != "/srv/pki" {
		t.Errorf("default dir %q", d)
	}
	for _, bad := range []string{"/etc/pki/../x", "relative", "/a b", "/"} {
		if _, err := (nodeEnv{Conf: map[string]string{"migrate_tls_x509_cert_dir": bad}}).certDir(); err == nil {
			t.Errorf("directory %q accepted", bad)
		}
	}
}

func TestCertNames(t *testing.T) {
	e := parseNodeEnv(envOut)
	n := certNames("kvm1", e, "10.0.0.5:22")
	if strings.Join(n.Hosts, ",") != "kvm1,kvm1.example.lan" {
		t.Errorf("hosts %v", n.Hosts)
	}
	got := map[string]bool{}
	for _, ip := range n.IPs {
		got[ip.String()] = true
	}
	if !got["10.0.0.5"] || !got["fd00::5"] {
		t.Errorf("ips %v", n.IPs)
	}
	n = certNames("kvm1", nodeEnv{}, "kvm1.lab.example")
	if strings.Join(n.Hosts, ",") != "kvm1,kvm1.lab.example" {
		t.Errorf("a DNS address belongs in the names: %v", n.Hosts)
	}
	n = certNames("bad name;", nodeEnv{FQDN: "x y"}, "")
	if len(n.Hosts) != 0 {
		t.Errorf("unsafe names kept: %v", n.Hosts)
	}
}

func TestAncestors(t *testing.T) {
	if got := strings.Join(ancestors("/etc/pki/qemu"), " "); got != "/etc /etc/pki /etc/pki/qemu" {
		t.Error(got)
	}
}

func TestParseState(t *testing.T) {
	ca, err := migpki.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, _, _ := ca.Issue(migpki.Names{Hosts: []string{"n1"}}, time.Now())
	marker := `{"caFingerprint":"` + ca.Fingerprint() + `","installed":{"ca-cert.pem":"aa"},"createdDirs":["/etc/pki"]}`
	out := "@@dir /etc 1\n@@dir /etc/pki 0\n@@dir /etc/pki/qemu 0\n" +
		"@@file ca-cert.pem abc root:root 644\n@@file server-key.pem def libvirt-qemu:root 400\n" +
		"@@content ca\n" + string(ca.CertPEM) + "@@content server\n" + string(cert) + "@@content client\n" +
		"@@content marker\n" + marker + "\n@@content ls\nca-cert.pem\nother.pem\n@@end\n"
	st, err := parseState(out, "/etc/pki/qemu", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if st.dirExists["/etc/pki"] || !st.dirExists["/etc"] {
		t.Errorf("dirs %v", st.dirExists)
	}
	if f := st.snap.Files["server-key.pem"]; !f.Exists || f.Owner != "libvirt-qemu:root" || f.Mode != "400" || f.SHA256 != "def" {
		t.Errorf("file %+v", f)
	}
	if st.snap.Files["client-cert.pem"].Exists {
		t.Error("absent file reported present")
	}
	if st.snap.Marker == nil || st.snap.Marker.CAFingerprint != ca.Fingerprint() || len(st.snap.Marker.CreatedDirs) != 1 {
		t.Errorf("marker %+v", st.snap.Marker)
	}
	if string(st.snap.CACert) != string(ca.CertPEM) || len(st.snap.Server) == 0 || len(st.snap.Client) != 0 {
		t.Error("contents")
	}
	if !st.listing["other.pem"] {
		t.Error("listing")
	}
	// A truncated answer is an error, a broken marker is flagged.
	if _, err := parseState("@@dir /etc 1\n", "/etc/pki/qemu", "n1"); err == nil {
		t.Error("truncated output accepted")
	}
	st, _ = parseState("@@content marker\n{not json\n@@end\n", "/etc/pki/qemu", "n1")
	if !st.snap.MarkerBad || st.snap.Marker != nil {
		t.Error("broken marker not flagged")
	}
}

func bptr(b bool) *bool { return &b }

func TestEncryptionCheck(t *testing.T) {
	ok := migpki.Status{State: migpki.StateOK}
	expiring := migpki.Status{State: migpki.StateExpiring, DaysLeft: 9}
	none := migpki.Status{State: migpki.StateNotSetUp}
	mism := migpki.Status{State: migpki.StateMismatch, Problems: []string{"ca-cert.pem on the node is not this controller's CA"}}
	expired := migpki.Status{State: migpki.StateExpired}
	base := encInput{Mode: "live", SrcName: "n1", DstName: "n2"}
	tests := []struct {
		name    string
		mod     func(*encInput)
		tls     bool
		ok      bool
		sev     string
		contain string
	}{
		{"both ok, default", func(i *encInput) { i.Src, i.Dst = ok, ok }, true, true, "info", "encrypted with TLS"},
		{"both ok, explicit true", func(i *encInput) { i.Src, i.Dst, i.Request = ok, ok, bptr(true) }, true, true, "info", "encrypted"},
		{"expiring still encrypts and says so", func(i *encInput) { i.Src, i.Dst = ok, expiring }, true, true, "info", "n2 expires in 9 days"},
		{"explicit off", func(i *encInput) { i.Src, i.Dst, i.Request = ok, ok, bptr(false) }, false, false, "warning", "unencrypted between n1 and n2"},
		{"neither set up: warning with pointer", func(i *encInput) { i.Src, i.Dst = none, none }, false, false, "warning", "unencrypted between n1 and n2 (n1: not set up; n2: not set up). Press Fix under Encrypted migration"},
		{"one set up", func(i *encInput) { i.Src, i.Dst = ok, none }, false, false, "warning", "n2: not set up"},
		{"mismatch explained", func(i *encInput) { i.Src, i.Dst = ok, mism }, false, false, "warning", "not this controller's CA"},
		{"expired", func(i *encInput) { i.Src, i.Dst = expired, ok }, false, false, "warning", "n1: expired"},
		{"requested but impossible is an error", func(i *encInput) { i.Src, i.Dst, i.Request = ok, none, bptr(true) }, false, false, "error", "requested but is not possible"},
		{"unreadable node", func(i *encInput) { i.Src, i.Dst, i.DstErr = ok, ok, errors.New("timeout") }, false, false, "warning", "n2: cannot be checked"},
		{"offline needs nothing", func(i *encInput) { i.Mode = "offline"; i.Src, i.Dst = none, none }, false, true, "info", "over SSH, node to node"},
		{"offline with explicit true is fine", func(i *encInput) { i.Mode = "offline"; i.Request = bptr(true) }, false, true, "info", "encrypted"},
		{"suspended is a live stream too", func(i *encInput) { i.Mode = "suspended"; i.Src, i.Dst = ok, ok }, true, true, "info", "encrypted with TLS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mod(&in)
			tls, c := encryptionCheck(in)
			if tls != tc.tls || c.OK != tc.ok || c.Severity != tc.sev || c.Check != "Encryption" {
				t.Fatalf("tls=%v %+v", tls, c)
			}
			if !strings.Contains(c.Message, tc.contain) {
				t.Fatalf("message %q lacks %q", c.Message, tc.contain)
			}
			// Only an error blocks a migration; the unencrypted case must not.
			if blocked := (checkList{c}).blocked(); blocked != (tc.sev == "error") {
				t.Fatalf("blocked = %v", blocked)
			}
		})
	}
}

func TestMigTLSAlert(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tr := newAlertTracker(alertConfig{})
	in := alertInput{Fresh: map[int64]bool{}, MigCerts: []alertMigCert{
		{Node: 1, NodeName: "n1", NotAfter: now.Add(10 * 24 * time.Hour)},
		{Node: 2, NodeName: "n2", NotAfter: now.Add(90 * 24 * time.Hour)},
		{Node: 3, NodeName: "n3", NotAfter: now.Add(-time.Hour)},
	}}
	_, _, all := tr.step(in, now)
	got := map[string]string{}
	for _, a := range all {
		got[a.ID] = a.Severity
	}
	if len(got) != 2 || got["migtls-expiry:1"] != "warning" || got["migtls-expiry:3"] != "critical" {
		t.Fatalf("alerts %v", got)
	}
}

// A marker written by the version before the rename is still read; the
// current name wins when both exist.
func TestParseStateLegacyMarker(t *testing.T) {
	marker := func(fp string) string {
		return `{"caFingerprint":"` + fp + `","installed":{"ca-cert.pem":"aa"}}`
	}
	st, err := parseState("@@content marker\n@@content oldmarker\n"+marker("old")+"\n@@end\n", "/d", "n1")
	if err != nil || st.snap.Marker == nil || st.snap.Marker.CAFingerprint != "old" || st.markerFile != migpki.FileMarkerLegacy {
		t.Fatalf("%+v %v", st.snap.Marker, err)
	}
	st, _ = parseState("@@content marker\n"+marker("new")+"\n@@content oldmarker\n"+marker("old")+"\n@@end\n", "/d", "n1")
	if st.snap.Marker == nil || st.snap.Marker.CAFingerprint != "new" || st.markerFile != migpki.FileMarker {
		t.Fatalf("%+v", st.snap.Marker)
	}
	st, _ = parseState("@@content marker\n@@content oldmarker\n@@end\n", "/d", "n1")
	if st.snap.Marker != nil || st.snap.MarkerBad {
		t.Fatal("no marker expected")
	}
}
