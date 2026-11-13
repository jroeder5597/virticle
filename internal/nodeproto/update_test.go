package nodeproto

import (
	"strings"
	"testing"
)

func TestParseDnfErrorCarriesTheReason(t *testing.T) {
	_, err := ParseDnf("__err=Error: Failed to download metadata for repo 'x'\n__rc=1")
	if err == nil || !strings.Contains(err.Error(), "status 1: Error: Failed to download metadata for repo 'x'") {
		t.Fatalf("%v", err)
	}
	if u, err := ParseDnf("nano.x86_64 8.1-1 baseos\n__rc=100"); err != nil || len(u) != 1 {
		t.Fatalf("%v %v", u, err)
	}
}

func TestKernelNewer(t *testing.T) {
	for _, c := range []struct {
		run, newest string
		want        bool
	}{
		{"6.12.0-211.9.1.el10.x86_64", "6.12.0-211.74.1.el10.x86_64", true},
		{"6.12.0-211.74.1.el10.x86_64", "6.12.0-211.9.1.el10.x86_64", false},
		{"6.12.43+deb13-amd64", "6.12.43+deb13-amd64", false},
		{"6.12.41", "6.12.43", true},
		{"", "6.12.43", false},
	} {
		if got := KernelNewer(c.run, c.newest); got != c.want {
			t.Errorf("%s vs %s: %v", c.run, c.newest, got)
		}
	}
}
