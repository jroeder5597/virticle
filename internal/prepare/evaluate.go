package prepare

import (
	"fmt"
	"strings"
)

const gib = 1 << 30

// Plan is everything the read-only look at a host found.
type Plan struct {
	Facts   Facts
	Account Account
	Distro  Distro
	Missing []string // packages that are not installed yet
	Want    []string // all packages wanted
}

// Evaluate turns what was found into the check rows, in the order they are
// shown. It never fails: a host that cannot be prepared gets error rows.
func Evaluate(p Plan, o Options) []Row {
	f := p.Facts
	var rows []Row
	add := func(id, title, status, detail string) {
		rows = append(rows, Row{ID: id, Title: title, Status: status, Detail: detail})
	}

	// Architecture
	switch f.Arch {
	case "x86_64":
		add("arch", "Architecture", OK, f.Arch)
	case "aarch64":
		add("arch", "Architecture", Warning, "aarch64 is untested")
	default:
		add("arch", "Architecture", Error, fmt.Sprintf("%s is not supported (x86_64 is tested, aarch64 untested)", f.Arch))
	}

	// Distribution
	d := p.Distro
	switch {
	case d.OK && d.Tested:
		add("distro", "Distribution", OK, d.Name+" (tested)")
	case d.OK:
		add("distro", "Distribution", Warning, d.Name+" is untested; Debian 13 and Rocky 10 are tested")
	default:
		add("distro", "Distribution", Error, d.Name+" is not supported. "+ManualInstall())
	}

	// CPU virtualisation
	switch {
	case f.CPUVirt != "":
		add("cpu", "CPU virtualisation", OK, "Flag "+f.CPUVirt)
	case f.Hypervisor != "":
		add("cpu", "CPU virtualisation", Error, "no vmx or svm flag; this host is a VM, so enable nested virtualisation for it on its host")
	default:
		add("cpu", "CPU virtualisation", Error, "no vmx or svm flag; turn on hardware virtualisation in the firmware")
	}

	// /dev/kvm
	switch {
	case f.KVMDev != "":
		add("kvm", "KVM device", OK, "/dev/kvm present ("+f.KVMDev+")")
	case f.CPUVirt == "":
		add("kvm", "KVM device", Error, "/dev/kvm is missing")
	default:
		add("kvm", "KVM device", Error, "/dev/kvm is missing although the CPU supports virtualisation; load the kvm_intel or kvm_amd module, or check the firmware")
	}

	// Nested virtualisation: information, only when this host is itself a VM.
	if f.Hypervisor != "" {
		switch f.Nested {
		case "Y", "y", "1":
			add("nested", "Nested virtualisation", Info, "This host is a VM; nested KVM is on, guests will run with hardware acceleration")
		case "":
			add("nested", "Nested virtualisation", Info, "This host is a VM; the kvm module is not loaded yet, so nested KVM could not be read")
		default:
			add("nested", "Nested virtualisation", Info, "This host is a VM; nested KVM is off in the kvm module")
		}
	}

	// IOMMU: information, for device passthrough.
	switch {
	case f.IOMMUGroups > 0:
		add("iommu", "IOMMU", Info, fmt.Sprintf("On (%d groups); device passthrough is possible", f.IOMMUGroups))
	case f.CPUVirt == "svm":
		add("iommu", "IOMMU", Info, "Off. For device passthrough add amd_iommu=on iommu=pt to the kernel command line and reboot; prepare does not change it and never reboots")
	default:
		add("iommu", "IOMMU", Info, "Off. For device passthrough add intel_iommu=on iommu=pt to the kernel command line and reboot; prepare does not change it and never reboots")
	}

	// Space under the default pool path
	switch {
	case f.FreeBytes == 0:
		add("space", "Free space", Warning, "could not read the free space under "+DefaultPoolPath)
	case f.FreeBytes < 10*gib:
		add("space", "Free space", Warning, fmt.Sprintf("%s free under %s; less than 10 GiB", gibText(f.FreeBytes), DefaultPoolPath))
	default:
		add("space", "Free space", OK, fmt.Sprintf("%s free under %s", gibText(f.FreeBytes), DefaultPoolPath))
	}

	// SELinux / AppArmor
	switch {
	case f.SELinux != "":
		add("mac", "Security module", Info, "SELinux "+strings.ToLower(f.SELinux))
	case strings.HasPrefix(strings.ToUpper(f.AppArmor), "Y"):
		add("mac", "Security module", Info, "AppArmor enabled")
	default:
		add("mac", "Security module", Info, "No SELinux or AppArmor active")
	}

	// Time
	switch f.TimeSync {
	case "yes":
		add("time", "Time sync", OK, "The clock is synchronised")
	case "no":
		add("time", "Time sync", Warning, "The clock is not synchronised; certificates, snapshots and migration need correct time")
	default:
		add("time", "Time sync", Warning, "Could not tell whether time sync is active (no timedatectl)")
	}

	// libvirt
	if f.Virsh != "" {
		add("libvirt", "libvirt", OK, "Installed, version "+f.LibvirtVersion)
	} else {
		add("libvirt", "libvirt", Info, "Not installed; the packages below would be installed")
	}

	// Another controller
	switch {
	case len(f.Controllers) == 0:
		add("controller", "Other controller", OK, "No sign of another controller")
	default:
		other := []string{}
		for _, id := range f.Controllers {
			if id != o.InstanceID {
				other = append(other, id)
			}
		}
		if len(other) == 0 {
			add("controller", "Other controller", OK, "Only this controller has taken scheduled snapshots here")
		} else {
			add("controller", "Other controller", Warning, "Another controller ("+strings.Join(other, ", ")+") runs snapshot schedules on this host; two controllers would both run them")
		}
	}

	// Existing VMs
	if f.Virsh != "" {
		add("vms", "Existing VMs", Info, fmt.Sprintf("%d. Existing VMs are left alone", f.Domains))
	}

	// Packages
	switch {
	case d.Family == FamilyOther:
		// the distribution row says it
	case len(p.Missing) == 0:
		add("packages", "Packages", OK, "All required packages are installed")
	default:
		add("packages", "Packages", Info, "To install: "+strings.Join(p.Missing, " "))
	}

	// Service account
	rows = append(rows, accountRows(p, o)...)
	return rows
}

func gibText(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/gib) }

// noLogin reports whether a login shell refuses logins.
func noLogin(shell string) bool {
	return shell == "" || strings.HasSuffix(shell, "/nologin") || strings.HasSuffix(shell, "/false")
}

// accountRows says what would be done about the service account.
func accountRows(p Plan, o Options) []Row {
	var rows []Row
	add := func(id, title, status, detail string) {
		rows = append(rows, Row{ID: id, Title: title, Status: status, Detail: detail})
	}
	a, u := p.Account, o.ServiceUser()
	switch {
	case o.AllowRoot:
		add("account", "Service account", Warning, "The controller will sign in as root. A service account is safer; leave the root option off to use one")
	case o.UseExisting && !a.Exists:
		add("account", "Service account", Error, fmt.Sprintf("The account %s does not exist. Leave out --use-existing-user and prepare creates it", u))
	case o.UseExisting:
		var missing []string
		for _, g := range []string{"libvirt", "kvm"} {
			exists := g == "libvirt" && a.GroupLibvirt || g == "kvm" && a.GroupKVM
			// The libvirt group appears with the packages, so it counts as wanted even before.
			if !a.Groups[g] && (exists || g == "libvirt") {
				missing = append(missing, g)
			}
		}
		if len(missing) > 0 {
			if o.Confirm {
				add("account", "Service account", Info, fmt.Sprintf("%s is not in the group %s; prepare will add it", u, strings.Join(missing, " and ")))
			} else {
				add("account", "Service account", Warning, fmt.Sprintf("%s is not in the group %s, so it cannot use libvirt. Add --confirm and prepare adds it", u, strings.Join(missing, " and ")))
			}
		} else {
			add("account", "Service account", OK, u+" exists and is in the groups libvirt and kvm (where they exist)")
		}
		if rule := u + " ALL=(root) NOPASSWD: " + nodeCmdPath; !strings.Contains(p.Facts.SudoersRaw, rule) {
			if o.Confirm {
				add("sudo", "Sudo rule", Info, "There is no sudo rule for "+u+" yet; prepare will add one that allows the node command only")
			} else {
				add("sudo", "Sudo rule", Warning, "There is no sudo rule that lets "+u+" run the node command. Add --confirm and prepare adds one, or add it yourself")
			}
		}
		if noLogin(a.Shell) {
			add("shell", "Login shell", Error, fmt.Sprintf("%s has the login shell %s, so the controller cannot sign in as it. Give it a login shell such as /bin/sh, or let prepare create its own account (leave out --use-existing-user)", u, a.Shell))
		}
	case a.Exists && strings.Contains(a.Comment, AccountComment):
		add("account", "Service account", changedStatus(accountFixes(a, u)), markedAccountText(a, u))
	case a.Exists:
		msg := fmt.Sprintf("An account named %s already exists and was not made by Virticle, so prepare leaves it alone. Choose another name with --user OTHERNAME on the command line", u)
		if u == LegacyUser {
			msg = "The account virticle belongs to the Virticle controller and prepare does not use it for a node. Leave --user out and prepare makes the account " + DefaultUser
		}
		add("account", "Service account", Error, msg)
	default:
		add("account", "Service account", Info, fmt.Sprintf("The account %s will be created: a system account with the home %s, the login shell /bin/sh and no password, in the groups libvirt and kvm", u, HomeFor(u)))
	}
	return rows
}

func changedStatus(fixes []string) string {
	if len(fixes) > 0 {
		return Info
	}
	return OK
}

// accountFixes lists what prepare would still change on an account of ours.
func accountFixes(a Account, u string) []string {
	var f []string
	if !a.Groups["libvirt"] {
		f = append(f, "add it to the group libvirt")
	}
	if a.GroupKVM && !a.Groups["kvm"] {
		f = append(f, "add it to the group kvm")
	}
	if noLogin(a.Shell) {
		f = append(f, "give it the login shell /bin/sh (now "+a.Shell+")")
	}
	switch {
	case a.HomeOwner == "":
		f = append(f, "create its home directory "+a.Home)
	case a.HomeOwner != u:
		f = append(f, "give its home directory to it")
	}
	return f
}

func markedAccountText(a Account, u string) string {
	old := ""
	if u == LegacyUser {
		old = " It was made by an earlier version of Virticle and is kept, so no second account is made."
	}
	if f := accountFixes(a, u); len(f) > 0 {
		return fmt.Sprintf("The account %s exists. Prepare will %s.%s", u, strings.Join(f, ", "), old)
	}
	return fmt.Sprintf("The account %s exists and is set up.%s", u, old)
}

// HasError reports whether any row is an error.
func HasError(rows []Row) bool {
	for _, r := range rows {
		if r.Status == Error {
			return true
		}
	}
	return false
}
