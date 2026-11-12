package main

// "virticle setup": run once on the controller's own machine after the
// package is installed. It opens the web port in the machine's firewall (when
// one is active), makes sure the service runs, and says which address to open.
// "virticle setup --undo" takes the firewall rule away again. Packages never
// change the firewall; this explicit step does.

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"virticle/internal/prepare"
	"virticle/internal/store"
	"virticle/internal/tlscert"
	"virticle/internal/webaccess"
)

const setupUsage = `usage: sudo virticle setup [--check] [--undo] [-data DIR]

Run once on the machine where the controller is installed, as root. Setup
  - opens the web port in the firewall when firewalld or ufw is active (a service or rule
    named virticle; when the port was changed since the last run the old port is closed again),
  - makes sure the virticle service is enabled and running,
  - prints the address to open.
--check changes nothing. --undo removes the firewall rule that setup added and nothing else.
Running setup again changes nothing. -data defaults to /var/lib/virticle on an installed machine.`

// webFirewallService names the firewalld service and the ufw rule comment of
// the controller's web port.
const webFirewallService = "virticle"

const unitName = "virticle.service"

// setupEnv is what setup needs from the machine; tests replace it.
type setupEnv struct {
	euid     int
	sh       prepare.RunFunc // runs a shell script as root on this machine
	settings func() (ws webaccess.Settings, source string, hasUsers bool, err error)
	addrs    []string
	hasKVM   bool
}

// webPorts are the TCP ports the controller listens on for other computers:
// the HTTPS or HTTP listener, and the redirect listener when there is one.
// Listeners on a loopback address need no firewall port.
func webPorts(ws webaccess.Settings) (ports []string) {
	addrs := []string{ws.Listen}
	if ws.IsTLS() {
		addrs = []string{ws.HTTPSListen, ws.HTTPRedirect}
	}
	for _, a := range addrs {
		if a == "" {
			continue
		}
		if tlscert.IsLoopback(a) {
			continue
		}
		if _, port, err := webaccess.SplitAddr(a); err == nil && port > 0 {
			p := fmt.Sprint(port)
			dup := false
			for _, q := range ports {
				dup = dup || p == q
			}
			if !dup {
				ports = append(ports, p)
			}
		}
	}
	return ports
}

func webFirewall(ports []string) prepare.FirewallSpec {
	return prepare.FirewallSpec{
		Service:     webFirewallService,
		Description: "Virticle: web interface",
		Ports:       ports,
		Actor:       "Setup",
		Purpose:     "for the web interface",
	}
}

const unitCheckScript = `
if ! systemctl cat ` + unitName + ` >/dev/null 2>&1; then echo unit=no; exit 0; fi
echo unit=yes
echo "active=$(systemctl is-active ` + unitName + ` 2>/dev/null)"
systemctl is-enabled --quiet ` + unitName + ` 2>/dev/null && echo enabled=yes || echo enabled=no
true`

const unitStartScript = `out=$(systemctl enable --now ` + unitName + ` 2>&1) || { echo "@@detail $(echo "$out" | head -n 1)"; echo @@failed; }
true`

// serviceRow reports (and unless check is set, fixes) the state of the
// service. It returns no row on a machine without the unit.
func serviceRow(e setupEnv, check bool) []prepare.Row {
	out, err := e.sh(unitCheckScript)
	if err != nil || !strings.Contains(out, "unit=yes") {
		return []prepare.Row{{ID: "service", Title: "Service", Status: prepare.Info,
			Detail: "There is no systemd service " + unitName + " on this machine. Start virticle yourself."}}
	}
	running := strings.Contains(out, "active=active")
	enabled := strings.Contains(out, "enabled=yes")
	row := prepare.Row{ID: "service", Title: "Service"}
	switch {
	case running && enabled:
		row.Status, row.Detail = prepare.OK, unitName+" is already running and enabled."
	case check:
		row.Status, row.Detail = prepare.Info, "Setup will enable and start "+unitName+"."
	default:
		sout, serr := e.sh(unitStartScript)
		if serr != nil || strings.Contains(sout, "@@failed") {
			detail := ""
			for _, l := range strings.Split(sout, "\n") {
				detail = strings.TrimPrefix(l, "@@detail ")
			}
			row.Status = prepare.Warning
			row.Detail = "Could not start " + unitName + " (" + strings.TrimSpace(detail+" "+errText(serr)) + "). Run: systemctl enable --now " + unitName + " and look at: journalctl -u virticle"
		} else {
			row.Status, row.Detail = prepare.OK, "Enabled and started "+unitName+"."
		}
	}
	return []prepare.Row{row}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runSetup does setup (or only looks, with check; or takes the firewall rule
// away, with undo) and returns the rows to print.
func runSetup(e setupEnv, check, undo bool) []prepare.Row {
	if e.euid != 0 {
		return []prepare.Row{{ID: "root", Title: "Administrator", Status: prepare.Error,
			Detail: "setup changes the firewall and the service, so it needs root. Run: sudo virticle setup"}}
	}
	if undo {
		var row prepare.Row
		if check {
			f := prepare.ParseFirewallFacts(mustSh(e, prepare.FirewallCheckScript(webFirewall(nil))))
			row = prepare.Row{ID: "firewall", Title: "Firewall", Status: prepare.Info, Detail: "Setup --undo would remove the rule named " + webFirewallService + " from " + f.Kind + "."}
			if f.Kind != "firewalld" && f.Kind != "ufw" {
				row.Status, row.Detail = prepare.OK, "No firewall is active on this host. Nothing to remove."
			}
		} else {
			row, _ = prepare.ApplyFirewall(e.sh, webFirewall(nil))
		}
		return []prepare.Row{row, {ID: "summary", Title: "Result", Status: prepare.Info, Detail: "Only the firewall rule was removed. The service and its data are unchanged."}}
	}

	ws, source, hasUsers, err := e.settings()
	if err != nil {
		return []prepare.Row{{ID: "web", Title: "Web access", Status: prepare.Error, Detail: err.Error()}}
	}
	var rows []prepare.Row
	ports := webPorts(ws)
	kind := map[bool]string{true: "stored settings", false: "the default for a first start"}[source == "stored"]
	desc := "HTTP"
	if ws.IsTLS() {
		desc = "HTTPS"
	}
	if len(ports) > 0 {
		rows = append(rows, prepare.Row{ID: "web", Title: "Web access", Status: prepare.Info,
			Detail: fmt.Sprintf("%s on %s (%s).", desc, prepare.PortsText(ports), kind)})
	} else {
		rows = append(rows, prepare.Row{ID: "web", Title: "Web access", Status: prepare.Info,
			Detail: fmt.Sprintf("%s on this computer only (%s), so no firewall port is needed (%s).", desc, listenOf(ws), kind)})
	}

	// Firewall. With no port to open this still removes a rule that an
	// earlier run added for a port that is gone.
	spec := webFirewall(ports)
	switch {
	case check && len(ports) > 0:
		f := prepare.ParseFirewallFacts(mustSh(e, prepare.FirewallCheckScript(spec)))
		rows = append(rows, prepare.FirewallCheckRow(f, spec))
	case check:
		// nothing to open; nothing to look at
	default:
		row, changed := prepare.ApplyFirewall(e.sh, spec)
		if len(ports) > 0 || changed || row.Status == prepare.Warning {
			rows = append(rows, row)
		}
	}

	rows = append(rows, serviceRow(e, check)...)

	if u := openURL(ws, e.addrs); u != "" {
		line := "open " + u + " to sign in"
		if !hasUsers {
			line = "open " + u + " to create the administrator"
		}
		rows = append(rows, prepare.Row{ID: "next", Title: "Next", Status: prepare.Info, Detail: line})
	}
	if e.hasKVM {
		rows = append(rows, prepare.Row{ID: "vms", Title: "Virtual machines", Status: prepare.Info,
			Detail: "To run virtual machines on this machine too, choose 'Set up this machine as a node' on the start page."})
	}
	return rows
}

func mustSh(e setupEnv, script string) string {
	out, _ := e.sh(script)
	return out
}

func listenOf(ws webaccess.Settings) string {
	if ws.IsTLS() {
		return ws.HTTPSListen
	}
	return ws.Listen
}

// localSh runs a script with sh on this machine.
func localSh(script string) (string, error) {
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return string(out), err
}

// storedWebSettings reads the settings the controller would use at its next
// start, the way the controller chooses them. A machine without a database
// yet gets what the controller picks on its first start.
func storedWebSettings(data string) func() (webaccess.Settings, string, bool, error) {
	return func() (webaccess.Settings, string, bool, error) {
		if !store.HasDatabase(data) {
			c := chooseSettings(webaccess.Settings{}, false, false)
			return c.settings, c.source, false, nil
		}
		dbPath, err := store.DBPath(data, log.Printf)
		if err != nil {
			return webaccess.Settings{}, "", false, fmt.Errorf("database: %v", err)
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return webaccess.Settings{}, "", false, fmt.Errorf("database: %v", err)
		}
		defer st.Close()
		m := &webaccess.Manager{Store: st, DataDir: data}
		stored, ok, err := m.Load()
		if err != nil {
			return webaccess.Settings{}, "", false, err
		}
		users, err := st.UserCount()
		if err != nil {
			return webaccess.Settings{}, "", false, fmt.Errorf("database: %v", err)
		}
		c := chooseSettings(stored, ok, users > 0)
		return c.settings, c.source, users > 0, nil
	}
}

func setupCommand(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, setupUsage) }
	check := fs.Bool("check", false, "look only; change nothing")
	undo := fs.Bool("undo", false, "remove the firewall rule that setup added")
	data := fs.String("data", defaultDataDir(), "the controller's data directory")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, setupUsage)
		return 2
	}
	_, kvmErr := os.Stat("/dev/kvm")
	e := setupEnv{euid: os.Geteuid(), sh: localSh, settings: storedWebSettings(*data), addrs: suggestedAddresses(), hasKVM: kvmErr == nil}
	rows := runSetup(e, *check, *undo)
	for _, r := range rows {
		printRow(r)
	}
	return exitCode(rows)
}
