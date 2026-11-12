// Package nodecmd is the node side of virticle: the code of the program
// virticle-node (cmd/virticle-node), which a node lets the controller run as
// root. It is installed at /usr/local/libexec/virticle-node, root:root 0755,
// and started on demand, through sudo for the service account or directly by
// root over SSH. There is no daemon and no listener, and nothing but a verb
// name and its arguments goes in. The package must not import the controller
// packages (api, store, cluster, web, sqlite).
//
// # Adding a verb
//
// A verb group lives in its own file (verbs_files.go, verbs_migtls.go,
// verbs_peer.go, verbs_backup.go, verbs_pool.go) and registers in init:
//
//	func init() { Register("df", verbDF) }
//
//	func verbDF(c *Ctx) error {
//		if err := c.Need(1, 1, "df DIR"); err != nil { // usage errors exit 2
//			return err
//		}
//		r, err := c.ResolveRead(c.Args[0])
//		...
//		fmt.Fprintln(c.Stdout, free)
//		return nil
//	}
//
// A verb keeps the name, arguments, stdout format and exit status the
// controller expects. Errors: Die(...) is a refusal of arguments or paths
// (exit 2), Fail(...) an operation that did not work (exit 1), ExitCode(n)
// ends with status n and prints nothing. Main prints the message to stderr
// with the prefix "virticle-node: ". Check all arguments before acting on any.
//
// # Resolving a confined path
//
// Every path a verb takes from the caller goes through one of three calls.
// Each checks PlainPath, canonicalises, requires the place to be inside an
// allowed root (a libvirt dir/fs/netfs pool target that is not a system
// directory, or the NVRAM directory) and returns a *Ref that holds an open
// descriptor of the parent directory. Close it. The Ref methods work relative
// to that descriptor with O_NOFOLLOW, so nothing is looked up by path twice.
//
//	ResolveRead(arg)  an existing path (links followed, must stay inside) or a missing one
//	ResolveNew(arg)   a name to create: refuses anything that exists, also a link
//	ResolveMod(arg)   a name to change or remove: refuses a final link and a pool directory
//
// Use r.Lstat(), r.Open(flags, perm), r.OpenDir(), r.Unlink(), r.DirFD() (for
// unix.Unlinkat, Renameat2, Mkdirat and the like), r.Path (canonical) and
// r.Arg (as given, for messages). Hand a path to an external tool only as
// r.Path, and only when the tool cannot be given a descriptor.
//
// # Creating a new file safely
//
// Use c.CreateNew(r, mode, fill) with a Ref from ResolveNew. fill writes the
// content to a hidden part file next to the final name (NAME.virticle-part.PID,
// created exclusively); then the part file gets mode and is hard linked to the
// final name, which fails if the name appeared meanwhile; the part file is
// removed in every case, also when the process is killed by a signal. Anything
// else that must not outlive a killed verb needs c.Cleanup(fn); call the
// function it returns once the file is meant to stay.
//
// # Running a tool
//
// c.Command("qemu-img", args...) returns an *exec.Cmd with the absolute path
// from Config.Tools or the fixed PATH (/usr/sbin:/usr/bin:/sbin:/bin), an argv
// slice, PATH and LC_ALL=C as the whole environment, and a kill on signal.
// Never build a shell command line. c.Output and c.CombinedOutput are the short
// forms; c.HasTool asks whether a tool exists. A tool that is not installed
// gives a Fail error "NAME is not installed on this node".
//
// # Testing a verb
//
// There is no environment variable or flag that changes roots or paths in the
// real binary. Tests build a Config that points into t.TempDir() (see
// testutil_test.go): newNode(t) makes two pools, an NVRAM directory, a
// service account home and a Config with Unprivileged set, and n.run, n.ok,
// n.refused and n.denied call Run with stdin and read the exit status:
//
//	func TestDF(t *testing.T) {
//		n := newNode(t)
//		n.denied("outside the pools", "", "df", n.abs("outside"))
//		if got := n.ok("", "df", n.abs("pools/default")); got == "" { ... }
//	}
//
// A fake external tool is a script whose path is put in n.cfg.Tools["name"]
// (an empty path means "not installed"). Paths in tests are real temporary
// paths, so they must be plain (letters, digits and ._+=,@/-).
package nodecmd
