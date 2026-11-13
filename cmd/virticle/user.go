package main

// "virticle user ...": the way back in for someone locked out of the web UI.
// It works on the database in the data directory, so it needs the access the
// controller itself has (run it as the service user or as root).

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"virticle/internal/api"
	"virticle/internal/store"
)

const userUsage = `usage: virticle user <command> USER [-data DIR]

  reset-2fa USER       turn two-step sign-in off for USER (lost phone); USER is signed out everywhere
  reset-password USER  set a new password for the local account USER; the new password is asked on
                       the terminal. Without a terminal, pass --password-stdin and send it on stdin.

These edit the database directly; the controller needs no restart.
-data defaults to /var/lib/virticle on an installed machine, else ./data.
`

// userIO is what the commands need from the terminal; tests replace it.
type userIO struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	isTerminal bool
	// readPassword reads a hidden line from the terminal.
	readPassword func() (string, error)
}

func realUserIO() userIO {
	fd := int(os.Stdin.Fd())
	return userIO{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, isTerminal: term.IsTerminal(fd),
		readPassword: func() (string, error) {
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		},
	}
}

func userCommand(args []string) int { return runUserCommand(args, realUserIO()) }

func runUserCommand(args []string, io userIO) int {
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(io.stderr, userUsage)
		return 2
	}
	cmd, name := args[0], args[1]
	fs := flag.NewFlagSet("user "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.stderr)
	data := fs.String("data", defaultDataDir(), "controller data directory")
	stdinPW := fs.Bool("password-stdin", false, "read the new password from stdin (reset-password)")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if cmd != "reset-2fa" && cmd != "reset-password" {
		fmt.Fprint(io.stderr, userUsage)
		return 2
	}
	if !store.HasDatabase(*data) {
		fmt.Fprintf(io.stderr, "no database in %s; use -data to name the controller's data directory\n", *data)
		return 1
	}
	dbPath, err := store.DBPath(*data, func(string, ...any) {})
	if err != nil {
		fmt.Fprintln(io.stderr, "database:", err)
		return 1
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(io.stderr, "database:", err)
		return 1
	}
	defer st.Close()
	u, err := st.UserByName(name)
	if err != nil {
		fmt.Fprintf(io.stderr, "there is no user %q\n", name)
		return 1
	}
	switch cmd {
	case "reset-2fa":
		if !u.TwoStep {
			fmt.Fprintf(io.stdout, "%s does not have two-step sign-in on; nothing changed.\n", name)
			return 0
		}
		if err := st.ResetTwoStep(u.ID); err != nil {
			fmt.Fprintln(io.stderr, err)
			return 1
		}
		fmt.Fprintf(io.stdout, "Two-step sign-in is off for %s, and %s is signed out everywhere.\n", name, name)
		return 0
	case "reset-password":
		if u.Source != store.SourceLocal {
			fmt.Fprintf(io.stderr, "%s signs in through an identity provider or directory and has no password here\n", name)
			return 1
		}
		pw, err := newPassword(io, *stdinPW)
		if err != nil {
			fmt.Fprintln(io.stderr, err)
			return 1
		}
		if err := api.ValidatePassword(pw); err != nil {
			fmt.Fprintln(io.stderr, err)
			return 1
		}
		if err := st.UpdateUser(u.ID, store.UserUpdate{Password: &pw}); err != nil {
			fmt.Fprintln(io.stderr, err)
			return 1
		}
		fmt.Fprintf(io.stdout, "The password of %s is changed, and %s is signed out everywhere.\n", name, name)
		return 0
	}
	return 2
}

// newPassword reads the new password: asked twice on a terminal, one line from
// stdin with --password-stdin, and refused when stdin is neither.
func newPassword(io userIO, fromStdin bool) (string, error) {
	switch {
	case fromStdin:
		line, err := bufio.NewReader(io.stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	case !io.isTerminal:
		return "", errors.New("stdin is not a terminal: run this in a terminal, or add --password-stdin and send the password on stdin")
	}
	fmt.Fprint(io.stderr, "New password: ")
	a, err := io.readPassword()
	if err != nil {
		return "", err
	}
	fmt.Fprint(io.stderr, "Again: ")
	b, err := io.readPassword()
	if err != nil {
		return "", err
	}
	if a != b {
		return "", errors.New("the two passwords differ")
	}
	return a, nil
}
