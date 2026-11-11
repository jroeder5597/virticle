package api

import (
	"strings"
	"testing"

	"virticle/internal/hostconn"
)

func hostWith(p hostconn.Privilege) *hostconn.Host {
	h := &hostconn.Host{}
	h.SetPrivilege(p)
	return h
}

func TestNodeCopyCmd(t *testing.T) {
	ep := migEndpoint{User: "ne-user", Host: "10.0.0.7"}

	// root on the target: plain scp
	got, err := nodeCopyCmd(hostWith(hostconn.PrivRoot), migEndpoint{User: "root", Host: "10.0.0.7"}, "/var/lib/libvirt/images/a.qcow2", "/var/lib/libvirt/images/a.qcow2", "")
	if err != nil || got != "scp -q -p -o BatchMode=yes -- '/var/lib/libvirt/images/a.qcow2' 'root@10.0.0.7:/var/lib/libvirt/images/a.qcow2'" {
		t.Errorf("root: %q %v", got, err)
	}
	// a port and an IPv6 address
	got, _ = nodeCopyCmd(hostWith(hostconn.PrivRoot), migEndpoint{User: "root", Host: "2001:db8::1", Port: "2222"}, "/a", "/b", "")
	if !strings.Contains(got, "-P 2222 ") || !strings.Contains(got, "'root@[2001:db8::1]:/b'") {
		t.Errorf("ipv6 scp: %q", got)
	}

	// sudo on the target: the bytes go through ssh into a root shell there
	got, err = nodeCopyCmd(hostWith(hostconn.PrivSudo), ep, "/src/f.qcow2", "/dst/it's.qcow2", "644")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ssh -o BatchMode=yes", "'ne-user@10.0.0.7'", "sudo -n sh -c", "chmod 644", "< '/src/f.qcow2'"} {
		if !strings.Contains(got, want) {
			t.Errorf("sudo: %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "scp") {
		t.Errorf("sudo path must not use scp: %q", got)
	}
	// the default mode is private
	got, _ = nodeCopyCmd(hostWith(hostconn.PrivSudo), ep, "/a", "/b", "")
	if !strings.Contains(got, "chmod 600") {
		t.Errorf("default mode: %q", got)
	}
	// none: refused
	if _, err := nodeCopyCmd(hostWith(hostconn.PrivNone), ep, "/a", "/b", ""); err == nil {
		t.Error("no privilege on the target must be refused")
	}
}

func TestReachCmdQuotes(t *testing.T) {
	c := reachCmd("10.0.0.1", "22")
	if !strings.Contains(c, "timeout 6 bash -c ") || !strings.Contains(c, "/dev/tcp/10.0.0.1/22") {
		t.Error(c)
	}
}
