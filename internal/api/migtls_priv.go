package api

// The root-requiring steps of the migration TLS setup. They are calls to the
// node command (internal/nodecmd), which checks its arguments itself; the
// service account runs it through sudo and a root login runs it directly.

import (
	"virticle/internal/hostconn"
)

// migState reads the certificate directory (private keys are readable by
// QEMU's account and root only).
func migState(h *hostconn.Host, dir string) (string, error) {
	return h.RunNode("migtls-state", dir)
}

func migMkdir(h *hostconn.Host, dir string) (string, error) {
	return h.RunNode("migtls-mkdir", dir)
}

// migPut stores content as dir/name.virticle-tmp with the given owner and mode.
func migPut(h *hostconn.Host, dir, name, owner, mode, content string) (string, error) {
	return h.RunNodeInput(content, "migtls-put", dir, name, owner, mode)
}

// migDiscard removes the temporary files of a failed run, and the directories
// that run created.
func migDiscard(h *hostconn.Host, dir string, createdDirs []string) {
	h.RunNode("migtls-discard", dir)
	for i := len(createdDirs) - 1; i >= 0; i-- {
		h.RunNode("migtls-rmdir", dir, createdDirs[i])
	}
}

// migCommit moves the temporary files into place. backups lists the existing
// files to keep as *.virticle-orig first (name, backup name).
func migCommit(h *hostconn.Host, dir string, backups [][2]string, dropLegacy, restorecon bool) (string, error) {
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
	return h.RunNode("migtls-commit", args...)
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
	args := []string{dir}
	for _, o := range ops {
		args = append(args, o.arg())
	}
	return h.RunNode("migtls-remove", args...)
}

// gpuUsers runs the check behind hostDriverInUse: it needs root to see other
// users' processes.
func gpuUsers(h *hostconn.Host, addr string) (string, error) {
	return h.RunNode("gpu-users", addr)
}

// migRmdir removes an empty directory the setup created.
func migRmdir(h *hostconn.Host, dir, target string) (string, error) {
	return h.RunNode("migtls-rmdir", dir, target)
}
