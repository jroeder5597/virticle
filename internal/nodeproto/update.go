package nodeproto

import (
	"errors"
	"strings"
)

// Package updates on a node: what the verbs update-start, update-status and
// selfcheck exchange with the controller, and the pure parsing both sides use.

// Pending is one package that has a newer version available.
type Pending struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	From    string `json:"from,omitempty"`
	Source  string `json:"source,omitempty"`
}

// Update run states, in UpdateStatus.State.
const (
	UpdateNone    = "none" // no run was started since the node came up (or ever)
	UpdateRunning = "running"
	UpdateOK      = "ok"
	UpdateFailed  = "failed"
)

// UpdateStatus is what `update-status` prints (one JSON object).
type UpdateStatus struct {
	State    string `json:"state"`
	Family   string `json:"family,omitempty"` // dnf | apt
	Started  int64  `json:"started,omitempty"`
	Finished int64  `json:"finished,omitempty"`
	// Before lists the packages that had an update when the run began, Updated the ones that are installed now,
	// HeldBack the ones that still have an update afterwards (held, excluded, or kept back by the package tool).
	Before   []string `json:"before,omitempty"`
	Updated  []string `json:"updated,omitempty"`
	HeldBack []string `json:"heldBack,omitempty"`
	// RebootNeeded and RebootWhy are the node's own verdict after the run.
	RebootNeeded bool   `json:"rebootNeeded,omitempty"`
	RebootWhy    string `json:"rebootWhy,omitempty"`
	// Phase says what a running update is doing.
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
	// Log is the end of the package tool's output.
	Log string `json:"log,omitempty"`
}

// Self-check statuses, in the first column of a `selfcheck` line.
const (
	CheckOK      = "ok"
	CheckFixed   = "fixed"
	CheckProblem = "problem"
)

// CheckRow is one line of `selfcheck`: STATUS, a tab, ID, a tab, the detail.
type CheckRow struct {
	Status, ID, Detail string
}

// ParseCheckRows reads the output of `selfcheck`.
func ParseCheckRows(out string) []CheckRow {
	var rows []CheckRow
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimRight(l, "\r"), "\t", 3)
		if len(f) < 2 || (f[0] != CheckOK && f[0] != CheckFixed && f[0] != CheckProblem) {
			continue
		}
		r := CheckRow{Status: f[0], ID: f[1]}
		if len(f) == 3 {
			r.Detail = f[2]
		}
		rows = append(rows, r)
	}
	return rows
}

// RebootPackage says why an update of the package asks for a restart of the node, or "" when it does not. Besides the
// kernel and the base of the system this includes libvirt and QEMU: the node has no VMs running while it is updated, and
// a restart is the one way to be sure that nothing keeps running the old programs.
func RebootPackage(name string) string {
	n := strings.ToLower(name)
	// dnf writes name.arch
	if i := strings.LastIndex(n, "."); i > 0 && archSuffix(n[i+1:]) {
		n = n[:i]
	}
	switch {
	case n == "kernel" || n == "kernel-core" || strings.HasPrefix(n, "kernel-modules") || n == "kernel-rt" || n == "kernel-uek":
		return "a new kernel"
	case strings.HasPrefix(n, "linux-image-") || strings.HasPrefix(n, "linux-modules-") || strings.HasPrefix(n, "linux-signed-image"):
		return "a new kernel"
	case n == "linux-firmware" || strings.HasPrefix(n, "linux-firmware-") || n == "firmware-linux" || n == "microcode_ctl" || strings.HasSuffix(n, "-microcode"):
		return "new firmware or CPU microcode"
	case n == "glibc" || n == "libc6" || n == "systemd" || n == "dbus-broker" || n == "dbus-daemon" || n == "dbus":
		return "a core system library or service"
	case strings.HasPrefix(n, "libvirt") || strings.HasPrefix(n, "qemu-kvm") || strings.HasPrefix(n, "qemu-system"):
		return "a new libvirt or QEMU"
	}
	return ""
}

func archSuffix(s string) bool {
	switch s {
	case "x86_64", "noarch", "aarch64", "i686", "ppc64le", "s390x", "src":
		return true
	}
	return false
}

// RebootReasons returns the distinct reasons RebootPackage gives for the named packages, in a stable order.
func RebootReasons(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if w := RebootPackage(n); w != "" && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// ParseApt reads `apt list --upgradable`:
//
//	Listing...
//	libssl3/stable-security 3.5.1-1 amd64 [upgradable from: 3.5.0-1]
func ParseApt(out string) []Pending {
	res := []Pending{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "Listing") || strings.HasPrefix(l, "WARNING") || strings.HasPrefix(l, "N:") {
			continue
		}
		name, rest, ok := strings.Cut(l, "/")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 2 {
			continue
		}
		u := Pending{Name: name, Source: f[0], Version: f[1]}
		if i := strings.Index(l, "[upgradable from: "); i >= 0 {
			u.From = strings.TrimSuffix(l[i+len("[upgradable from: "):], "]")
		}
		res = append(res, u)
	}
	return res
}

// ErrDnfOutput is returned by ParseDnf for text it cannot read.
var ErrDnfOutput = errors.New("unexpected dnf output")

// ParseDnf reads `dnf -q check-update` followed by a line "__rc=N". Exit
// status 100 means updates are available, 0 none; anything else is an error
// that names the status.
func ParseDnf(out string) ([]Pending, error) {
	i := strings.LastIndex(out, "__rc=")
	if i < 0 {
		return nil, ErrDnfOutput
	}
	rc := strings.TrimSpace(out[i+len("__rc="):])
	body := out[:i]
	res := []Pending{}
	switch rc {
	case "0":
		return res, nil
	case "100":
	default:
		msg := "dnf check-update exited with status " + rc
		var why []string
		for _, l := range strings.Split(body, "\n") {
			if t, ok := strings.CutPrefix(strings.TrimSpace(l), "__err="); ok && t != "" {
				why = append(why, t)
			}
		}
		if len(why) > 0 {
			msg += ": " + strings.Join(why, " ")
		}
		return nil, errors.New(msg)
	}
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "Obsoleting Packages") {
			break
		}
		f := strings.Fields(l)
		if len(f) != 3 || !strings.Contains(f[0], ".") {
			continue
		}
		res = append(res, Pending{Name: f[0], Version: f[1], Source: f[2]})
	}
	return res, nil
}

// Names lists the package names of a pending list.
func Names(p []Pending) []string {
	out := make([]string, len(p))
	for i, x := range p {
		out[i] = x.Name
	}
	return out
}

// KernelNewer reports whether the installed kernel version newest is newer than the running one, comparing the
// numbers in them (6.12.0-211.74.1 against 6.12.0-211.9.1). Equal or unreadable versions give false.
func KernelNewer(running, newest string) bool {
	if running == "" || newest == "" || running == newest {
		return false
	}
	a, b := versionParts(running), versionParts(newest)
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return b[i] > a[i]
		}
	}
	return len(b) > len(a)
}

func versionParts(v string) []int {
	var out []int
	n, in := 0, false
	for _, r := range v {
		if r >= '0' && r <= '9' {
			n, in = n*10+int(r-'0'), true
			continue
		}
		if in {
			out = append(out, n)
			n, in = 0, false
		}
	}
	if in {
		out = append(out, n)
	}
	return out
}
