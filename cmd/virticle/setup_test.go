package main

import (
	"strings"
	"testing"

	"virticle/internal/prepare"
	"virticle/internal/webaccess"
)

func TestWebPorts(t *testing.T) {
	https := webaccess.FirstRunDefaults()
	loop := webaccess.Defaults()
	redirect := https
	redirect.HTTPRedirect = ":8080"
	moved := https
	moved.HTTPSListen = "0.0.0.0:9443"
	lan := webaccess.Defaults()
	lan.Listen = "192.0.2.10:8080"
	same := https
	same.HTTPRedirect = "0.0.0.0:8443"
	for _, c := range []struct {
		name string
		ws   webaccess.Settings
		want string
	}{
		{"first start", https, "8443"},
		{"loopback http", loop, ""},
		{"https with redirect", redirect, "8443 8080"},
		{"moved", moved, "9443"},
		{"http on an interface", lan, "8080"},
		{"same port twice", same, "8443"},
	} {
		if got := strings.Join(webPorts(c.ws), " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// host scripts a machine: the answers of the firewall and service scripts.
type host struct {
	log      []string
	fwFacts  string // answer to the firewall check
	fwApply  string // answer to the firewall apply script
	unit     string
	startOut string
}

func (h *host) sh(script string) (string, error) {
	h.log = append(h.log, script)
	switch {
	case strings.Contains(script, "fw_firewalld() {"):
		return h.fwApply, nil
	case strings.Contains(script, `echo "fw=$fwkind"`):
		return h.fwFacts, nil
	case strings.Contains(script, "systemctl cat"):
		return h.unit, nil
	case strings.Contains(script, "enable --now"):
		return h.startOut, nil
	}
	return "", nil
}

func (h *host) applied(ports string) bool {
	for _, s := range h.log {
		if strings.Contains(s, "fw_firewalld() {") && strings.Contains(s, "fwports='"+ports+"'") {
			return true
		}
	}
	return false
}

func envFor(h *host, ws webaccess.Settings, source string, users bool) setupEnv {
	return setupEnv{euid: 0, sh: h.sh, addrs: []string{"192.0.2.10", "box"}, hasKVM: true,
		settings: func() (webaccess.Settings, string, bool, error) { return ws, source, users, nil }}
}

func rowText(rows []prepare.Row) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.Status + " | " + r.Title + " | " + r.Detail + "\n")
	}
	return b.String()
}

func TestSetupNeedsRoot(t *testing.T) {
	e := envFor(&host{}, webaccess.FirstRunDefaults(), "default", false)
	e.euid = 1000
	rows := runSetup(e, false, false)
	if len(rows) != 1 || rows[0].Status != prepare.Error || !strings.Contains(rows[0].Detail, "needs root. Run: sudo virticle setup") {
		t.Errorf("%+v", rows)
	}
}

func TestSetupOpensTheWebPortAndSaysWhereToGo(t *testing.T) {
	h := &host{fwApply: "@@kind firewalld\n@@changed\n", unit: "unit=yes\nactive=inactive\nenabled=no\n"}
	rows := runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), false, false)
	want := `info | Web access | HTTPS on TCP port 8443 (the default for a first start).
ok | Firewall | Opened TCP port 8443 in firewalld (service virticle).
ok | Service | Enabled and started virticle.service.
info | Next | open https://192.0.2.10:8443 to create the administrator
info | Virtual machines | To run virtual machines on this machine too, choose 'Set up this machine as a node' on the start page.
`
	if got := rowText(rows); got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
	if !h.applied("8443") {
		t.Error("the web port was not sent to the firewall script")
	}
}

func TestSetupAgainSaysAlreadyAndUsesStoredSettings(t *testing.T) {
	ws := webaccess.FirstRunDefaults()
	ws.HTTPSListen = ":9443"
	h := &host{fwApply: "@@kind firewalld\n@@unchanged\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	e := envFor(h, ws, "stored", true)
	e.hasKVM = false
	got := rowText(runSetup(e, false, false))
	want := `info | Web access | HTTPS on TCP port 9443 (stored settings).
ok | Firewall | TCP port 9443 is already open in firewalld (service virticle).
ok | Service | virticle.service is already running and enabled.
info | Next | open https://192.0.2.10:9443 to sign in
`
	if got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
	for _, s := range h.log {
		if strings.Contains(s, "enable --now") {
			t.Error("a running service was started again")
		}
	}
}

func TestSetupCheckChangesNothing(t *testing.T) {
	h := &host{fwFacts: "fw=ufw\nfw_open=no\n", unit: "unit=yes\nactive=inactive\nenabled=no\n"}
	got := rowText(runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), true, false))
	for _, must := range []string{"info | Firewall | ufw is active. Setup will open TCP port 8443 for the web interface.", "info | Service | Setup will enable and start virticle.service."} {
		if !strings.Contains(got, must) {
			t.Errorf("lacks %q:\n%s", must, got)
		}
	}
	for _, s := range h.log {
		if strings.Contains(s, "fw_firewalld() {") || strings.Contains(s, "enable --now") {
			t.Error("--check changed something")
		}
	}
}

func TestSetupLoopbackNeedsNoPortAndClosesAnOldOne(t *testing.T) {
	h := &host{fwApply: "@@kind firewalld\n@@changed\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	got := rowText(runSetup(envFor(h, webaccess.Defaults(), "stored", true), false, false))
	for _, must := range []string{"HTTP on this computer only (127.0.0.1:8080), so no firewall port is needed", "Removed the firewalld service virticle.", "open http://127.0.0.1:8080 to sign in"} {
		if !strings.Contains(got, must) {
			t.Errorf("lacks %q:\n%s", must, got)
		}
	}
	if !h.applied("") {
		t.Error("the old rule was not removed")
	}
}

func TestSetupFirewallFailureIsAWarningAndSetupGoesOn(t *testing.T) {
	h := &host{fwApply: "@@kind ufw\n@@failed ufw allow 8443/tcp comment 'virticle'\n@@detail boom\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	rows := runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), false, false)
	got := rowText(rows)
	if !strings.Contains(got, "warning | Firewall | The firewall command `ufw allow 8443/tcp comment 'virticle'` failed (boom)") || !strings.Contains(got, "Run as root: ufw allow 8443/tcp comment 'virticle'") {
		t.Errorf("%s", got)
	}
	if !strings.Contains(got, "info | Next |") || exitCode(rows) != 0 {
		t.Errorf("setup must go on and exit 0:\n%s", got)
	}
}

func TestSetupUndoOnlyRemovesTheFirewallRule(t *testing.T) {
	h := &host{fwApply: "@@kind ufw\n@@changed\n"}
	got := rowText(runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), false, true))
	if !strings.Contains(got, "Removed the ufw rules marked virticle.") || strings.Contains(got, "Service") {
		t.Errorf("%s", got)
	}
	for _, s := range h.log {
		if strings.Contains(s, "systemctl") {
			t.Error("undo touched the service")
		}
	}
}

func TestSetupWithoutDatabaseUsesTheFirstStartDefault(t *testing.T) {
	ws, source, users, err := storedWebSettings(t.TempDir())()
	if err != nil || source != "default" || users || ws.Mode != webaccess.ModeSelfSigned || ws.HTTPSListen != ":8443" {
		t.Errorf("%+v %s %v %v", ws, source, users, err)
	}
}
