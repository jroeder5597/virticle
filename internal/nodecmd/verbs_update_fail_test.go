package nodecmd

import (
	"strings"
	"testing"
)

func TestToolFailurePicksTheErrorLines(t *testing.T) {
	out := "Upgrading: foo\n  Running scriptlet: ru-testpkg-1.6-1.noarch\nError: Transaction failed\nFailed:\n  ru-testpkg-1.0-1.noarch   ru-testpkg-1.6-1.noarch\n"
	got := toolFailure(out)
	if !strings.Contains(got, "Error: Transaction failed") || strings.Contains(got, "ru-testpkg-1.0-1") {
		t.Fatalf("%q", got)
	}
	if got := toolFailure("one\ntwo"); got != "two" {
		t.Fatalf("%q", got)
	}
}
