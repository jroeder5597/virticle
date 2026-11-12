package api

// The root-requiring steps of the migration TLS setup, in two forms: the
// scripts a root or broad-sudo login runs, and the calls to the node's
// virticle-helper (internal/hostconn/virticle-helper.sh) when the login may
// run only that. Both do the same thing; the helper checks its arguments
// itself.

import (
	"path"
	"strings"

	"virticle/internal/hostconn"
	"virticle/internal/migpki"
)

func scoped(h *hostconn.Host) bool { return h.Privilege() == hostconn.PrivHelper }

// migState reads the certificate directory (private keys are readable by
// QEMU's account and root only).
func migState(h *hostconn.Host, dir string) (string, error) {
	if scoped(h) {
		return h.RunHelper("migtls-state", dir)
	}
	return h.RunRoot(stateScript(dir))
}

func migMkdir(h *hostconn.Host, dir string) (string, error) {
	if scoped(h) {
		return h.RunHelper("migtls-mkdir", dir)
	}
	return h.RunRoot("umask 022; mkdir -p -- " + shq(dir))
}

// migPut stores content as dir/name.virticle-tmp with the given owner and mode.
func migPut(h *hostconn.Host, dir, name, owner, mode, content string) (string, error) {
	if scoped(h) {
		return h.RunHelperInput(content, "migtls-put", dir, name, owner, mode)
	}
	tmp := shq(path.Join(dir, name+tmpSuffix))
	return h.RunInputRoot("umask 077; cat > "+tmp+" && chown "+shq(owner)+" "+tmp+" && chmod "+mode+" "+tmp, content)
}

// migDiscard removes the temporary files of a failed run, and the directories
// that run created.
func migDiscard(h *hostconn.Host, dir string, createdDirs []string) {
	if scoped(h) {
		h.RunHelper("migtls-discard", dir)
		for i := len(createdDirs) - 1; i >= 0; i-- {
			h.RunHelper("migtls-rmdir", dir, createdDirs[i])
		}
		return
	}
	var b strings.Builder
	b.WriteString("rm -f --")
	for _, f := range append(append([]string{}, migpki.Files...), migpki.FileMarker) {
		b.WriteString(" " + shq(path.Join(dir, f+tmpSuffix)))
	}
	h.RunRoot(b.String())
	for i := len(createdDirs) - 1; i >= 0; i-- {
		h.RunRoot("rmdir -- " + shq(createdDirs[i]) + " 2>/dev/null; true")
	}
}

// migCommit moves the temporary files into place. backups lists the existing
// files to keep as *.virticle-orig first (name, backup name).
func migCommit(h *hostconn.Host, dir string, backups [][2]string, dropLegacy, restorecon bool) (string, error) {
	if scoped(h) {
		args := []string{dir}
		for _, b := range backups {
			args = append(args, "backup:"+b[0])
		}
		if dropLegacy {
			args = append(args, "rm-legacy")
		}
		if restorecon {
			args = append(args, "restorecon")
		}
		return h.RunHelper("migtls-commit", args...)
	}
	var sh strings.Builder
	sh.WriteString("set -e; ")
	for _, b := range backups {
		sh.WriteString("mv -- " + shq(path.Join(dir, b[0])) + " " + shq(path.Join(dir, b[1])) + "; ")
	}
	for _, f := range append(append([]string{}, migpki.Files...), migpki.FileMarker) {
		sh.WriteString("mv -f -- " + shq(path.Join(dir, f+tmpSuffix)) + " " + shq(path.Join(dir, f)) + "; ")
	}
	if dropLegacy {
		sh.WriteString("rm -f -- " + shq(path.Join(dir, migpki.FileMarkerLegacy)) + "; ")
	}
	// SELinux hosts (RHEL family) need the file contexts libvirt expects.
	if restorecon {
		sh.WriteString("restorecon -R " + shq(dir) + " >/dev/null 2>&1 || true; ")
	}
	sh.WriteString("true")
	return h.RunRoot(sh.String())
}

// migOp is one step of a removal: delete a file, or put its backup back.
type migOp struct {
	Restore bool
	Name    string // file name in the certificate directory
}

func (o migOp) arg() string {
	if o.Restore {
		return "restore:" + o.Name
	}
	return "rm:" + o.Name
}

func migRemove(h *hostconn.Host, dir string, ops []migOp) (string, error) {
	if scoped(h) {
		args := []string{dir}
		for _, o := range ops {
			args = append(args, o.arg())
		}
		return h.RunHelper("migtls-remove", args...)
	}
	var sh strings.Builder
	sh.WriteString("set -e; ")
	for _, o := range ops {
		if o.Restore {
			sh.WriteString("mv -- " + shq(path.Join(dir, o.Name+migBackupSuffix)) + " " + shq(path.Join(dir, o.Name)) + "; ")
		} else {
			sh.WriteString("rm -f -- " + shq(path.Join(dir, o.Name)) + "; ")
		}
	}
	sh.WriteString("true")
	return h.RunRoot(sh.String())
}

// gpuUsers runs the check behind hostDriverInUse: it needs root to see other
// users' processes.
func gpuUsers(h *hostconn.Host, addr string) (string, error) {
	if scoped(h) {
		return h.RunHelper("gpu-users", addr)
	}
	return h.RunRoot(gpuUsersCommand(addr))
}

// migRmdir removes an empty directory the setup created.
func migRmdir(h *hostconn.Host, dir, target string) (string, error) {
	if scoped(h) {
		return h.RunHelper("migtls-rmdir", dir, target)
	}
	return h.RunRoot("rmdir -- " + shq(target))
}
