package api

import (
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestPeerMode(t *testing.T) {
	for in, want := range map[string]string{"600": "600", "644": "644", "4755": "755", "0600": "600", "": "600", "6": "600", "rw-": "600", "888": "600"} {
		if got := peerMode(in); got != want {
			t.Errorf("peerMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithParamReplaces(t *testing.T) {
	base := []libvirt.TypedParam{
		{Field: libvirt.MigrateParamURI, Value: *libvirt.NewTypedParamValueString("tcp://10.0.0.2")},
		{Field: libvirt.MigrateParamDisksURI, Value: *libvirt.NewTypedParamValueString("tcp://10.0.0.2")},
	}
	out := withParam(base, libvirt.MigrateParamURI, "tcp:10.0.0.2:49153")
	if len(out) != 2 || len(base) != 2 {
		t.Fatalf("lengths %d %d", len(out), len(base))
	}
	n := 0
	for _, p := range out {
		if p.Field == libvirt.MigrateParamURI {
			n++
			if p.Value.I != "tcp:10.0.0.2:49153" {
				t.Errorf("value %v", p.Value.I)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d URI parameters", n)
	}
	if base[0].Value.I != "tcp://10.0.0.2" {
		t.Error("the input was modified")
	}
	if got := withParam(base, libvirt.MigrateParamDestXML, "<domain/>"); len(got) != 3 {
		t.Errorf("append: %d", len(got))
	}
}
