package prepare

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The firewall scripts are run for real in sh against stand-ins for
// firewall-cmd, ufw and nft that keep their state in files, so the generated
// commands, the idempotence and the removal are tested without a firewall.

const fakeFirewallCmd = `#!/bin/sh
S=$FWSTATE
echo "firewall-cmd $*" >> "$S/log"
if [ -f "$S/failon" ] && echo "$*" | grep -qF -- "$(cat "$S/failon")"; then echo "boom: $*" >&2; exit 1; fi
perm=0; zone=public; svc=; act=
for a in "$@"; do case "$a" in
  --permanent) perm=1;;
  --zone=*) zone=${a#--zone=};;
  --service=*) svc=${a#--service=};;
  *) act="$act $a";;
esac; done
d=$S/run; [ $perm = 1 ] && d=$S/perm
set -- $act
case "$1" in
--state) [ -f "$S/off" ] && { echo "not running"; exit 252; }; echo running;;
--get-default-zone) echo public;;
--get-zones) echo "public home";;
--get-services) ls "$d/services" | tr '\n' ' '; echo;;
--get-ports) [ -f "$d/services/$svc" ] || exit 1; grep -v '^#' "$d/services/$svc" | tr '\n' ' '; echo;;
--new-service=*) n=${1#--new-service=}; [ -f "$d/services/$n" ] && exit 1; : > "$d/services/$n";;
--set-short=*|--set-description=*) [ -f "$d/services/$svc" ] || exit 1; echo "# $1" >> "$d/services/$svc";;
--add-port=*) [ -f "$d/services/$svc" ] || exit 1; echo "${1#--add-port=}" >> "$d/services/$svc";;
--remove-port=*) grep -vxF "${1#--remove-port=}" "$d/services/$svc" > "$d/t"; mv "$d/t" "$d/services/$svc";;
--query-service=*) grep -qxF "${1#--query-service=}" "$d/zone-$zone" 2>/dev/null;;
--add-service=*) n=${1#--add-service=}; [ -f "$d/services/$n" ] || exit 1; grep -qxF "$n" "$d/zone-$zone" 2>/dev/null || echo "$n" >> "$d/zone-$zone";;
--remove-service=*) grep -vxF "${1#--remove-service=}" "$d/zone-$zone" > "$d/t"; mv "$d/t" "$d/zone-$zone";;
--delete-service=*) rm -f "$d/services/${1#--delete-service=}";;
--reload) rm -rf "$S/run"; cp -r "$S/perm" "$S/run";;
*) echo "unknown $*" >&2; exit 2;;
esac
`

const fakeUfw = `#!/bin/sh
S=$FWSTATE
echo "ufw $*" >> "$S/log"
if [ -f "$S/failon" ] && echo "$*" | grep -qF -- "$(cat "$S/failon")"; then echo "boom: $*" >&2; exit 1; fi
case "$1" in
status) [ -f "$S/off" ] && echo "Status: inactive" || echo "Status: active";;
show) echo "Added user rules (see 'ufw status' for running firewall):"; cat "$S/added";;
allow) spec=$2; line="ufw allow $spec"; [ "$3" = comment ] && line="$line comment '$4'"
  grep -qF "ufw allow $spec " "$S/added" 2>/dev/null && exit 0; echo "$line" >> "$S/added";;
delete) grep -vF "ufw allow $3 " "$S/added" > "$S/t"; mv "$S/t" "$S/added";;
*) exit 2;;
esac
`

type fwHost struct {
	t     *testing.T
	dir   string
	state string
}

// newFwHost makes a host with the given tools: "firewalld", "ufw", "nft".
func newFwHost(t *testing.T, tools ...string) *fwHost {
	t.Helper()
	h := &fwHost{t: t, dir: t.TempDir(), state: t.TempDir()}
	for _, d := range []string{"perm/services", "run/services"} {
		os.MkdirAll(filepath.Join(h.state, d), 0o755)
	}
	// The administrator's own SSH rule, which must never change.
	for _, d := range []string{"perm", "run"} {
		os.WriteFile(filepath.Join(h.state, d, "zone-public"), []byte("ssh\n"), 0o644)
	}
	os.WriteFile(filepath.Join(h.state, "added"), []byte("ufw allow 22/tcp\n"), 0o644)
	os.WriteFile(filepath.Join(h.state, "log"), nil, 0o644)
	// A PATH of our own: only the basic tools and the ones of this test.
	for _, name := range []string{"sh", "grep", "sed", "tr", "sort", "cat", "head", "ls", "rm", "mv", "cp", "printf"} {
		if p, err := exec.LookPath(name); err == nil {
			os.Symlink(p, filepath.Join(h.dir, name))
		}
	}
	for _, tool := range tools {
		switch tool {
		case "firewalld":
			os.WriteFile(filepath.Join(h.dir, "firewall-cmd"), []byte(fakeFirewallCmd), 0o755)
		case "ufw":
			os.WriteFile(filepath.Join(h.dir, "ufw"), []byte(fakeUfw), 0o755)
		case "nft":
			os.WriteFile(filepath.Join(h.dir, "nft"), []byte("#!/bin/sh\necho 'table inet filter { chain input { type filter hook input priority 0; } }'\n"), 0o755)
		}
	}
	return h
}

func (h *fwHost) run(script string) (string, error) {
	cmd := exec.Command(filepath.Join(h.dir, "sh"), "-c", script)
	cmd.Env = []string{"PATH=" + h.dir, "FWSTATE=" + h.state}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *fwHost) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.state, name))
	return string(b)
}

func (h *fwHost) set(name, v string) {
	os.WriteFile(filepath.Join(h.state, name), []byte(v), 0o644)
}

// mutations are the logged commands that change something.
func (h *fwHost) mutations() []string {
	var m []string
	for _, l := range strings.Split(h.read("log"), "\n") {
		for _, w := range []string{"--new-service", "--set-", "--add-", "--remove-", "--delete-", "--reload", "ufw allow", "ufw delete"} {
			if strings.Contains(l, w) {
				m = append(m, l)
				break
			}
		}
	}
	return m
}

func (h *fwHost) clearLog() { h.set("log", "") }

func mustRun(t *testing.T, h *fwHost, script string) string {
	t.Helper()
	out, err := h.run(script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return out
}

func TestFirewalldOpensTheMigrationPortsAndChangesNothingTheSecondTime(t *testing.T) {
	h := newFwHost(t, "firewalld")
	row, changed := ApplyFirewall(h.run, NodeFirewall())
	if row.Status != OK || !changed || row.Detail != "Opened TCP ports 49152 to 49215 in firewalld (service virticle-node)." {
		t.Fatalf("%+v changed %v", row, changed)
	}
	want := []string{
		"firewall-cmd --permanent --new-service=virticle-node",
		"firewall-cmd --permanent --service=virticle-node --set-short=virticle-node",
		"firewall-cmd --permanent --service=virticle-node --set-description=Virticle: live migration data between nodes",
		"firewall-cmd --permanent --service=virticle-node --add-port=49152-49215/tcp",
		"firewall-cmd --permanent --add-service=virticle-node",
		"firewall-cmd --reload",
	}
	if got := h.mutations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if z := h.read("run/zone-public"); !strings.Contains(z, "virticle-node") || !strings.Contains(z, "ssh") {
		t.Errorf("runtime zone: %q", z)
	}
	f := ParseFirewallFacts(mustRun(t, h, FirewallCheckScript(NodeFirewall())))
	if f.Kind != "firewalld" || !f.Open {
		t.Errorf("facts after: %+v", f)
	}

	h.clearLog()
	row, changed = ApplyFirewall(h.run, NodeFirewall())
	if row.Status != OK || changed || !strings.Contains(row.Detail, "already open") {
		t.Errorf("second run: %+v changed %v", row, changed)
	}
	if m := h.mutations(); len(m) != 0 {
		t.Errorf("the second run changed the firewall: %v", m)
	}
}

func TestFirewalldRuntimeMissingReloadsOnly(t *testing.T) {
	h := newFwHost(t, "firewalld")
	ApplyFirewall(h.run, NodeFirewall())
	// The daemon restarted with a lost runtime: the permanent config is there.
	os.RemoveAll(filepath.Join(h.state, "run"))
	os.MkdirAll(filepath.Join(h.state, "run/services"), 0o755)
	h.clearLog()
	row, changed := ApplyFirewall(h.run, NodeFirewall())
	if row.Status != OK || !changed {
		t.Fatalf("%+v", row)
	}
	if m := h.mutations(); len(m) != 1 || m[0] != "firewall-cmd --reload" {
		t.Errorf("only a reload is needed: %v", m)
	}
}

func TestFirewalldWebPortChangeReplacesOnlyOurOldPort(t *testing.T) {
	h := newFwHost(t, "firewalld")
	web := FirewallSpec{Service: "virticle", Description: "Virticle web interface", Ports: []string{"8443"}, Actor: "Setup", Purpose: "for the web interface"}
	row, _ := ApplyFirewall(h.run, web)
	if row.Detail != "Opened TCP port 8443 in firewalld (service virticle)." {
		t.Errorf("%+v", row)
	}
	web.Ports = []string{"9443", "80"}
	h.clearLog()
	row, changed := ApplyFirewall(h.run, web)
	if row.Status != OK || !changed || !strings.Contains(row.Detail, "TCP ports 9443 and 80") {
		t.Errorf("%+v", row)
	}
	ports := h.read("perm/services/virticle")
	if strings.Contains(ports, "8443") || !strings.Contains(ports, "9443/tcp") || !strings.Contains(ports, "80/tcp") {
		t.Errorf("service ports after the change:\n%s", ports)
	}
	for _, m := range h.mutations() {
		if strings.Contains(m, "ssh") {
			t.Errorf("touched ssh: %s", m)
		}
	}
}

func TestFirewalldRemoveTakesOutOnlyTheService(t *testing.T) {
	h := newFwHost(t, "firewalld")
	ApplyFirewall(h.run, NodeFirewall())
	row, changed := ApplyFirewall(h.run, FirewallSpec{Service: NodeFirewallService})
	if row.Status != OK || !changed || row.Detail != "Removed the firewalld service virticle-node." {
		t.Fatalf("%+v", row)
	}
	if _, err := os.Stat(filepath.Join(h.state, "perm/services/virticle-node")); err == nil {
		t.Error("the service is still defined")
	}
	for _, d := range []string{"perm", "run"} {
		if z := h.read(d + "/zone-public"); strings.TrimSpace(z) != "ssh" {
			t.Errorf("%s zone after removal: %q", d, z)
		}
	}
	row, changed = ApplyFirewall(h.run, FirewallSpec{Service: NodeFirewallService})
	if changed || !strings.Contains(row.Detail, "Nothing to remove") {
		t.Errorf("removing twice: %+v", row)
	}
}

func TestFirewalldCommandFailureIsAWarningWithTheCommands(t *testing.T) {
	h := newFwHost(t, "firewalld")
	h.set("failon", "--add-port")
	row, changed := ApplyFirewall(h.run, NodeFirewall())
	if row.Status != Warning || changed {
		t.Fatalf("%+v", row)
	}
	for _, must := range []string{"`firewall-cmd --permanent --service=virticle-node --add-port=49152-49215/tcp` failed", "boom", "Run as root: firewall-cmd --permanent --new-service=virticle-node && firewall-cmd --permanent --service=virticle-node --add-port=49152-49215/tcp && firewall-cmd --permanent --add-service=virticle-node && firewall-cmd --reload"} {
		if !strings.Contains(row.Detail, must) {
			t.Errorf("warning lacks %q:\n%s", must, row.Detail)
		}
	}
}

func TestUfwRulesAreAddedOnceAndRemovedByName(t *testing.T) {
	h := newFwHost(t, "ufw")
	row, changed := ApplyFirewall(h.run, NodeFirewall())
	if row.Status != OK || !changed || row.Detail != "Opened TCP ports 49152 to 49215 in ufw (rule virticle-node)." {
		t.Fatalf("%+v", row)
	}
	if got := h.mutations(); len(got) != 1 || got[0] != "ufw allow 49152:49215/tcp comment virticle-node" {
		t.Errorf("commands: %v", got)
	}
	f := ParseFirewallFacts(mustRun(t, h, FirewallCheckScript(NodeFirewall())))
	if f.Kind != "ufw" || !f.Open {
		t.Errorf("facts: %+v", f)
	}
	h.clearLog()
	row, changed = ApplyFirewall(h.run, NodeFirewall())
	if changed || len(h.mutations()) != 0 || !strings.Contains(row.Detail, "already open") {
		t.Errorf("second run: %+v %v", row, h.mutations())
	}
	row, changed = ApplyFirewall(h.run, FirewallSpec{Service: NodeFirewallService})
	if !changed || row.Detail != "Removed the ufw rules marked virticle-node." {
		t.Errorf("remove: %+v", row)
	}
	if added := h.read("added"); added != "ufw allow 22/tcp\n" {
		t.Errorf("the administrator's rules after removal: %q", added)
	}
}

func TestUfwWebPortChange(t *testing.T) {
	h := newFwHost(t, "ufw")
	web := FirewallSpec{Service: "virticle", Ports: []string{"8443"}, Actor: "Setup", Purpose: "for the web interface"}
	ApplyFirewall(h.run, web)
	web.Ports = []string{"9000"}
	h.clearLog()
	row, changed := ApplyFirewall(h.run, web)
	if !changed || row.Status != OK {
		t.Fatalf("%+v", row)
	}
	want := []string{"ufw delete allow 8443/tcp", "ufw allow 9000/tcp comment virticle"}
	if got := h.mutations(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commands %v want %v", got, want)
	}
	if added := h.read("added"); strings.Contains(added, "8443") || !strings.Contains(added, "22/tcp") {
		t.Errorf("rules: %q", added)
	}
}

func TestUfwFailureIsAWarning(t *testing.T) {
	h := newFwHost(t, "ufw")
	h.set("failon", "allow")
	row, _ := ApplyFirewall(h.run, NodeFirewall())
	if row.Status != Warning || !strings.Contains(row.Detail, "ufw allow 49152:49215/tcp comment 'virticle-node'") {
		t.Errorf("%+v", row)
	}
}

func TestNoFirewallAndNftablesOnly(t *testing.T) {
	h := newFwHost(t)
	row, changed := ApplyFirewall(h.run, NodeFirewall())
	if row.Status != OK || changed || row.Detail != "No firewall is active on this host. Nothing to open." {
		t.Errorf("none: %+v", row)
	}
	f := ParseFirewallFacts(mustRun(t, h, FirewallCheckScript(NodeFirewall())))
	if f.Kind != "none" || f.Nft {
		t.Errorf("facts: %+v", f)
	}

	h = newFwHost(t, "nft")
	row, changed = ApplyFirewall(h.run, NodeFirewall())
	if row.Status != Info || changed || !strings.HasPrefix(row.Detail, "A firewall may be active (nftables rules found); open these ports yourself: TCP ports 49152 to 49215") {
		t.Errorf("nft: %+v", row)
	}
	f = ParseFirewallFacts(mustRun(t, h, FirewallCheckScript(NodeFirewall())))
	if f.Kind != "none" || !f.Nft {
		t.Errorf("facts: %+v", f)
	}
	// Removing with nft rules and no front end: nothing to remove, no advice.
	row, _ = ApplyFirewall(h.run, FirewallSpec{Service: NodeFirewallService})
	if row.Status != OK || !strings.Contains(row.Detail, "Nothing to remove") {
		t.Errorf("nft remove: %+v", row)
	}
}

func TestInactiveFirewallsAreNotTouched(t *testing.T) {
	h := newFwHost(t, "firewalld", "ufw")
	h.set("off", "1")
	row, changed := ApplyFirewall(h.run, NodeFirewall())
	if changed || row.Status != OK || len(h.mutations()) != 0 {
		t.Errorf("%+v %v", row, h.mutations())
	}
}

func TestFirewallCheckRows(t *testing.T) {
	s := NodeFirewall()
	cases := []struct {
		f      FirewallFacts
		status string
		detail string
	}{
		{FirewallFacts{Kind: "firewalld"}, Info, "firewalld is active. Prepare will open TCP ports 49152 to 49215 for migrations between nodes."},
		{FirewallFacts{Kind: "firewalld", Open: true}, OK, "firewalld is active and TCP ports 49152 to 49215 are already open (service virticle-node)."},
		{FirewallFacts{Kind: "ufw"}, Info, "ufw is active. Prepare will open TCP ports 49152 to 49215 for migrations between nodes."},
		{FirewallFacts{Kind: "ufw", Open: true}, OK, "ufw is active and TCP ports 49152 to 49215 are already open (rule virticle-node)."},
		{FirewallFacts{Kind: "none"}, OK, "No firewall is active on this host. Nothing to open."},
		{FirewallFacts{Kind: "none", Nft: true}, Info, "A firewall may be active (nftables rules found); open these ports yourself: TCP ports 49152 to 49215 for migrations between nodes."},
		{FirewallFacts{}, OK, "No firewall is active on this host. Nothing to open."},
	}
	for _, c := range cases {
		if r := FirewallCheckRow(c.f, s); r.Status != c.status || r.Detail != c.detail {
			t.Errorf("%+v:\n got %s %q\nwant %s %q", c.f, r.Status, r.Detail, c.status, c.detail)
		}
	}
}

func TestPortsText(t *testing.T) {
	for in, want := range map[string]string{
		"8443":        "TCP port 8443",
		"80,8443":     "TCP ports 80 and 8443",
		"49152-49215": "TCP ports 49152 to 49215",
	} {
		if got := PortsText(strings.Split(in, ",")); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
}

// --- through the node steps ---

func firewallHost(facts string, fwOut string) *fakeNode {
	return hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
		switch {
		case strings.Contains(cmd, "os_id="):
			return facts, true, nil
		case strings.Contains(cmd, "fw_firewalld() {") && strings.Contains(cmd, "fwdesc="):
			return fwOut, true, nil
		}
		return "", false, nil
	})
}

func TestPrepareFirewalldRowsAndStepOrder(t *testing.T) {
	n := firewallHost(debian13Facts+"fw=firewalld\nfw_open=no\n", "@@kind firewalld\n@@changed\n")
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "firewall"); r.Status != Info || r.Detail != "firewalld is active. Prepare will open TCP ports 49152 to 49215 for migrations between nodes." {
		t.Errorf("check row: %+v", r)
	}
	if r := find(rows, "do-firewall"); r.Status != OK || r.Detail != "Opened TCP ports 49152 to 49215 in firewalld (service virticle-node)." {
		t.Errorf("step row: %+v", r)
	}
	var ids []string
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "do-") {
			ids = append(ids, r.ID)
		}
	}
	if ids[0] != "do-packages" || ids[1] != "do-firewall" {
		t.Errorf("the firewall step comes right after the packages: %v", ids)
	}
	all := strings.Join(n.log, "\n")
	if !strings.Contains(all, "fwports=") && strings.Contains(all, "49152-49215") {
		t.Error("the migration ports were not sent")
	}
}

func TestPrepareFirewallFailureIsAWarningAndPrepareContinues(t *testing.T) {
	n := firewallHost(debian13Facts+"fw=ufw\nfw_open=no\n", "@@kind ufw\n@@failed ufw allow 49152:49215/tcp comment 'virticle-node'\n@@detail ERROR: problem running ufw\n")
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	r := find(rows, "do-firewall")
	if r.Status != Warning || !strings.Contains(r.Detail, "ufw allow 49152:49215/tcp comment 'virticle-node'") || !strings.Contains(r.Detail, "Run as root") {
		t.Errorf("%+v", r)
	}
	if HasError(rows) || find(rows, "do-peerkey").Status != OK {
		t.Errorf("prepare must go on: %+v", rows)
	}
}

func TestCheckOnlyReadsTheFirewall(t *testing.T) {
	n := firewallHost(debian13Facts+"fw=firewalld\nfw_open=no\n", "")
	rows, _, err := (&Runner{N: n}).Check(Options{ControllerKey: testKey})
	if err != nil || find(rows, "firewall").Status != Info {
		t.Fatalf("%v %+v", err, rows)
	}
	for _, c := range n.log {
		if strings.Contains(c, "fwdesc=") {
			t.Error("--check ran the firewall script")
		}
	}
}

func TestUnprepareRemovesTheFirewallService(t *testing.T) {
	n := &fakeNode{}
	n.handlers = append(n.handlers, func(cmd, stdin string) (string, bool, error) {
		switch {
		case strings.Contains(cmd, "echo uid=$(id -u); echo user"):
			return "uid=0\nuser=root", true, nil
		case strings.Contains(cmd, "fwdesc="):
			if strings.Contains(cmd, "49152") {
				t.Error("unprepare must ask for removal (no ports)")
			}
			return "@@kind firewalld\n@@changed\n", true, nil
		case strings.Contains(cmd, "userdel"):
			return "key removed\n@@changed", true, nil
		}
		return "", true, nil
	})
	rows := (&Runner{N: n}).Unprepare(Options{ControllerKey: testKey})
	if r := find(rows, "firewall"); r.Status != OK || r.Detail != "Removed the firewalld service virticle-node." {
		t.Errorf("%+v", rows)
	}
}

func TestFirewallScriptsNeverDropOrTouchSSH(t *testing.T) {
	for _, s := range []string{FirewallScript(NodeFirewall()), FirewallScript(FirewallSpec{Service: "x"})} {
		for _, never := range []string{"rich-rule", "reject", "drop", "deny", "ufw enable", "ufw reset", "set-target", "ssh", "runtime-to-permanent", "complete-reload"} {
			if strings.Contains(strings.ToLower(s), never) {
				t.Errorf("the firewall script contains %q", never)
			}
		}
	}
}
