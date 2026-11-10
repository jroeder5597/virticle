// Command virt-proj is the controller: one binary that serves the web UI and
// API and manages libvirt nodes over SSH.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/pem"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"virt-proj/internal/api"
	"virt-proj/internal/cluster"
	"virt-proj/internal/store"
	"virt-proj/internal/tlscert"
	"virt-proj/web"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve the web UI and API on")
	data := flag.String("data", "data", "directory for the database and the controller's SSH key")
	tlsCert := flag.String("tls-cert", "", "PEM certificate to serve HTTPS with (needs -tls-key)")
	tlsKey := flag.String("tls-key", "", "PEM private key for -tls-cert")
	insecure := flag.Bool("insecure-http", false, "serve plain HTTP on a non-loopback address (passwords and sessions travel unencrypted)")
	var alerts api.AlertSettings
	flag.Float64Var(&alerts.PoolWarn, "alert-pool-warn", 85, "storage pool usage in percent that raises a warning alert")
	flag.Float64Var(&alerts.PoolCrit, "alert-pool-critical", 95, "storage pool usage in percent that raises a critical alert")
	flag.Float64Var(&alerts.Mem, "alert-memory", 90, "host memory use in percent that raises an alert")
	flag.IntVar(&alerts.CertDays, "alert-cert-days", 30, "alert when the web UI certificate expires within this many days")
	flag.StringVar(&alerts.Webhook, "alert-webhook", "", "http(s) URL that receives a JSON POST whenever an alert is raised or cleared (empty: no notifications)")
	flag.Parse()
	if alerts.Webhook != "" {
		if err := api.ValidateWebhookURL(alerts.Webhook); err != nil {
			log.Fatal(err)
		}
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be given together")
	}

	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	keyFile := filepath.Join(*data, "id_ed25519")
	pub, err := ensureKey(keyFile)
	if err != nil {
		log.Fatalf("SSH key: %v", err)
	}
	st, err := store.Open(filepath.Join(*data, "virt-proj.db"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer st.Close()

	mgr := cluster.NewManager(keyFile, filepath.Join(*data, "known_hosts"))
	nodes, err := st.Nodes()
	if err != nil {
		log.Fatal(err)
	}
	for _, n := range nodes {
		mgr.Add(n)
	}

	certFile, keyFile := *tlsCert, *tlsKey
	switch {
	case certFile != "":
	case tlscert.IsLoopback(*listen) || *insecure:
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
	srv := &api.Server{Store: st, Nodes: mgr, PubKey: pub, Static: web.FS(), Alerts: alerts}
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
		if !tlscert.IsLoopback(*listen) {
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
	block, err := ssh.MarshalPrivateKey(priv, "virt-proj controller")
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
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " virt-proj-controller"
	return line, os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
}
