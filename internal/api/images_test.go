package api

import (
	"strings"
	"testing"
)

func TestCompareDigest(t *testing.T) {
	a := strings.Repeat("ab", 32)
	if err := compareDigest("sha256", a, strings.ToUpper(a)); err != nil {
		t.Error(err)
	}
	err := compareDigest("sha256", a, strings.Repeat("00", 32))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("mismatch not reported clearly: %v", err)
	}
}

func TestImageRunTable(t *testing.T) {
	tab := &runTable{m: map[string]*imageRun{}}
	k := runKey("1", "default", "a.img")
	if !tab.add(k, &imageRun{}) || tab.add(k, &imageRun{}) {
		t.Fatal("a second download of the same volume must be refused")
	}
	tab.add(runKey("1", "default", "b.img"), &imageRun{})
	tab.add(runKey("2", "default", "c.img"), &imageRun{})
	if got := strings.Join(tab.names("1", "default"), ","); got != "a.img,b.img" {
		t.Error(got)
	}
	tab.remove(k)
	if tab.get(k) != nil || !tab.add(k, &imageRun{}) {
		t.Error("remove did not free the name")
	}
}
