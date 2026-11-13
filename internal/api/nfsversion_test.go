package api

import (
	"strings"
	"testing"
)

func TestParseMountVersion(t *testing.T) {
	for in, want := range map[string]string{
		"rw,nosuid,nodev,noexec,relatime,vers=4.2,rsize=262144,hard": "4.2",
		"rw,vers=3,rsize=262144":                                     "3",
		"rw,hard":                                                    "",
		"":                                                           "",
	} {
		if got := parseMountVersion(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestVersionNote(t *testing.T) {
	if got := versionNote([]nodeOutcome{{NodeName: "a", OK: true, Version: "4.2"}, {NodeName: "b", OK: true}}); got != "" {
		t.Errorf("version 4 needs no note: %q", got)
	}
	got := versionNote([]nodeOutcome{{NodeName: "a", OK: true, Version: "3"}, {NodeName: "b", OK: true, Version: "4.2"}, {NodeName: "c", OK: false, Version: "3"}})
	if !strings.Contains(got, "NFS version 3 on a:") {
		t.Errorf("%q", got)
	}
}
