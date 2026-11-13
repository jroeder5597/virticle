package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripAndKeyFile(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	s1, _ := b.Seal("JBSWY3DPEHPK3PXP")
	s2, _ := b.Seal("JBSWY3DPEHPK3PXP")
	if s1 == s2 || strings.Contains(s1, "JBSWY") {
		t.Fatal("sealed values must differ and hide the text")
	}
	b2, err := Open(dir) // a second start reads the same key
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b2.Unseal(s1); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("got %q %v", got, err)
	}
	other, _ := Open(t.TempDir())
	if _, err := other.Unseal(s1); err == nil {
		t.Fatal("a different key must not open it")
	}
	if _, err := b.Unseal("plain"); err == nil {
		t.Fatal("unsealed text must be refused")
	}
	s1 = s1[:len(s1)-2] + "AA"
	if _, err := b.Unseal(s1); err == nil {
		t.Fatal("tampering must be detected")
	}
}

func TestDamagedKeyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, keyFile), []byte("short"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("a damaged key must not be replaced silently")
	}
}
