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

// accountRows says what would be done about the service account.
func accountRows(p Plan, o Options) []Row {
	var rows []Row
	add := func(id, title, status, detail string) {
		rows = append(rows, Row{ID: id, Title: title, Status: status, Detail: detail})
	}
	a, u := p.Account, o.ServiceUser()
	switch {
	case o.AllowRoot:
		add("account", "Service account", Warning, "Uses root; a dedicated account is safer")
	case o.UseExisting && !a.Exists:
		add("account", "Service account", Error, fmt.Sprintf("The account %s does not exist (drop use-existing-user to create it)", u))
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
			st := Warning
			hint := "; rerun with the confirm option to add them"
			if o.Confirm {
				st, hint = Info, "; they will be added"
			}
			add("account", "Service account", st, fmt.Sprintf("%s exists but is not in the group(s) %s%s", u, strings.Join(missing, ", "), hint))
		} else {
			add("account", "Service account", OK, u+" exists and is in the libvirt and kvm groups where they exist")
		}
		if rule := u + " ALL=(root) NOPASSWD: " + helperPath; !o.BroadSudo && !strings.Contains(p.Facts.SudoersRaw, rule) {
			st, hint := Warning, "; rerun with the confirm option to add it"
			if o.Confirm {
				st, hint = Info, "; it will be added"
			}
			add("sudo", "Sudo rule", st, "No sudo rule lets "+u+" run the helper"+hint)
		}
		if a.Shell == "/usr/sbin/nologin" || a.Shell == "/sbin/nologin" || a.Shell == "/bin/false" {
			add("shell", "Login shell", Error, fmt.Sprintf("%s has the shell %s, so the controller cannot run commands as it", u, a.Shell))
		}
	case a.Exists:
		if strings.Contains(a.Comment, AccountComment) {
			add("account", "Service account", OK, u+" exists")
		} else {
			add("account", "Service account", Error, fmt.Sprintf("An account named %s exists and was not created by this tool; pick another name or use use-existing-user", u))
		}
	default:
		add("account", "Service account", Info, fmt.Sprintf("%s would be created (home %s, shell /bin/sh, no password)", u, DefaultHome))
	}
	return rows
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
