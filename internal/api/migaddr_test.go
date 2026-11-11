package api

import (
	"net"
	"testing"

	"virt-proj/internal/store"
)

func TestMigrationTargetAndURIs(t *testing.T) {
	cases := []struct {
		name         string
		node         store.Node
		host, port   string
		ssh, tcp     string
		wantEndpoint string
	}{
		{"ipv4", store.Node{Addr: "192.168.1.5", User: "root"}, "192.168.1.5", "", "qemu+ssh://root@192.168.1.5/system", "tcp://192.168.1.5", "root@192.168.1.5"},
		{"hostname", store.Node{Addr: "kvm2.lab.example", User: "bob"}, "kvm2.lab.example", "", "qemu+ssh://bob@kvm2.lab.example/system", "tcp://kvm2.lab.example", "bob@kvm2.lab.example"},
		{"ipv6", store.Node{Addr: "2001:db8::5", User: "root"}, "2001:db8::5", "", "qemu+ssh://root@[2001:db8::5]/system", "tcp://[2001:db8::5]", "root@2001:db8::5"},
		{"ipv6 with port", store.Node{Addr: "[2001:db8::5]:2222", User: "root"}, "2001:db8::5", "2222", "qemu+ssh://root@[2001:db8::5]:2222/system", "tcp://[2001:db8::5]", "root@2001:db8::5 (port 2222)"},
		{"port", store.Node{Addr: "10.0.0.9:2200", User: "u1"}, "10.0.0.9", "2200", "qemu+ssh://u1@10.0.0.9:2200/system", "tcp://10.0.0.9", "u1@10.0.0.9 (port 2200)"},
		{"migration address wins", store.Node{Addr: "n2.jump.example", User: "root", MigrationAddr: "10.9.9.2"}, "10.9.9.2", "", "qemu+ssh://root@10.9.9.2/system", "tcp://10.9.9.2", "root@10.9.9.2"},
		{"default user", store.Node{Addr: "10.0.0.1"}, "10.0.0.1", "", "qemu+ssh://root@10.0.0.1/system", "tcp://10.0.0.1", "root@10.0.0.1"},
	}
	for _, c := range cases {
		h, p := migrationTarget(c.node)
		if h != c.host || p != c.port {
			t.Errorf("%s: target %q %q, want %q %q", c.name, h, p, c.host, c.port)
		}
		ep, err := endpointFor(c.node)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if ep.URI() != c.ssh {
			t.Errorf("%s: ssh URI %s, want %s", c.name, ep.URI(), c.ssh)
		}
		if tcpURI(ep.Host) != c.tcp {
			t.Errorf("%s: tcp URI %s, want %s", c.name, tcpURI(ep.Host), c.tcp)
		}
		if ep.String() != c.wantEndpoint {
			t.Errorf("%s: endpoint %q, want %q", c.name, ep.String(), c.wantEndpoint)
		}
	}
	if _, err := endpointFor(store.Node{Addr: "bad host;rm", User: "root"}); err == nil {
		t.Error("unsafe address accepted")
	}
	if _, err := endpointFor(store.Node{Addr: "10.0.0.1", User: "x y"}); err == nil {
		t.Error("unsafe user accepted")
	}
}

func TestMigrateAddrParams(t *testing.T) {
	ps := migrateAddrParams("2001:db8::5")
	got := map[string]string{}
	for _, p := range ps {
		got[p.Field], _ = p.Value.I.(string)
	}
	if got["migrate_uri"] != "tcp://[2001:db8::5]" || got["disks_uri"] != "tcp://[2001:db8::5]" {
		t.Errorf("params: %v", got)
	}
}

func TestTLSDestinationAndSANs(t *testing.T) {
	// The TLS destination is the libvirt host name when it is a valid DNS name
	// and certNames always puts that name into the certificate.
	for _, c := range []struct{ hostname, mig, want string }{
		{"kvm1", "10.0.0.1", "kvm1"},
		{"kvm1.lab.example", "10.0.0.1", "kvm1.lab.example"},
		{"bad name", "10.0.0.1", "10.0.0.1"},
		{"", "10.0.0.1", "10.0.0.1"},
	} {
		if got := tlsDestination(c.hostname, c.mig); got != c.want {
			t.Errorf("tlsDestination(%q) = %q, want %q", c.hostname, got, c.want)
		}
	}

	n := certNames("kvm1", nodeEnv{FQDN: "kvm1.lab.example", Addrs: []net.IP{net.ParseIP("192.168.5.5")}}, "10.0.0.5:22", "mig.example", "10.9.9.9", "2001:db8::7")
	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	for _, d := range []string{"kvm1", "kvm1.lab.example", "mig.example"} {
		if !has(n.Hosts, d) {
			t.Errorf("DNS names %v lack %s", n.Hosts, d)
		}
	}
	ips := map[string]bool{}
	for _, ip := range n.IPs {
		ips[ip.String()] = true
	}
	for _, ip := range []string{"10.0.0.5", "10.9.9.9", "2001:db8::7", "192.168.5.5"} {
		if !ips[ip] {
			t.Errorf("IPs %v lack %s", n.IPs, ip)
		}
	}

}

func TestMergeNodeEdit(t *testing.T) {
	cur := store.Node{ID: 3, Addr: "10.0.0.1", User: "root", Jump: "j@h", Privilege: "root", Maintenance: true}
	s := func(v string) *string { return &v }
	// unchanged connection keeps the privilege and the flag
	n, detect, err := mergeNodeEdit(cur, nodeEditReq{MigrationAddress: s("10.5.5.5")})
	if err != nil || detect || n.Privilege != "root" || !n.Maintenance || n.MigrationAddr != "10.5.5.5" {
		t.Errorf("%+v %v %v", n, detect, err)
	}
	// a new user means the old detection no longer applies
	n, detect, err = mergeNodeEdit(cur, nodeEditReq{User: s("alice")})
	if err != nil || !detect || n.User != "alice" {
		t.Errorf("%+v %v %v", n, detect, err)
	}
	// an explicit privilege is taken as given
	n, detect, err = mergeNodeEdit(cur, nodeEditReq{User: s("alice"), Privilege: s("sudo")})
	if err != nil || detect || n.Privilege != "sudo" {
		t.Errorf("%+v %v %v", n, detect, err)
	}
	if _, detect, _ = mergeNodeEdit(cur, nodeEditReq{Privilege: s("auto")}); !detect {
		t.Error("auto must detect")
	}
	for _, bad := range []nodeEditReq{{Addr: s("")}, {Addr: s("a b")}, {User: s("a;b")}, {Jump: s("nouser")}, {Privilege: s("admin")}, {MigrationAddress: s("x y")}} {
		if _, _, err := mergeNodeEdit(cur, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestProbeParse(t *testing.T) {
	uid, acc, uuid := parseProbe("uid=1001\nlibvirt=ok\nuuid=4c4c4544-0000-1111-2222-333344445555\n")
	if uid != "1001" || acc != "ok" || uuid != "4c4c4544-0000-1111-2222-333344445555" {
		t.Errorf("%q %q %q", uid, acc, uuid)
	}
}
