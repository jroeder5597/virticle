package api

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func fakeLocalRoot(t *testing.T, cpuflags string, files ...string) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "proc"), 0o755)
	os.WriteFile(filepath.Join(root, "proc/cpuinfo"), []byte("processor\t: 0\nflags\t\t: fpu "+cpuflags+" sse\n"), 0o644)
	for _, f := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), nil, 0o644)
	}
	return root
}

func fakeLocal(root string) localProbe {
	return localProbe{
		Root:      root,
		Env:       func(string) string { return "" },
		Hostname:  func() (string, error) { return "vmhost", nil },
		SSHOpen:   func() bool { return true },
		RouteAddr: func() string { return "10.1.2.3" },
		IfAddrs: func() []net.IP {
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.1.2.3"), net.ParseIP("fe80::1")}
		},
	}
}

func TestLocalNodeCapable(t *testing.T) {
	p := fakeLocal(fakeLocalRoot(t, "vmx", "dev/kvm"))
	i := p.info(nil)
	if !i.Capable || !i.KVMDevice || !i.SSHReachable || i.Address != "10.1.2.3" || i.Hostname != "vmhost" || i.Registered || i.Reason != "" {
		t.Fatalf("%+v", i)
	}
	if !fakeLocal(fakeLocalRoot(t, "svm")).info(nil).Capable {
		t.Fatal("svm not recognised")
	}
}

func TestLocalNodeNotCapable(t *testing.T) {
	if i := fakeLocal(fakeLocalRoot(t, "")).info(nil); i.Capable || i.Reason == "" || i.SSHReachable {
		t.Fatalf("no cpu flag: %+v", i)
	}
	if i := fakeLocal(fakeLocalRoot(t, "vmx", ".dockerenv")).info(nil); i.Capable || i.Reason != "This is a container; add another machine as a node." {
		t.Fatalf("docker: %+v", i)
	}
	if i := fakeLocal(fakeLocalRoot(t, "vmx", "run/.containerenv")).info(nil); i.Capable {
		t.Fatalf("podman: %+v", i)
	}
	p := fakeLocal(fakeLocalRoot(t, "vmx"))
	p.Env = func(k string) string {
		if k == "container" {
			return "lxc"
		}
		return ""
	}
	if p.info(nil).Capable {
		t.Fatal("container env")
	}
}

func TestLocalNodeSSHClosed(t *testing.T) {
	p := fakeLocal(fakeLocalRoot(t, "vmx"))
	p.SSHOpen = func() bool { return false }
	if i := p.info(nil); !i.Capable || i.SSHReachable {
		t.Fatalf("%+v", i)
	}
}

func TestLocalNodeAddress(t *testing.T) {
	p := fakeLocal(fakeLocalRoot(t, "vmx"))
	p.RouteAddr = func() string { return "" }
	p.IfAddrs = func() []net.IP {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("169.254.1.1"), net.ParseIP("192.168.5.6")}
	}
	if a := p.address(); a != "192.168.5.6" {
		t.Fatal(a)
	}
	p.IfAddrs = func() []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} }
	if a := p.address(); a != "127.0.0.1" {
		t.Fatal(a)
	}
	p.RouteAddr = func() string { return "127.0.0.1" }
	p.IfAddrs = func() []net.IP { return []net.IP{net.ParseIP("10.0.0.9")} }
	if a := p.address(); a != "10.0.0.9" {
		t.Fatal(a)
	}
}

func TestLocalNodeRegistered(t *testing.T) {
	p := fakeLocal(fakeLocalRoot(t, "vmx"))
	for _, a := range []string{"127.0.0.1", "localhost", "10.1.2.3:2222", "VMHOST", "[::1]:22"} {
		if !p.info([]string{"10.9.9.9", a}).Registered {
			t.Errorf("%s should be this machine", a)
		}
	}
	for _, a := range []string{"10.9.9.9", "other", ""} {
		if p.info([]string{a}).Registered {
			t.Errorf("%s should not be this machine", a)
		}
	}
}
