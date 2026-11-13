package nodecmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"virticle/internal/nodeproto"
)

// Package updates, run by the controller's rolling update (internal/api/rolling.go).
//
//	update-start     start the package update in a unit of its own (it must outlive the SSH session that asked)
//	update-run       the update itself; started by update-start, never by hand
//	update-status    print the state of the last update as one JSON object (nodeproto.UpdateStatus)
//	node-reboot      restart the node in a few seconds, from a unit of its own so that the answer still reaches the caller
//	selfcheck [repair]
//	                 check what "virticle node prepare" set up and the things a package update can disturb; with
//	                 "repair" put right what can be put right. Prints "STATUS<TAB>ID<TAB>DETAIL" lines.
//
// Everything is done with the distribution's own tool and without questions: dnf -y, and apt-get -y with the
// old configuration files kept (a file that was changed on the node stays as it is).
func init() {
	Register("update-start", verbUpdateStart)
	Register("update-run", verbUpdateRun)
	Register("update-status", verbUpdateStatus)
	Register("node-reboot", verbNodeReboot)
	Register("selfcheck", verbSelfcheck)
}

const (
	updateUnit = "virticle-update"
	// updateMaxTime is how long a package update may take before it is stopped.
	updateMaxTime = 90 * time.Minute
)

func (c *Ctx) updDir() string        { return c.hostPath("var/lib/virticle-update") }
func (c *Ctx) updStatusPath() string { return filepath.Join(c.updDir(), "status.json") }
func (c *Ctx) updLogPath() string    { return filepath.Join(c.updDir(), "update.log") }

func (c *Ctx) updSelf() string {
	if c.Cfg.Self != "" {
		return c.Cfg.Self
	}
	return NodePath
}

func (c *Ctx) updUnitActive() bool {
	_, err := c.netRun("systemctl", "is-active", "--quiet", updateUnit+".service")
	return err == nil
}

func (c *Ctx) writeUpdStatus(st *nodeproto.UpdateStatus) error {
	if err := os.MkdirAll(c.updDir(), 0o700); err != nil {
		return err
	}
	return c.writeJSON(c.updStatusPath(), st)
}

func (c *Ctx) readUpdStatus() nodeproto.UpdateStatus {
	var st nodeproto.UpdateStatus
	b, err := os.ReadFile(c.updStatusPath())
	if err != nil || json.Unmarshal(b, &st) != nil || st.State == "" {
		return nodeproto.UpdateStatus{State: nodeproto.UpdateNone}
	}
	return st
}

func verbUpdateStart(c *Ctx) error {
	if err := c.Need(0, 0, "update-start"); err != nil {
		return err
	}
	if c.updUnitActive() {
		return Fail("a package update is already running on this node")
	}
	if err := os.MkdirAll(c.updDir(), 0o700); err != nil {
		return Fail("cannot create %s: %v", c.updDir(), err)
	}
	st := &nodeproto.UpdateStatus{State: nodeproto.UpdateRunning, Started: time.Now().Unix(), Phase: "starting"}
	if err := c.writeUpdStatus(st); err != nil {
		return Fail("cannot write the update state: %v", err)
	}
	_ = os.Remove(c.updLogPath())
	if _, err := c.netRun("systemd-run", "--quiet", "--collect", "--unit="+updateUnit,
		"--description=virticle package update", c.updSelf(), "update-run"); err != nil {
		st.State, st.Error = nodeproto.UpdateFailed, "could not start the update: "+err.Error()
		st.Finished = time.Now().Unix()
		_ = c.writeUpdStatus(st)
		return Fail("%s", st.Error)
	}
	fmt.Fprintln(c.Stdout, "started")
	return nil
}

func verbUpdateStatus(c *Ctx) error {
	if err := c.Need(0, 0, "update-status"); err != nil {
		return err
	}
	st := c.readUpdStatus()
	if st.State == nodeproto.UpdateRunning && !c.updUnitActive() {
		st.State, st.Finished = nodeproto.UpdateFailed, time.Now().Unix()
		st.Error = "the update stopped before it finished (the node may have restarted while it ran); see " + c.updLogPath() + " on the node"
		_ = c.writeUpdStatus(&st)
	}
	return json.NewEncoder(c.Stdout).Encode(st)
}

func verbNodeReboot(c *Ctx) error {
	if err := c.Need(0, 0, "node-reboot"); err != nil {
		return err
	}
	if c.updUnitActive() {
		return Fail("a package update is still running on this node; it is not restarted")
	}
	if _, err := c.netRun("systemd-run", "--quiet", "--collect", "--unit=virticle-reboot",
		"--description=virticle node restart", "--on-active=5s", "systemctl", "reboot"); err != nil {
		return Fail("could not schedule the restart: %v", err)
	}
	fmt.Fprintln(c.Stdout, "restarting in a few seconds")
	return nil
}

// --- the update ---

// tailBuf keeps the last bytes written to it.
type tailBuf struct{ b []byte }

const tailMax = 6000

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = t.b[len(t.b)-tailMax:]
	}
	return len(p), nil
}

func (t *tailBuf) lines(n int) string {
	l := strings.Split(strings.TrimSpace(string(t.b)), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func rcOf(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ExitStatus(err)
}

// updRun runs a package tool. Its output goes to w; the standard output is also returned. A hook in
// Config.Net.Run replaces the tool in tests.
func (c *Ctx) updRun(w io.Writer, env []string, argv ...string) (stdout string, rc int) {
	fmt.Fprintf(w, "$ %s\n", strings.Join(argv, " "))
	if h := c.netHooks(); h.Run != nil {
		out, err := h.Run(argv)
		io.WriteString(w, out)
		return out, rcOf(err)
	}
	cmd, err := c.Command(argv[0], argv[1:]...)
	if err != nil {
		fmt.Fprintln(w, err)
		return "", 127
	}
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(&out, w)
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return out.String(), rcOf(err)
	}
	return out.String(), 0
}

var aptEnv = []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_SUSPEND=1", "NEEDRESTART_MODE=l", "APT_LISTCHANGES_FRONTEND=none", "UCF_FORCE_CONFFOLD=1"}

func (c *Ctx) osFamily() string {
	b, _ := os.ReadFile(c.hostPath("etc/os-release"))
	vals := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			vals[k] = strings.Trim(v, `"'`)
		}
	}
	for _, id := range strings.Fields(strings.ToLower(vals["ID"] + " " + vals["ID_LIKE"])) {
		switch id {
		case "debian", "ubuntu":
			return "apt"
		case "rhel", "fedora", "centos", "rocky", "almalinux":
			return "dnf"
		}
	}
	return ""
}

func verbUpdateRun(c *Ctx) error {
	if err := c.Need(0, 0, "update-run"); err != nil {
		return err
	}
	if err := os.MkdirAll(c.updDir(), 0o700); err != nil {
		return Fail("cannot create %s: %v", c.updDir(), err)
	}
	st := c.readUpdStatus()
	if st.State != nodeproto.UpdateRunning {
		st = nodeproto.UpdateStatus{State: nodeproto.UpdateRunning, Started: time.Now().Unix()}
	}
	logf, err := os.OpenFile(c.updLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Fail("cannot write the update log: %v", err)
	}
	defer logf.Close()
	tail := &tailBuf{}
	w := io.MultiWriter(logf, tail)
	phase := func(p string) {
		st.Phase = p
		_ = c.writeUpdStatus(&st)
	}
	fail := func(msg string) error {
		st.State, st.Error, st.Finished, st.Phase = nodeproto.UpdateFailed, msg, time.Now().Unix(), ""
		st.Log = tail.lines(14)
		_ = c.writeUpdStatus(&st)
		return Fail("%s", msg)
	}
	st.Family = c.osFamily()
	switch st.Family {
	case "dnf", "apt":
	default:
		return fail("this system is not based on dnf or apt; updates are not supported on it")
	}

	// 1. refresh the package lists and read what is pending
	phase("reading the package lists")
	var before []nodeproto.Pending
	if st.Family == "dnf" {
		out, rc := c.updRun(w, nil, "dnf", "-q", "check-update", "--refresh")
		if rc != 0 && rc != 100 {
			return fail("dnf cannot read its repositories: " + lastLine(tail.lines(4)) + ". Fix the repository on the node (dnf repolist), then update again")
		}
		before, _ = nodeproto.ParseDnf(out + fmt.Sprintf("\n__rc=%d", rc))
	} else {
		out, rc := c.updRun(w, aptEnv, "apt-get", "update", "-o", "Acquire::Retries=2")
		if rc != 0 || aptUpdateFailed(out+tail.lines(60)) {
			return fail("apt could not refresh the package lists: " + aptFailure(tail.lines(60)) + ". Fix the repository on the node, then update again")
		}
		lst, _ := c.updRun(w, aptEnv, "apt", "list", "--upgradable")
		before = nodeproto.ParseApt(lst)
	}
	st.Before = nodeproto.Names(before)
	_ = c.writeUpdStatus(&st)

	// 2. install
	if len(before) > 0 {
		phase(fmt.Sprintf("installing %d updates", len(before)))
		var rc int
		if st.Family == "dnf" {
			_, rc = c.updRun(w, nil, "dnf", "-y", "upgrade")
		} else {
			_, rc = c.updRun(w, aptEnv, "apt-get", "-y", "--with-new-pkgs",
				"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
				"-o", "Dpkg::Lock::Timeout=300", "upgrade")
		}
		if rc != 0 {
			return fail("the package tool failed (status " + fmt.Sprint(rc) + "): " + toolFailure(tail.lines(40)))
		}
	}

	// 3. what is left, and whether the node must restart
	phase("checking the result")
	var after []nodeproto.Pending
	if st.Family == "dnf" {
		out, rc := c.updRun(w, nil, "dnf", "-q", "check-update")
		after, _ = nodeproto.ParseDnf(out + fmt.Sprintf("\n__rc=%d", rc))
	} else {
		lst, _ := c.updRun(w, aptEnv, "apt", "list", "--upgradable")
		after = nodeproto.ParseApt(lst)
	}
	st.HeldBack = nodeproto.Names(after)
	left := map[string]bool{}
	for _, n := range st.HeldBack {
		left[n] = true
	}
	st.Updated = nil
	for _, n := range st.Before {
		if !left[n] {
			st.Updated = append(st.Updated, n)
		}
	}
	st.RebootNeeded, st.RebootWhy = c.rebootVerdict(st.Family, st.Updated)
	st.State, st.Finished, st.Phase, st.Log = nodeproto.UpdateOK, time.Now().Unix(), "", tail.lines(8)
	return c.writeUpdStatus(&st)
}

// toolFailure picks the lines of a package tool's output that say what went wrong (an error, a failed script, a
// problem). When there are none, the last line.
func toolFailure(out string) string {
	var hit []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		low := strings.ToLower(t)
		if t == "" || strings.HasPrefix(low, "failed:") {
			continue
		}
		if strings.HasPrefix(low, "error") || strings.HasPrefix(low, "e:") || strings.Contains(low, "scriptlet") || strings.Contains(low, "problem") || strings.Contains(low, "failed") {
			hit = append(hit, t)
		}
	}
	if len(hit) == 0 {
		return lastLine(out)
	}
	if len(hit) > 3 {
		hit = hit[len(hit)-3:]
	}
	return strings.Join(hit, " | ")
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l[len(l)-1])
}

// aptUpdateFailed is true when apt-get update reported a source it could not read (it exits with 0 for that).
func aptUpdateFailed(out string) bool {
	if len(aptErrLines(out)) > 0 {
		return true
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "E:") || strings.Contains(l, "Failed to fetch") || strings.Contains(l, "is not signed") {
			return true
		}
	}
	return false
}

// aptErrLines are the "Err:N ..." lines of apt-get update that stand. A download that apt first reported as "Err:"
// while it tried the next compression format of an optional index (a repository without Translation files) and then
// gave up on with a later "Ign:N" is not a failure: apt-get update goes on and exits with 0.
func aptErrLines(out string) []string {
	var errs []string
	ids := map[string]bool{} // ids of the lines in errs
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "Err:"):
			id := aptLineID(l)
			if !ids[id] || !contains(errs, l) {
				errs = append(errs, l)
			}
			ids[id] = true
		case strings.HasPrefix(l, "Ign:"):
			id := aptLineID(l)
			if ids[id] {
				kept := errs[:0]
				for _, e := range errs {
					if aptLineID(e) != id {
						kept = append(kept, e)
					}
				}
				errs = kept
				delete(ids, id)
			}
		}
	}
	return errs
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// aptLineID is the number apt gives a download in "Err:10 http://... " and "Ign:10 http://...".
func aptLineID(l string) string {
	_, rest, _ := strings.Cut(l, ":")
	id, _, _ := strings.Cut(rest, " ")
	return id
}

func aptFailure(out string) string {
	bad := aptErrLines(out)
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if (strings.HasPrefix(l, "E:") || strings.HasPrefix(l, "W: Failed to fetch")) && !contains(bad, l) {
			bad = append(bad, l)
		}
	}
	if len(bad) == 0 {
		return lastLine(out)
	}
	if len(bad) > 3 {
		bad = bad[:3]
	}
	return strings.Join(bad, " / ")
}

// rebootVerdict decides whether the node must restart after the update: the system's own flag, a kernel that is newer
// than the running one, or an updated package of the kinds RebootPackage lists.
func (c *Ctx) rebootVerdict(family string, updated []string) (bool, string) {
	var why []string
	running, _ := c.netRun("uname", "-r")
	running = strings.TrimSpace(running)
	switch family {
	case "dnf":
		if c.HasTool("needs-restarting") {
			if _, err := c.netRun("needs-restarting", "-r"); err != nil {
				why = append(why, "core libraries or services were updated")
			}
		}
		out, _ := c.netRun("rpm", "-q", "kernel-core", "kernel", "--qf", "%{VERSION}-%{RELEASE}.%{ARCH}\n")
		newest := ""
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			if l != "" && !strings.Contains(l, "not installed") && (newest == "" || nodeproto.KernelNewer(newest, l)) {
				newest = l
			}
		}
		if nodeproto.KernelNewer(running, newest) {
			why = append(why, "a newer kernel ("+newest+") is installed")
		}
	case "apt":
		if _, err := os.Stat(c.hostPath("var/run/reboot-required")); err == nil {
			why = append(why, "the system asks for a restart (/var/run/reboot-required)")
		}
		out, _ := c.netRun("sh", "-c", "ls /boot/vmlinuz-* 2>/dev/null | sed 's#.*/vmlinuz-##'")
		newest := ""
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			if l != "" && (newest == "" || nodeproto.KernelNewer(newest, l)) {
				newest = l
			}
		}
		if nodeproto.KernelNewer(running, newest) {
			why = append(why, "a newer kernel ("+newest+") is installed")
		}
	}
	if len(why) == 0 {
		why = nodeproto.RebootReasons(updated)
		for i := range why {
			why[i] = "updated: " + why[i]
		}
	}
	return len(why) > 0, strings.Join(why, "; ")
}

// --- selfcheck ---

// The checks look at what prepare sets up and what a package update can disturb: libvirt itself, its services, the
// order of the storage daemon before the VM daemon, the SELinux setting for NFS storage, the owner of the swtpm
// files, and the firewall service for migrations. "repair" puts right what can be put right without asking. The
// scripts are the ones of "virticle node prepare" (internal/prepare/steps.go), cut down to one result line each.
// The sudo rule needs no check: this command could not run through it otherwise.
var selfchecks = []struct{ id, script string }{
	{"libvirt", `
if timeout 30 virsh -c qemu:///system version >/dev/null 2>&1; then printf 'ok\tlibvirt\tlibvirt answers\n'
else printf 'problem\tlibvirt\tlibvirt does not answer; look at "systemctl status libvirtd" or "systemctl status virtqemud" on the node\n'; fi`},
	{"services", `
mode=$1
if systemctl cat libvirtd.service >/dev/null 2>&1; then units="libvirtd.service"
else units="virtqemud.socket virtnetworkd.socket virtstoraged.socket virtnodedevd.socket virtsecretd.socket virtproxyd.socket virtlogd.socket"; fi
bad=""
for u in $units; do systemctl is-enabled --quiet "$u" 2>/dev/null || bad="$bad $u"; done
if [ -z "$bad" ]; then printf 'ok\tservices\tthe libvirt services are enabled\n'; exit 0; fi
if [ "$mode" = repair ] && systemctl enable --now $units >/dev/null 2>&1; then printf 'fixed\tservices\tenabled again:%s\n' "$bad"; exit 0; fi
printf 'problem\tservices\tnot enabled:%s. Run: systemctl enable --now%s\n' "$bad" "$bad"`},
	{"storage-order", `
mode=$1
systemctl cat virtstoraged.service >/dev/null 2>&1 && systemctl cat virtqemud.service >/dev/null 2>&1 || { printf 'ok\tstorage-order\tnot needed on this node\n'; exit 0; }
f=/etc/systemd/system/virtqemud.service.d/10-virticle-storage.conf
want=$(printf '[Unit]\nWants=virtstoraged.service\nAfter=virtstoraged.service')
if [ "$(cat "$f" 2>/dev/null)" = "$want" ]; then printf 'ok\tstorage-order\tthe storage daemon starts before the VM daemon\n'; exit 0; fi
if [ "$mode" = repair ] && mkdir -p /etc/systemd/system/virtqemud.service.d && printf '%s\n' "$want" > "$f" && systemctl daemon-reload; then printf 'fixed\tstorage-order\twrote the start order of the libvirt daemons again\n'; exit 0; fi
printf 'problem\tstorage-order\t%s is missing; run "virticle node prepare" for this node\n' "$f"`},
	{"selinux", `
mode=$1
command -v getenforce >/dev/null 2>&1 && command -v setsebool >/dev/null 2>&1 || { printf 'ok\tselinux\tno SELinux on this node\n'; exit 0; }
[ "$(getenforce 2>/dev/null)" = Disabled ] && { printf 'ok\tselinux\tSELinux is off\n'; exit 0; }
getsebool virt_use_nfs >/dev/null 2>&1 || { printf 'ok\tselinux\tno virt_use_nfs setting on this node\n'; exit 0; }
if [ "$(getsebool virt_use_nfs | awk '{print $NF}')" = on ]; then printf 'ok\tselinux\tvirt_use_nfs is on\n'; exit 0; fi
if [ "$mode" = repair ] && setsebool -P virt_use_nfs on; then printf 'fixed\tselinux\tturned the SELinux setting virt_use_nfs on again (VMs need it for NFS storage)\n'; exit 0; fi
printf 'problem\tselinux\tthe SELinux setting virt_use_nfs is off; run: setsebool -P virt_use_nfs on\n'`},
	{"swtpm", `
mode=$1
d=/var/lib/swtpm-localca
id -u tss >/dev/null 2>&1 && [ -d "$d" ] || { printf 'ok\tswtpm\tnot used on this node\n'; exit 0; }
g=$(id -gn tss)
bad=$(find "$d" ! -path "$d" \( -not -user tss -o -not -group "$g" \) 2>/dev/null | wc -l)
if [ "$(stat -c %U "$d")" != tss ]; then bad=$((bad+1)); fi
if [ "$bad" = 0 ]; then printf 'ok\tswtpm\tthe swtpm files belong to tss\n'; exit 0; fi
if [ "$mode" = repair ] && chown tss "$d" && find "$d" ! -path "$d" -exec chown tss:"$g" {} +; then printf 'fixed\tswtpm\tgave the swtpm files back to tss (TPM VMs could not start)\n'; exit 0; fi
printf 'problem\tswtpm\tthe swtpm files in %s do not belong to tss; run: chown -R tss:%s %s\n' "$d" "$g" "$d"`},
	{"firewall", `
mode=$1
svc=virticle-node
if command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
  if ! firewall-cmd --permanent --get-services 2>/dev/null | tr ' ' '\n' | grep -qx "$svc"; then
    printf 'problem\tfirewall\tthe firewalld service %s is gone; run "virticle node prepare" for this node\n' "$svc"; exit 0; fi
  if firewall-cmd --permanent --query-service="$svc" >/dev/null 2>&1 && firewall-cmd --query-service="$svc" >/dev/null 2>&1; then
    printf 'ok\tfirewall\tthe migration ports are open (firewalld)\n'; exit 0; fi
  if [ "$mode" = repair ] && firewall-cmd --permanent --add-service="$svc" >/dev/null 2>&1 && firewall-cmd --add-service="$svc" >/dev/null 2>&1; then
    printf 'fixed\tfirewall\topened the migration ports again (firewalld)\n'; exit 0; fi
  printf 'problem\tfirewall\tthe migration ports are closed; run: firewall-cmd --permanent --add-service=%s && firewall-cmd --reload\n' "$svc"; exit 0
fi
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi '^Status: active'; then
  if ufw show added 2>/dev/null | grep -qF "comment '$svc'"; then printf 'ok\tfirewall\tthe migration ports are open (ufw)\n'
  else printf 'problem\tfirewall\tthe ufw rule for migrations is gone; run "virticle node prepare" for this node\n'; fi
  exit 0
fi
printf 'ok\tfirewall\tno firewall is active\n'`},
}

func verbSelfcheck(c *Ctx) error {
	if err := c.Need(0, 1, "selfcheck [repair]"); err != nil {
		return err
	}
	mode := "check"
	if len(c.Args) == 1 {
		if c.Args[0] != "repair" {
			return Die("usage: selfcheck [repair]")
		}
		mode = "repair"
	}
	for _, ck := range selfchecks {
		out, err := c.netRun("sh", "-c", ck.script, "sh", mode)
		rows := nodeproto.ParseCheckRows(out)
		if len(rows) == 0 {
			msg := "the check did not run"
			if err != nil {
				msg += ": " + err.Error()
			}
			rows = []nodeproto.CheckRow{{Status: nodeproto.CheckProblem, ID: ck.id, Detail: msg}}
		}
		for _, r := range rows {
			fmt.Fprintf(c.Stdout, "%s\t%s\t%s\n", r.Status, r.ID, strings.ReplaceAll(r.Detail, "\n", " "))
		}
	}
	return nil
}
