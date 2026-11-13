package prepare

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"virticle/internal/hostconn"
	"virticle/internal/nodebin"
)

// Node is the administrator's shell on the host: the plain SSH connection.
type Node interface {
	Run(cmd string) (string, error)
	RunInput(cmd, stdin string) (string, error)
}

// Runner runs the steps. SudoPassword, when set, feeds "sudo -S" for an
// administrator that is not root and has no passwordless sudo. It is held in
// memory only and is never put in a row, an error or a log line.
type Runner struct {
	N            Node
	SudoPassword string

	// User is the account the controller will sign in as, known after Check
	// (or Unprepare): the name given, else an account an earlier version
	// made, else DefaultUser; "root" for root access. The node is registered
	// with this name.
	User string

	isRoot  bool
	useSudo bool
}

// root runs cmd as root on the host. stdin reaches the command.
func (r *Runner) root(cmd, stdin string) (string, error) {
	switch {
	case r.isRoot:
		return r.N.RunInput("sh -c "+shq(cmd), stdin)
	case r.SudoPassword != "":
		out, err := r.N.RunInput("sudo -S -p '' sh -c "+shq(cmd), r.SudoPassword+"\n"+stdin)
		return r.scrub(out), err
	default:
		return r.N.RunInput("sudo -n sh -c "+shq(cmd), stdin)
	}
}

// scrub removes the sudo password from text that is about to be shown.
func (r *Runner) scrub(s string) string {
	if r.SudoPassword == "" {
		return s
	}
	return strings.ReplaceAll(s, r.SudoPassword, "")
}

// Access finds out how the administrator can act as root and says so as a row.
func (r *Runner) Access() Row {
	row := Row{ID: "admin", Title: "Administrator access"}
	out, err := r.N.Run("echo uid=$(id -u); echo user=$(id -un)")
	if err != nil {
		row.Status, row.Detail = Error, "could not run a command on the host: "+firstLine(r.scrub(out+" "+err.Error()))
		return row
	}
	var user string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case l == "uid=0":
			r.isRoot = true
		case strings.HasPrefix(l, "user="):
			user = strings.TrimPrefix(l, "user=")
		}
	}
	if r.isRoot {
		row.Status, row.Detail = OK, "Signed in as root"
		return row
	}
	if _, err := r.N.Run("sudo -n true"); err == nil {
		r.useSudo = true
		row.Status, row.Detail = OK, user+" has passwordless sudo"
		return row
	}
	if r.SudoPassword != "" {
		if _, err := r.root("true", ""); err == nil {
			r.useSudo = true
			row.Status, row.Detail = OK, user+" uses sudo with its password"
			return row
		}
		row.Status, row.Detail = Error, user+" is not root and sudo did not accept the password"
		return row
	}
	row.Status, row.Detail = Error, user+" is not root and has no passwordless sudo; sign in as root, or give a user that may use sudo (a password works when sudo accepts it)"
	return row
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// probeAccount reads what the host knows about the account name.
func (r *Runner) probeAccount(name string) (Account, error) {
	out, err := r.root("set -- "+shq(name)+"\n"+accountScript, "")
	if err != nil {
		return Account{}, fmt.Errorf("reading the account %s: %w", name, err)
	}
	return ParseAccount(out), nil
}

// Marked reports whether the account was made by this tool.
func (a Account) Marked() bool { return a.Exists && strings.Contains(a.Comment, AccountComment) }

// resolveAccount settles which account is meant and reads it. A name that was
// given is taken as it is. Without a name an account of ours that exists is
// used (DefaultUser first, then LegacyUser from an earlier version), so that
// running prepare again never makes a second account; otherwise DefaultUser.
// The controller package's own account "virticle" has no marker and is never
// chosen. root needs no account.
func (r *Runner) resolveAccount(o Options) (Options, Account, error) {
	if o.AllowRoot {
		r.User = "root"
		return o, Account{}, nil
	}
	if o.User == "" {
		def, err := r.probeAccount(DefaultUser)
		if err != nil {
			return o, Account{}, err
		}
		if def.Marked() {
			o.User = DefaultUser
			r.User = o.User
			return o, def, nil
		}
		old, err := r.probeAccount(LegacyUser)
		if err != nil {
			return o, Account{}, err
		}
		if old.Marked() {
			o.User = LegacyUser
			r.User = o.User
			return o, old, nil
		}
		o.User = DefaultUser
		r.User = o.User
		return o, def, nil
	}
	r.User = o.User
	a, err := r.probeAccount(o.User)
	return o, a, err
}

// Check looks at the host and returns the check rows. It changes nothing.
// r.User is set to the account that is meant.
func (r *Runner) Check(o Options) ([]Row, Plan, error) {
	rows, p, _, err := r.check(o)
	return rows, p, err
}

// check is Check that also returns the options with the account name settled.
func (r *Runner) check(o Options) ([]Row, Plan, Options, error) {
	var p Plan
	if err := o.Validate(); err != nil {
		return nil, p, o, err
	}
	access := r.Access()
	if access.Status == Error {
		return []Row{access}, p, o, errors.New(access.Detail)
	}
	out, err := r.root(FactsScript, "")
	if err != nil {
		return []Row{access}, p, o, fmt.Errorf("reading the host's facts: %w", err)
	}
	p.Facts = ParseFacts(out)
	p.Distro = Classify(p.Facts.OSID, p.Facts.OSVersion, p.Facts.OSLike, p.Facts.OSName)
	o, p.Account, err = r.resolveAccount(o)
	if err != nil {
		return []Row{access}, p, o, err
	}
	if p.Distro.Family != FamilyOther {
		p.Want = PackageList(p.Distro.Family, o.Minimal)
		mout, err := r.root(MissingScript(p.Distro.Family, p.Want), "")
		if err != nil {
			return []Row{access}, p, o, fmt.Errorf("listing installed packages: %w", err)
		}
		p.Missing = strings.Fields(mout)
	}
	return append([]Row{access}, Evaluate(p, o)...), p, o, nil
}

// step marker protocol: a script prints its detail lines and ends with one of
// these.
const (
	markChanged   = "@@changed"
	markUnchanged = "@@unchanged"
)

func parseStep(out string) (detail string, changed bool) {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		switch strings.TrimSpace(l) {
		case markChanged:
			changed = true
		case markUnchanged, "":
		default:
			lines = append(lines, upperFirst(strings.TrimSpace(l)))
		}
	}
	return strings.Join(lines, "; "), changed
}

type stepDef struct {
	id, title string
	run       func(r *Runner, p Plan, o Options) (detail string, changed bool, err error)
	skip      func(p Plan, o Options) string // non-empty: not applicable, shown as info with this text
}

func steps(o Options) []stepDef {
	list := []stepDef{
		{id: "do-packages", title: "Install packages", run: stepPackages},
		{id: "do-firewall", title: "Firewall", run: stepFirewall},
		{id: "do-services", title: "Enable libvirt", run: stepServices},
		{id: "do-network", title: "Default network", run: stepNetwork},
		{id: "do-pool", title: "Default pool", run: stepPool},
	}
	if !o.AllowRoot {
		list = append(list, stepDef{id: "do-account", title: "Service account", run: stepAccount})
	}
	list = append(list, stepDef{id: "do-key", title: "Install SSH key", run: stepKey})
	// The node command is installed for every login: the service account
	// runs it through sudo, root directly.
	list = append(list, stepDef{id: "do-nodecmd", title: "Install node command", run: stepNodeCommand})
	if !o.AllowRoot {
		// An existing account gets a sudo rule only when the change is confirmed.
		rule := !o.UseExisting || o.Confirm
		if rule {
			list = append(list, stepDef{id: "do-sudoers", title: "Install sudo rule", run: stepSudoers})
		}
	}
	list = append(list, stepDef{id: "do-peerkey", title: "Node key", run: stepPeerKey})
	return list
}

// Prepare checks the host and, when the check finds no error, makes it ready.
// report is called with the whole row list after every change to it; rows
// that did not run yet show as skipped. verify, when set, runs last (the
// connection test as the service account) and its rows are appended.
func (r *Runner) Prepare(o Options, report func([]Row), verify func() []Row) []Row {
	rows, plan, o, err := r.check(o)
	if err != nil && len(rows) == 0 {
		rows = []Row{{ID: "check", Title: "Check", Status: Error, Detail: r.scrub(err.Error())}}
	}
	publish := func() {
		if report != nil {
			report(append([]Row(nil), rows...))
		}
	}
	defs := steps(o)
	if HasError(rows) || err != nil {
		for _, d := range defs {
			rows = append(rows, Row{ID: d.id, Title: d.title, Status: Skipped, Detail: "Not run"})
		}
		if verify != nil {
			rows = append(rows, Row{ID: "do-test", Title: "Test connection", Status: Skipped, Detail: "Not run"})
		}
		publish()
		return rows
	}
	first := len(rows)
	for _, d := range defs {
		rows = append(rows, Row{ID: d.id, Title: d.title, Status: Skipped, Detail: "Not run"})
	}
	if verify != nil {
		rows = append(rows, Row{ID: "do-test", Title: "Test connection", Status: Skipped, Detail: "Not run"})
	}
	publish()
	changedAny, failed := false, false
	for i, d := range defs {
		detail, changed, err := d.run(r, plan, o)
		row := &rows[first+i]
		var rowErr *statusError
		switch {
		case errors.As(err, &rowErr):
			// Not a failure: the step went on, but the administrator has a
			// thing to do or know.
			row.Status, row.Detail = rowErr.Status, r.scrub(rowErr.Detail)
		case err != nil:
			row.Status, row.Detail = Error, r.scrub(err.Error())
			failed = true
		case changed:
			row.Status, row.Detail = OK, detail
			changedAny = true
		default:
			row.Status, row.Detail = OK, "Already in place. "+detail
			row.Detail = strings.TrimSpace(row.Detail)
		}
		publish()
		if failed {
			break
		}
	}
	if failed {
		return rows
	}
	if verify != nil {
		vr := verify()
		rows = append(rows[:len(rows)-1], vr...)
		if HasError(vr) {
			rows = append(rows, Row{ID: "summary", Title: "Result", Status: Warning, Detail: "The host was prepared, but the controller cannot use it yet; see the failed row"})
			publish()
			return rows
		}
	}
	if changedAny {
		rows = append(rows, Row{ID: "summary", Title: "Result", Status: Info, Detail: "The host is prepared"})
	} else {
		rows = append(rows, Row{ID: "summary", Title: "Result", Status: Info, Detail: "Nothing changed; the host was already prepared"})
	}
	publish()
	return rows
}

// --- steps ---

func stepPackages(r *Runner, p Plan, o Options) (string, bool, error) {
	if len(p.Missing) == 0 {
		return "All " + fmt.Sprint(len(p.Want)) + " packages are installed", false, nil
	}
	pk := make([]string, len(p.Missing))
	for i, m := range p.Missing {
		pk[i] = shq(m)
	}
	var cmd string
	if p.Distro.Family == FamilyDebian {
		cmd = "export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null 2>&1; out=$(timeout 1500 apt-get install -y -qq --no-install-recommends " + strings.Join(pk, " ") + " 2>&1); rc=$?; [ $rc -eq 0 ] || { echo \"$out\" | tail -n 12; exit $rc; }"
	} else {
		cmd = "out=$(timeout 1500 dnf install -y " + strings.Join(pk, " ") + " 2>&1); rc=$?; [ $rc -eq 0 ] || { echo \"$out\" | tail -n 12; exit $rc; }"
	}
	out, err := r.root(cmd, "")
	if err != nil {
		return "", false, fmt.Errorf("installing %s failed: %w: %s", strings.Join(p.Missing, " "), err, firstLines(out, 12))
	}
	return "Installed " + strings.Join(p.Missing, " "), true, nil
}

// statusError is returned by a step that did not fail but wants its row to
// show a status other than OK (a warning or a note); the steps after it run.
type statusError struct{ Status, Detail string }

func (e *statusError) Error() string { return e.Detail }

// stepFirewall opens the migration ports on the node. A firewall command that
// fails is a warning with the commands to run by hand, never an error.
func stepFirewall(r *Runner, p Plan, o Options) (string, bool, error) {
	row, changed := ApplyFirewall(func(script string) (string, error) { return r.root(script, "") }, NodeFirewall())
	if row.Status != OK {
		return "", false, &statusError{row.Status, row.Detail}
	}
	return row.Detail, changed, nil
}

func firstLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " / ")
}

const servicesScript = `
if systemctl cat libvirtd.service >/dev/null 2>&1; then
  units="libvirtd.service"; kind="libvirtd"
else
  units="virtqemud.socket virtnetworkd.socket virtstoraged.socket virtnodedevd.socket virtsecretd.socket virtproxyd.socket virtlogd.socket"; kind="modular daemons"
fi
need=0
for u in $units; do systemctl is-enabled --quiet "$u" 2>/dev/null || need=1; done
timeout 30 virsh -c qemu:///system version >/dev/null 2>&1 || need=1
if [ "$need" = 0 ]; then echo "Libvirt ($kind) is running and enabled"; echo @@unchanged; exit 0; fi
out=$(systemctl enable --now $units 2>&1) || { echo "$out" | tail -n 3; exit 1; }
i=0; while [ $i -lt 20 ]; do timeout 10 virsh -c qemu:///system version >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
timeout 10 virsh -c qemu:///system version >/dev/null 2>&1 || { echo "libvirt does not answer after starting $kind"; exit 1; }
echo "Enabled and started libvirt ($kind)"; echo @@changed`

func stepServices(r *Runner, p Plan, o Options) (string, bool, error) {
	out, err := r.root(servicesScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

const networkScript = `
V="virsh -c qemu:///system"
if $V net-info default >/dev/null 2>&1; then
  if $V net-info default 2>/dev/null | grep -q '^Active:[[:space:]]*yes'; then echo "default network is active"; echo @@unchanged; exit 0; fi
  $V net-start default >/dev/null 2>&1 || { echo "the existing default network does not start: $($V net-start default 2>&1 | head -n 2)"; exit 1; }
  echo "started the existing default network (its autostart setting is unchanged)"; echo @@changed; exit 0
fi
$V net-define /dev/stdin >/dev/null <<'XML'
<network>
  <name>default</name>
  <forward mode='nat'/>
  <bridge name='virbr0' stp='on' delay='0'/>
  <ip address='192.168.122.1' netmask='255.255.255.0'>
    <dhcp><range start='192.168.122.2' end='192.168.122.254'/></dhcp>
  </ip>
</network>
XML
$V net-autostart default >/dev/null && $V net-start default >/dev/null || { echo "could not start the new default network"; exit 1; }
echo "defined, started and set to autostart the default network (192.168.122.0/24)"; echo @@changed`

func stepNetwork(r *Runner, p Plan, o Options) (string, bool, error) {
	out, err := r.root(networkScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

const poolScript = `
V="virsh -c qemu:///system"
if $V pool-info default >/dev/null 2>&1; then
  if $V pool-info default 2>/dev/null | grep -q '^State:[[:space:]]*running'; then echo "default pool is running"; echo @@unchanged; exit 0; fi
  $V pool-start default >/dev/null 2>&1 || { echo "the existing default pool does not start: $($V pool-start default 2>&1 | head -n 2)"; exit 1; }
  echo "started the existing default pool (its settings are unchanged)"; echo @@changed; exit 0
fi
$V pool-define-as default dir --target ` + DefaultPoolPath + ` >/dev/null && $V pool-build default >/dev/null 2>&1; $V pool-autostart default >/dev/null && $V pool-start default >/dev/null || { echo "could not create the default pool"; exit 1; }
echo "defined, started and set to autostart the default pool (` + DefaultPoolPath + `)"; echo @@changed`

func stepPool(r *Runner, p Plan, o Options) (string, bool, error) {
	out, err := r.root(poolScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

// accountCreateScript makes sure the service account $u is in order. An account
// that carries the marker (made by an earlier run) is repaired: groups, login
// shell and home directory. An account without the marker is left alone
// unless $existing is 1, and then only the groups with $confirm.
const accountCreateScript = `
changed=0
marked=0
if id "$u" >/dev/null 2>&1; then
  getent passwd "$u" | grep -q "` + AccountComment + `" && marked=1
  if [ "$marked" != 1 ] && [ "$existing" != 1 ]; then echo "an account named $u exists and was not made by Virticle; use another name (--user NAME)"; exit 1; fi
elif [ "$existing" = 1 ]; then
  echo "the account $u does not exist"; exit 1
else
  useradd --system --create-home --home-dir "$home" --shell /bin/sh --user-group --comment "` + AccountComment + `" -p '*' "$u" || exit 1
  echo "Created the account $u (home $home, login shell /bin/sh, no password)"; changed=1; marked=1
fi
for g in libvirt kvm; do
  getent group "$g" >/dev/null || continue
  id -nG "$u" | tr ' ' '\n' | grep -qx "$g" && continue
  if [ "$marked" = 1 ] || [ "$confirm" = 1 ]; then
    usermod -aG "$g" "$u" || exit 1
    echo "Added $u to the group $g"; changed=1
  else
    echo "$u is not in the group $g; run again with --confirm to add it"
  fi
done
if [ "$marked" = 1 ]; then
  cur=$(getent passwd "$u" | cut -d: -f7)
  case "$cur" in
    ""|*/nologin|*/false) usermod -s /bin/sh "$u" || exit 1; echo "Set the login shell of $u to /bin/sh (it was ${cur:-empty})"; changed=1;;
  esac
  h=$(getent passwd "$u" | cut -d: -f6); g=$(id -gn "$u")
  if [ ! -d "$h" ]; then
    install -d -m 700 -o "$u" -g "$g" "$h" || exit 1
    command -v restorecon >/dev/null 2>&1 && restorecon "$h" >/dev/null 2>&1
    echo "Created the home directory $h"; changed=1
  elif [ "$(stat -c %U "$h")" != "$u" ]; then
    chown "$u:$g" "$h" || exit 1
    echo "Gave the home directory $h to $u"; changed=1
  fi
fi
if [ "$changed" = 1 ]; then echo @@changed; else echo "Account $u is already set up"; echo @@unchanged; fi`

func stepAccount(r *Runner, p Plan, o Options) (string, bool, error) {
	vars := "u=" + shq(o.ServiceUser()) + "\nhome=" + shq(HomeFor(o.ServiceUser())) + "\nexisting=" + b2s(o.UseExisting) + "\nconfirm=" + b2s(o.Confirm) + "\n"
	out, err := r.root(vars+accountCreateScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

const keyScript = `
home=$(getent passwd "$u" | cut -d: -f6)
[ -n "$home" ] && [ -d "$home" ] || { echo "no home directory for $u"; exit 1; }
grp=$(id -gn "$u")
umask 077
new=$(cat)
d="$home/.ssh"; f="$d/authorized_keys"
[ -d "$d" ] || install -d -m 700 -o "$u" -g "$grp" "$d"
# every line except the ones that carry the controller's key
t=$(mktemp "$d/.authorized_keys.XXXXXX")
if [ -f "$f" ]; then grep -vF -- "$blob" "$f" > "$t"; fi
echo "$new" >> "$t"
if [ -f "$f" ] && cmp -s "$t" "$f"; then rm -f "$t"; echo "the key is installed with its restrictions"; echo @@unchanged; exit 0; fi
chown "$u:$grp" "$t"; chmod 600 "$t"; mv -f "$t" "$f"
command -v restorecon >/dev/null 2>&1 && restorecon -R "$d" >/dev/null 2>&1
echo "installed the controller key in $f"; echo @@changed`

func stepKey(r *Runner, p Plan, o Options) (string, bool, error) {
	blob, _ := KeyBlob(o.ControllerKey)
	vars := "u=" + shq(o.ServiceUser()) + "\nblob=" + shq(blob) + "\n"
	line := AuthorizedKeyLine(o)
	out, err := r.root(vars+keyScript, line+"\n")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d + " (options: " + strings.SplitN(line, " ", 2)[0] + ")", ch, nil
}

const installFileScript = `
umask 077
d=$(dirname "$dest")
install -d -m "$dirmode" -o root -g root "$d" || exit 1
t=$(mktemp "$d/.virticle-tmp.XXXXXX") || exit 1
trap 'rm -f "$t"' EXIT
cat > "$t"
got=$(sha256sum "$t" | cut -d' ' -f1)
[ "$got" = "$sha" ] || { echo "the file arrived damaged"; exit 1; }
if [ -n "$check" ]; then sh -c "$check" sh "$t" >/dev/null 2>&1 || { echo "$checkmsg"; exit 1; }; fi
if [ -f "$dest" ] && [ ! -L "$dest" ] && cmp -s "$t" "$dest" && [ "$(stat -c '%U:%G %a' "$dest")" = "root:root $mode" ]; then echo "$what is current"; echo @@unchanged; exit 0; fi
chown root:root "$t"; chmod "$mode" "$t"
mv -f "$t" "$dest" || exit 1
command -v restorecon >/dev/null 2>&1 && restorecon "$dest" >/dev/null 2>&1
echo "installed $dest"; echo @@changed`

func installFile(r *Runner, what, dest, dirmode, mode, content, check, checkmsg string) (string, bool, error) {
	sum := sha256.Sum256([]byte(content))
	vars := "what=" + shq(what) + "\ndest=" + shq(dest) + "\ndirmode=" + dirmode + "\nmode=" + mode + "\nsha=" + hex.EncodeToString(sum[:]) + "\ncheck=" + shq(check) + "\ncheckmsg=" + shq(checkmsg) + "\n"
	out, err := r.root(vars+installFileScript, content)
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

// nodeBinary returns the node command for a machine name (`uname -m`). A
// variable so that tests do not need the built binaries.
var nodeBinary = nodebin.Binary

// removeOldScript removes the shell script of versions before the node
// command.
const removeOldScript = `
if [ -e ` + oldHelperPath + ` ] || [ -L ` + oldHelperPath + ` ]; then rm -f ` + oldHelperPath + `; echo "removed the old shell script"; echo @@changed; else echo @@unchanged; fi`

// stepNodeCommand uploads the node command for the node's architecture, installs it
// root:root 0755, removes the old shell script and checks that the program
// runs and speaks this controller's protocol.
func stepNodeCommand(r *Runner, p Plan, o Options) (string, bool, error) {
	bin, err := nodeBinary(p.Facts.Arch)
	if err != nil {
		return "", false, err
	}
	d, ch, err := installFile(r, "the node command", hostconn.NodeCmdPath, "755", "755", string(bin), "", "")
	if err != nil {
		return "", false, err
	}
	out, err := r.root(removeOldScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	if od, och := parseStep(out); och {
		d, ch = d+"; "+od, true
	}
	got, err := r.root(hostconn.NodeCmdPath+" version", "")
	if err != nil || strings.TrimSpace(got) != hostconn.NodeCmdVersion {
		return "", false, fmt.Errorf("the node command does not run on this host (expected protocol %s): %s", hostconn.NodeCmdVersion, firstLines(got, 3))
	}
	return d + " (version " + hostconn.NodeCmdVersion + ")", ch, nil
}

func stepSudoers(r *Runner, p Plan, o Options) (string, bool, error) {
	content := hostconn.SudoersLine(o.ServiceUser())
	what := "the sudo rule for the node command only"
	// A temporary name starting with a dot is ignored by sudo, so a rule that
	// does not parse is never active; visudo checks it before it is moved.
	d, ch, err := installFile(r, what, hostconn.SudoersPath, "755", "440", content, `visudo -cf "$1"`, "visudo rejected the sudo rule; nothing was installed")
	if err != nil {
		return d, ch, err
	}
	// The rule works only if the account really can run the node command.
	got, verr := r.root("runuser -u "+shq(o.ServiceUser())+" -- sudo -n "+hostconn.NodeCmdPath+" version 2>&1", "")
	if verr != nil || strings.TrimSpace(got) != hostconn.NodeCmdVersion {
		return "", false, fmt.Errorf("%s cannot run the node command through sudo: %s", o.ServiceUser(), firstLines(got, 3))
	}
	return d, ch, nil
}

// peerKeyScript creates the service account's own key pair for node to node
// transfers (the node command's peer-send uses it; the private half stays here).
const peerKeyScript = `
home=$(getent passwd "$u" | cut -d: -f6)
[ -n "$home" ] && [ -d "$home" ] || { echo "no home directory for $u"; exit 1; }
grp=$(id -gn "$u")
d="$home/.ssh"; k="$d/virticle_peer_ed25519"
if [ -f "$k" ] && [ -f "$k.pub" ] && [ ! -L "$k" ]; then echo "the node key exists ($k.pub)"; echo @@unchanged; exit 0; fi
command -v ssh-keygen >/dev/null 2>&1 || { echo "ssh-keygen is missing (install the OpenSSH client)"; exit 1; }
command -v runuser >/dev/null 2>&1 || { echo "runuser is missing (util-linux)"; exit 1; }
[ -d "$d" ] || install -d -m 700 -o "$u" -g "$grp" "$d"
rm -f "$k" "$k.pub"
runuser -u "$u" -- ssh-keygen -q -t ed25519 -N '' -C "virticle-peer@$(hostname)" -f "$k" || exit 1
command -v restorecon >/dev/null 2>&1 && restorecon -R "$d" >/dev/null 2>&1
echo "created the node key $k.pub"; echo @@changed`

func stepPeerKey(r *Runner, p Plan, o Options) (string, bool, error) {
	out, err := r.root("u="+shq(o.ServiceUser())+"\n"+peerKeyScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 6))
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

// --- unprepare ---

const unprepareScript = `
home=$(getent passwd "$u" | cut -d: -f6)
changed=0
if [ -n "$home" ] && [ -f "$home/.ssh/authorized_keys" ] && grep -qF -- "$blob" "$home/.ssh/authorized_keys"; then
  t=$(mktemp "$home/.ssh/.authorized_keys.XXXXXX"); grep -vF -- "$blob" "$home/.ssh/authorized_keys" > "$t"
  chown --reference="$home/.ssh/authorized_keys" "$t"; chmod 600 "$t"; mv -f "$t" "$home/.ssh/authorized_keys"
  echo "key removed"; changed=1
else echo "no controller key"; fi
if [ -n "$home" ] && [ -d "$home/.ssh" ]; then
  if [ -f "$home/.ssh/virticle_peer_ed25519" ] || [ -f "$home/.ssh/virticle_peer_known_hosts" ]; then rm -f "$home/.ssh/virticle_peer_ed25519" "$home/.ssh/virticle_peer_ed25519.pub" "$home/.ssh/virticle_peer_known_hosts"; echo "node key removed"; changed=1; fi
  for mark in "virticle-node peer-recv" "virticle-helper peer-recv"; do
    if [ -f "$home/.ssh/authorized_keys" ] && grep -qF -- "$mark" "$home/.ssh/authorized_keys"; then
      t=$(mktemp "$home/.ssh/.authorized_keys.XXXXXX"); grep -vF -- "$mark" "$home/.ssh/authorized_keys" > "$t"
      chown --reference="$home/.ssh/authorized_keys" "$t"; chmod 600 "$t"; mv -f "$t" "$home/.ssh/authorized_keys"
      echo "peer keys removed"; changed=1
    fi
  done
fi
if [ -f ` + sudoersPath + ` ] && head -c 200 ` + sudoersPath + ` | grep -q 'Written by virticle node prepare' && grep -q "^$u ALL=" ` + sudoersPath + `; then rm -f ` + sudoersPath + `; echo "sudo rule removed"; changed=1; else echo "no sudo rule for $u"; fi
if { [ -f ` + nodeCmdPath + ` ] || [ -f ` + oldHelperPath + ` ]; } && [ ! -f ` + sudoersPath + ` ]; then rm -f ` + nodeCmdPath + ` ` + oldHelperPath + `; systemctl disable virticle-net-boot.service >/dev/null 2>&1; rm -f /etc/systemd/system/virticle-net-boot.service; rmdir /usr/local/libexec 2>/dev/null; echo "node command removed"; changed=1; elif [ -f ` + nodeCmdPath + ` ] || [ -f ` + oldHelperPath + ` ]; then echo "node command kept, another account's sudo rule uses it"; else echo "no node command"; fi
if [ "$u" != root ] && getent passwd "$u" | grep -q "` + AccountComment + `"; then
  pkill -KILL -u "$u" 2>/dev/null; sleep 1
  userdel -r "$u" >/dev/null 2>&1; if id "$u" >/dev/null 2>&1; then echo "the account could not be removed (is the controller still connected as $u? remove the node there first)"; exit 1; fi
  echo "account removed"; changed=1
elif [ "$u" != root ] && id "$u" >/dev/null 2>&1; then echo "the account $u was kept: Virticle did not create it"; else echo "no service account"; fi
if [ "$changed" = 1 ]; then echo @@changed; else echo @@unchanged; fi`

// Unprepare removes what Prepare added for the service account: the key, the
// sudo rule, the node command (and the old shell script) and (when this tool created it) the account. Packages,
// VMs, pools and networks stay.
func (r *Runner) Unprepare(o Options) []Row {
	access := r.Access()
	rows := []Row{access}
	if access.Status == Error {
		return rows
	}
	if err := (Options{User: o.User, AllowRoot: o.AllowRoot, ControllerKey: o.ControllerKey}).Validate(); err != nil {
		return append(rows, Row{ID: "unprepare", Title: "Remove", Status: Error, Detail: err.Error()})
	}
	// Without a name, remove the account of ours that exists (virticle-node,
	// else virticle from an earlier version); never an unmarked one.
	o, _, err := r.resolveAccount(o)
	if err != nil {
		return append(rows, Row{ID: "unprepare", Title: "Remove", Status: Error, Detail: err.Error()})
	}
	blob, _ := KeyBlob(o.ControllerKey)
	vars := "u=" + shq(o.ServiceUser()) + "\nblob=" + shq(blob) + "\n"
	out, err := r.root(vars+unprepareScript, "")
	d, changed := parseStep(r.scrub(out))
	if err != nil {
		return append(rows, Row{ID: "unprepare", Title: "Remove controller access", Status: Error, Detail: d})
	}
	for _, part := range strings.Split(d, "; ") {
		if part != "" {
			rows = append(rows, Row{ID: "unprepare", Title: titleOf(part), Status: OK, Detail: part})
		}
	}
	// The firewall service that prepare added. The script prints nothing when
	// it cannot run, which then adds no row.
	if fout, ferr := r.root(FirewallScript(FirewallSpec{Service: NodeFirewallService}), ""); ferr == nil && strings.TrimSpace(fout) != "" {
		frow, fch := ApplyFirewall(func(string) (string, error) { return fout, nil }, FirewallSpec{Service: NodeFirewallService})
		changed = changed || fch
		rows = append(rows, frow)
	}
	if changed {
		rows = append(rows, Row{ID: "summary", Title: "Result", Status: Info, Detail: "Controller access removed; packages, VMs, pools and networks were left alone"})
	} else {
		rows = append(rows, Row{ID: "summary", Title: "Result", Status: Info, Detail: "Nothing changed; the host had nothing of this tool's to remove"})
	}
	return rows
}

func titleOf(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "node key"), strings.Contains(s, "peer key"):
		return "Node key"
	case strings.Contains(s, "key"):
		return "SSH key"
	case strings.Contains(s, "node command"), strings.Contains(s, "shell script"):
		return "Node command"
	case strings.Contains(s, "sudo"):
		return "Sudo rule"
	}
	return "Service account"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
