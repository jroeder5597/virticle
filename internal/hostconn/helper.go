package hostconn

import (
	"fmt"
	"regexp"
	"strings"

	"virticle/internal/nodeproto"
)

// The node command (virticle-node, internal/nodecmd) is the one program the
// controller runs as root on a node. The service account may run it through
// sudo and nothing else; a root login runs it directly. Its verbs are fixed
// and validate their arguments; there is no way to run an arbitrary command
// through it, and no way to replace it from the service account (an update
// takes an administrator: "virticle node prepare").

const (
	// HelperPath is where the node command is installed on a node.
	HelperPath = nodeproto.Path
	// OldHelperPath is the shell script of versions before the node command.
	// A node that still has it was prepared by an older version.
	OldHelperPath = "/usr/local/libexec/virticle-helper"
	// SudoersPath is the drop-in that allows the service account to run it.
	SudoersPath = "/etc/sudoers.d/virticle"
	// HelperVersion is the protocol number the node command must report.
	HelperVersion = nodeproto.Version
	// OlderVersionMessage is what a node with the old script is told.
	OlderVersionMessage = "prepared by an older version; run `virticle node prepare` again"
)

// SudoersLine is the content of the sudoers drop-in for user: the node command
// and nothing else, without a password. sudoers cannot pin the arguments of a
// command that takes any, so the node command validates them itself.
func SudoersLine(user string) string {
	return fmt.Sprintf("# Written by virticle node prepare. Lets %s run the virticle node command as root, and nothing else.\n%s ALL=(root) NOPASSWD: %s\n", user, user, HelperPath)
}

var userNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidUserName reports whether s is a plain account name.
func ValidUserName(s string) bool { return userNameRE.MatchString(s) }

// NodeCmd is the command line that runs one verb of the node command as root
// for a login with privilege p: through "sudo -n" for the service account,
// directly for root. Every argument is quoted. For any other privilege there
// is no node command line and ok is false.
func NodeCmd(p Privilege, verb string, args ...string) (cmd string, ok bool) {
	var b strings.Builder
	switch p {
	case PrivService:
		b.WriteString("sudo -n ")
	case PrivRoot:
	default:
		return "", false
	}
	b.WriteString(HelperPath + " " + verb)
	for _, a := range args {
		b.WriteString(" " + Quote(a))
	}
	return b.String(), true
}

// nodeCmd is NodeCmd for this connection's privilege.
func (h *Host) nodeCmd(verb string, args ...string) (string, error) {
	cmd, ok := NodeCmd(h.priv, verb, args...)
	if !ok {
		return "", ErrNoPrivilege
	}
	return cmd, nil
}

// RunNode runs a verb of the node command for the connection's own privilege:
// with sudo for the service account, directly for root. A refused run returns
// the reason as its output as well, as callers show the output.
func (h *Host) RunNode(verb string, args ...string) (string, error) {
	cmd, err := h.nodeCmd(verb, args...)
	if err != nil {
		return err.Error(), err
	}
	return h.Run(cmd)
}

// RunNodeInput is RunNode with stdin.
func (h *Host) RunNodeInput(stdin, verb string, args ...string) (string, error) {
	cmd, err := h.nodeCmd(verb, args...)
	if err != nil {
		return err.Error(), err
	}
	return h.RunInput(cmd, stdin)
}
