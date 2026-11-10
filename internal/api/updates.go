package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"virt-proj/internal/hostconn"
)

// Pending host updates. Strictly read-only: the commands below only list what
// the node already knows about. `apt update` is never run, so the list is as
// fresh as the node's last metadata refresh (which the response says), and
// nothing is ever installed.

const updatesTTL = time.Hour

type pendingUpdate struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	From    string `json:"from,omitempty"`
	Source  string `json:"source,omitempty"`
}

type hostUpdates struct {
	Family         string          `json:"family"` // apt | dnf | unsupported
	Distro         string          `json:"distro,omitempty"`
	Updates        []pendingUpdate `json:"updates"`
	RebootRequired bool            `json:"rebootRequired"`
	RebootReason   string          `json:"rebootReason,omitempty"`
	RunningKernel  string          `json:"runningKernel,omitempty"`
	NewestKernel   string          `json:"newestKernel,omitempty"`
	MetadataAt     int64           `json:"metadataAt,omitempty"` // Unix time of the node's last package list refresh
	CheckedAt      int64           `json:"checkedAt"`
	Note           string          `json:"note,omitempty"`
	Error          string          `json:"error,omitempty"`
}

var updatesCache = struct {
	sync.Mutex
	m map[int64]hostUpdates
}{m: map[int64]hostUpdates{}}

func (s *Server) nodeUpdates(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	updatesCache.Lock()
	c, ok := updatesCache.m[id]
	updatesCache.Unlock()
	if !ok || refresh || time.Since(time.Unix(c.CheckedAt, 0)) > updatesTTL {
		c = collectUpdates(h, time.Now())
		if c.Error == "" {
			updatesCache.Lock()
			updatesCache.m[id] = c
			updatesCache.Unlock()
		}
	}
	writeJSON(w, c)
}

// Every command here is fixed text; nothing from the request reaches a shell.
const (
	cmdOSRelease = `cat /etc/os-release 2>/dev/null; true`
	cmdAptList   = `apt list --upgradable 2>/dev/null; true`
	cmdAptStamp  = `stat -c %Y /var/lib/apt/periodic/update-success-stamp 2>/dev/null || stat -c %Y /var/lib/apt/lists 2>/dev/null; true`
	cmdDnfCheck  = `dnf -q check-update 2>/dev/null; echo "__rc=$?"`
	cmdKernelDeb = `echo "running=$(uname -r)"; echo "newest=$(ls /boot/vmlinuz-* 2>/dev/null | sed 's#.*/vmlinuz-##' | sort -V | tail -1)"; ` +
		`if [ -e /var/run/reboot-required ]; then echo "flag=1"; cat /var/run/reboot-required.pkgs 2>/dev/null | sed 's/^/pkg=/'; fi; true`
	cmdKernelRPM = `echo "running=$(uname -r)"; echo "newest=$(rpm -q kernel-core kernel --qf '%{VERSION}-%{RELEASE}.%{ARCH}\n' 2>/dev/null | grep -v 'not installed' | sort -V | tail -1)"; ` +
		`if command -v needs-restarting >/dev/null 2>&1 && ! needs-restarting -r >/dev/null 2>&1; then echo "flag=1"; fi; true`
)

func collectUpdates(h *hostconn.Host, now time.Time) hostUpdates {
	res := hostUpdates{Updates: []pendingUpdate{}, CheckedAt: now.Unix()}
	osr, err := h.Run(cmdOSRelease)
	if err != nil {
		res.Error = "cannot read /etc/os-release: " + err.Error()
		return res
	}
	res.Family, res.Distro = osFamily(osr)
	var kernelCmd string
	switch res.Family {
	case "apt":
		out, err := h.Run(cmdAptList)
		if err != nil {
			res.Error = "apt list failed: " + err.Error() + " " + out
			return res
		}
		res.Updates = parseAptUpgradable(out)
		if st, _ := h.Run(cmdAptStamp); st != "" {
			if ts, err := strconv.ParseInt(firstLine(st), 10, 64); err == nil {
				res.MetadataAt = ts
			}
		}
		res.Note = "Reflects the package lists as of the node's last apt update; nothing is refreshed or installed from here."
		kernelCmd = cmdKernelDeb
	case "dnf":
		out, err := h.Run(cmdDnfCheck)
		if err != nil {
			res.Error = "dnf check-update failed: " + err.Error()
			return res
		}
		var perr error
		res.Updates, perr = parseDnfCheck(out)
		if perr != nil {
			res.Error = perr.Error()
			return res
		}
		res.Note = "Reflects the repository metadata dnf has cached on the node; nothing is installed from here."
		kernelCmd = cmdKernelRPM
	default:
		res.Family = "unsupported"
		res.Note = "Pending updates are only read for apt and dnf based systems."
		return res
	}
	kout, err := h.Run(kernelCmd)
	if err == nil {
		applyKernelInfo(&res, kout)
	}
	return res
}

// osFamily maps /etc/os-release to the package manager family.
func osFamily(osr string) (family, pretty string) {
	vals := map[string]string{}
	for _, l := range strings.Split(osr, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok {
			vals[k] = strings.Trim(v, `"'`)
		}
	}
	pretty = vals["PRETTY_NAME"]
	ids := strings.Fields(strings.ToLower(vals["ID"] + " " + vals["ID_LIKE"]))
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "apt", pretty
		case "rhel", "fedora", "centos", "rocky", "almalinux":
			return "dnf", pretty
		}
	}
	return "unsupported", pretty
}

// parseAptUpgradable reads `apt list --upgradable`:
//
//	Listing...
//	libssl3/stable-security 3.5.1-1 amd64 [upgradable from: 3.5.0-1]
func parseAptUpgradable(out string) []pendingUpdate {
	res := []pendingUpdate{}
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
		u := pendingUpdate{Name: name, Source: f[0], Version: f[1]}
		if i := strings.Index(l, "[upgradable from: "); i >= 0 {
			u.From = strings.TrimSuffix(l[i+len("[upgradable from: "):], "]")
		}
		res = append(res, u)
	}
	return res
}

// parseDnfCheck reads `dnf -q check-update` followed by "__rc=N". Exit code
// 100 means updates are available, 0 none; anything else is an error.
func parseDnfCheck(out string) ([]pendingUpdate, error) {
	i := strings.LastIndex(out, "__rc=")
	if i < 0 {
		return nil, badRequest("unexpected dnf output")
	}
	rc := strings.TrimSpace(out[i+len("__rc="):])
	body := out[:i]
	res := []pendingUpdate{}
	switch rc {
	case "0":
		return res, nil
	case "100":
	default:
		return nil, badRequest("dnf check-update exited with status " + rc)
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
		res = append(res, pendingUpdate{Name: f[0], Version: f[1], Source: f[2]})
	}
	return res, nil
}

func applyKernelInfo(res *hostUpdates, out string) {
	var pkgs []string
	flag := false
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch k {
		case "running":
			res.RunningKernel = v
		case "newest":
			res.NewestKernel = v
		case "flag":
			flag = v == "1"
		case "pkg":
			pkgs = append(pkgs, v)
		}
	}
	switch {
	case flag:
		res.RebootRequired = true
		res.RebootReason = "the system reports that a reboot is required"
		if len(pkgs) > 0 {
			res.RebootReason += " (" + strings.Join(pkgs, ", ") + ")"
		}
	case res.NewestKernel != "" && res.RunningKernel != "" && res.NewestKernel != res.RunningKernel:
		res.RebootRequired = true
		res.RebootReason = "running kernel " + res.RunningKernel + " differs from the newest installed " + res.NewestKernel
	}
}
