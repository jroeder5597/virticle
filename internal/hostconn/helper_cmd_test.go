package hostconn

import (
	"errors"
	"strings"
	"testing"

	"virticle/internal/nodecmd"
)

func TestSudoersLine(t *testing.T) {
	got := SudoersLine("virticle")
	if !strings.Contains(got, "virticle ALL=(root) NOPASSWD: /usr/local/libexec/virticle-node\n") {
		t.Errorf("sudoers: %q", got)
	}
	// One rule only: the account may run exactly the node command.
	rules := 0
	for _, l := range strings.Split(got, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			rules++
		}
	}
	if rules != 1 || strings.Contains(got, "ALL=(ALL)") || strings.Contains(got, "NOPASSWD: ALL") {
		t.Errorf("sudoers must hold a single rule for the node command only: %q", got)
	}
}

func TestProtocolAgreesWithTheNodeCommand(t *testing.T) {
	if HelperVersion != nodecmd.Version || HelperPath != nodecmd.NodePath {
		t.Errorf("controller %s %s, node command %s %s", HelperVersion, HelperPath, nodecmd.Version, nodecmd.NodePath)
	}
	if HelperPath == OldHelperPath {
		t.Error("the node command must not live at the old script's path")
	}
}

func TestHelperCmdQuotes(t *testing.T) {
	got := HelperCmd("migtls-put", "/etc/pki/qemu", "it's", "$(id)")
	want := `sudo -n /usr/local/libexec/virticle-node migtls-put '/etc/pki/qemu' 'it'\''s' '$(id)'`
	if got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
}

func TestNodeCmdByPrivilege(t *testing.T) {
	if got, ok := NodeCmd(PrivHelper, "peer-key"); !ok || got != "sudo -n /usr/local/libexec/virticle-node peer-key" {
		t.Errorf("helper: %q %v", got, ok)
	}
	if got, ok := NodeCmd(PrivRoot, "stat", "/a b"); !ok || got != "/usr/local/libexec/virticle-node stat '/a b'" {
		t.Errorf("root: %q %v", got, ok)
	}
	for _, p := range []Privilege{PrivSudo, PrivNone, ""} {
		if got, ok := NodeCmd(p, "version"); ok || got != "" {
			t.Errorf("%q: %q %v", p, got, ok)
		}
	}
}

func TestPrivilegeDetection(t *testing.T) {
	v := HelperVersion
	cases := []struct {
		name, in string
		want     Privilege
		note     string
	}{
		{"root", "uid=0\nsudo=no\nuser=root", PrivRoot, "root"},
		{"root with the node command installed", "uid=0\nhelper=" + v + "\nsudo=yes\nuser=root", PrivRoot, "root"},
		{"node command", "uid=998\nhelper=" + v + "\nsudo=no\nuser=virticle", PrivHelper, "node command"},
		{"node command beats broad sudo", "uid=998\nhelper=" + v + "\nsudo=yes\nuser=virticle", PrivHelper, "broad sudo works too"},
		{"other protocol, no broad sudo", "uid=998\nhelper=0\nsudo=no\nuser=virticle", PrivNone, "Prepare node"},
		{"other protocol, broad sudo", "uid=998\nhelper=0\nsudo=yes\nuser=virticle", PrivSudo, "ships"},
		{"node command does not answer", "uid=998\nhelper=\nsudo=no\nuser=virticle", PrivNone, "sudoers"},
		{"old script, no broad sudo", "uid=998\noldhelper=yes\nsudo=no\nuser=virticle", PrivNone, OlderVersionMessage},
		{"old script, broad sudo", "uid=998\noldhelper=yes\nsudo=yes\nuser=virticle", PrivSudo, OlderVersionMessage},
		{"old script next to a current node command", "uid=998\nhelper=" + v + "\noldhelper=yes\nsudo=no\nuser=virticle", PrivHelper, "node command"},
		{"broad sudo", "uid=1000\nsudo=yes\nuser=admin", PrivSudo, "sudo"},
		{"nothing", "uid=1000\nsudo=no\nuser=admin", PrivNone, "fails"},
		{"empty", "", PrivNone, ""},
	}
	for _, c := range cases {
		got, note := ParseDetect(c.in)
		if got != c.want || !strings.Contains(note, c.note) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, got, note, c.want, c.note)
		}
	}
	if !strings.Contains(DetectScript, HelperPath+" version") {
		t.Error("DetectScript does not ask the node command for its version")
	}
	if !strings.Contains(DetectScript, OldHelperPath) {
		t.Error("DetectScript does not look for the old helper script")
	}
	if _, err := WrapRoot(PrivHelper, "id"); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("a free-form command under the node command: %v", err)
	}
	if !PrivHelper.Valid() {
		t.Error("helper is not a valid setting")
	}
}
