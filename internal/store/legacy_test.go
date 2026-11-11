package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDBPath(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	t.Run("old only is migrated with companions", func(t *testing.T) {
		logs = nil
		d := t.TempDir()
		write(t, d, "virt-proj.db", "main")
		write(t, d, "virt-proj.db-wal", "wal")
		write(t, d, "virt-proj.db-shm", "shm")
		p, err := DBPath(d, logf)
		if err != nil || p != filepath.Join(d, "virticle.db") {
			t.Fatalf("%q %v", p, err)
		}
		if read(t, d, "virticle.db") != "main" || read(t, d, "virticle.db-wal") != "wal" || read(t, d, "virticle.db-shm") != "shm" {
			t.Fatal("content not carried over")
		}
		for _, n := range []string{"virt-proj.db", "virt-proj.db-wal", "virt-proj.db-shm"} {
			if _, err := os.Stat(filepath.Join(d, n)); !os.IsNotExist(err) {
				t.Fatalf("%s still there", n)
			}
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "renamed") {
			t.Fatalf("logs %q", logs)
		}
	})
	t.Run("old without companions", func(t *testing.T) {
		d := t.TempDir()
		write(t, d, "virt-proj.db", "main")
		if _, err := DBPath(d, logf); err != nil || read(t, d, "virticle.db") != "main" {
			t.Fatal(err)
		}
	})
	t.Run("interrupted rename is finished", func(t *testing.T) {
		d := t.TempDir()
		write(t, d, "virt-proj.db", "main")
		write(t, d, "virticle.db-wal", "wal") // the wal was moved, the main file not yet
		if _, err := DBPath(d, logf); err != nil || read(t, d, "virticle.db") != "main" || read(t, d, "virticle.db-wal") != "wal" {
			t.Fatal(err)
		}
	})
	t.Run("both present: new wins, warning, old untouched", func(t *testing.T) {
		logs = nil
		d := t.TempDir()
		write(t, d, "virt-proj.db", "old")
		write(t, d, "virticle.db", "new")
		p, err := DBPath(d, logf)
		if err != nil || p != filepath.Join(d, "virticle.db") || read(t, d, "virticle.db") != "new" || read(t, d, "virt-proj.db") != "old" {
			t.Fatalf("%q %v", p, err)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "WARNING") {
			t.Fatalf("logs %q", logs)
		}
	})
	t.Run("neither: fresh", func(t *testing.T) {
		logs = nil
		d := t.TempDir()
		p, err := DBPath(d, logf)
		if err != nil || p != filepath.Join(d, "virticle.db") || len(logs) != 0 {
			t.Fatalf("%q %v %q", p, err, logs)
		}
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Fatal("created files")
		}
	})
	t.Run("migrated database opens", func(t *testing.T) {
		d := t.TempDir()
		s, err := Open(filepath.Join(d, "virt-proj.db"))
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		p, err := DBPath(d, logf)
		if err != nil {
			t.Fatal(err)
		}
		s, err = Open(p)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	})
}
