// Package prepare makes a Linux host ready to be managed by the controller:
// libvirt and QEMU installed, a dedicated service account with a restricted
// SSH key, and the node command through which that account may act as root,
// and nothing else. It runs over an administrator's SSH connection that exists for
// one run only (the credential is never stored).
//
// Everything here is pure Go around shell snippets: Evaluate turns a host's
// facts into check rows, the step functions run snippets, and the tests cover
// both without a host.
package prepare

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"virticle/internal/nodeproto"
	"virticle/internal/snapsched"
)

// Row statuses, the same words the connection test uses.
const (
	OK      = "ok"
	Warning = "warning"
	Error   = "error"
	Info    = "info"
	Skipped = "skipped" // an earlier step failed, so this one did not run
)

// Row is one line of a result: a check or a step.
type Row struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Default locations on a node.
const (
	DefaultPoolPath = "/var/lib/libvirt/images"
	// DefaultUser is the node's service account. It is deliberately not
	// "virticle": that is the account of the controller package, which
	// prepare never touches.
	DefaultUser = "virticle-node"
	DefaultHome = "/var/lib/virticle-node"
	// LegacyUser is the account name earlier versions used on nodes. A
	// LegacyUser account that carries AccountComment is kept and reused.
	LegacyUser = "virticle"
	// AccountComment marks an account this tool created; unprepare removes
	// only accounts that carry it.
	AccountComment = "virticle service account"
)

// HomeFor is the home directory of a service account that prepare creates.
func HomeFor(user string) string { return "/var/lib/" + user }

// FactsScript prints key=value lines describing the host. It changes nothing
// and always succeeds. It runs as root (or through sudo) so that /dev/kvm,
// libvirt and other users' files can be inspected.
const FactsScript = `
os() { ( . /etc/os-release 2>/dev/null; eval "echo \"\$$1\"" ); }
echo "uid=$(id -u)"
echo "arch=$(uname -m)"
echo "os_id=$(os ID)"
echo "os_version=$(os VERSION_ID)"
echo "os_like=$(os ID_LIKE)"
echo "os_name=$(os PRETTY_NAME)"
echo "kernel=$(uname -r)"
echo "cpu_virt=$(grep -m1 -o -w -E 'vmx|svm' /proc/cpuinfo 2>/dev/null | head -n 1)"
echo "hypervisor=$(grep -m1 -o -w hypervisor /proc/cpuinfo 2>/dev/null | head -n 1)"
if [ -e /dev/kvm ]; then echo "kvm_dev=$(stat -c '%a %U:%G' /dev/kvm 2>/dev/null)"; else echo "kvm_dev="; fi
echo "nested=$(cat /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested 2>/dev/null | head -n 1)"
echo "iommu_groups=$(ls /sys/kernel/iommu_groups 2>/dev/null | wc -l)"
echo "iommu_cmdline=$(grep -o -w -E 'intel_iommu=on|amd_iommu=on|iommu=pt' /proc/cmdline 2>/dev/null | tr '\n' ' ')"
p=` + DefaultPoolPath + `; while [ ! -d "$p" ] && [ "$p" != / ]; do p=$(dirname "$p"); done
echo "free_path=$p"
echo "free_bytes=$(df -P -B1 "$p" 2>/dev/null | awk 'NR==2{print $4}')"
echo "selinux=$(getenforce 2>/dev/null)"
echo "apparmor=$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)"
echo "timesync=$(timedatectl show -p NTPSynchronized --value 2>/dev/null)"
echo "virsh=$(command -v virsh 2>/dev/null)"
if command -v virsh >/dev/null 2>&1; then
  echo "libvirt_version=$(virsh --version 2>/dev/null | head -n 1)"
  echo "libvirt_units=$(for u in libvirtd virtqemud libvirtd.socket virtqemud.socket virtproxyd.socket; do systemctl is-active "$u" 2>/dev/null | sed "s/^/$u:/"; done | tr '\n' ' ')"
  timeout 20 virsh -c qemu:///system list --all --name 2>/dev/null | grep -c . | sed 's/^/domains=/'
  timeout 60 virsh -c qemu:///system list --all --name 2>/dev/null | while IFS= read -r d; do
    [ -n "$d" ] || continue
    timeout 20 virsh -c qemu:///system snapshot-list "$d" --name 2>/dev/null | while IFS= read -r s; do
      [ -n "$s" ] || continue
      timeout 20 virsh -c qemu:///system snapshot-dumpxml "$d" "$s" 2>/dev/null | sed -n 's|.*<description>\(.*\)</description>.*|snapdesc=\1|p'
    done
  done
fi
echo "sudo_bin=$(command -v sudo 2>/dev/null)"
echo "visudo_bin=$(command -v visudo 2>/dev/null)"
echo "old_script=$([ -e ` + oldHelperPath + ` ] && echo yes)"
echo "node_version=$(` + nodeCmdPath + ` version 2>/dev/null | head -n 1)"
echo "node_sha=$(sha256sum ` + nodeCmdPath + ` 2>/dev/null | cut -d' ' -f1)"
echo "sudoers_file=$([ -f ` + sudoersPath + ` ] && head -c 4096 ` + sudoersPath + ` | tr '\n' '|')"
` + fwProbe

// accountScript prints what is known about the service account $1 (a
// validated name). It always succeeds.
const accountScript = `u=$1
pw=$(getent passwd "$u")
echo "acct=$pw"
if [ -n "$pw" ]; then
  echo "acct_groups=$(id -nG "$u" 2>/dev/null)"
  home=$(echo "$pw" | cut -d: -f6)
  if [ -d "$home" ]; then echo "acct_home_owner=$(stat -c %U "$home" 2>/dev/null)"; fi
  if [ -r "$home/.ssh/authorized_keys" ]; then sed 's/^/authkey=/' "$home/.ssh/authorized_keys"; fi
  if su -s /bin/sh -c 'exit 0' "$u" 2>/dev/null; then echo acct_login=ok; fi
  if [ -r ` + sudoersPath + ` ]; then echo "acct_sudoers=yes"; fi
fi
getent group libvirt >/dev/null && echo group_libvirt=yes
getent group kvm >/dev/null && echo group_kvm=yes
true`

const (
	nodeCmdPath   = nodeproto.Path
	oldHelperPath = "/usr/local/libexec/virticle-helper"
	sudoersPath   = "/etc/sudoers.d/virticle"
)

// Facts are the parsed lines of FactsScript.
type Facts struct {
	UID                                                        string
	Arch, OSID, OSVersion, OSLike, OSName, Kernel              string
	CPUVirt, Hypervisor, KVMDev, Nested                        string
	IOMMUGroups                                                int
	IOMMUCmdline                                               string
	FreePath                                                   string
	FreeBytes                                                  int64
	SELinux, AppArmor, TimeSync                                string
	Virsh, LibvirtVersion, LibvirtUnits                        string
	Domains                                                    int
	Controllers                                                []string // instance ids found in snapshot descriptions
	SudoBin, VisudoBin, NodeCmdVersion, NodeCmdSHA, SudoersRaw string
	OldScript                                                  bool // the shell script of an earlier version is still installed
	Firewall                                                   FirewallFacts
}

// ParseFacts reads FactsScript's output.
func ParseFacts(out string) Facts {
	var f Facts
	ctl := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "uid":
			f.UID = v
		case "arch":
			f.Arch = v
		case "os_id":
			f.OSID = strings.ToLower(v)
		case "os_version":
			f.OSVersion = v
		case "os_like":
			f.OSLike = strings.ToLower(v)
		case "os_name":
			f.OSName = v
		case "kernel":
			f.Kernel = v
		case "cpu_virt":
			f.CPUVirt = v
		case "hypervisor":
			f.Hypervisor = v
		case "kvm_dev":
			f.KVMDev = v
		case "nested":
			f.Nested = v
		case "iommu_groups":
			f.IOMMUGroups, _ = strconv.Atoi(v)
		case "iommu_cmdline":
			f.IOMMUCmdline = v
		case "free_path":
			f.FreePath = v
		case "free_bytes":
			f.FreeBytes, _ = strconv.ParseInt(v, 10, 64)
		case "selinux":
			f.SELinux = v
		case "apparmor":
			f.AppArmor = v
		case "timesync":
			f.TimeSync = v
		case "virsh":
			f.Virsh = v
		case "libvirt_version":
			f.LibvirtVersion = v
		case "libvirt_units":
			f.LibvirtUnits = v
		case "domains":
			f.Domains, _ = strconv.Atoi(v)
		case "snapdesc":
			if id := snapsched.Controller(htmlUnescape(v)); id != "" {
				ctl[id] = true
			}
		case "sudo_bin":
			f.SudoBin = v
		case "visudo_bin":
			f.VisudoBin = v
		case "old_script":
			f.OldScript = v == "yes"
		case "node_version":
			f.NodeCmdVersion = v
		case "node_sha":
			f.NodeCmdSHA = v
		case "sudoers_file":
			f.SudoersRaw = v
		}
	}
	for id := range ctl {
		f.Controllers = append(f.Controllers, id)
	}
	sort.Strings(f.Controllers)
	f.Firewall = ParseFirewallFacts(out)
	return f
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&").Replace(s)
}

// Account is what AccountScript found.
type Account struct {
	Exists       bool
	UID, GID     string
	Home, Shell  string
	Comment      string
	Groups       map[string]bool
	AuthKeys     []string
	HomeOwner    string // owner of the home directory; empty when it does not exist
	GroupLibvirt bool   // the group exists on the host
	GroupKVM     bool
}

// ParseAccount reads accountScript's output.
func ParseAccount(out string) Account {
	a := Account{Groups: map[string]bool{}}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "=")
		if !ok {
			continue
		}
		switch k {
		case "acct":
			if f := strings.Split(v, ":"); len(f) >= 7 {
				a.Exists, a.UID, a.GID, a.Comment, a.Home, a.Shell = true, f[2], f[3], f[4], f[5], f[6]
			}
		case "acct_groups":
			for _, g := range strings.Fields(v) {
				a.Groups[g] = true
			}
		case "acct_home_owner":
			a.HomeOwner = strings.TrimSpace(v)
		case "authkey":
			a.AuthKeys = append(a.AuthKeys, v)
		case "group_libvirt":
			a.GroupLibvirt = true
		case "group_kvm":
			a.GroupKVM = true
		}
	}
	return a
}

// Family is the package manager family of a distribution.
type Family string

const (
	FamilyDebian Family = "debian"
	FamilyRHEL   Family = "rhel"
	FamilyOther  Family = ""
)

// Distro says what is known about the host's distribution.
type Distro struct {
	Family Family
	Name   string // "Debian GNU/Linux 13 (trixie)"
	Tested bool   // in the tested matrix
	OK     bool   // supported at all (tested or untested)
}

func major(v string) string { m, _, _ := strings.Cut(v, "."); return m }

// Classify places a distribution in the matrix: Debian 13 and Rocky 10 are
// tested; Debian 12, Ubuntu 22.04 and 24.04, RHEL, AlmaLinux and Rocky 9 and
// 10, and Fedora are expected to work but are not tested; the rest is not
// supported.
func Classify(id, version, like, name string) Distro {
	d := Distro{Name: name}
	if d.Name == "" {
		d.Name = strings.TrimSpace(id + " " + version)
	}
	fam := FamilyOther
	for _, s := range append([]string{id}, strings.Fields(like)...) {
		switch s {
		case "debian", "ubuntu":
			fam = FamilyDebian
		case "rhel", "fedora", "centos", "rocky", "almalinux":
			if fam == FamilyOther {
				fam = FamilyRHEL
			}
		}
	}
	d.Family = fam
	maj := major(version)
	switch id {
	case "debian":
		d.Tested, d.OK = maj == "13", maj == "13" || maj == "12"
	case "ubuntu":
		d.OK = version == "22.04" || version == "24.04"
	case "rocky":
		d.Tested, d.OK = maj == "10", maj == "10" || maj == "9"
	case "rhel", "almalinux":
		d.OK = maj == "9" || maj == "10"
	case "fedora":
		d.OK = true
	}
	return d
}

// Package names per family. The lists were checked on Debian 13 and Rocky 10.
// The guest disk tools provide virt-sysprep, which "sysprep on clone" uses;
// "prepare --minimal" leaves them out.
var (
	debianBase  = []string{"qemu-system-x86", "qemu-utils", "libvirt-daemon-system", "libvirt-clients", "dnsmasq-base", "ovmf", "swtpm", "swtpm-tools", "sudo", "curl", "ca-certificates", "openssh-client", "iproute2"}
	rhelBase    = []string{"qemu-kvm", "libvirt", "virt-install", "edk2-ovmf", "swtpm", "swtpm-tools", "sudo", "curl", "openssh-clients", "iproute-tc"}
	debianGuest = []string{"libguestfs-tools"}
	rhelGuest   = []string{"guestfs-tools"}
)

// PackageList is the package names to install for a family. minimal leaves
// out the guest disk tools.
func PackageList(f Family, minimal bool) []string {
	var out []string
	switch f {
	case FamilyDebian:
		out = append(out, debianBase...)
		if !minimal {
			out = append(out, debianGuest...)
		}
	case FamilyRHEL:
		out = append(out, rhelBase...)
		if !minimal {
			out = append(out, rhelGuest...)
		}
	}
	return out
}

// MissingScript prints the packages of the list that are not installed.
func MissingScript(f Family, pkgs []string) string {
	q := make([]string, len(pkgs))
	for i, p := range pkgs {
		q[i] = shq(p)
	}
	if f == FamilyDebian {
		return "for p in " + strings.Join(q, " ") + `; do dpkg -s "$p" 2>/dev/null | grep -q '^Status: install ok installed' || echo "$p"; done; true`
	}
	return "for p in " + strings.Join(q, " ") + `; do rpm -q --quiet "$p" 2>/dev/null || echo "$p"; done; true`
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ManualInstall is the text shown for a distribution that is not supported:
// what to install by hand.
func ManualInstall() string {
	return "Install by hand: Debian family " + strings.Join(append(append([]string{}, debianBase...), debianGuest...), " ") + "; RHEL family " + strings.Join(append(append([]string{}, rhelBase...), rhelGuest...), " ") + ". Then enable libvirt and run this again"
}

// Options say what to do and how.
type Options struct {
	User          string // service account; empty: an account made by an earlier version, else DefaultUser (see Runner.Check)
	UseExisting   bool   // the account exists and is managed by hand; change nothing about it except the key
	Confirm       bool   // with UseExisting: also add it to the groups and grant the sudo rule
	AllowRoot     bool   // the controller logs in as root (the node command runs directly, no service account)
	From          string // controller address(es) the key is restricted to
	Minimal       bool   // leave out the guest disk tools
	ControllerKey string // the controller's public key line
	InstanceID    string // this controller's instance id, to tell other controllers apart
	// TakeOver is the administrator's answer to "This node was managed by another Virticle controller. Take it over?".
	// Without it a node that trusts another controller's key is not changed; with it the other controller's key is
	// removed from the node's authorized keys.
	TakeOver bool
}

// ServiceUser is the account the controller will log in as. With no name given
// it is DefaultUser; Runner.Check replaces an empty name by the account an
// earlier version made, when there is one.
func (o Options) ServiceUser() string {
	switch {
	case o.AllowRoot:
		return "root"
	case o.User == "":
		return DefaultUser
	}
	return o.User
}

// Validate checks the options before anything is done.
func (o Options) Validate() error {
	if o.AllowRoot && (o.UseExisting || (o.User != "" && o.User != "root")) {
		return fmt.Errorf("root cannot be combined with an account name or use-existing-user")
	}
	if !o.AllowRoot && !validUser(o.ServiceUser()) {
		return fmt.Errorf("%q is not a valid account name (lower case letters, digits, - and _)", o.ServiceUser())
	}
	if o.ServiceUser() == "root" && !o.AllowRoot {
		return fmt.Errorf("the account root needs the root option")
	}
	if o.From != "" {
		if err := validFrom(o.From); err != nil {
			return err
		}
	}
	if _, err := KeyBlob(o.ControllerKey); err != nil {
		return fmt.Errorf("controller key: %w", err)
	}
	return nil
}

func validUser(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case i > 0 && (c >= '0' && c <= '9' || c == '-'):
		default:
			return false
		}
	}
	return true
}

func validFrom(s string) error {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune(".:*?/!,-_", c):
		default:
			return fmt.Errorf("the controller address %q may contain letters, digits and . : * ? / ! , - _ only", s)
		}
	}
	return nil
}

// KeyBlob returns the "type base64" part of a public key line.
func KeyBlob(line string) (string, error) {
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[0], "ssh-") && !strings.HasPrefix(f[0], "ecdsa-") && !strings.HasPrefix(f[0], "sk-") {
		return "", fmt.Errorf("not a public key line")
	}
	return f[0] + " " + f[1], nil
}

// Restriction options on the controller's key. restrict turns everything off;
// pty is needed for the long-running event stream (a command whose remote
// process must end when the session closes); port-forwarding is what the
// controller uses to reach libvirt's socket and the VNC and serial consoles of
// guests on the node's loopback address.
const keyOptions = "restrict,pty,port-forwarding"

// AuthorizedKeyLine is the line installed for the controller's key.
func AuthorizedKeyLine(o Options) string {
	blob, _ := KeyBlob(o.ControllerKey)
	opts := keyOptions
	if o.From != "" {
		opts += `,from="` + o.From + `"`
	}
	return opts + " " + blob + " virticle-controller"
}

// controllerComment is the comment on every key line a controller installs.
const controllerComment = "virticle-controller"

// OtherControllerKeys returns the key lines among authorized_keys lines that
// belong to a controller other than the one with public key ours: lines
// commented "virticle-controller" with a different key. Other keys, such as
// an administrator's own, are never counted.
func OtherControllerKeys(lines []string, ours string) []string {
	mine, _ := KeyBlob(ours)
	var out []string
	for _, l := range lines {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			if !strings.HasPrefix(f[i], "ssh-") && !strings.HasPrefix(f[i], "ecdsa-") && !strings.HasPrefix(f[i], "sk-") {
				continue
			}
			if strings.Join(f[i+2:], " ") == controllerComment && f[i]+" "+f[i+1] != mine {
				out = append(out, l)
			}
			break
		}
	}
	return out
}
