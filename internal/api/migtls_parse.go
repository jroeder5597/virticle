package api

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"

	"virt-proj/internal/migpki"
)

// Pure parts of the encrypted-migration support: reading what the node tells
// us, and deciding about the pre-flight check. The node I/O is in migtls.go.

const defaultCertDir = "/etc/pki/qemu"

// nodeEnv is what one look at a node reveals.
type nodeEnv struct {
	UID      string
	Conf     map[string]string // uncommented qemu.conf settings of interest
	FQDN     string
	Addrs    []net.IP
	Users    map[string]bool // libvirt-qemu / qemu accounts that exist
	Restore  bool            // restorecon is available (SELinux)
	ConfRead bool
}

// envScript prints the facts parseNodeEnv understands. It always succeeds.
const envScript = `echo '@@uid'; id -u
echo '@@conf'; grep -E '^[[:space:]]*(user|default_tls_x509_cert_dir|default_tls_x509_verify|migrate_tls_x509_cert_dir|migrate_tls_x509_verify|migrate_tls_force)[[:space:]]*=' /etc/libvirt/qemu.conf 2>/dev/null
echo '@@fqdn'; hostname -f 2>/dev/null
echo '@@addr'; ip -o addr show scope global 2>/dev/null
echo '@@pw'; getent passwd libvirt-qemu qemu 2>/dev/null
echo '@@se'; command -v restorecon
echo '@@conffile'; [ -r /etc/libvirt/qemu.conf ] && echo yes
true`

func sections(out string) map[string][]string {
	m := map[string][]string{}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "@@") {
			cur = strings.TrimPrefix(l, "@@")
			if _, ok := m[cur]; !ok {
				m[cur] = []string{}
			}
			continue
		}
		if cur != "" {
			m[cur] = append(m[cur], l)
		}
	}
	return m
}

var confLineRE = regexp.MustCompile(`^\s*([a-z0-9_]+)\s*=\s*(?:"([^"]*)"|([^\s#]+))`)

func parseQemuConf(lines []string) map[string]string {
	m := map[string]string{}
	for _, l := range lines {
		if g := confLineRE.FindStringSubmatch(l); g != nil {
			v := g[2]
			if v == "" {
				v = g[3]
			}
			m[g[1]] = v
		}
	}
	return m
}

// Interfaces whose addresses are not how another node reaches this one.
var skipIfRE = regexp.MustCompile(`^(virbr|docker|veth|br-|vnet|cni|flannel|tap|lxc|lxd|podman)`)

func parseAddrs(lines []string) []net.IP {
	var out []net.IP
	for _, l := range lines {
		f := strings.Fields(l)
		// "2: eth0    inet 192.168.1.5/24 brd ..."
		if len(f) < 4 || (f[2] != "inet" && f[2] != "inet6") || skipIfRE.MatchString(strings.TrimSuffix(f[1], ":")) {
			continue
		}
		ip, _, err := net.ParseCIDR(f[3])
		if err != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func parseNodeEnv(out string) nodeEnv {
	sec := sections(out)
	e := nodeEnv{Conf: parseQemuConf(sec["conf"]), Users: map[string]bool{}, Addrs: parseAddrs(sec["addr"])}
	if l := sec["uid"]; len(l) > 0 {
		e.UID = strings.TrimSpace(l[0])
	}
	if l := sec["fqdn"]; len(l) > 0 {
		e.FQDN = strings.TrimSpace(l[0])
	}
	for _, l := range sec["pw"] {
		if name, _, ok := strings.Cut(l, ":"); ok && name != "" {
			e.Users[name] = true
		}
	}
	e.Restore = len(sec["se"]) > 0 && strings.TrimSpace(sec["se"][0]) != ""
	e.ConfRead = len(sec["conffile"]) > 0 && strings.TrimSpace(sec["conffile"][0]) == "yes"
	return e
}

var numericUserRE = regexp.MustCompile(`^\+?[0-9]+$`)

// certDir is the directory libvirt hands QEMU for migration: the migrate
// setting, else the default one, else /etc/pki/qemu.
func (e nodeEnv) certDir() (string, error) {
	d := e.Conf["migrate_tls_x509_cert_dir"]
	if d == "" {
		d = e.Conf["default_tls_x509_cert_dir"]
	}
	if d == "" {
		d = defaultCertDir
	}
	if !safePathRE.MatchString(d) || path.Clean(d) != d || d == "/" {
		return "", fmt.Errorf("the certificate directory %q configured in qemu.conf is not supported (use a plain absolute path)", d)
	}
	return d, nil
}

// qemuUser is the account QEMU processes run as, which must be able to read
// the private key. Without a user in qemu.conf the distribution default
// applies: libvirt-qemu on Debian, qemu on RHEL.
func (e nodeEnv) qemuUser() (string, error) {
	if u := e.Conf["user"]; u != "" {
		switch {
		case numericUserRE.MatchString(u):
			return strings.TrimPrefix(u, "+"), nil
		case userRE.MatchString(u):
			return u, nil
		}
		return "", fmt.Errorf("unsupported user %q in qemu.conf", u)
	}
	for _, u := range []string{"libvirt-qemu", "qemu"} {
		if e.Users[u] {
			return u, nil
		}
	}
	return "", fmt.Errorf("cannot tell which account QEMU runs as (no libvirt-qemu or qemu user, none set in qemu.conf)")
}

// notes are facts about the node's qemu.conf the user should know.
func (e nodeEnv) notes(dir string) []string {
	var n []string
	if d := e.Conf["migrate_tls_x509_cert_dir"]; d != "" {
		n = append(n, "qemu.conf sets migrate_tls_x509_cert_dir = "+d+"; the certificates are installed there")
	} else if d := e.Conf["default_tls_x509_cert_dir"]; d != "" {
		n = append(n, "qemu.conf sets default_tls_x509_cert_dir = "+d+"; the certificates are installed there")
	}
	for _, k := range []string{"migrate_tls_x509_verify", "default_tls_x509_verify"} {
		if v := e.Conf[k]; v == "0" || strings.EqualFold(v, "false") {
			n = append(n, "qemu.conf sets "+k+" = 0: streams are encrypted but QEMU does not check the peer's certificate")
			break
		}
	}
	if v := e.Conf["migrate_tls_force"]; v == "1" {
		n = append(n, "qemu.conf sets migrate_tls_force = 1: libvirt rejects unencrypted migrations on this node, so \"encrypt: false\" cannot work here")
	}
	return n
}

// certNames collects what the node's certificate must cover: the host name
// libvirt reports (the name QEMU verifies), the FQDN, the address the
// controller reaches the node at, and the node's own addresses.
func certNames(hostname string, e nodeEnv, nodeAddrs ...string) migpki.Names {
	var n migpki.Names
	addDNS := func(h string) {
		if h != "" && hostRE.MatchString(h) {
			n.Hosts = append(n.Hosts, h)
		}
	}
	addDNS(hostname)
	addDNS(e.FQDN)
	for _, a := range nodeAddrs {
		a, _ = splitAddr(a)
		if ip := net.ParseIP(a); ip != nil {
			n.IPs = append(n.IPs, ip)
		} else {
			addDNS(a)
		}
	}
	n.IPs = append(n.IPs, e.Addrs...)
	return n
}

// ancestors lists dir and its parents, outermost first, without "/".
func ancestors(dir string) []string {
	var out []string
	for d := dir; d != "/" && d != "."; d = path.Dir(d) {
		out = append([]string{d}, out...)
	}
	return out
}

// stateScript prints the certificate directory of a node in the form
// parseState reads.
func stateScript(dir string) string {
	var b strings.Builder
	b.WriteString("d=" + shq(dir) + "\n")
	b.WriteString("for a in")
	for _, a := range ancestors(dir) {
		b.WriteString(" " + shq(a))
	}
	b.WriteString(`; do if [ -d "$a" ]; then echo "@@dir $a 1"; else echo "@@dir $a 0"; fi; done` + "\n")
	b.WriteString(`for f in server-key.pem client-key.pem ca-cert.pem server-cert.pem client-cert.pem .virt-proj-migration.json; do p="$d/$f"; if [ -e "$p" ]; then echo "@@file $f $(sha256sum < "$p" | cut -d' ' -f1) $(stat -c '%U:%G %a' "$p")"; fi; done` + "\n")
	b.WriteString(`for c in ca-cert.pem:ca server-cert.pem:server client-cert.pem:client .virt-proj-migration.json:marker; do f=${c%%:*}; n=${c##*:}; echo "@@content $n"; cat "$d/$f" 2>/dev/null; done` + "\n")
	b.WriteString(`echo "@@content ls"; ls -A "$d" 2>/dev/null` + "\n")
	b.WriteString("echo '@@end'\ntrue")
	return b.String()
}

// nodeState is the parsed output of stateScript.
type nodeState struct {
	snap      migpki.Snapshot
	dirExists map[string]bool // ancestors that exist
	listing   map[string]bool // names in the directory
}

func parseState(out, dir, hostname string) (nodeState, error) {
	st := nodeState{
		snap:      migpki.Snapshot{Dir: dir, Hostname: hostname, Files: map[string]migpki.FileState{}},
		dirExists: map[string]bool{}, listing: map[string]bool{},
	}
	content := map[string][]string{}
	cur := ""
	ended := false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case l == "@@end":
			ended = true
			cur = ""
		case strings.HasPrefix(l, "@@dir "):
			f := strings.Fields(l)
			if len(f) == 3 {
				st.dirExists[f[1]] = f[2] == "1"
			}
		case strings.HasPrefix(l, "@@file "):
			f := strings.Fields(l)
			if len(f) == 5 {
				st.snap.Files[f[1]] = migpki.FileState{Exists: true, SHA256: f[2], Owner: f[3], Mode: f[4]}
			}
		case strings.HasPrefix(l, "@@content "):
			cur = strings.TrimPrefix(l, "@@content ")
			content[cur] = []string{}
		default:
			if cur != "" {
				content[cur] = append(content[cur], l)
			}
		}
	}
	if !ended {
		return st, fmt.Errorf("incomplete answer from the node: %s", out)
	}
	join := func(k string) []byte {
		s := strings.TrimSpace(strings.Join(content[k], "\n"))
		if s == "" {
			return nil
		}
		return []byte(s + "\n")
	}
	st.snap.CACert, st.snap.Server, st.snap.Client = join("ca"), join("server"), join("client")
	for _, l := range content["ls"] {
		if l = strings.TrimSpace(l); l != "" {
			st.listing[l] = true
		}
	}
	if mk := join("marker"); mk != nil {
		m, err := parseMarker(mk)
		if err != nil {
			st.snap.MarkerBad = true
		} else {
			st.snap.Marker = m
		}
	}
	return st, nil
}

// --- the pre-flight check ---

type encInput struct {
	Mode             string // live | suspended | offline
	Request          *bool
	SrcName, DstName string
	Src, Dst         migpki.Status
	SrcErr, DstErr   error // the node could not be read
}

func (in encInput) problems() []string {
	var p []string
	for _, n := range []struct {
		name string
		st   migpki.Status
		err  error
	}{{in.SrcName, in.Src, in.SrcErr}, {in.DstName, in.Dst, in.DstErr}} {
		switch {
		case n.err != nil:
			p = append(p, n.name+": cannot be checked ("+n.err.Error()+")")
		case n.st.State == migpki.StateNotSetUp:
			p = append(p, n.name+": not set up")
		case !n.st.Usable():
			msg := n.name + ": " + n.st.State
			if len(n.st.Problems) > 0 {
				msg += " (" + strings.Join(n.st.Problems, "; ") + ")"
			}
			p = append(p, msg)
		}
	}
	return p
}

const setupHint = "Use \"Set up encrypted migration\" on the Nodes page for these nodes (or POST /api/nodes/{node}/migration-tls)."

// encryptionCheck decides whether the memory and disk streams are encrypted
// and what the pre-flight check says about it.
func encryptionCheck(in encInput) (useTLS bool, c migCheck) {
	const name = "Encryption"
	if in.Mode == "offline" {
		return false, migCheck{name, true, "info", "offline migration copies the disks with scp over SSH, which is encrypted; no TLS setup is needed"}
	}
	probs := in.problems()
	if in.Request != nil && !*in.Request {
		return false, migCheck{name, false, "warning", fmt.Sprintf("encryption was switched off for this migration: migration data is sent unencrypted between %s and %s", in.SrcName, in.DstName)}
	}
	if len(probs) == 0 {
		msg := "memory and disk data are encrypted with TLS between " + in.SrcName + " and " + in.DstName + " (certificates issued by this controller's CA)"
		for _, n := range []struct {
			name string
			st   migpki.Status
		}{{in.SrcName, in.Src}, {in.DstName, in.Dst}} {
			if n.st.State == migpki.StateExpiring {
				msg += fmt.Sprintf("; the certificate of %s expires in %d days, issue it again soon", n.name, n.st.DaysLeft)
			}
		}
		return true, migCheck{name, true, "info", msg}
	}
	if in.Request != nil && *in.Request {
		return false, migCheck{name, false, "error", "encryption was requested but is not possible: " + strings.Join(probs, "; ") + ". " + setupHint}
	}
	return false, migCheck{name, false, "warning", fmt.Sprintf("migration data is sent unencrypted between %s and %s (%s). %s", in.SrcName, in.DstName, strings.Join(probs, "; "), setupHint)}
}
