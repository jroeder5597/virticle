package api

import (
	"os"
	"strings"
	"testing"
)

// The helper carries its own copy of the first-boot script it hands to
// virt-sysprep, so the caller cannot supply commands. It must stay the same as
// the controller's copy that root and sudo nodes run.
func TestHelperSysprepScriptMatches(t *testing.T) {
	const begin, end = "<<'FIXUPS_END'\n", "\nFIXUPS_END\n"
	// TODO(pool verbs port): the script is no longer embedded; it is read from
	// the tree until the sysprep verb moves to internal/nodecmd, which then
	// compares its own copy of the script instead.
	raw, err := os.ReadFile("../hostconn/virticle-helper.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
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
