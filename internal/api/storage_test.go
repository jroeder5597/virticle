package api

import (
	"strings"
	"testing"
)

func TestPoolDefaults(t *testing.T) {
	if got := poolMountDefault("vms"); got != "/mnt/vms" {
		t.Errorf("mount point: %s", got)
	}
	if got := poolDirDefault("data"); got != "/var/lib/libvirt/images/data" {
		t.Errorf("directory: %s", got)
	}
	doc, err := poolRequest{Name: "nfs1", Type: "netfs", SourceHost: "nas", SourcePath: "/export"}.xml()
	if err != nil || !strings.Contains(doc, "<path>/mnt/nfs1</path>") {
		t.Errorf("netfs without mount point: %v %s", err, doc)
	}
	doc, err = poolRequest{Name: "d1", Type: "dir"}.xml()
	if err != nil || !strings.Contains(doc, "<path>/var/lib/libvirt/images/d1</path>") {
		t.Errorf("dir without path: %v %s", err, doc)
	}
	if _, err = (poolRequest{Name: "a/b", Type: "dir"}).xml(); err == nil {
		t.Error("slash in a name must be refused when the path is derived")
	}
}
