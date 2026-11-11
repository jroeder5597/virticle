package api

import "virt-proj/internal/hostconn"

// capability says whether a feature can work on a node, given how its login
// user gets root, and if not, why in words the UI can show.
type capability struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Features that run commands as root on the node (see internal/hostconn
// privilege.go). Everything else the app does goes through libvirt, which a
// member of the libvirt group may use, or reads world-readable system files.
var rootFeatures = []struct{ key, reason string }{
	{"installTLS", "Encrypted migration needs root or sudo on this node: the certificates go into /etc/pki/qemu"},
	{"writePools", "Downloading or importing into a storage pool needs root or sudo on this node: the pool directories belong to root"},
	{"backups", "Backups need root or sudo on this node: they read and write disk images in the pool directories"},
	{"migration", "Moving a VM to or from this node needs root or sudo on it: disk images and snapshot files are copied as root"},
	{"diskMove", "Moving a disk between pools needs root or sudo on this node"},
	{"clone", "Cloning with guest preparation needs root or sudo on this node"},
}

// nodeCapabilities evaluates every feature for a privilege setting. An
// undetected privilege (the node never connected) allows everything; the
// commands then fail with their own error if the guess was wrong.
func nodeCapabilities(priv string) map[string]capability {
	out := map[string]capability{}
	for _, f := range rootFeatures {
		c := capability{OK: true}
		if hostconn.Privilege(priv) == hostconn.PrivNone {
			c = capability{OK: false, Reason: f.reason}
		}
		out[f.key] = c
	}
	return out
}
