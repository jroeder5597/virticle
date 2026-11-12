package hostconn

// How a node's login user reaches the node command.
//
// Everything privileged the controller does on a node goes through one root
// program, the node command (helper.go). A service account runs it with
// "sudo -n", a root login runs it directly. Feature code never writes "sudo"
// and never sends a free-form command as root: it calls the typed operations
// in nodeops.go and the like, or RunNode with a verb.

import (
	"errors"
	"strings"
)

// Privilege says how a node's login user obtains root.
type Privilege string

const (
	PrivRoot Privilege = "root" // the login user is root and runs the node command directly
	// PrivService is the service account: it may run the node command through
	// "sudo -n" and nothing else. The stored value is still "helper", the name
	// nodes were saved under before the access modes were renamed.
	PrivService Privilege = "helper"
	PrivNone    Privilege = "none" // no usable node command: privileged operations are refused
)

// Valid reports whether p is one of the settings.
func (p Privilege) Valid() bool {
	return p == PrivRoot || p == PrivService || p == PrivNone
}

// ParsePrivilege reads a stored or submitted privilege value. An empty,
// unknown or retired value (including "sudo") gives "", meaning "detect it".
func ParsePrivilege(s string) Privilege {
	if p := Privilege(s); p.Valid() {
		return p
	}
	return ""
}

// NoNodeCommandMessage is what a node without the node command is told.
const NoNodeCommandMessage = `this node has no node command; run "virticle node prepare" for it`

// ErrNoPrivilege is returned for an operation that needs the node command on a
// node that has none.
var ErrNoPrivilege = errors.New(NoNodeCommandMessage)

// Quote returns s as one shell word in single quotes.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// SetPrivilege records how this connection obtains root; the cluster sets it
// from the node's configuration.
func (h *Host) SetPrivilege(p Privilege) { h.priv = p }

// Privilege is the setting last given to SetPrivilege.
func (h *Host) Privilege() Privilege { return h.priv }

// CanRoot reports whether the node command can be used on this host.
func (h *Host) CanRoot() bool { return h.priv != PrivNone }

// DetectScript prints what ParseDetect reads: the uid, the version the node
// command reports (through sudo for a service account, directly for root), and
// whether the old helper script of an earlier version is still there. It
// always succeeds.
const DetectScript = `echo "uid=$(id -u)"; if [ -x ` + HelperPath + ` ]; then if [ "$(id -u)" = 0 ]; then echo "helper=$(` + HelperPath + ` version 2>/dev/null | head -n 1)"; else echo "helper=$(sudo -n ` + HelperPath + ` version 2>/dev/null | head -n 1)"; fi; fi; if [ -e ` + OldHelperPath + ` ]; then echo oldhelper=yes; fi; echo "user=$(id -un)"`

// DetectPrivilege runs DetectScript and decides: a node command of this
// version that answers is root (uid 0) or the service account, anything else
// is none. The detail is a one-line explanation.
func (h *Host) DetectPrivilege() (Privilege, string, error) {
	out, err := h.Run(DetectScript)
	if err != nil {
		return "", "", err
	}
	p, detail := ParseDetect(out)
	return p, detail, nil
}

// ParseDetect interprets DetectScript's output.
func ParseDetect(out string) (Privilege, string) {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			kv[k] = v
		}
	}
	user := kv["user"]
	isRoot := kv["uid"] == "0"
	hv, hasNode := kv["helper"]
	old := kv["oldhelper"] == "yes" && !hasNode
	switch {
	case hasNode && hv == HelperVersion:
		if isRoot {
			return PrivRoot, "logged in as root; the virticle node command (version " + hv + ") is installed"
		}
		return PrivService, user + " may run the virticle node command (version " + hv + ") as root, and nothing else"
	case hasNode && hv != "":
		return PrivNone, user + ": the installed node command is version " + hv + " and this controller ships " + HelperVersion + ": run \"virticle node prepare\" to update it"
	case hasNode:
		if isRoot {
			return PrivNone, "the node command in " + HelperPath + " does not answer"
		}
		return PrivNone, user + " cannot run the node command in " + HelperPath + " through sudo (check " + SudoersPath + ")"
	case old:
		return PrivNone, "this node was " + OlderVersionMessage
	}
	return PrivNone, NoNodeCommandMessage
}
