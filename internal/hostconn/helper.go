package hostconn

import (
	"fmt"
	"regexp"
	"strings"

	"virticle/internal/nodeproto"
)

// The scoped root access. A node can give the controller's service account the
// right to run exactly one program as root, the node command (virticle-node,
// internal/nodecmd). Its verbs are fixed and validate their arguments; there is
// no way to run an arbitrary command through it, and no way to replace it from
// the service account (an update takes an administrator: "virticle node
// prepare"). A login as root runs the same program directly, without sudo.

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

// BroadSudoersLine is the legacy alternative: unrestricted passwordless sudo.
func BroadSudoersLine(user string) string {
	return fmt.Sprintf("# Written by virticle node prepare (broad sudo was asked for). Gives %s root.\n%s ALL=(ALL) NOPASSWD: ALL\n", user, user)
}

var userNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidUserName reports whether s is a plain account name.
func ValidUserName(s string) bool { return userNameRE.MatchString(s) }

// ErrScoped is returned for a command that needs root on a node where the
// service account has only the node command.
type ErrScoped struct{}

func (ErrScoped) Error() string {
	return "this needs broad sudo or root on the node; the account may run the virticle node command only, which does not cover it"
}

// Is makes errors.Is(err, ErrNoPrivilege) true, so callers that already
// answer "no privilege" handle it.
func (ErrScoped) Is(target error) bool { return target == ErrNoPrivilege }

// NodeCmd is the command line that runs one verb of the node command as root
// for a login with privilege p: through "sudo -n" for the service account
// (PrivHelper), directly for root (PrivRoot). Every argument is quoted. For
// any other privilege there is no node command line and ok is false.
func NodeCmd(p Privilege, verb string, args ...string) (cmd string, ok bool) {
	var b strings.Builder
	switch p {
	case PrivHelper:
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

// HelperCmd is the command line that runs one verb through sudo, for the
// service account. See NodeCmd for the other logins.
func HelperCmd(sub string, args ...string) string {
	c, _ := NodeCmd(PrivHelper, sub, args...)
	return c
}

// RunHelper runs a verb of the node command as root through sudo.
func (h *Host) RunHelper(sub string, args ...string) (string, error) {
	return h.Run(HelperCmd(sub, args...))
}

// RunHelperInput is RunHelper with stdin.
func (h *Host) RunHelperInput(stdin, sub string, args ...string) (string, error) {
	return h.RunInput(HelperCmd(sub, args...), stdin)
}

// RunNode runs a verb of the node command for the connection's own privilege:
// with sudo for the service account, directly for root.
func (h *Host) RunNode(verb string, args ...string) (string, error) {
	cmd, ok := NodeCmd(h.priv, verb, args...)
	if !ok {
		return "", ErrNoPrivilege
	}
	return h.Run(cmd)
}

// RunNodeInput is RunNode with stdin.
func (h *Host) RunNodeInput(stdin, verb string, args ...string) (string, error) {
	cmd, ok := NodeCmd(h.priv, verb, args...)
	if !ok {
		return "", ErrNoPrivilege
	}
	return h.RunInput(cmd, stdin)
}
