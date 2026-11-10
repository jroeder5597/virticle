package api

import "testing"

func TestOSFamily(t *testing.T) {
	cases := map[string]string{
		"PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\n": "apt",
		"ID=ubuntu\nID_LIKE=debian\n":                               "apt",
		"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n":            "dnf",
		"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n":                 "apt",
		"ID=alpine\n": "unsupported",
		"":            "unsupported",
	}
	for in, want := range cases {
		if got, _ := osFamily(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestParseAptUpgradable(t *testing.T) {
	out := "Listing...\nlibssl3t64/stable-security 3.5.1-1+deb13u1 amd64 [upgradable from: 3.5.0-2]\nlinux-image-amd64/stable 6.12.43-1 amd64 [upgradable from: 6.12.41-1]\n\nWARNING: apt does not have a stable CLI interface.\n"
	u := parseAptUpgradable(out)
	if len(u) != 2 {
		t.Fatalf("got %+v", u)
	}
	if u[0].Name != "libssl3t64" || u[0].Version != "3.5.1-1+deb13u1" || u[0].From != "3.5.0-2" || u[0].Source != "stable-security" {
		t.Errorf("first: %+v", u[0])
	}
	if got := parseAptUpgradable("Listing...\n"); len(got) != 0 || got == nil {
		t.Errorf("empty list should be empty non-nil, got %#v", got)
	}
}

func TestParseDnfCheck(t *testing.T) {
	out := "\nkernel.x86_64   5.14.0-70   baseos\nopenssl.x86_64  3.0.7-2  appstream\n\nObsoleting Packages\nfoo.x86_64  1.0  repo\n__rc=100"
	u, err := parseDnfCheck(out)
	if err != nil || len(u) != 2 || u[1].Name != "openssl.x86_64" || u[1].Source != "appstream" {
		t.Fatalf("got %+v %v", u, err)
	}
	if u, err := parseDnfCheck("__rc=0"); err != nil || len(u) != 0 {
		t.Errorf("rc 0: %+v %v", u, err)
	}
	if _, err := parseDnfCheck("boom\n__rc=1"); err == nil {
		t.Error("rc 1 should be an error")
	}
}

func TestKernelInfo(t *testing.T) {
	var r hostUpdates
	applyKernelInfo(&r, "running=6.12.41\nnewest=6.12.43\n")
	if !r.RebootRequired {
		t.Error("newer installed kernel should require a reboot")
	}
	r = hostUpdates{}
	applyKernelInfo(&r, "running=6.12.43\nnewest=6.12.43\n")
	if r.RebootRequired {
		t.Error("same kernel: no reboot")
	}
	r = hostUpdates{}
	applyKernelInfo(&r, "running=6.12.43\nnewest=6.12.43\nflag=1\npkg=libc6\n")
	if !r.RebootRequired || r.RebootReason == "" {
		t.Errorf("flag file should require a reboot: %+v", r)
	}
}
