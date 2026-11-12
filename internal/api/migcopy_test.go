package api

import (
	"strings"
	"testing"
)

func TestReachCmdQuotes(t *testing.T) {
	c := reachCmd("10.0.0.1", "22")
	if !strings.Contains(c, "timeout 6 bash -c ") || !strings.Contains(c, "/dev/tcp/10.0.0.1/22") {
		t.Error(c)
	}
}
