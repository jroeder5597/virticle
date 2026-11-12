package main

// "virticle node prepare" and "virticle node unprepare": make a host ready to
// be managed, and take that away again. The administrator's credential (the
// SSH agent, or a key file, or a password on standard input) is used for this
// one run and never stored.

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"virticle/internal/cluster"
	"virticle/internal/prepare"
)

const nodeUsage = `usage:
  virticle node prepare   [--check] [--user NAME] [--use-existing-user[=NAME]] [--confirm] [--root]
                          [--from ADDR] [--extras guestfs,virtiofs] [--jump user@host]
                          [--identity FILE] [--password-stdin] [--data DIR] [user@]host[:port]
  virticle node unprepare [--user NAME] [--root] [--jump user@host] [--identity FILE]
                          [--password-stdin] [--data DIR] [user@]host[:port]

prepare signs in as the administrator (your SSH agent, or --identity, or a password read from
standard input), installs libvirt and QEMU, installs the node command (virticle-node) and tests
the connection. There are two ways for the controller to reach the node:
  Service account (default, recommended): prepare creates an account for the controller with a
    restricted SSH key and a sudo rule that lets it run the node command as root, and nothing else.
  Root (--root): the controller signs in as root and runs the node command directly. No account
    is created.
--check changes nothing. Running prepare again changes nothing. unprepare removes the account,
key, node command and sudo rule; packages, VMs, pools and networks stay.
(--allow-root is the old name of --root.)`

// useExistingFlag is a boolean flag that also accepts --use-existing-user=NAME.
type useExistingFlag struct {
	on   bool
	name string
}

func (f *useExistingFlag) IsBoolFlag() bool { return true }
func (f *useExistingFlag) String() string   { return fmt.Sprint(f.on) }
func (f *useExistingFlag) Set(s string) error {
	switch s {
	case "true":
		f.on = true
	case "false":
		f.on = false
	default:
		f.on, f.name = true, s
	}
	return nil
}

func nodeCommand(args []string) int {
	if len(args) == 0 || (args[0] != "prepare" && args[0] != "unprepare") {
		fmt.Fprintln(os.Stderr, nodeUsage)
		return 2
	}
	unprepare := args[0] == "unprepare"
	fs := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, nodeUsage) }
	var (
		check     = fs.Bool("check", false, "read-only preflight; change nothing")
		user      = fs.String("user", "", "service account name (default virticle)")
		existing  useExistingFlag
		confirm   = fs.Bool("confirm", false, "with --use-existing-user: add the account to the libvirt and kvm groups and give it the sudo rule for the node command")
		root      = fs.Bool("root", false, "Root access: the controller signs in as root instead of using a service account (a service account is safer)")
		allowRoot = fs.Bool("allow-root", false, "")
		from      = fs.String("from", "", "restrict the controller's key to this source address (comma-separated patterns)")
		extras    = fs.String("extras", "", "comma-separated extra packages: guestfs, virtiofs")
		jump      = fs.String("jump", "", "user@host to connect through")
		identity  = fs.String("identity", "", "administrator's private key file (the SSH agent is used as well)")
		pwStdin   = fs.Bool("password-stdin", false, "read the administrator's password from standard input")
		data      = fs.String("data", "data", "the controller's data directory (its key and known_hosts)")
	)
	fs.Var(&existing, "use-existing-user", "the account already exists: only add the key (and with --confirm groups and the sudo rule)")

	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			if err == flag.ErrHelp {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	target := ""
	for _, p := range pos {
		if existing.on && existing.name == "" && !strings.Contains(p, "@") && len(pos) == 2 && target == "" && p == pos[0] {
			existing.name = p // --use-existing-user NAME
			continue
		}
		if target != "" {
			fmt.Fprintln(os.Stderr, "one host please:", nodeUsage)
			return 2
		}
		target = p
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, nodeUsage)
		return 2
	}
	adminUser, addr := "root", target
	if u, a, ok := strings.Cut(target, "@"); ok {
		adminUser, addr = u, a
	}

	if err := os.MkdirAll(*data, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	keyFile := filepath.Join(*data, "id_ed25519")
	pub, err := ensureKey(keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "controller key:", err)
		return 1
	}
	instance := ""
	if b, err := os.ReadFile(filepath.Join(*data, "instance-id")); err == nil {
		instance = strings.TrimSpace(string(b))
	}
	renameEnv("VIRT_JUMP_PASSWORD", "VIRTICLE_JUMP_PASSWORD")

	o := prepare.Options{User: *user, UseExisting: existing.on, Confirm: *confirm, AllowRoot: *root || *allowRoot,
		From: *from, ControllerKey: pub, InstanceID: instance}
	if existing.name != "" {
		o.User = existing.name
	}
	for _, e := range strings.Split(*extras, ",") {
		if e = strings.TrimSpace(e); e != "" {
			o.Extras = append(o.Extras, e)
		}
	}
	if err := o.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	a := prepare.Admin{Addr: addr, User: adminUser, Jump: *jump, Agent: os.Getenv("SSH_AUTH_SOCK") != ""}
	if *identity != "" {
		b, err := os.ReadFile(*identity)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		a.KeyPEM = b
	}
	if *pwStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		a.Password = strings.TrimRight(line, "\r\n")
	}
	if a.Password == "" && len(a.KeyPEM) == 0 && !a.Agent {
		fmt.Fprintln(os.Stderr, "no administrator credential: start an SSH agent with the key loaded, or give --identity FILE or --password-stdin")
		return 2
	}

	mgr := cluster.NewManager(keyFile, filepath.Join(*data, "known_hosts"))
	conn, err := prepare.Dial(mgr, a)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: could not sign in as the administrator:", err)
		return 1
	}
	defer conn.Close()
	r := &prepare.Runner{N: conn, SudoPassword: a.Password}

	var rows []prepare.Row
	switch {
	case unprepare:
		rows = r.Unprepare(o)
	case *check:
		rows, _, _ = r.Check(o)
	default:
		printed := 0
		rows = r.Prepare(o, func(rs []prepare.Row) {
			// Print rows as they settle; the final list is printed below for
			// anything not shown yet.
			for printed < len(rs) && rs[printed].Status != prepare.Skipped {
				printRow(rs[printed])
				printed++
			}
		}, func() []prepare.Row { return prepare.Verify(mgr, addr, o.ServiceUser(), *jump) })
		for ; printed < len(rows); printed++ {
			printRow(rows[printed])
		}
		return exitCode(rows)
	}
	for _, r := range rows {
		printRow(r)
	}
	return exitCode(rows)
}

func printRow(r prepare.Row) {
	word := map[string]string{prepare.OK: "Ok", prepare.Warning: "Warning", prepare.Error: "Error", prepare.Info: "Info", prepare.Skipped: "Not run"}[r.Status]
	if r.Detail != "" && r.Status != prepare.Skipped {
		fmt.Printf("%-8s %s: %s\n", word, r.Title, r.Detail)
	} else {
		fmt.Printf("%-8s %s\n", word, r.Title)
	}
}

func exitCode(rows []prepare.Row) int {
	if prepare.HasError(rows) {
		return 1
	}
	return 0
}
