package prepare

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runNetworkScript runs networkScript with a fake virsh that knows one "default" network with the given state and
// records the commands it gets.
func runNetworkScript(t *testing.T, active, autostart string) (out string, calls string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	fake := `#!/bin/sh
shift 2
echo "$*" >> ` + log + `
case "$1" in
net-info) printf 'Name: default\nActive: ` + active + `\nAutostart: ` + autostart + `\n' ;;
*) ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "virsh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", networkScript)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v: %s", err, b)
	}
	c, _ := os.ReadFile(log)
	return string(b), string(c)
}

// Debian defines the default network without autostart: prepare turns it on, because a VM on that network that
// starts with the node could not start otherwise.
func TestNetworkScriptTurnsOnAutostart(t *testing.T) {
	out, calls := runNetworkScript(t, "yes", "no")
	d, changed := parseStep(out)
	if !changed || !strings.Contains(d, "autostart") {
		t.Errorf("active, no autostart: detail %q changed %v", d, changed)
	}
	if !strings.Contains(calls, "net-autostart default") || strings.Contains(calls, "net-start") {
		t.Errorf("calls: %q", calls)
	}

	out, calls = runNetworkScript(t, "no", "no")
	d, changed = parseStep(out)
	if !changed || !strings.Contains(d, "Started the existing default network") || !strings.Contains(d, "autostart") {
		t.Errorf("inactive, no autostart: detail %q changed %v", d, changed)
	}
	if !strings.Contains(calls, "net-autostart default") || !strings.Contains(calls, "net-start default") {
		t.Errorf("calls: %q", calls)
	}
}

func TestNetworkScriptLeavesAGoodNetworkAlone(t *testing.T) {
	out, calls := runNetworkScript(t, "yes", "yes")
	d, changed := parseStep(out)
	if changed || d != "Default network is active" {
		t.Errorf("detail %q changed %v", d, changed)
	}
	if strings.Contains(calls, "net-autostart") || strings.Contains(calls, "net-start") {
		t.Errorf("calls: %q", calls)
	}
}
