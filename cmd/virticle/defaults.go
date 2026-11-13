package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"virticle/internal/store"
	"virticle/internal/webaccess"
)

// systemDataDir is where the packages keep the controller's data.
const systemDataDir = "/var/lib/virticle"

// defaultDataDir is the -data value when none is given: the packages' data
// directory if it exists and holds a controller database (so that "virticle
// web show" on an installed machine reads the real settings), else "data".
func defaultDataDir() string { return pickDataDir(systemDataDir) }

func pickDataDir(system string) string {
	if st, err := os.Stat(system); err == nil && st.IsDir() && store.HasDatabase(system) {
		return system
	}
	return "data"
}

// choice is what the controller serves when no listener flag is given.
type choice struct {
	settings webaccess.Settings
	source   string // stored or default
	notice   string // shown on the Web access page
	persist  bool   // store the settings so that they stay once an administrator exists
}

const upgradeNotice = "This controller still serves plain HTTP on 127.0.0.1:8080, as before the update. Choose a mode below to reach it from other computers."

// chooseSettings is the one rule for the listener when no flag is given:
//   - stored settings are kept;
//   - no stored settings and an administrator exists (an install from before
//     this default): keep the old loopback HTTP listener and say so;
//   - no stored settings and no users (a new install): HTTPS with a
//     self-signed certificate on all interfaces, port 8443. It is stored, so a
//     restart after the administrator is created keeps it.
func chooseSettings(stored webaccess.Settings, hasStored, hasUsers bool) choice {
	switch {
	case hasStored:
		return choice{settings: stored, source: "stored"}
	case hasUsers:
		return choice{settings: webaccess.Defaults(), source: "default", notice: upgradeNotice}
	}
	return choice{settings: webaccess.FirstRunDefaults(), source: "default", persist: true}
}

// openURL is the address to open for ws: scheme://host:port, with the first
// of addrs as the host for a listener on every interface. Empty when the
// listen address cannot be read.
func openURL(ws webaccess.Settings, addrs []string) string {
	scheme, listen := "http", ws.Listen
	if ws.IsTLS() {
		scheme, listen = "https", ws.HTTPSListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if webaccess.IsWildcard(listen) {
		host = "this-computer"
		if len(addrs) > 0 {
			host = addrs[0]
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%s", scheme, host, port)
}

// setupHint is the journal line that says where to create the administrator;
// empty when one exists. addrs are the addresses to offer for a wildcard
// listener, best first.
func setupHint(hasUsers bool, ws webaccess.Settings, addrs []string) string {
	if hasUsers {
		return ""
	}
	u := openURL(ws, addrs)
	if u == "" {
		return ""
	}
	return "open " + u + " to create the administrator"
}

// suggestedAddresses are this machine's addresses and host name, IP addresses
// first (they work from another computer without DNS).
func suggestedAddresses() []string {
	names := webaccess.SuggestedNames()
	sort.SliceStable(names, func(i, j int) bool {
		return net.ParseIP(names[i]) != nil && net.ParseIP(names[j]) == nil
	})
	return names
}

// Deprecated flags and environment variables keep working but are hidden from
// the help and announced once at start.
var deprecatedFlags = map[string]string{
	"tls-cert":            "use the Web access page, or: virticle web set -mode own -cert FILE -key FILE",
	"tls-key":             "use the Web access page, or: virticle web set -mode own -cert FILE -key FILE",
	"insecure-http":       "use the Web access page, or: virticle web set -mode off -listen ADDRESS",
	"trusted-proxy":       "use the Web access page, or: virticle web set -mode proxy -trusted-proxy ADDRESSES",
	"alert-pool-warn":     "it will be removed; the default stays",
	"alert-pool-critical": "it will be removed; the default stays",
	"alert-memory":        "it will be removed; the default stays",
	"alert-cert-days":     "it will be removed; the default stays",
	"alert-webhook":       "use the Notifications page; the address stays as one more, read-only destination there",
}

var deprecatedEnvs = map[string]string{
	"VIRTICLE_TLS_CERT":           "use the Web access page, or: virticle web set",
	"VIRTICLE_TLS_KEY":            "use the Web access page, or: virticle web set",
	"VIRTICLE_INSECURE_HTTP":      "use the Web access page, or: virticle web set",
	"VIRTICLE_TRUSTED_PROXY":      "use the Web access page, or: virticle web set",
	"VIRTICLE_VIRTIO_WIN_PATTERN": "it will be removed; the default stays",
}

// warnDeprecated logs one line for each deprecated flag given and each
// deprecated environment variable set.
func warnDeprecated(logf func(string, ...any)) {
	var names []string
	flag.Visit(func(f *flag.Flag) {
		if _, ok := deprecatedFlags[f.Name]; ok {
			names = append(names, f.Name)
		}
	})
	sort.Strings(names)
	for _, n := range names {
		logf("DEPRECATED: -%s is deprecated: %s", n, deprecatedFlags[n])
	}
	var envs []string
	for e := range deprecatedEnvs {
		if os.Getenv(e) != "" {
			envs = append(envs, e)
		}
	}
	sort.Strings(envs)
	for _, e := range envs {
		logf("DEPRECATED: %s is deprecated: %s", e, deprecatedEnvs[e])
	}
}

// usage prints the help without the deprecated flags.
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `usage: virticle [flags]                  run the controller
       virticle version
       virticle setup [--check] [--undo] [-data DIR]
                                         after installing: open the web port in the firewall (as root)
       virticle node prepare|unprepare [options] [user@]host
                                         prepare a host as a node, or undo it (virticle node -h)
       virticle backup [-o FILE|DIR] [--encrypt] [--passphrase-file FILE]
       virticle restore [--passphrase-file FILE] FILE
                                         back up or restore the controller's own data
       virticle web show|set|reset [-data DIR] ...
                                         read or change the stored web access settings
       virticle user reset-2fa|reset-password USER [-data DIR]
                                         recover a sign-in from the controller's machine

Run a command with -h for its options.

flags:
`)
	flag.VisitAll(func(f *flag.Flag) {
		if _, hidden := deprecatedFlags[f.Name]; hidden {
			return
		}
		name, use := flag.UnquoteUsage(f)
		line := "  -" + f.Name
		if name != "" {
			line += " " + name
		}
		fmt.Fprintf(out, "%s\n    \t%s", line, strings.ReplaceAll(use, "\n", "\n    \t"))
		if f.DefValue != "" && f.DefValue != "false" && f.Name != "listen" && f.Name != "data" {
			fmt.Fprintf(out, " (default %q)", f.DefValue)
		}
		fmt.Fprintln(out)
	})
}
