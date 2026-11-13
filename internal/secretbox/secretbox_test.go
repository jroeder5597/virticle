package secretbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenAndKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	b, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	enc, _ := b.Seal("https://hooks.example/abc")
	if strings.Contains(enc, "hooks") || enc == "" {
		t.Fatalf("not encrypted: %q", enc)
	}
	again, _ := OpenFile(p) // the same key is read back
	if got, err := again.Open(enc); err != nil || got != "https://hooks.example/abc" {
		t.Fatalf("got %q %v", got, err)
	}
	other, _ := OpenFile("")
	if _, err := other.Open(enc); err != ErrUnreadable {
		t.Errorf("a different key must not open it: %v", err)
	}
	if e, _ := b.Seal(""); e != "" {
		t.Error("empty stays empty")
	}
}
