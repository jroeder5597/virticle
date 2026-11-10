package tlscert

import (
	"testing"
	"time"
)

func TestNotAfter(t *testing.T) {
	now := time.Now()
	cert, _, _, err := Ensure(t.TempDir(), NamesFor("127.0.0.1:8443"), now)
	if err != nil {
		t.Fatal(err)
	}
	na, err := NotAfter(cert)
	if err != nil || na.Sub(now) < 300*24*time.Hour {
		t.Fatalf("NotAfter = %v, %v", na, err)
	}
	if _, err := NotAfter(cert + ".missing"); err == nil {
		t.Error("missing file should be an error")
	}
}
