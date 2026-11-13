package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"virticle/internal/api"
	"virticle/internal/store"
	"virticle/internal/tlscert"
	"virticle/internal/webaccess"
)

// envDefault is the value of an environment variable, else def. It lets the
// listener flags be set from the environment (flags win over it).
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type flagListen struct {
	listen, cert, key string
	insecure          bool
	trusted           string
	proxies           []netip.Prefix
}

// flagsGiven reports whether any listener flag or its environment variable is
// set; then the command line decides and the stored settings are not used.
func flagsGiven() bool {
	given := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen", "tls-cert", "tls-key", "insecure-http", "trusted-proxy":
			given = true
		}
	})
	for _, e := range []string{"VIRTICLE_LISTEN", "VIRTICLE_TLS_CERT", "VIRTICLE_TLS_KEY", "VIRTICLE_INSECURE_HTTP", "VIRTICLE_TRUSTED_PROXY"} {
		given = given || os.Getenv(e) != ""
	}
	return given
}

// settingsFromFlags is what the listener flags have always meant: a
// certificate means HTTPS with it, a loopback or -insecure-http address means
// plain HTTP, trusted proxies on a private address mean plain HTTP behind a
// proxy, and anything else gets a self-signed certificate.
func settingsFromFlags(f flagListen) webaccess.Settings {
	s := webaccess.Defaults()
	switch {
	case f.cert != "":
		s.Mode, s.HTTPSListen, s.Own = webaccess.ModeOwn, f.listen, webaccess.Own{CertFile: f.cert, KeyFile: f.key}
	case tlscert.IsLoopback(f.listen) || f.insecure:
		s.Mode, s.Listen = webaccess.ModeOff, f.listen
	case len(f.proxies) > 0 && tlscert.IsPrivate(f.listen):
		s.Mode, s.Listen = webaccess.ModeProxy, f.listen
	default:
		s.Mode, s.HTTPSListen = webaccess.ModeSelfSigned, f.listen
	}
	for _, p := range f.proxies {
		s.TrustedProxies = append(s.TrustedProxies, p.String())
	}
	return s
}

// startWeb opens the listeners and then serves until the process is killed.
// SIGHUP re-reads certificate files.
func startWeb(srv *api.Server, st *store.Store, data string, f flagListen, nodes int) {
	m := &webaccess.Manager{Store: st, DataDir: data, Logf: log.Printf, Source: "default", OnChange: srv.UseWebSettings}
	if v, err := strconv.Atoi(os.Getenv("VIRTICLE_WEB_CONFIRM_SECONDS")); err == nil && v > 0 {
		m.Window = time.Duration(v) * time.Second
	}
	srv.Web = m
	m.Handler = srv.Handler()

	users, err := st.UserCount()
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	var ws webaccess.Settings
	persist := false
	if flagsGiven() {
		ws, m.Source = settingsFromFlags(f), "flags"
		m.Locked = "The listener is set by command-line flags or VIRTICLE_* environment variables (-listen, -tls-cert, -tls-key, -insecure-http, -trusted-proxy). Remove them to manage it here."
	} else {
		stored, ok, err := m.Load()
		if err != nil {
			log.Fatalf("web access settings: %v", err)
		}
		c := chooseSettings(stored, ok, users > 0, legacyListen())
		ws, m.Source, m.Notice, persist = c.settings, c.source, c.notice, c.persist
	}
	used, warning, err := m.StartOrFallBack(ws)
	if warning != "" {
		log.Printf("WARNING: %s", warning)
		m.Notice = warning
	}
	ws = used
	if err != nil {
		if m.Source == "stored" || persist {
			log.Printf("ERROR: the web access settings cannot be used (%v); serving plain HTTP on %s instead. Fix them in Settings, or run: virticle web reset", err, webaccess.DefaultListen)
			persist = false
			ws = webaccess.Defaults()
			err = m.Start(ws)
		}
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
	}
	if persist {
		b, _ := json.Marshal(ws)
		if err := st.SetWebSetting("web", string(b)); err != nil {
			log.Printf("WARNING: could not store the web access settings: %v", err)
		}
	}
	for _, l := range m.Status("").Listeners {
		scheme := "http"
		if l.Kind == "https" {
			scheme = "https"
		}
		switch l.Kind {
		case "redirect":
			log.Printf("redirecting http://%s to HTTPS", l.Addr)
		default:
			log.Printf("listening on %s://%s (%d nodes, web access: %s, from %s)", scheme, l.Addr, nodes, ws.Mode, m.Source)
		}
	}
	if cf := m.CertFile(); cf != "" {
		if fp, err := tlscert.Fingerprint(cf); err == nil {
			log.Printf("TLS certificate %s SHA-256 fingerprint %s", cf, fp)
		}
	}
	if hint := setupHint(users > 0, ws, suggestedAddresses()); hint != "" {
		log.Printf("%s", hint)
	}
	if ws.Mode == webaccess.ModeOff && !tlscert.IsLoopback(ws.Listen) {
		log.Printf("WARNING: serving plain HTTP on %s: passwords and sessions are not encrypted", ws.Listen)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	for range hup {
		m.Reload()
	}
}

const webUsage = `usage: virticle web <command> [-data DIR] [options]

  show    print the stored web access settings as JSON (takes only -data)
  reset   put the web UI back on plain HTTP at 127.0.0.1:8080 (for a locked-out admin)
  set     change stored settings: -mode off|selfsigned|own|acme|proxy, -listen, -https-listen,
          -http-redirect, -public-url, -names a,b, -cert FILE -key FILE, -acme-domains a,b,
          -acme-email, -acme-directory, -acme-accept-terms, -trusted-proxy a,b, -hsts

These edit the database; restart the controller (it re-reads them at start).
-data defaults to /var/lib/virticle on an installed machine, else ./data.
A -listen flag (or the older -tls-cert, -tls-key, -insecure-http, -trusted-proxy flags)
overrides stored settings; "reset" does not remove them from the service file.
`

// webCommand implements "virticle web ...".
func webCommand(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(webUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("web "+cmd, flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "controller data directory")
	// Only "set" has the settings flags: show and reset take just -data.
	str := func(name, def, usage string) *string {
		if cmd == "set" {
			return fs.String(name, def, usage)
		}
		return new(string)
	}
	boolean := func(name string, usage string) *bool {
		if cmd == "set" {
			return fs.Bool(name, false, usage)
		}
		return new(bool)
	}
	mode := str("mode", "", "off, selfsigned, own, acme or proxy")
	listen := str("listen", "", "plain HTTP address (modes off and proxy)")
	httpsListen := str("https-listen", "", "HTTPS address (modes selfsigned, own, acme)")
	redirect := str("http-redirect", "", "address of an HTTP listener that only redirects to HTTPS")
	public := str("public-url", "", "address users type, e.g. https://virticle.example.org")
	names := str("names", "", "comma-separated names for a self-signed certificate")
	cert := str("cert", "", "certificate chain file (mode own)")
	key := str("key", "", "private key file (mode own)")
	domains := str("acme-domains", "", "comma-separated domain names (mode acme)")
	email := str("acme-email", "", "ACME contact e-mail")
	dir := str("acme-directory", "", "ACME directory URL (default Let's Encrypt)")
	terms := boolean("acme-accept-terms", "accept the ACME provider's terms of service")
	trusted := str("trusted-proxy", "", "comma-separated proxy IPs/CIDRs (mode proxy)")
	hsts := boolean("hsts", "send Strict-Transport-Security")
	fs.Parse(args[1:])
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if err := os.MkdirAll(*data, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dbPath, err := store.DBPath(*data, log.Printf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		return 1
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		return 1
	}
	defer st.Close()
	m := &webaccess.Manager{Store: st, DataDir: filepath.Clean(*data)}
	cur, ok, err := m.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !ok {
		cur = webaccess.Defaults()
	}
	save := func(s webaccess.Settings) int {
		b, _ := json.Marshal(s)
		if err := st.SetWebSetting("web", string(b)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	switch cmd {
	case "show":
		b, _ := json.MarshalIndent(cur, "", "  ")
		fmt.Println(string(b))
		return 0
	case "reset":
		d := webaccess.Defaults()
		if rc := save(d); rc != 0 {
			return rc
		}
		st.DeleteWebSetting("web.pending")
		fmt.Printf("Web access reset: plain HTTP on %s. Restart the controller to apply it.\n", d.Listen)
		return 0
	case "set":
		s := cur
		list := func(v string) []string {
			return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
		}
		if given["mode"] {
			s.Mode = *mode
		}
		if given["listen"] {
			s.Listen = *listen
		}
		if given["https-listen"] {
			s.HTTPSListen = *httpsListen
		}
		if given["http-redirect"] {
			s.HTTPRedirect = *redirect
		}
		if given["public-url"] {
			s.PublicURL = *public
		}
		if given["names"] {
			s.Names = list(*names)
		}
		if given["cert"] || given["key"] {
			s.Own = webaccess.Own{CertFile: *cert, KeyFile: *key}
		}
		if given["acme-domains"] {
			s.ACME.Domains = list(*domains)
		}
		if given["acme-email"] {
			s.ACME.Email = *email
		}
		if given["acme-directory"] {
			s.ACME.Directory = *dir
		}
		if given["acme-accept-terms"] {
			s.ACME.AcceptTerms = *terms
		}
		if given["trusted-proxy"] {
			s.TrustedProxies = list(*trusted)
		}
		if given["hsts"] {
			s.HSTS = *hsts
		}
		s = s.Normalize()
		checks := webaccess.Validate(s, webaccess.CheckEnv{Bind: func(string) error { return nil }})
		for _, c := range checks {
			if c.Status != "ok" {
				fmt.Printf("%-8s %s: %s\n", c.Status, c.Title, c.Detail)
			}
		}
		if webaccess.HasError(checks) {
			fmt.Fprintln(os.Stderr, "not saved")
			return 1
		}
		if rc := save(s); rc != 0 {
			return rc
		}
		fmt.Println("Saved. Restart the controller to apply it.")
		return 0
	}
	fmt.Print(webUsage)
	return 2
}
