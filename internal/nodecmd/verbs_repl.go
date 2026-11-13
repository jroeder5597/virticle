package nodecmd

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// The replication verbs. A replica on a standby node is a work directory
// (.virticle-*) that holds, for every disk, a base image and a chain of small
// overlays, one per replication point. All of them work on plain file names
// inside such a directory:
//
//	repl-overlay WORKDIR NAME BACKING   a new empty overlay NAME on BACKING
//	repl-rebase  WORKDIR NAME BACKING   point NAME at BACKING (header only, no data is touched)
//	repl-commit  WORKDIR NAME [BASE]    write NAME (and the overlays between) into BASE, which
//	                                    defaults to its backing file; NAME is left empty
//	repl-ls      WORKDIR                NAME|SIZE of every regular file in it
//	mtime        PATH...                modification time (ns) and size of each file
//
// repl-commit is the only one that rewrites data of an image that may be the
// only copy. It is safe to cut short and to run again: the chain above BASE
// reads the same whether or not a block was already written down (see
// internal/api/replica_points.go).

func init() {
	Register("repl-overlay", verbReplOverlay)
	Register("repl-rebase", verbReplRebase)
	Register("repl-commit", verbReplCommit)
	Register("mtime", verbMtime)
	Register("repl-ls", verbReplLs)
}

// replArgs opens the work directory and checks the file names: the first
// count names must be plain and, when exist is set, regular files there.
func replArgs(c *Ctx, count int, exist bool) (*Ref, int, error) {
	w, wfd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return nil, -1, err
	}
	for _, n := range c.Args[1 : 1+count] {
		if !nameOK(n) {
			unix.Close(wfd)
			w.Close()
			return nil, -1, Die("unsupported file name")
		}
		if _, ok := regularAt(wfd, n); exist && !ok {
			unix.Close(wfd)
			w.Close()
			return nil, -1, Fail("%s is not a regular file", n)
		}
	}
	return w, wfd, nil
}

func qemuFail(what string, out []byte, err error) error {
	if _, ok := err.(*Error); ok {
		return err
	}
	return Fail("qemu-img could not %s: %s", what, strings.TrimSpace(string(out)))
}

func verbReplOverlay(c *Ctx) error {
	if err := c.Need(3, 3, "repl-overlay WORKDIR NAME BACKING"); err != nil {
		return err
	}
	w, wfd, err := replArgs(c, 2, false)
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	name, backing := c.Args[1], c.Args[2]
	if !nameOK(backing) {
		return Die("unsupported file name")
	}
	if _, ok := regularAt(wfd, backing); !ok {
		return Fail("%s is not a regular file", backing)
	}
	if _, ok := regularAt(wfd, name); ok {
		return Fail("%s exists already", name)
	}
	out, err := c.CombinedOutput("qemu-img", "create", "-f", "qcow2", "-b", backing, "-F", "qcow2", "--", w.Path+"/"+name)
	if err != nil {
		return qemuFail("create "+name, out, err)
	}
	return nil
}

func verbReplRebase(c *Ctx) error {
	if err := c.Need(3, 3, "repl-rebase WORKDIR NAME BACKING"); err != nil {
		return err
	}
	w, wfd, err := replArgs(c, 2, true)
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	name, backing := c.Args[1], c.Args[2]
	if !nameOK(backing) {
		return Die("unsupported file name")
	}
	if _, ok := regularAt(wfd, backing); !ok {
		return Fail("%s is not a regular file", backing)
	}
	// -u: only the header changes. The image holds only what changed since the backing file.
	out, err := c.CombinedOutput("qemu-img", "rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", backing, "--", w.Path+"/"+name)
	if err != nil {
		return qemuFail("rebase "+name, out, err)
	}
	return nil
}

func verbReplCommit(c *Ctx) error {
	if err := c.Need(2, 3, "repl-commit WORKDIR NAME [BASE]"); err != nil {
		return err
	}
	count := len(c.Args) - 1
	w, wfd, err := replArgs(c, count, true)
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	args := []string{"commit", "-f", "qcow2", "-t", "none"}
	if count == 2 {
		args = append(args, "-b", c.Args[2])
	}
	// qemu-img must not be sent anywhere else: every backing file of the image stays inside the roots.
	top := w.Path + "/" + c.Args[1]
	if err := c.CheckChain(top); err != nil {
		return err
	}
	out, err := c.CombinedOutput("qemu-img", append(args, "--", top)...)
	if err != nil {
		if _, ok := err.(*Error); ok {
			return err
		}
		// A commit that was killed half way can leave the base with refcounts that lag behind its tables
		// (found in the lab: "refcount=0 reference=1"). Its data is whole; qemu-img check repairs the
		// bookkeeping, then the commit is run again.
		base := ""
		if count == 2 {
			base = w.Path + "/" + c.Args[2]
		} else if bn, herr := HeaderBacking(top); herr == nil && bn != "" && !strings.Contains(bn, "/") {
			base = w.Path + "/" + bn
		}
		if base == "" {
			return qemuFail("commit "+c.Args[1], out, err)
		}
		// The exit status of check says what it found (2 corrupt, 3 leaks); what matters is that the commit works now.
		if _, cerr := c.CombinedOutput("qemu-img", "check", "-r", "all", "-f", "qcow2", "--", base); cerr != nil {
			if _, ok := cerr.(*Error); ok {
				return cerr
			}
		}
		if out2, err2 := c.CombinedOutput("qemu-img", append(args, "--", top)...); err2 != nil {
			return qemuFail("commit "+c.Args[1]+" after repairing the base", out2, err2)
		}
	}
	return nil
}

// mtime PATH...: "NS|SIZE" per path (modification time in nanoseconds and size
// of the file, links followed inside the roots) or MISSING.
func verbMtime(c *Ctx) error {
	if err := c.Need(1, 200, "mtime PATH..."); err != nil {
		return err
	}
	if err := checkPlain(c.Args...); err != nil {
		return err
	}
	for _, a := range c.Args {
		r, err := c.ResolveRead(a)
		if err != nil {
			return err
		}
		st, exists, err := r.Lstat()
		r.Close()
		if err != nil {
			return Fail("cannot stat %s", a)
		}
		if !exists || !isRegMode(st.Mode) {
			fmt.Fprintln(c.Stdout, "MISSING")
			continue
		}
		fmt.Fprintf(c.Stdout, "%d|%d\n", int64(st.Mtim.Sec)*1e9+int64(st.Mtim.Nsec), st.Size)
	}
	return nil
}

// repl-ls WORKDIR: "NAME|SIZE" for each regular file of the work directory
// (names that are not plain are left out).
func verbReplLs(c *Ctx) error {
	if err := c.Need(1, 1, "repl-ls WORKDIR"); err != nil {
		return err
	}
	w, wfd, err := workDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer w.Close()
	defer unix.Close(wfd)
	names, err := listDir(wfd)
	if err != nil {
		return Fail("cannot list %s", c.Args[0])
	}
	sort.Strings(names)
	for _, n := range names {
		if !nameOK(n) {
			continue
		}
		if sz, ok := regularAt(wfd, n); ok {
			fmt.Fprintf(c.Stdout, "%s|%d\n", n, sz)
		}
	}
	return nil
}
