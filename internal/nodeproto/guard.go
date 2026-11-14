package nodeproto

import (
	"fmt"
	"strings"
)

// The start guard: a libvirt QEMU hook that asks the standby whether a replicated VM was started there from
// its copy (a failover), and keeps the old VM off when it was. See internal/nodecmd/verbs_guard.go.

const (
	// HookPath is where "virticle node prepare" installs the hook: the directory form of libvirt's hooks, so
	// an administrator's own /etc/libvirt/hooks/qemu is never touched.
	HookPath = "/etc/libvirt/hooks/qemu.d/virticle-guard"
	// HookMark is in the hook file; only a file that carries it is replaced or removed.
	HookMark = "Written by virticle node prepare"
	// HookRefuse is the exit status of "hook-qemu" that means "do not start this VM". The hook file turns it
	// into a failure for libvirt and every other status (an error, a missing program) into a start.
	HookRefuse = 13
	// AppArmorMain and AppArmorLocal are libvirt's AppArmor profile on Debian and Ubuntu and the file it includes
	// for local rules. libvirtd runs the hook in its own profile, which does not let it start the node command;
	// prepare adds one block there (between AppArmorBegin and AppArmorEnd) that lets the hook run the node
	// command in a profile of its own: it may run ssh and use the network, nothing else.
	AppArmorMain  = "/etc/apparmor.d/usr.sbin.libvirtd"
	AppArmorLocal = "/etc/apparmor.d/local/usr.sbin.libvirtd"
	AppArmorBegin = "# BEGIN virticle start guard (written by virticle node prepare)"
	AppArmorEnd   = "# END virticle start guard"
	// SelftestVM is the name of the probe VM prepare starts once to see that libvirt runs the hook.
	SelftestVM = "virticle-guard-selftest"
	// SelftestMark starts the line the hook answers the probe with.
	SelftestMark = "virticle-guard selftest:"
)

// HookScript is the hook file. It only calls the node command: the one decision that stops a start is the
// command's exit status 13; anything else (a command that is missing, crashes or times out) lets the VM start.
func HookScript(account string) string {
	return fmt.Sprintf(`#!/bin/sh
# %s. Keeps the old copy of a failed-over VM off (see the user guide, "Replication and failover").
n=%s
[ -x "$n" ] || exit 0
"$n" hook-qemu %s "$@"
[ $? -eq %d ] && exit 1
exit 0
`, HookMark, Path, account, HookRefuse)
}

// IsGuardHook says whether text is a hook file this program wrote.
func IsGuardHook(text string) bool { return strings.Contains(text, HookMark) }

// TookOver is the standby's answer to "did you take this VM over?": it did when it holds a failed-over
// record of a later generation than the asking node's own VM has (0 when that VM never took over). It is
// the rule of the controller's decideOldCopy (internal/api/replica_reconcile.go) seen from the old node.
func TookOver(standbyHasRecord bool, standbyGen, askerGen int) bool {
	return standbyHasRecord && standbyGen > askerGen
}

// AppArmorBlock is the rule block for the hook's node command; AppArmorHome in it stands for the home directory of
// the account whose node key the command uses (prepare fills it in on the node).
const AppArmorHome = "@HOME@"

const AppArmorBlock = AppArmorBegin + `
` + Path + ` Cx -> virticle_node,
profile virticle_node {
  #include <abstractions/base>
  #include <abstractions/nameservice>
  ` + Path + ` mr,
  /usr/bin/ssh ix,
  /usr/bin/getent ix,
  ` + AppArmorHome + `/.ssh/* r,
  /proc/*/cgroup r,
  /proc/*/mountinfo r,
  network inet stream,
  network inet6 stream,
  capability setuid,
  capability setgid,
  capability dac_override,
  capability dac_read_search,
}
` + AppArmorEnd + "\n"
