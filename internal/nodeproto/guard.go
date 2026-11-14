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
