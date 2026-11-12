package hostconn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStat(t *testing.T) {
	paths := []string{"/p/a.qcow2", "/p/gone", "/p/dir", "/p/dev"}
	out := "regular file|2048|512|600|1049088\nMISSING\ndirectory|0|4096|711|60\ncharacter special file|0|4096|660|0"
	st, err := parseStat(paths, out)
	if err != nil {
		t.Fatal(err)
	}
	if !st[0].Exists || st[0].Kind != "file" || st[0].Allocated() != 2048*512 || st[0].Mode != "600" || st[0].Size != 1049088 {
		t.Errorf("file: %+v", st[0])
	}
	if st[1].Exists || st[1].Path != "/p/gone" {
		t.Errorf("missing: %+v", st[1])
	}
	if st[2].Kind != "dir" || st[3].Kind != "other" {
		t.Errorf("kinds: %+v %+v", st[2], st[3])
	}
	for _, bad := range []string{"", "MISSING", "regular file|x|512|600|1\nMISSING\nMISSING\nMISSING", "a|b\nMISSING\nMISSING\nMISSING"} {
		if _, err := parseStat(paths, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// an empty regular file is a file
	st, _ = parseStat([]string{"/x"}, "regular empty file|0|4096|600|0")
	if st[0].Kind != "file" {
		t.Errorf("empty file: %+v", st[0])
	}
}

func TestParseProgress(t *testing.T) {
	if d, tot, ok := parseProgress("@@progress 1048576 4194304"); !ok || d != 1048576 || tot != 4194304 {
		t.Error("progress line")
	}
	for _, l := range []string{"", "@@done 5", "@@progress", "@@progress a b", "@@progress 1 2 3", "progress 1 2"} {
		if _, _, ok := parseProgress(l); ok {
			t.Errorf("accepted %q", l)
		}
	}
}

func TestOperationsNeedTheNodeCommand(t *testing.T) {
	var h Host
	h.SetPrivilege(PrivNone)
	if _, err := h.PeerKey(); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("PeerKey without the node command: %v", err)
	}
	if err := h.PeerAllow("ssh-ed25519 AAAA"); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("PeerAllow: %v", err)
	}
	if _, err := h.AllowedRoots(); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("AllowedRoots: %v", err)
	}
	if err := h.Remove("/x"); !errors.Is(err, ErrNoPrivilege) && (err == nil || !strings.Contains(err.Error(), "node command")) {
		t.Errorf("Remove: %v", err)
	}
	if err := h.PeerSend(context.Background(), "h", "22", "u", "/a", "/b", "600", nil); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("PeerSend: %v", err)
	}
}

func TestKnownHostKeys(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	line := "[10.0.0.5]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGnLCn9YZVJWYvCwl8vlK6NfBvmOBxJSbCqhHCMY6Dm1\n" +
		"10.0.0.6 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGnLCn9YZVJWYvCwl8vlK6NfBvmOBxJSbCqhHCMY6Dm1\n"
	if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := KnownHostKeys(kh, "10.0.0.5:2222")
	if err != nil || len(keys) != 1 || !strings.HasPrefix(keys[0], "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGnLCn9") {
		t.Errorf("keys %v, err %v", keys, err)
	}
	if keys, err = KnownHostKeys(kh, "10.0.0.6"); err != nil || len(keys) != 1 {
		t.Errorf("port 22 default: %v %v", keys, err)
	}
	if _, err = KnownHostKeys(kh, "10.0.0.7"); err == nil {
		t.Error("an unknown host should be an error")
	}
	if _, err = KnownHostKeys(filepath.Join(dir, "nothing"), "10.0.0.6"); err == nil {
		t.Error("a missing file should be an error")
	}
}
