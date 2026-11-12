package hostconn

// Privileged commands for a login that is not root.
//
// Everything the controller runs on a node over SSH runs as the login user.
// A few things need root (certificate install, writing into pool directories,
// reading disk images). They go through RunRoot, RunInputRoot, StreamRoot and
// PipeRoot, and nowhere else is the elevation done: feature code never writes
// "sudo".

import (
	"context"
	"errors"
	"io"
	"strings"
)

// Privilege says how a node's login user obtains root.
type Privilege string

const (
	PrivRoot   Privilege = "root"   // the login user is root
	PrivHelper Privilege = "helper" // sudo right for the virticle helper only (helper.go): scoped, fixed subcommands
	PrivSudo   Privilege = "sudo"   // broad passwordless sudo: privileged commands run as "sudo -n sh -c ..."
	PrivNone   Privilege = "none"   // unprivileged: privileged commands are refused
)

// Valid reports whether p is one of the settings.
func (p Privilege) Valid() bool {
	return p == PrivRoot || p == PrivHelper || p == PrivSudo || p == PrivNone
}

// ErrNoPrivilege is returned for a command that needs root on a node whose
// login user has none.
var ErrNoPrivilege = errors.New("this needs root or passwordless sudo on the node, and the login user has neither")

// Quote returns s as one shell word in single quotes.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// WrapRoot returns the command line that runs cmd as root for a login with
// privilege p. For sudo the whole command, pipes and redirections included,
// is handed to one "sh -c", so it behaves exactly as it would for root.
func WrapRoot(p Privilege, cmd string) (string, error) {
	switch p {
	case PrivRoot, "":
		// An empty setting is a node whose privilege was not detected yet:
		// behave as before this setting existed.
		return cmd, nil
	case PrivSudo:
		return "sudo -n sh -c " + Quote(cmd), nil
	case PrivHelper:
		// Free-form commands are exactly what the scoped access does not allow.
		return "", ErrScoped{}
	}
	return "", ErrNoPrivilege
}

// SetPrivilege records how this connection obtains root; the cluster sets it
// from the node's configuration.
func (h *Host) SetPrivilege(p Privilege) { h.priv = p }

// Privilege is the setting last given to SetPrivilege.
func (h *Host) Privilege() Privilege { return h.priv }

// CanRoot reports whether privileged commands can run on this host.
func (h *Host) CanRoot() bool { return h.priv != PrivNone }

// RunRoot is Run as root.
func (h *Host) RunRoot(cmd string) (string, error) {
	w, err := WrapRoot(h.priv, cmd)
	if err != nil {
		// Callers show the output in their messages; for a refused command
		// that is the reason.
		return err.Error(), err
	}
	return h.Run(w)
}

// RunInputRoot is RunInput as root; stdin reaches the command unchanged.
func (h *Host) RunInputRoot(cmd, stdin string) (string, error) {
	w, err := WrapRoot(h.priv, cmd)
	if err != nil {
		return err.Error(), err
	}
	return h.RunInput(w, stdin)
}

// StreamRoot is Stream as root.
func (h *Host) StreamRoot(ctx context.Context, cmd string, line func(string)) error {
	w, err := WrapRoot(h.priv, cmd)
	if err != nil {
		return err
	}
	return h.Stream(ctx, w, line)
}

// PipeRoot is Pipe as root.
func (h *Host) PipeRoot(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	w, err := WrapRoot(h.priv, cmd)
	if err != nil {
		return err
	}
	return h.Pipe(ctx, w, stdin, stdout)
}

// RootCmd is the command line that runs cmd as root on this host, for a
// command that is itself sent to another machine (see the migration's file
// copy).
func (h *Host) RootCmd(cmd string) (string, error) { return WrapRoot(h.priv, cmd) }

// DetectScript prints what ParseDetect reads: the uid, whether the node
// command answers through sudo (and its version), whether the old helper
// script of an earlier version is still there, and whether broad sudo works
// without a password. It always succeeds.
const DetectScript = `echo "uid=$(id -u)"; if [ -x ` + HelperPath + ` ]; then echo "helper=$(sudo -n ` + HelperPath + ` version 2>/dev/null | head -n 1)"; fi; if [ -e ` + OldHelperPath + ` ]; then echo oldhelper=yes; fi; if sudo -n true >/dev/null 2>&1; then echo sudo=yes; else echo sudo=no; fi; echo "user=$(id -un)"`

// DetectPrivilege runs DetectScript and decides: uid 0 is root, else a helper
// of this version that answers through sudo is helper, else working
// passwordless sudo is sudo, else none. The detail is a one-line explanation.
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
	hv, hasHelper := kv["helper"]
	old := kv["oldhelper"] == "yes" && !hasHelper
	switch {
	case kv["uid"] == "0":
		return PrivRoot, "logged in as root"
	case hasHelper && hv == HelperVersion:
		if kv["sudo"] == "yes" {
			return PrivHelper, user + " may run the virticle node command (version " + hv + ") as root; broad sudo works too, but the node command is used"
		}
		return PrivHelper, user + " may run the virticle node command (version " + hv + ") as root, and nothing else"
	case kv["sudo"] == "yes":
		if hasHelper && hv != "" {
			return PrivSudo, user + " is not root but \"sudo -n\" works without a password (the installed node command is version " + hv + ", this controller ships " + HelperVersion + ")"
		}
		if old {
			return PrivSudo, user + " is not root but \"sudo -n\" works without a password (this node was " + OlderVersionMessage + ")"
		}
		return PrivSudo, user + " is not root but \"sudo -n\" works without a password"
	}
	if hasHelper {
		if hv == "" {
			return PrivNone, user + " is not root and the node command in " + HelperPath + " does not answer through sudo (check " + SudoersPath + ")"
		}
		return PrivNone, user + " is not root; the installed node command is version " + hv + " and this controller ships " + HelperVersion + ": run Prepare node to update it"
	}
	if old {
		return PrivNone, user + " is not root and this node was " + OlderVersionMessage
	}
	return PrivNone, user + " is not root and \"sudo -n true\" fails (no passwordless sudo)"
}
