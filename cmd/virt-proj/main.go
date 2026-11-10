// Command virt-proj is the controller: one binary that serves the web UI and
// API and manages libvirt nodes over SSH.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
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
	"virt-proj/web"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve the web UI and API on")
	data := flag.String("data", "data", "directory for the database and the controller's SSH key")
	flag.Parse()

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

	srv := &api.Server{Store: st, Nodes: mgr, PubKey: pub, Static: web.FS()}
	log.Printf("listening on http://%s (%d nodes)", *listen, len(nodes))
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.ListenAndServe())
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
