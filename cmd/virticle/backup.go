package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"virticle/internal/buildinfo"
	"virticle/internal/ctlbackup"
	"virticle/internal/store"
)

const backupUsage = `usage: virticle backup [-data DIR] [-o FILE|DIR] [--encrypt] [--passphrase-file FILE]
       virticle restore [-data DIR] [--passphrase-file FILE] FILE

backup writes the controller's own data (users, node list, SSH key, certificates)
to one file. It works while the controller runs. restore puts it back and needs
the controller stopped.`

// controllerSource is what a backup of the data directory takes.
func controllerSource(data string) ctlbackup.Source {
	host, _ := os.Hostname()
	return ctlbackup.Source{
		DataDir:  data,
		Snapshot: func(dst string) error { return store.SnapshotFile(filepath.Join(data, store.DBName), dst) },
		App:      buildinfo.Version,
		Host:     host,
	}
}

// backupPassphrase gets the passphrase for a new backup: from a file, or twice
// from the terminal. Never from a pipe.
func backupPassphrase(file string, confirm bool) (string, error) {
	if file != "" {
		return passphraseFromFile(file)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("a passphrase can only be typed on a terminal; use --passphrase-file FILE in a script")
	}
	fmt.Fprint(os.Stderr, "Passphrase: ")
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if confirm {
		fmt.Fprint(os.Stderr, "Passphrase again: ")
		p2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(p1) != string(p2) {
			return "", errors.New("the two passphrases differ")
		}
	}
	return string(p1), nil
}

func passphraseFromFile(file string) (string, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %s can be read by other users; run chmod 600 on it\n", file)
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	line, err := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", fmt.Errorf("%s is empty", file)
	}
	return line, nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// parseMixed parses flags that may come before or after one positional
// argument ("restore FILE -data DIR" works like "restore -data DIR FILE").
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func backupCommand(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, backupUsage) }
	data := fs.String("data", defaultDataDir(), "the controller's data directory")
	out := fs.String("o", "", "the file to write, or a directory to put it in (default: the current directory)")
	encrypt := fs.Bool("encrypt", false, "encrypt the backup with a passphrase asked on the terminal")
	passFile := fs.String("passphrase-file", "", "read the passphrase from this file (implies --encrypt)")
	pos, err := parseMixed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if len(pos) > 0 {
		fmt.Fprintln(os.Stderr, backupUsage)
		return 2
	}
	if err := runBackup(*data, *out, *encrypt || *passFile != "", *passFile); err != nil {
		fmt.Fprintln(os.Stderr, "backup failed:", err)
		return 1
	}
	return 0
}

func runBackup(data, out string, encrypt bool, passFile string) error {
	if _, err := os.Stat(filepath.Join(data, store.DBName)); err != nil {
		return fmt.Errorf("there is no controller database in %s (use -data DIR to name the data directory)", data)
	}
	pass := ""
	if encrypt {
		var err error
		if pass, err = backupPassphrase(passFile, passFile == ""); err != nil {
			return err
		}
		if err := ctlbackup.CheckPassphrase(pass); err != nil {
			return err
		}
	}
	src := controllerSource(data)
	src.Now = time.Now
	name := ctlbackup.FileName(src.Host, time.Now(), encrypt)
	path := name
	if out != "" {
		path = out
		if fi, err := os.Stat(out); (err == nil && fi.IsDir()) || strings.HasSuffix(out, "/") {
			path = filepath.Join(out, name)
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	_, size, err := ctlbackup.WriteFile(path, src, pass)
	if err != nil {
		return err
	}
	rec := ctlbackup.Record{Time: time.Now(), Size: size, Encrypted: encrypt, OK: true}
	var h ctlbackup.Host = ctlbackup.SystemHost{}
	var owner *ctlbackup.Owner
	if o, ok := h.OwnerOf(data); ok && h.IsRoot() && o.UID != 0 {
		owner = &o
	}
	if err := ctlbackup.Remember(data, "cli", rec, owner, h); err != nil {
		fmt.Fprintln(os.Stderr, "note: could not record the time of this backup:", err)
	}
	if encrypt {
		fmt.Printf("Backup written: %s (%s, encrypted). Keep the passphrase safe: without it the backup cannot be opened.\n", path, humanSize(size))
	} else {
		fmt.Printf("Backup written: %s (%s, not encrypted).\n", path, humanSize(size))
		fmt.Println("WARNING: this file holds the controller's private keys, which the nodes trust. Keep it as safe as a root password, or make it with --encrypt.")
	}
	return nil
}

// restoreDataDir is the default for restore: the packages' directory when
// this machine has the packaged service (its account or directory exists),
// else "data".
func restoreDataDir() string {
	if fi, err := os.Stat(systemDataDir); err == nil && fi.IsDir() {
		return systemDataDir
	}
	if _, ok := (ctlbackup.SystemHost{}).ServiceAccount(); ok {
		return systemDataDir
	}
	return "data"
}

func restoreCommand(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, backupUsage) }
	data := fs.String("data", "", "the data directory to restore into (default: "+systemDataDir+" on a packaged install, else data)")
	passFile := fs.String("passphrase-file", "", "read the passphrase of an encrypted backup from this file")
	pos, err := parseMixed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, backupUsage)
		return 2
	}
	dir := *data
	if dir == "" {
		dir = restoreDataDir()
	}
	if err := runRestore(pos[0], dir, *passFile, ctlbackup.SystemHost{}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "restore failed:", err)
		return 1
	}
	return 0
}

func runRestore(file, dir, passFile string, host ctlbackup.Host, out io.Writer) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	ask := func() (string, error) { return backupPassphrase(passFile, false) }
	plain, err := ctlbackup.Open(f, ask)
	if err != nil {
		return err
	}
	res, err := ctlbackup.Restore(ctlbackup.RestoreOptions{Archive: plain, DataDir: dir, Host: host, SchemaMax: store.SchemaVersion})
	if err != nil {
		return err
	}
	m := res.Manifest
	fmt.Fprintf(out, "Checked the backup: made %s on %s by virticle %s, database schema %d, %d files, every checksum matches.\n",
		m.Created.Local().Format("2006-01-02 15:04"), m.Host, m.App, m.SchemaVersion, len(m.Files))
	if res.Aside != "" {
		fmt.Fprintf(out, "The data directory that was there is now %s. Delete it when you are sure you do not need it.\n", res.Aside)
	}
	who := "the user who ran this command"
	if res.Owner != nil {
		who = fmt.Sprintf("user id %d", res.Owner.UID)
	}
	abs, _ := filepath.Abs(dir)
	fmt.Fprintf(out, "Restored into %s (owned by %s).\n\nNext:\n", abs, who)
	fmt.Fprintln(out, "  1. Start the controller: sudo systemctl start virticle")
	fmt.Fprintln(out, "  2. The nodes need nothing: they trust the restored SSH key, and the migration certificates match.")
	fmt.Fprintln(out, "  3. If this machine has a different address: open the controller at the new address. The nodes still accept it.")
	fmt.Fprintln(out, "     Exception: a node whose key was restricted with --from to the old address refuses the new one;")
	fmt.Fprintln(out, "     run: virticle node prepare ... --from NEW-ADDRESS (for that node).")
	fmt.Fprintln(out, "  4. If it is a new machine, run: sudo virticle setup")
	return nil
}
