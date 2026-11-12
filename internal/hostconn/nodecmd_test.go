package hostconn

import (
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
	if NodeCmdVersion != nodecmd.Version || NodeCmdPath != nodecmd.NodePath {
		t.Errorf("controller %s %s, node command %s %s", NodeCmdVersion, NodeCmdPath, nodecmd.Version, nodecmd.NodePath)
	}
	if NodeCmdPath == OldHelperPath {
		t.Error("the node command must not live at the old script's path")
	}
}

func TestNodeCmdQuotes(t *testing.T) {
	got, _ := NodeCmd(PrivService, "migtls-put", "/etc/pki/qemu", "it's", "$(id)")
	want := `sudo -n /usr/local/libexec/virticle-node migtls-put '/etc/pki/qemu' 'it'\''s' '$(id)'`
	if got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
}

func TestNodeCmdByPrivilege(t *testing.T) {
	if got, ok := NodeCmd(PrivService, "peer-key"); !ok || got != "sudo -n /usr/local/libexec/virticle-node peer-key" {
		t.Errorf("service account: %q %v", got, ok)
	}
	if got, ok := NodeCmd(PrivRoot, "stat", "/a b"); !ok || got != "/usr/local/libexec/virticle-node stat '/a b'" {
		t.Errorf("root: %q %v", got, ok)
	}
	for _, p := range []Privilege{"sudo", PrivNone, ""} {
		if got, ok := NodeCmd(p, "version"); ok || got != "" {
			t.Errorf("%q: %q %v", p, got, ok)
		}
	}
}

func TestPrivilegeDetection(t *testing.T) {
	v := NodeCmdVersion
	cases := []struct {
		name, in string
		want     Privilege
		note     string
	}{
		{"root with the node command", "uid=0\nnode=" + v + "\nuser=root", PrivRoot, "node command"},
		{"root without the node command", "uid=0\nuser=root", PrivNone, NoNodeCommandMessage},
		{"root, other protocol", "uid=0\nnode=0\nuser=root", PrivNone, "virticle node prepare"},
		{"service account", "uid=998\nnode=" + v + "\nuser=virticle", PrivService, "node command"},
		{"service account, other protocol", "uid=998\nnode=0\nuser=virticle", PrivNone, "ships"},
		{"node command does not answer", "uid=998\nnode=\nuser=virticle", PrivNone, "sudoers"},
		{"old script", "uid=998\noldscript=yes\nuser=virticle", PrivNone, OlderVersionMessage},
		{"old script, root", "uid=0\noldscript=yes\nuser=root", PrivNone, OlderVersionMessage},
		{"old script next to a current node command", "uid=998\nnode=" + v + "\noldscript=yes\nuser=virticle", PrivService, "node command"},
		{"general sudo but no node command is not prepared", "uid=1000\nsudo=yes\nuser=admin", PrivNone, NoNodeCommandMessage},
		{"nothing", "uid=1000\nuser=admin", PrivNone, NoNodeCommandMessage},
		{"empty", "", PrivNone, NoNodeCommandMessage},
	}
	for _, c := range cases {
		got, note := ParseDetect(c.in)
		if got != c.want || !strings.Contains(note, c.note) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, got, note, c.want, c.note)
		}
	}
	if !strings.Contains(DetectScript, NodeCmdPath+" version") {
		t.Error("DetectScript does not ask the node command for its version")
	}
	if !strings.Contains(DetectScript, OldHelperPath) {
		t.Error("DetectScript does not look for the old shell script")
	}
	if strings.Contains(DetectScript, "sudo -n true") {
		t.Error("DetectScript still probes for general sudo")
	}
	if !PrivService.Valid() {
		t.Error("the service account is not a valid setting")
	}
}
