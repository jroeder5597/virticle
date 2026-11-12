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

// Features that run commands as root on the node (see internal/hostconn
// privilege.go). Everything else the app does goes through libvirt, which a
// member of the libvirt group may use, or reads world-readable system files.
// scoped marks the features the virticle helper covers (scoped sudo access).
var rootFeatures = []struct {
	key, reason string
	scoped      bool
}{
	{"installTLS", "Encrypted migration needs root or sudo on this node: the certificates go into /etc/pki/qemu", true},
	{"writePools", "Downloading or importing into a storage pool needs root or sudo on this node: the pool directories belong to root", true},
	{"backups", "Backups need root or sudo on this node: they read and write disk images in the pool directories", true},
	{"migration", "Moving a VM to or from this node needs root or sudo on it: disk images and snapshot files are copied as root", true},
	{"diskMove", "Moving a disk between pools needs root or sudo on this node", true},
	{"clone", "Cloning with guest preparation needs root or sudo on this node", true},
}

// nodeCapabilities evaluates every feature for a privilege setting. An
// undetected privilege (the node never connected) allows everything; the
// commands then fail with their own error if the guess was wrong.
func nodeCapabilities(priv string) map[string]capability {
	out := map[string]capability{}
	for _, f := range rootFeatures {
		c := capability{OK: true}
		switch {
		case hostconn.Privilege(priv) == hostconn.PrivNone:
			c = capability{OK: false, Reason: f.reason}
		case hostconn.Privilege(priv) == hostconn.PrivHelper && !f.scoped:
			c = capability{OK: false, Reason: "Needs broad sudo or root on this node; the scoped account (virticle helper) does not cover it"}
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
