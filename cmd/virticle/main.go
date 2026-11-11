// Command virticle is the controller: one binary that serves the web UI and
// API and manages libvirt nodes over SSH.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"virticle/internal/api"
	"virticle/internal/cluster"
	"virticle/internal/store"
	"virticle/internal/tlscert"
	"virticle/web"
)

// renameEnv keeps the old name of an environment variable working: its value
// is used when the new name is not set.
func renameEnv(old, name string) {
	if v, ok := os.LookupEnv(old); ok && os.Getenv(name) == "" {
		os.Setenv(name, v)
		log.Printf("DEPRECATED: environment variable %s is now called %s; the old name still works for now", old, name)
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve the web UI and API on")
	data := flag.String("data", "data", "directory for the database and the controller's SSH key")
	tlsCert := flag.String("tls-cert", "", "PEM certificate to serve HTTPS with (needs -tls-key)")
	tlsKey := flag.String("tls-key", "", "PEM private key for -tls-cert")
	insecure := flag.Bool("insecure-http", false, "serve plain HTTP on a non-loopback address (passwords and sessions travel unencrypted)")
	trusted := flag.String("trusted-proxy", "", "comma-separated IPs/CIDRs of reverse proxies whose X-Forwarded-For/-Proto/-Host headers are believed (client address, https, public host name). "+
		"Empty: those headers are ignored. With this set, a loopback or private -listen address serves plain HTTP without -insecure-http, because TLS ends at the proxy")
	noScheduler := flag.Bool("no-scheduler", false, "do not run scheduled snapshots and backups from this controller (use on all but one when several controllers manage the same nodes)")
	var alerts api.AlertSettings
	flag.Float64Var(&alerts.PoolWarn, "alert-pool-warn", 85, "storage pool usage in percent that raises a warning alert")
	flag.Float64Var(&alerts.PoolCrit, "alert-pool-critical", 95, "storage pool usage in percent that raises a critical alert")
	flag.Float64Var(&alerts.Mem, "alert-memory", 90, "host memory use in percent that raises an alert")
	flag.IntVar(&alerts.CertDays, "alert-cert-days", 30, "alert when the web UI certificate expires within this many days")
	flag.StringVar(&alerts.Webhook, "alert-webhook", "", "http(s) URL that receives a JSON POST whenever an alert is raised or cleared (empty: no notifications)")
	flag.Parse()
	if versionRequested() {
		return
	}
	renameEnv("VIRT_JUMP_PASSWORD", "VIRTICLE_JUMP_PASSWORD")
	renameEnv("VIRT_VIRTIO_WIN_PATTERN", "VIRTICLE_VIRTIO_WIN_PATTERN")
	if alerts.Webhook != "" {
		if err := api.ValidateWebhookURL(alerts.Webhook); err != nil {
			log.Fatal(err)
		}
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be given together")
	}
	proxies, err := api.ParseTrustedProxies(*trusted)
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range proxies {
		if p.Bits() == 0 {
			log.Printf("WARNING: -trusted-proxy %s trusts every client to set its own address; list the proxies only", p)
		}
	}

	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	keyFile := filepath.Join(*data, "id_ed25519")
	pub, err := ensureKey(keyFile)
	if err != nil {
		log.Fatalf("SSH key: %v", err)
	}
	dbPath, err := store.DBPath(*data, log.Printf)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer st.Close()

	mgr := cluster.NewManager(keyFile, filepath.Join(*data, "known_hosts"))
	mgr.Persist = st
	nodes, err := st.Nodes()
	if err != nil {
		log.Fatal(err)
	}
	for _, n := range nodes {
		mgr.Add(n)
	}

	certFile, keyFile := *tlsCert, *tlsKey
	plainBehindProxy := false // plain HTTP on a private address because a trusted proxy ends TLS
	switch {
	case certFile != "":
	case tlscert.IsLoopback(*listen) || *insecure:
	case len(proxies) > 0 && tlscert.IsPrivate(*listen):
		plainBehindProxy = true
	default:
		var generated bool
		certFile, keyFile, generated, err = tlscert.Ensure(*data, tlscert.NamesFor(*listen), time.Now())
		if err != nil {
			log.Fatalf("TLS certificate: %v", err)
		}
		if generated {
			log.Printf("generated a self-signed certificate in %s", certFile)
		}
	}
	alerts.CertFile = certFile
	id, err := ensureInstanceID(filepath.Join(*data, "instance-id"))
	if err != nil {
		log.Fatalf("instance id: %v", err)
	}
	srv := &api.Server{Store: st, Nodes: mgr, PubKey: pub, Static: web.FS(), Alerts: alerts,
		TrustedProxies: proxies, NoScheduler: *noScheduler, InstanceID: id, DataDir: *data}
	if *noScheduler {
		log.Printf("scheduled snapshots and backups are disabled in this controller (-no-scheduler)")
	}
	hs := &http.Server{
		Addr:    *listen,
		Handler: srv.Handler(),
		// No Read/WriteTimeout: they would cut off console WebSockets, volume
		// uploads and long task requests. Slow header attacks are bounded here.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if certFile == "" {
		if plainBehindProxy {
			log.Printf("serving plain HTTP on %s for the trusted proxies %s; TLS must end at the proxy, and only the proxies may reach this address", *listen, *trusted)
		} else if !tlscert.IsLoopback(*listen) {
			log.Printf("WARNING: serving plain HTTP on %s (-insecure-http): passwords and sessions are not encrypted", *listen)
		}
		log.Printf("listening on http://%s (%d nodes)", *listen, len(nodes))
		log.Fatal(hs.ListenAndServe())
	}
	if fp, err := tlscert.Fingerprint(certFile); err == nil {
		log.Printf("TLS certificate %s SHA-256 fingerprint %s", certFile, fp)
	}
	log.Printf("listening on https://%s (%d nodes)", *listen, len(nodes))
	log.Fatal(hs.ListenAndServeTLS(certFile, keyFile))
}

// ensureInstanceID returns this controller's random id, creating it on first
// run. It marks the snapshots this controller schedules, so that a second
// controller on the same nodes can be recognised.
func ensureInstanceID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) >= 8 && !strings.ContainsAny(id, "[] \t") {
			return id, nil
		}
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	return id, os.WriteFile(path, []byte(id+"\n"), 0o600)
}

// ensureKey loads the controller's SSH key pair, creating it on first run,
// and returns the public key in authorized_keys form.
func ensureKey(path string) (string, error) {
	if b, err := os.ReadFile(path + ".pub"); err == nil {
		if _, err := os.Stat(path); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "virticle controller")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " virticle-controller"
	return line, os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
}
