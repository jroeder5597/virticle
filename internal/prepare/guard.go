package prepare

import (
	"fmt"
	"strings"

	"virticle/internal/nodeproto"
)

// The start guard: a libvirt QEMU hook in the hook directory (qemu.d), so an administrator's own
// /etc/libvirt/hooks/qemu is never touched. It calls the node command, which asks the standby of a replicated VM
// whether it took the VM over (see internal/nodecmd/verbs_guard.go).

// guardReloadScript makes the running libvirt daemon look for hooks again: it reads the hook directory at
// start and on SIGHUP only. A reload does not touch running VMs.
const guardReloadScript = `
for u in virtqemud libvirtd; do
  if systemctl is-active --quiet "$u" 2>/dev/null; then systemctl reload "$u" >/dev/null 2>&1 || systemctl kill -s HUP "$u" >/dev/null 2>&1; fi
done
true`

// guardSelftestScript starts a probe VM that the hook refuses on purpose, and prints what the hook answered. The
// probe VM has no devices and never runs.
const guardSelftestScript = `
t=$(mktemp) || exit 0
trap 'rm -f "$t"' EXIT
cat > "$t" <<'EOF'
<domain type='kvm'><name>` + nodeproto.SelftestVM + `</name><memory unit='MiB'>16</memory><vcpu>1</vcpu><os><type>hvm</type></os></domain>
EOF
out=$(timeout 90 virsh -c qemu:///system create "$t" 2>&1); rc=$?
virsh -c qemu:///system destroy ` + nodeproto.SelftestVM + ` >/dev/null 2>&1
line=$(echo "$out" | grep -o '` + nodeproto.SelftestMark + `.*' | head -n 1)
if [ -n "$line" ]; then echo "result: $line"; else echo "nohook rc=$rc: $(echo "$out" | tr '\n' ' ' | head -c 300)"; fi`

// guardRemoveScript removes the hook file when this tool wrote it, and its directory when it is empty.
const guardRemoveScript = `
h=` + nodeproto.HookPath + `
if [ -f "$h" ] && grep -q '` + nodeproto.HookMark + `' "$h"; then
  rm -f "$h"; rmdir "$(dirname "$h")" 2>/dev/null
` + "  " + `echo "start guard removed"; echo @@changed
else echo @@unchanged; fi
` + appArmorRemoveScript

// appArmorRemoveScript removes the AppArmor block of the start guard and loads libvirt's profile again. It prints
// nothing and never fails.
const appArmorRemoveScript = `
l=` + nodeproto.AppArmorLocal + `
if [ -f "$l" ] && grep -qF '` + nodeproto.AppArmorBegin + `' "$l"; then
  sed -i '\%` + nodeproto.AppArmorBegin + `%,\%` + nodeproto.AppArmorEnd + `%d' "$l"
  apparmor_parser -r ` + nodeproto.AppArmorMain + ` >/dev/null 2>&1
fi
true`

// guardAppArmorScript adds the AppArmor block to libvirt's local profile rules when AppArmor is active and the
// profile includes that file (Debian, Ubuntu). The variable a is the account whose home holds the node key. A block
// that does not load is taken out again. It prints @@changed or @@unchanged.
const guardAppArmorScript = `
main=` + nodeproto.AppArmorMain + `
loc=` + nodeproto.AppArmorLocal + `
if [ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)" != Y ] || [ ! -f "$main" ] || [ ! -d "$(dirname "$loc")" ] ||
   ! command -v apparmor_parser >/dev/null 2>&1 || ! grep -q 'local/usr.sbin.libvirtd' "$main"; then echo @@unchanged; exit 0; fi
home=$(getent passwd "$a" | cut -d: -f6)
case "$home" in /*) ;; *) echo @@unchanged; exit 0 ;; esac
case "$home" in *[!A-Za-z0-9/._-]*) echo @@unchanged; exit 0 ;; esac
new=$(mktemp) || exit 1
old=$(mktemp) || exit 1
bak=$(mktemp) || exit 1
trap 'rm -f "$new" "$old" "$bak"' EXIT
sed "s#` + nodeproto.AppArmorHome + `#$home#" > "$new" <<'EOF'
` + nodeproto.AppArmorBlock + `EOF
if [ -f "$loc" ]; then sed -n '\%` + nodeproto.AppArmorBegin + `%,\%` + nodeproto.AppArmorEnd + `%p' "$loc" > "$old"; fi
if cmp -s "$new" "$old"; then echo @@unchanged; exit 0; fi
if [ -f "$loc" ]; then cp -p "$loc" "$bak"; sed -i '\%` + nodeproto.AppArmorBegin + `%,\%` + nodeproto.AppArmorEnd + `%d' "$loc"; else : > "$bak"; fi
cat "$new" >> "$loc"
if ! out=$(apparmor_parser -r "$main" 2>&1); then
  cat "$bak" > "$loc"; apparmor_parser -r "$main" >/dev/null 2>&1
  echo "apparmor_parser failed: $(echo "$out" | head -n 2 | tr '\n' ' ')"; exit 1
fi
echo "AppArmor rule added for the start guard"; echo @@changed`

// stepGuard installs the hook, makes libvirt read it, and proves with a probe VM that libvirt runs it and what
// the hook may do there (run programs, read the node key, use the network).
func stepGuard(r *Runner, p Plan, o Options) (string, bool, error) {
	f := p.Facts
	if f.Virsh != "" && f.LibvirtVersion != "" && libvirtMajor(f.LibvirtVersion) < guardMinLibvirt {
		return "", false, &statusError{Warning, "libvirt " + f.LibvirtVersion + " has no hook directory, so the start guard is not installed"}
	}
	if f.GuardSHA != "" && !f.GuardOurs {
		return "", false, &statusError{Warning, nodeproto.HookPath + " exists and was not written by Virticle; it is left alone, so there is no start guard"}
	}
	d, ch, err := installFile(r, "the start guard", nodeproto.HookPath, "755", "755", nodeproto.HookScript(o.ServiceUser()), "", "")
	if err != nil {
		return "", false, err
	}
	// Debian and Ubuntu: libvirtd's AppArmor profile does not let the hook run the node command.
	aa, aach, err := installAppArmor(r, o)
	if err != nil {
		return "", false, err
	}
	if aach {
		d, ch = d+"; "+aa, true
	}
	reloaded := false
	if ch {
		if out, err := r.root(guardReloadScript, ""); err != nil {
			return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 4))
		}
		reloaded = true
	}
	// The probe runs every time: a hook that is in place but that libvirt does not run (an interrupted earlier
	// prepare, a changed profile) must show.
	out, err := r.root(guardSelftestScript, "")
	if err != nil {
		return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 4))
	}
	if !reloaded && strings.HasPrefix(strings.TrimSpace(out), "nohook rc=0") {
		// Libvirt may have started before the hook existed.
		if rout, err := r.root(guardReloadScript, ""); err != nil {
			return "", false, fmt.Errorf("%w: %s", err, firstLines(rout, 4))
		}
		if out, err = r.root(guardSelftestScript, ""); err != nil {
			return "", false, fmt.Errorf("%w: %s", err, firstLines(out, 4))
		}
		ch = true
	}
	return guardVerdict(r, d, out, ch)
}

// installAppArmor adds the AppArmor rules the hook needs; changed is false when there was nothing to do.
func installAppArmor(r *Runner, o Options) (string, bool, error) {
	out, err := r.root("a="+shq(o.ServiceUser())+"\n"+guardAppArmorScript, "")
	if err != nil {
		return "", false, &statusError{Warning, "The start guard is installed, but its AppArmor rule could not be added (" + firstLines(out, 2) + "). AppArmor may keep the guard from asking the standby, and VMs then start as before"}
	}
	d, ch := parseStep(out)
	return d, ch, nil
}

// guardVerdict turns the probe's output into the step's result.
func guardVerdict(r *Runner, installed, out string, changed bool) (string, bool, error) {
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "result: ") {
		var bad []string
		for _, w := range strings.Fields(strings.TrimPrefix(out, "result: "+nodeproto.SelftestMark)) {
			k, v, _ := strings.Cut(w, "=")
			if v != "ok" && !(k == "key" && v == "missing") {
				bad = append(bad, k+" "+v)
			}
		}
		if len(bad) > 0 {
			return "", true, &statusError{Warning, "The start guard is installed and libvirt runs it, but on this host it cannot do everything it needs (" + strings.Join(bad, ", ") + "). A security module (SELinux or AppArmor) may forbid it. The guard then cannot ask the standby, and VMs start as before"}
		}
		return installed + "; a test start showed that libvirt runs it and that it may use ssh, the node key and the network", changed, nil
	}
	if strings.HasPrefix(out, "nohook rc=0") {
		return "", true, &statusError{Warning, "The start guard is installed, but a test start did not reach it: libvirt did not run it, or a security module (AppArmor, SELinux) did not let it run the node command. Restart libvirt (virtqemud or libvirtd) when it suits you and run prepare again; if this stays, look at the security module's log (journalctl -k)"}
	}
	// libvirt failed to run the hook: it must not stay, or it could keep every VM from starting.
	if rout, err := r.root(guardRemoveScript+"\n"+guardReloadScript, ""); err != nil {
		return "", true, fmt.Errorf("the start guard fails here (%s) and could not be removed: %w: %s", out, err, firstLines(rout, 3))
	}
	return "", true, &statusError{Warning, "libvirt could not run the start guard on this host (" + out + "), so it was removed again and VMs start as before"}
}
