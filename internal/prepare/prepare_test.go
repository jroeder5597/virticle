package prepare

import (
	"errors"
	"strings"
	"testing"

	"virticle/internal/hostconn"
)

func init() {
	// The tests do not need the built node command ("make nodebin").
	nodeBinary = func(arch string) ([]byte, error) { return []byte("\x7fELF fake node command for " + arch), nil }
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholderPlaceholderPlaceholder0000 virticle-controller"

const debian13Facts = `uid=0
arch=x86_64
os_id=debian
os_version=13
os_like=
os_name=Debian GNU/Linux 13 (trixie)
kernel=6.12.0
cpu_virt=vmx
hypervisor=hypervisor
kvm_dev=660 root:kvm
nested=Y
iommu_groups=0
iommu_cmdline=
free_path=/var/lib
free_bytes=16000000000
selinux=
apparmor=Y
timesync=yes
virsh=
domains=0
`

const rocky10Facts = `uid=0
arch=x86_64
os_id=rocky
os_version=10.0
os_like=rhel centos fedora
os_name=Rocky Linux 10.0 (Red Quartz)
cpu_virt=vmx
hypervisor=hypervisor
kvm_dev=666 root:kvm
nested=1
iommu_groups=0
free_bytes=12000000000
selinux=Enforcing
timesync=yes
virsh=/usr/bin/virsh
libvirt_version=11.10.0
domains=3
snapdesc=Scheduled snapshot (virticle) [controller abc123]
`

func plan(facts string, missing ...string) Plan {
	f := ParseFacts(facts)
	return Plan{Facts: f, Distro: Classify(f.OSID, f.OSVersion, f.OSLike, f.OSName), Missing: missing}
}

func find(rows []Row, id string) Row {
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	return Row{}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		id, ver, like string
		fam           Family
		tested, ok    bool
	}{
		{"debian", "13", "", FamilyDebian, true, true},
		{"debian", "12", "", FamilyDebian, false, true},
		{"debian", "11", "", FamilyDebian, false, false},
		{"ubuntu", "24.04", "debian", FamilyDebian, false, true},
		{"ubuntu", "22.04", "debian", FamilyDebian, false, true},
		{"ubuntu", "20.04", "debian", FamilyDebian, false, false},
		{"rocky", "10.0", "rhel centos fedora", FamilyRHEL, true, true},
		{"rocky", "9.5", "rhel centos fedora", FamilyRHEL, false, true},
		{"almalinux", "10.0", "rhel centos fedora", FamilyRHEL, false, true},
		{"rhel", "9.4", "fedora", FamilyRHEL, false, true},
		{"rhel", "8.10", "fedora", FamilyRHEL, false, false},
		{"fedora", "42", "", FamilyRHEL, false, true},
		{"arch", "", "", FamilyOther, false, false},
		{"alpine", "3.20", "", FamilyOther, false, false},
	}
	for _, c := range cases {
		d := Classify(c.id, c.ver, c.like, "")
		if d.Family != c.fam || d.Tested != c.tested || d.OK != c.ok {
			t.Errorf("%s %s: %+v, want family %q tested %v ok %v", c.id, c.ver, d, c.fam, c.tested, c.ok)
		}
	}
}

func TestEvaluateDebian13Fresh(t *testing.T) {
	rows := Evaluate(plan(debian13Facts, "qemu-system-x86", "libvirt-daemon-system"), Options{ControllerKey: testKey})
	want := map[string]string{
		"arch": OK, "distro": OK, "cpu": OK, "kvm": OK, "nested": Info, "iommu": Info, "space": OK, "mac": Info,
		"time": OK, "libvirt": Info, "controller": OK, "packages": Info, "account": Info,
	}
	for id, st := range want {
		if got := find(rows, id); got.Status != st {
			t.Errorf("%s: %q (%s), want %s", id, got.Status, got.Detail, st)
		}
	}
	if find(rows, "vms").ID != "" {
		t.Error("existing VMs are only reported when libvirt is installed")
	}
	if HasError(rows) {
		t.Errorf("a fresh Debian 13 VM must have no error: %+v", rows)
	}
	if d := find(rows, "iommu").Detail; !strings.Contains(d, "intel_iommu=on") || !strings.Contains(d, "never reboots") {
		t.Errorf("iommu hint: %q", d)
	}
	if d := find(rows, "distro").Detail; !strings.Contains(d, "tested") {
		t.Errorf("distro: %q", d)
	}
}

func TestEvaluateRocky10WithLibvirtAndOtherController(t *testing.T) {
	o := Options{ControllerKey: testKey, InstanceID: "mine"}
	rows := Evaluate(plan(rocky10Facts), o)
	if r := find(rows, "libvirt"); r.Status != OK || !strings.Contains(r.Detail, "11.10.0") {
		t.Errorf("libvirt: %+v", r)
	}
	if r := find(rows, "vms"); r.Status != Info || !strings.Contains(r.Detail, "3. Existing VMs are left alone") {
		t.Errorf("vms: %+v", r)
	}
	if r := find(rows, "controller"); r.Status != Warning || !strings.Contains(r.Detail, "abc123") {
		t.Errorf("controller: %+v", r)
	}
	if r := find(rows, "mac"); r.Detail != "SELinux enforcing" {
		t.Errorf("mac: %+v", r)
	}
	if r := find(rows, "iommu"); !strings.Contains(r.Detail, "intel_iommu=on") {
		t.Errorf("iommu: %+v", r)
	}
	o.InstanceID = "abc123"
	if r := find(Evaluate(plan(rocky10Facts), o), "controller"); r.Status != OK {
		t.Errorf("our own schedules are not another controller: %+v", r)
	}
	if r := find(rows, "packages"); r.Status != OK {
		t.Errorf("nothing missing: %+v", r)
	}
}

func TestEvaluateProblems(t *testing.T) {
	// No CPU flag, not a VM: firmware.
	f := strings.ReplaceAll(strings.ReplaceAll(debian13Facts, "cpu_virt=vmx", "cpu_virt="), "hypervisor=hypervisor", "hypervisor=")
	f = strings.Replace(f, "kvm_dev=660 root:kvm", "kvm_dev=", 1)
	rows := Evaluate(plan(f), Options{ControllerKey: testKey})
	if r := find(rows, "cpu"); r.Status != Error || !strings.Contains(r.Detail, "firmware") {
		t.Errorf("cpu: %+v", r)
	}
	if r := find(rows, "kvm"); r.Status != Error {
		t.Errorf("kvm: %+v", r)
	}
	if find(rows, "nested").ID != "" {
		t.Error("nested is only for VMs")
	}
	// A VM without the flag: nested virtualisation hint.
	f2 := strings.Replace(debian13Facts, "cpu_virt=vmx", "cpu_virt=", 1)
	if r := find(Evaluate(plan(f2), Options{ControllerKey: testKey}), "cpu"); !strings.Contains(r.Detail, "nested virtualisation") {
		t.Errorf("cpu in a VM: %+v", r)
	}
	// Unsupported distribution: error with the package list.
	f3 := strings.NewReplacer("os_id=debian", "os_id=alpine", "os_version=13", "os_version=3.20", "os_name=Debian GNU/Linux 13 (trixie)", "os_name=Alpine Linux").Replace(debian13Facts)
	r := find(Evaluate(plan(f3), Options{ControllerKey: testKey}), "distro")
	if r.Status != Error || !strings.Contains(r.Detail, "qemu-system-x86") || !strings.Contains(r.Detail, "edk2-ovmf") {
		t.Errorf("unsupported distro: %+v", r)
	}
	// Untested distribution: warning.
	f4 := strings.NewReplacer("os_version=13", "os_version=12", "(trixie)", "(bookworm)").Replace(debian13Facts)
	if r := find(Evaluate(plan(f4), Options{ControllerKey: testKey}), "distro"); r.Status != Warning || !strings.Contains(r.Detail, "untested") {
		t.Errorf("untested distro: %+v", r)
	}
	// Low space, no time sync, wrong architecture.
	f5 := strings.NewReplacer("free_bytes=16000000000", "free_bytes=2000000000", "timesync=yes", "timesync=no", "arch=x86_64", "arch=ppc64le").Replace(debian13Facts)
	rows = Evaluate(plan(f5), Options{ControllerKey: testKey})
	for id, st := range map[string]string{"space": Warning, "time": Warning, "arch": Error} {
		if got := find(rows, id); got.Status != st {
			t.Errorf("%s: %+v, want %s", id, got, st)
		}
	}
}

func TestEvaluateAccount(t *testing.T) {
	base := plan(debian13Facts)
	o := Options{ControllerKey: testKey}
	if r := find(Evaluate(base, o), "account"); r.Status != Info || !strings.Contains(r.Detail, "virticle would be created") {
		t.Errorf("new account: %+v", r)
	}
	base.Account = Account{Exists: true, Comment: "somebody else", Shell: "/bin/bash", Groups: map[string]bool{}}
	if r := find(Evaluate(base, o), "account"); r.Status != Error {
		t.Errorf("a foreign account of that name: %+v", r)
	}
	base.Account.Comment = AccountComment
	if r := find(Evaluate(base, o), "account"); r.Status != OK {
		t.Errorf("our account: %+v", r)
	}
	o.UseExisting = true
	base.Account = Account{Exists: true, Shell: "/bin/bash", Groups: map[string]bool{"kvm": true}, GroupLibvirt: true, GroupKVM: true}
	r := find(Evaluate(base, o), "account")
	if r.Status != Warning || !strings.Contains(r.Detail, "libvirt") || strings.Contains(r.Detail, "kvm,") {
		t.Errorf("existing account missing a group: %+v", r)
	}
	o.Confirm = true
	if r := find(Evaluate(base, o), "account"); r.Status != Info {
		t.Errorf("confirmed: %+v", r)
	}
	base.Account.Shell = "/usr/sbin/nologin"
	if r := find(Evaluate(base, o), "shell"); r.Status != Error {
		t.Errorf("nologin: %+v", r)
	}
	r = find(Evaluate(base, Options{AllowRoot: true, ControllerKey: testKey}), "account")
	if r.Status != Warning || r.Detail != "Uses root; a dedicated account is safer" {
		t.Errorf("root: %+v", r)
	}
}

func TestValidate(t *testing.T) {
	ok := Options{ControllerKey: testKey}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Options{
		{ControllerKey: testKey, User: "Bad Name"},
		{ControllerKey: testKey, User: "a;b"},
		{ControllerKey: testKey, User: "root"},
		{ControllerKey: testKey, AllowRoot: true, BroadSudo: true},
		{ControllerKey: testKey, AllowRoot: true, UseExisting: true},
		{ControllerKey: testKey, Extras: []string{"docker"}},
		{ControllerKey: testKey, From: `10.0.0.1" command="id`},
		{ControllerKey: testKey, From: "a b"},
		{},
		{ControllerKey: "not a key"},
	}
	for i, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%d: accepted %+v", i, o)
		}
	}
	if err := (Options{ControllerKey: testKey, AllowRoot: true}).Validate(); err != nil {
		t.Errorf("allow-root: %v", err)
	}
	if err := (Options{ControllerKey: testKey, From: "10.0.0.0/24,192.168.1.5,!10.0.0.9", Extras: []string{"guestfs", "virtiofs"}}).Validate(); err != nil {
		t.Errorf("from/extras: %v", err)
	}
}

func TestAuthorizedKeyLine(t *testing.T) {
	got := AuthorizedKeyLine(Options{ControllerKey: testKey})
	if got != "restrict,pty,port-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholderPlaceholderPlaceholder0000 virticle-controller" {
		t.Errorf("%s", got)
	}
	got = AuthorizedKeyLine(Options{ControllerKey: testKey, From: "10.1.2.3"})
	if !strings.HasPrefix(got, `restrict,pty,port-forwarding,from="10.1.2.3" ssh-ed25519 `) {
		t.Errorf("%s", got)
	}
}

func TestPackageLists(t *testing.T) {
	d := PackageList(FamilyDebian, []string{"guestfs", "virtiofs"})
	r := PackageList(FamilyRHEL, []string{"guestfs", "virtiofs"})
	has := func(l []string, p string) bool {
		for _, x := range l {
			if x == p {
				return true
			}
		}
		return false
	}
	for _, p := range []string{"qemu-system-x86", "libvirt-daemon-system", "libvirt-clients", "ovmf", "swtpm", "swtpm-tools", "libguestfs-tools", "virtiofsd"} {
		if !has(d, p) {
			t.Errorf("debian list lacks %s", p)
		}
	}
	for _, p := range []string{"qemu-kvm", "libvirt", "virt-install", "edk2-ovmf", "swtpm", "swtpm-tools", "guestfs-tools", "virtiofsd"} {
		if !has(r, p) {
			t.Errorf("rhel list lacks %s", p)
		}
	}
	if PackageList(FamilyOther, nil) != nil {
		t.Error("no list for an unknown family")
	}
}

func TestParseFactsSnapshots(t *testing.T) {
	f := ParseFacts("snapdesc=Scheduled snapshot (virticle) [controller b]\nsnapdesc=hand made\nsnapdesc=Scheduled snapshot (virticle) [controller a]\nsnapdesc=Scheduled snapshot (virticle) [controller a]\n")
	if len(f.Controllers) != 2 || f.Controllers[0] != "a" || f.Controllers[1] != "b" {
		t.Errorf("%v", f.Controllers)
	}
}

func TestParseAccount(t *testing.T) {
	a := ParseAccount("acct=virticle:x:998:998:" + AccountComment + ":/var/lib/virticle:/bin/sh\nacct_groups=virticle libvirt kvm\nauthkey=restrict ssh-ed25519 AAA c\ngroup_libvirt=yes\n")
	if !a.Exists || a.Home != "/var/lib/virticle" || a.Shell != "/bin/sh" || !a.Groups["libvirt"] || len(a.AuthKeys) != 1 || !a.GroupLibvirt || a.GroupKVM {
		t.Errorf("%+v", a)
	}
	if ParseAccount("acct=\n").Exists {
		t.Error("an empty passwd entry is no account")
	}
}

// --- the runner against a scripted host ---

type fakeNode struct {
	log      []string
	handlers []func(cmd, stdin string) (string, bool, error)
}

func (n *fakeNode) Run(cmd string) (string, error) { return n.RunInput(cmd, "") }
func (n *fakeNode) RunInput(cmd, stdin string) (string, error) {
	n.log = append(n.log, cmd+"\n--stdin--\n"+stdin)
	for _, h := range n.handlers {
		if out, ok, err := h(cmd, stdin); ok {
			return out, err
		}
	}
	return "", nil
}

func hostOnDebian(changed bool, extra func(cmd, stdin string) (string, bool, error)) *fakeNode {
	n := &fakeNode{}
	mark := "@@changed"
	if !changed {
		mark = "@@unchanged"
	}
	n.handlers = append(n.handlers, func(cmd, stdin string) (string, bool, error) {
		if extra != nil {
			if out, ok, err := extra(cmd, stdin); ok {
				return out, ok, err
			}
		}
		switch {
		case strings.Contains(cmd, "echo uid=$(id -u); echo user"):
			return "uid=0\nuser=root", true, nil
		case strings.Contains(cmd, "os_id="):
			return debian13Facts, true, nil
		case strings.Contains(cmd, "getent passwd \"$u\""):
			return "", true, nil
		case strings.Contains(cmd, hostconn.HelperPath+" version") && !strings.Contains(cmd, "os_id="):
			return hostconn.HelperVersion, true, nil
		case strings.Contains(cmd, "dpkg -s"):
			if changed {
				return "libvirt-daemon-system\nqemu-system-x86", true, nil
			}
			return "", true, nil
		default:
			return "did something\n" + mark, true, nil
		}
	})
	return n
}

func TestPrepareRunsStepsInOrder(t *testing.T) {
	n := hostOnDebian(true, nil)
	var reports int
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, func([]Row) { reports++ }, func() []Row {
		return []Row{{ID: "test-ssh", Title: "SSH sign-in as virticle", Status: OK}}
	})
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	order := []string{"admin", "arch", "distro", "cpu", "kvm", "nested", "iommu", "space", "mac", "time", "libvirt", "controller", "packages", "account",
		"do-packages", "do-services", "do-network", "do-pool", "do-account", "do-key", "do-helper", "do-sudoers", "do-peerkey", "test-ssh", "summary"}
	if strings.Join(ids, ",") != strings.Join(order, ",") {
		t.Errorf("rows:\n%v\nwant\n%v", ids, order)
	}
	if HasError(rows) {
		t.Errorf("errors: %+v", rows)
	}
	if r := find(rows, "summary"); !strings.Contains(r.Detail, "prepared") || strings.Contains(r.Detail, "Nothing changed") {
		t.Errorf("summary: %+v", r)
	}
	if reports < 8 {
		t.Errorf("progress was reported %d times", reports)
	}
	// The sudo rule carries the node command path only, the key line the restrictions.
	all := strings.Join(n.log, "\n")
	if !strings.Contains(all, "virticle ALL=(root) NOPASSWD: /usr/local/libexec/virticle-node\n") {
		t.Error("the sudoers rule was not sent")
	}
	if !strings.Contains(all, "restrict,pty,port-forwarding ssh-ed25519 ") {
		t.Error("the restricted key line was not sent")
	}
	if strings.Contains(all, "NOPASSWD: ALL") {
		t.Error("a broad rule was sent")
	}
}

func TestPrepareSecondRunChangesNothing(t *testing.T) {
	n := hostOnDebian(false, nil)
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "summary"); !strings.Contains(r.Detail, "Nothing changed") {
		t.Errorf("summary: %+v", r)
	}
	for _, id := range []string{"do-packages", "do-services", "do-key", "do-helper", "do-sudoers"} {
		if r := find(rows, id); r.Status != OK || !strings.HasPrefix(r.Detail, "Already in place") {
			t.Errorf("%s: %+v", id, r)
		}
	}
}

func TestPrepareStopsAtFirstFailure(t *testing.T) {
	n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "net-define") {
			return "could not start the new default network", true, errors.New("exit status 1")
		}
		return "", false, nil
	})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, func() []Row { t.Error("verify must not run"); return nil })
	if r := find(rows, "do-network"); r.Status != Error {
		t.Errorf("network: %+v", r)
	}
	for _, id := range []string{"do-pool", "do-account", "do-key", "do-helper", "do-sudoers", "do-test"} {
		if r := find(rows, id); r.Status != Skipped || r.Detail != "Not run" {
			t.Errorf("%s should be Not run: %+v", id, r)
		}
	}
	if r := find(rows, "do-services"); r.Status != OK {
		t.Errorf("services ran before the failure: %+v", r)
	}
}

func TestPrepareNothingRunsAfterACheckError(t *testing.T) {
	bad := strings.Replace(debian13Facts, "cpu_virt=vmx", "cpu_virt=", 1)
	n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "os_id=") {
			return bad, true, nil
		}
		return "", false, nil
	})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if !HasError(rows) {
		t.Fatal("expected an error row")
	}
	for _, id := range []string{"do-packages", "do-key", "do-sudoers"} {
		if r := find(rows, id); r.Status != Skipped {
			t.Errorf("%s: %+v", id, r)
		}
	}
	for _, c := range n.log {
		if strings.Contains(c, "apt-get") || strings.Contains(c, "useradd") {
			t.Error("a step ran although the check failed")
		}
	}
}

func TestCheckChangesNothing(t *testing.T) {
	n := hostOnDebian(true, nil)
	rows, _, err := (&Runner{N: n}).Check(Options{ControllerKey: testKey})
	if err != nil || HasError(rows) {
		t.Fatalf("%v %+v", err, rows)
	}
	for _, c := range n.log {
		for _, w := range []string{"apt-get", "dnf install", "useradd", "usermod", "systemctl enable", "net-define", "pool-define", "install -d", "mv -f", "rm -f"} {
			if strings.Contains(c, w) {
				t.Errorf("--check ran %q:\n%s", w, c)
			}
		}
	}
}

func TestBroadSudoSkipsTheHelper(t *testing.T) {
	n := hostOnDebian(true, nil)
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey, BroadSudo: true}, nil, nil)
	if find(rows, "do-helper").ID != "" {
		t.Error("the helper is not installed with broad sudo")
	}
	if !strings.Contains(strings.Join(n.log, "\n"), "NOPASSWD: ALL") {
		t.Error("the broad rule was not sent")
	}
}

func TestAllowRootHasNoAccountOrSudoersButTheNodeCommandAndKey(t *testing.T) {
	n := hostOnDebian(true, nil)
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey, AllowRoot: true}, nil, nil)
	for _, id := range []string{"do-account", "do-sudoers"} {
		if find(rows, id).ID != "" {
			t.Errorf("%s with root", id)
		}
	}
	for _, id := range []string{"do-helper", "do-peerkey"} {
		if r := find(rows, id); r.Status != OK {
			t.Errorf("%s with root: %+v", id, r)
		}
	}
	if r := find(rows, "account"); r.Status != Warning {
		t.Errorf("root: %+v", r)
	}
}

func TestSudoPasswordNeverShown(t *testing.T) {
	const pw = "hunter2-secret"
	n := &fakeNode{}
	n.handlers = append(n.handlers, func(cmd, stdin string) (string, bool, error) {
		switch {
		case strings.Contains(cmd, "echo uid=$(id -u); echo user"):
			return "uid=1000\nuser=admin", true, nil
		case cmd == "sudo -n true":
			return "sudo: a password is required", true, errors.New("exit status 1")
		case strings.Contains(cmd, "sudo -S"):
			if !strings.HasPrefix(stdin, pw+"\n") {
				return "sudo: no password", true, errors.New("exit status 1")
			}
			if strings.Contains(cmd, "os_id=") {
				return debian13Facts, true, nil
			}
			if strings.Contains(cmd, "apt-get") {
				return "E: leaked " + pw + " in output", true, errors.New("exit status 100")
			}
			if strings.Contains(cmd, "dpkg -s") {
				return "sudo", true, nil
			}
			return "ok\n@@changed", true, nil
		}
		return "", true, nil
	})
	rows := (&Runner{N: n, SudoPassword: pw}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "admin"); r.Status != OK || !strings.Contains(r.Detail, "password") {
		t.Errorf("admin: %+v", r)
	}
	if r := find(rows, "do-packages"); r.Status != Error {
		t.Errorf("packages should fail: %+v", r)
	}
	for _, r := range rows {
		if strings.Contains(r.Title+r.Detail, pw) {
			t.Errorf("the password is in a row: %+v", r)
		}
	}
	for _, c := range n.log {
		head, _, _ := strings.Cut(c, "\n--stdin--\n")
		if strings.Contains(head, pw) {
			t.Error("the password is on a command line")
		}
	}
}

func TestAdminWithoutSudoIsAnError(t *testing.T) {
	n := &fakeNode{}
	n.handlers = append(n.handlers, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "echo uid=$(id -u); echo user") {
			return "uid=1000\nuser=bob", true, nil
		}
		if cmd == "sudo -n true" {
			return "", true, errors.New("exit status 1")
		}
		return "", true, nil
	})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "admin"); r.Status != Error || !strings.Contains(r.Detail, "bob") {
		t.Errorf("admin: %+v", r)
	}
	if r := find(rows, "do-packages"); r.Status != Skipped {
		t.Errorf("packages: %+v", r)
	}
}

func TestParseStep(t *testing.T) {
	d, ch := parseStep("created x\nadded to kvm\n@@changed\n")
	if d != "Created x; Added to kvm" || !ch {
		t.Errorf("%q %v", d, ch)
	}
	if d, ch = parseStep("fine\n@@unchanged"); d != "Fine" || ch {
		t.Errorf("%q %v", d, ch)
	}
}

func TestMissingScriptQuotes(t *testing.T) {
	s := MissingScript(FamilyDebian, []string{"a", "b'c"})
	if !strings.Contains(s, `'b'\''c'`) {
		t.Error(s)
	}
}

func TestUnprepareScriptTouchesOnlyOurThings(t *testing.T) {
	for _, must := range []string{"Written by virticle node prepare", AccountComment, `grep -vF -- "$blob"`} {
		if !strings.Contains(unprepareScript, must) {
			t.Errorf("the removal script lacks %q", must)
		}
	}
	for _, never := range []string{"apt-get remove", "dnf remove", "virsh", "undefine", "pool-delete", "net-undefine"} {
		if strings.Contains(unprepareScript, never) {
			t.Errorf("the removal script contains %q", never)
		}
	}
}

func TestExistingAccountSudoRuleAndConfirm(t *testing.T) {
	base := plan(debian13Facts)
	base.Account = Account{Exists: true, Shell: "/bin/bash", Groups: map[string]bool{"libvirt": true, "kvm": true}, GroupLibvirt: true, GroupKVM: true}
	o := Options{ControllerKey: testKey, User: "svc", UseExisting: true}
	if r := find(Evaluate(base, o), "sudo"); r.Status != Warning || !strings.Contains(r.Detail, "confirm") {
		t.Errorf("no rule yet: %+v", r)
	}
	base.Facts.SudoersRaw = "# x|svc ALL=(root) NOPASSWD: /usr/local/libexec/virticle-node|"
	if r := find(Evaluate(base, o), "sudo"); r.ID != "" {
		t.Errorf("a rule exists, nothing to report: %+v", r)
	}
	// Without confirmation an existing account gets the key (and the helper file) but no sudo rule.
	withSvc := func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "acct_groups") {
			return "acct=svc:x:1000:1000::/home/svc:/bin/bash\nacct_groups=svc libvirt kvm\ngroup_libvirt=yes\ngroup_kvm=yes", true, nil
		}
		return "", false, nil
	}
	n := hostOnDebian(true, withSvc)
	rows := (&Runner{N: n}).Prepare(o, nil, nil)
	if find(rows, "do-sudoers").ID != "" || find(rows, "do-key").Status != OK {
		t.Errorf("rows: %+v", rows)
	}
	o.Confirm = true
	rows = (&Runner{N: hostOnDebian(true, withSvc)}).Prepare(o, nil, nil)
	if find(rows, "do-sudoers").Status != OK {
		t.Errorf("confirmed: %+v", rows)
	}
}

func TestUnprepareRows(t *testing.T) {
	n := &fakeNode{}
	n.handlers = append(n.handlers, func(cmd, stdin string) (string, bool, error) {
		switch {
		case strings.Contains(cmd, "echo uid=$(id -u); echo user"):
			return "uid=0\nuser=root", true, nil
		case strings.Contains(cmd, "userdel"):
			return "key removed\nsudo rule removed\nnode command removed\naccount removed\n@@changed", true, nil
		}
		return "", true, nil
	})
	rows := (&Runner{N: n}).Unprepare(Options{ControllerKey: testKey})
	var titles []string
	for _, r := range rows {
		titles = append(titles, r.Title)
	}
	got := strings.Join(titles, ",")
	if got != "Administrator access,SSH key,Sudo rule,Node command,Service account,Result" {
		t.Errorf("rows: %s", got)
	}
}

// The node command is uploaded as the matching architecture's binary and
// installed under the new name; the old script is removed.
func TestStepHelperInstallsTheNodeCommandAndRemovesTheOldScript(t *testing.T) {
	n := hostOnDebian(true, nil)
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "do-helper"); r.Status != OK || !strings.Contains(r.Detail, "version "+hostconn.HelperVersion) {
		t.Errorf("do-helper: %+v", r)
	}
	all := strings.Join(n.log, "\n")
	for _, want := range []string{"fake node command for x86_64", "dest=", "/usr/local/libexec/virticle-node", "rm -f /usr/local/libexec/virticle-helper"} {
		if !strings.Contains(all, want) {
			t.Errorf("the install lacks %q", want)
		}
	}
	// The sudo rule is checked as the service account.
	if !strings.Contains(all, "runuser -u") && strings.Contains(all, "sudo -n /usr/local/libexec/virticle-node version") {
		t.Error("the sudo rule was not tried as the service account")
	}
}

func TestStepHelperUsesTheNodesArchitecture(t *testing.T) {
	var seen string
	old := nodeBinary
	defer func() { nodeBinary = old }()
	nodeBinary = func(arch string) ([]byte, error) { seen = arch; return []byte("bin"), nil }
	facts := strings.Replace(debian13Facts, "arch=x86_64", "arch=aarch64", 1)
	n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "os_id=") {
			return facts, true, nil
		}
		return "", false, nil
	})
	(&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if seen != "aarch64" {
		t.Errorf("binary asked for %q", seen)
	}
}

func TestStepHelperWithoutBinaryAndWithBrokenBinary(t *testing.T) {
	old := nodeBinary
	defer func() { nodeBinary = old }()
	nodeBinary = func(string) ([]byte, error) { return nil, errors.New("built without the node command") }
	rows := (&Runner{N: hostOnDebian(true, nil)}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "do-helper"); r.Status != Error || !strings.Contains(r.Detail, "built without") {
		t.Errorf("no binary: %+v", r)
	}
	if r := find(rows, "do-sudoers"); r.Status != Skipped {
		t.Errorf("nothing runs after the failed step: %+v", r)
	}
	nodeBinary = func(string) ([]byte, error) { return []byte("x"), nil }
	n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "sh -c '/usr/local/libexec/virticle-node version'") {
			return "exec format error", true, errors.New("exit status 126")
		}
		return "", false, nil
	})
	rows = (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	if r := find(rows, "do-helper"); r.Status != Error || !strings.Contains(r.Detail, "does not run") {
		t.Errorf("a binary that does not run: %+v", r)
	}
}

func TestOldHelperIsReportedInFacts(t *testing.T) {
	f := ParseFacts("old_helper=yes\nhelper_version=\n")
	if !f.OldHelper {
		t.Error("the old script was not noticed")
	}
	if ParseFacts("old_helper=\n").OldHelper {
		t.Error("no old script")
	}
}

func TestUnprepareScriptRemovesBothNames(t *testing.T) {
	for _, must := range []string{"/usr/local/libexec/virticle-node", "/usr/local/libexec/virticle-helper", "virticle-node peer-recv", "virticle-helper peer-recv"} {
		if !strings.Contains(unprepareScript, must) {
			t.Errorf("the removal script lacks %q", must)
		}
	}
}
