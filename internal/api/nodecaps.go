package api

import (
	"net/http"

	"virticle/internal/hostconn"
)

// capability says whether a feature can work on a node, given how its login
// user gets root, and if not, why in words the UI can show.
type capability struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Features that run something as root on the node. They all go through the
// node command (internal/hostconn nodecmd.go), for the service account and for a
// root login alike, so a node either has all of them or none. Everything else
// the app does goes through libvirt, which a member of the libvirt group may
// use, or reads world-readable system files.
var rootFeatures = []struct {
	key, reason string
}{
	{"installTLS", "Encrypted migration needs the node command on this node: the certificates go into /etc/pki/qemu"},
	{"writePools", "Downloading or importing into a storage pool needs the node command on this node: the pool directories belong to root"},
	{"backups", "Backups need the node command on this node: they read and write disk images in the pool directories"},
	{"migration", "Moving a VM to or from this node needs the node command on it: disk images and snapshot files are copied as root"},
	{"diskMove", "Moving a disk between pools needs the node command on this node"},
	{"clone", "Cloning with guest preparation needs the node command on this node"},
}

// nodeCapabilities evaluates every feature for a privilege setting. An
// undetected privilege (the node never connected) allows everything; the
// commands then fail with their own error if the guess was wrong. The
// reason of a refused feature ends with how to fix it.
func nodeCapabilities(priv string) map[string]capability {
	out := map[string]capability{}
	for _, f := range rootFeatures {
		c := capability{OK: true}
		if hostconn.Privilege(priv) == hostconn.PrivNone {
			c = capability{OK: false, Reason: f.reason + `; run "virticle node prepare" for it`}
		}
		out[f.key] = c
	}
	return out
}

// needs refuses a request for a feature the node's login user cannot run, with
// the reason, before the handler starts any work. A node that is offline or
// unknown passes through: the handler reports that itself.
func (s *Server) needs(key string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, h, err := s.host(r); err == nil {
			if c := nodeCapabilities(string(h.Privilege()))[key]; !c.OK {
				conflict(w, c.Reason)
				return
			}
		}
		next(w, r)
	}
}
