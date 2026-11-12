package hostconn

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

// The scoped root access. A node can give the controller's service account the
// right to run exactly one program as root, the helper. Its subcommands are
// fixed and validate their arguments; there is no way to run an arbitrary
// command through it, and no way to replace it from the service account (an
// update of the helper takes an administrator: "virticle node prepare").

// HelperScript is the helper as shipped in this binary.
//
//go:embed virticle-helper.sh
var HelperScript string

const (
	// HelperPath is where the helper is installed on a node.
	HelperPath = "/usr/local/libexec/virticle-helper"
	// SudoersPath is the drop-in that allows the service account to run it.
	SudoersPath = "/etc/sudoers.d/virticle"
)

var helperVersionRE = regexp.MustCompile(`(?m)^VERSION=([0-9]+)$`)

// HelperVersion is the version number inside HelperScript.
var HelperVersion = func() string {
	m := helperVersionRE.FindStringSubmatch(HelperScript)
	if m == nil {
		panic("virticle-helper.sh has no VERSION line")
	}
	return m[1]
}()

// SudoersLine is the content of the sudoers drop-in for user: the helper and
// nothing else, without a password. sudoers cannot pin the arguments of a
// command that takes any, so the helper validates them itself.
func SudoersLine(user string) string {
	return fmt.Sprintf("# Written by virticle node prepare. Lets %s run the virticle helper as root, and nothing else.\n%s ALL=(root) NOPASSWD: %s\n", user, user, HelperPath)
}

// BroadSudoersLine is the legacy alternative: unrestricted passwordless sudo.
func BroadSudoersLine(user string) string {
	return fmt.Sprintf("# Written by virticle node prepare (broad sudo was asked for). Gives %s root.\n%s ALL=(ALL) NOPASSWD: ALL\n", user, user)
}

var userNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidUserName reports whether s is a plain account name.
func ValidUserName(s string) bool { return userNameRE.MatchString(s) }

// ErrScoped is returned for a command that needs root on a node where the
// service account has only the helper.
type ErrScoped struct{}

func (ErrScoped) Error() string {
	return "this needs broad sudo or root on the node; the account may run the virticle helper only, which does not cover it"
}

// Is makes errors.Is(err, ErrNoPrivilege) true, so callers that already
// answer "no privilege" handle it.
func (ErrScoped) Is(target error) bool { return target == ErrNoPrivilege }

// HelperCmd is the command line that runs one helper subcommand as root. Every
// argument is quoted.
func HelperCmd(sub string, args ...string) string {
	var b strings.Builder
	b.WriteString("sudo -n " + HelperPath + " " + sub)
	for _, a := range args {
		b.WriteString(" " + Quote(a))
	}
	return b.String()
}

// RunHelper runs a helper subcommand as root.
func (h *Host) RunHelper(sub string, args ...string) (string, error) {
	return h.Run(HelperCmd(sub, args...))
}

// RunHelperInput is RunHelper with stdin.
func (h *Host) RunHelperInput(stdin, sub string, args ...string) (string, error) {
	return h.RunInput(HelperCmd(sub, args...), stdin)
}
