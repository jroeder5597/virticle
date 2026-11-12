package api

import (
	"strings"
	"testing"

	"virticle/internal/hostconn"
)

// The helper carries its own copy of the first-boot script it hands to
// virt-sysprep, so the caller cannot supply commands. It must stay the same as
// the controller's copy that root and sudo nodes run.
func TestHelperSysprepScriptMatches(t *testing.T) {
	const begin, end = "<<'FIXUPS_END'\n", "\nFIXUPS_END\n"
	s := hostconn.HelperScript
	a := strings.Index(s, begin)
	b := strings.Index(s, end)
	if a < 0 || b < a {
		t.Fatal("the helper has no first-boot script")
	}
	got := strings.TrimSpace(s[a+len(begin) : b])
	if want := strings.TrimSpace(linuxFirstBootFixups); got != want {
		t.Errorf("the helper's first-boot script differs from linuxFirstBootFixups:\nhelper:\n%s\ncontroller:\n%s", got, want)
	}
}
