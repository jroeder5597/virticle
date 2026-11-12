package api

import (
	"strings"
	"testing"

	"virticle/internal/nodecmd"
)

// The node command carries its own copy of the first-boot script it hands to
// virt-sysprep, so the caller cannot supply commands. It must stay the same as
// the controller's copy that root and sudo nodes run.
func TestNodeCommandSysprepScriptMatches(t *testing.T) {
	got := strings.TrimSpace(nodecmd.SysprepFixups)
	if want := strings.TrimSpace(linuxFirstBootFixups); got != want {
		t.Errorf("the node command's first-boot script differs from linuxFirstBootFixups:\nnode command:\n%s\ncontroller:\n%s", got, want)
	}
}
