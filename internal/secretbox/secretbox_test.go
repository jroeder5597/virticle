package secretbox

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenAndKeyFile(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, KeyFile)
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	enc, _ := b.Seal("https://hooks.example/abc")
	enc2, _ := b.Seal("https://hooks.example/abc")
	if strings.Contains(enc, "hooks") || enc == "" || enc == enc2 {
		t.Fatalf("not encrypted: %q", enc)
	}
	again, _ := OpenDir(dir) // the same key is read back
	if got, err := again.Open(enc); err != nil || got != "https://hooks.example/abc" {
		t.Fatalf("got %q %v", got, err)
	}
	other, _ := OpenDir("")
	if _, err := other.Open(enc); err != ErrUnreadable {
		t.Errorf("a different key must not open it: %v", err)
	}
	if _, err := b.Open("plain"); err != ErrUnreadable {
		t.Errorf("unsealed text must be refused: %v", err)
	}
	if _, err := b.Open(enc[:len(enc)-2] + "AA"); err != ErrUnreadable {
		t.Errorf("tampering must be detected: %v", err)
	}
	if e, _ := b.Seal(""); e != "" {
		t.Error("empty stays empty")
	}
}

func TestDamagedKeyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, KeyFile), []byte("short"), 0o600)
	if _, err := OpenDir(dir); err == nil {
		t.Fatal("a damaged key must not be replaced silently")
	}
}

// A data directory from the notification build has settings.key (raw bytes) and
// sealed values under it: they must still open, and the old file goes away.
func TestAdoptsSettingsKey(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	old, _ := New(key)
	sealed, _ := old.Seal("kept")
	if err := os.WriteFile(filepath.Join(dir, "settings.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open(sealed); err != nil || got != "kept" {
		t.Fatalf("adopted key must open old values: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.key")); !os.IsNotExist(err) {
		t.Error("settings.key must be renamed away")
	}
	if st, err := os.Stat(filepath.Join(dir, KeyFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("secret.key: %v %v", st, err)
	}
	// With both present, secret.key wins and settings.key is left alone.
	dir2 := t.TempDir()
	if _, err := OpenDir(dir2); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir2, "settings.key"), key, 0o600)
	b2, _ := OpenDir(dir2)
	if _, err := b2.Open(sealed); err == nil {
		t.Error("an existing secret.key must not be replaced by settings.key")
	}
}
