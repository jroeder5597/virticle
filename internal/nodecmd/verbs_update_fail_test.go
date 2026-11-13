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

// apt names the package that failed only in the dpkg line; the message must carry it.
func TestToolFailureNamesTheAptPackage(t *testing.T) {
	out := "Setting up dw-conf (3.0) ...\ndw-conf postinst failing on purpose\ndpkg: error processing package dw-conf (--configure):\n installed dw-conf package post-installation script subprocess returned error exit status 1\nErrors were encountered while processing:\n dw-conf\nE: Sub-process /usr/bin/dpkg returned an error code (1)\n"
	got := toolFailure(out)
	if !strings.Contains(got, "error processing package dw-conf") {
		t.Fatalf("%q", got)
	}
}
