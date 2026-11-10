package api

// Encrypted migration streams.
//
// QEMU's migration data (guest memory) and the NBD disk mirror travel node to
// node over plain TCP. libvirt encrypts both with TLS when the migration is
// started with VIR_MIGRATE_TLS: QEMU then reads CA, certificate and key from
// /etc/pki/qemu (or the directory qemu.conf names) on both nodes and
// authenticates the peer. Tunnelled migration is not an alternative: libvirt
// 11.3 refuses it together with a disk copy ("migration of non-shared storage
// is not supported with tunnelled migration and this QEMU").
//
// The controller keeps a small CA (internal/migpki). The admin action "set up
// encrypted migration" issues a certificate for one node and installs it with
// ordinary SSH commands; nothing runs on the node afterwards and qemu.conf is
// not touched. A marker file next to the certificates records what was
// installed so that status, re-issue and removal know which files are ours.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"virt-proj/internal/hostconn"
	"virt-proj/internal/migpki"
)

const (
	migTLSDirName   = "migration-ca"
	migBackupSuffix = ".virt-proj-orig"
	tmpSuffix       = ".vp-tmp"
	migTLSPublicMsg = "Key material is generated on the controller and sent to the node over the existing SSH connection (on standard input of an install command, never on a command line)."
)

// migCA returns the controller's migration CA. create makes it when missing;
// reading the status never does.
func (s *Server) migCA(create bool) (*migpki.CA, error) {
	if s.DataDir == "" {
		return nil, errors.New("the controller has no data directory configured")
	}
	dir := filepath.Join(s.DataDir, migTLSDirName)
	if create {
		return migpki.LoadOrCreateCA(dir, time.Now())
	}
	return migpki.LoadCA(dir)
}

func parseMarker(b []byte) (*migpki.Marker, error) {
	var m migpki.Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.CAFingerprint == "" || len(m.Installed) == 0 {
		return nil, errors.New("incomplete marker")
	}
	return &m, nil
}

// migTLSInfo is the API representation of a node's state.
type migTLSInfo struct {
	Node      int64            `json:"node,omitempty"`
	Hostname  string           `json:"hostname"`
	Dir       string           `json:"dir,omitempty"`
	QemuUser  string           `json:"qemuUser,omitempty"`
	CAPrint   string           `json:"caFingerprint,omitempty"` // this controller's CA
	Notes     []string         `json:"notes,omitempty"`
	Files     []migTLSFileInfo `json:"files,omitempty"`
	Changes   []string         `json:"changes,omitempty"` // what a setup/removal did
	ConfEdits []string         `json:"configChanges"`     // edits to qemu.conf; always empty today
	Note      string           `json:"note,omitempty"`
	migpki.Status
}

type migTLSFileInfo struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Mode  string `json:"mode"`
}

// readMigTLS looks at a node and evaluates its state against the controller's
// CA. The environment and parsed directory state are returned for the callers
// that go on to change something.
func (s *Server) readMigTLS(h *hostconn.Host) (migTLSInfo, nodeEnv, nodeState, error) {
	info := migTLSInfo{Hostname: h.Name, ConfEdits: []string{}}
	out, err := h.Run(envScript)
	if err != nil {
		return info, nodeEnv{}, nodeState{}, fmt.Errorf("cannot inspect %s: %w: %s", h.Name, err, out)
	}
	env := parseNodeEnv(out)
	dir, err := env.certDir()
	if err != nil {
		return info, env, nodeState{}, badRequest(err.Error())
	}
	info.Dir = dir
	info.Notes = env.notes(dir)
	if u, err := env.qemuUser(); err == nil {
		info.QemuUser = u
	}
	out, err = h.Run(stateScript(dir))
	if err != nil {
		return info, env, nodeState{}, fmt.Errorf("cannot read %s on %s: %w: %s", dir, h.Name, err, out)
	}
	st, err := parseState(out, dir, h.Name)
	if err != nil {
		return info, env, st, err
	}
	for _, f := range migpki.Files {
		if fs := st.snap.Files[f]; fs.Exists {
			info.Files = append(info.Files, migTLSFileInfo{f, fs.Owner, fs.Mode})
		}
	}
	ca, err := s.migCA(false)
	switch {
	case errors.Is(err, migpki.ErrNoCA):
		// No CA yet: nothing can have been installed by this controller.
		info.Status = migpki.Status{State: migpki.StateNotSetUp}
		if st.snap.Marker != nil || st.snap.MarkerBad || len(info.Files) > 0 {
			info.Status = migpki.Status{State: migpki.StateMismatch, Problems: []string{dir + " already contains certificate files, and this controller has no migration CA"}}
		}
	case err != nil:
		return info, env, st, err
	default:
		info.CAPrint = ca.Fingerprint()
		info.Status = ca.Evaluate(st.snap, time.Now())
	}
	return info, env, st, nil
}

// --- pre-flight ---

// checkEncryption adds the "Encryption" check and records the decision in the
// plan.
func (s *Server) checkEncryption(srcID int64, src *hostconn.Host, dstID int64, dst *hostconn.Host, plan *migPlan, req migReq, c *checkList) {
	in := encInput{Mode: plan.Mode, Request: req.Encrypt, SrcName: src.Name, DstName: dst.Name}
	if plan.Mode != "offline" && !(req.Encrypt != nil && !*req.Encrypt) {
		si, _, _, serr := s.readMigTLS(src)
		di, _, _, derr := s.readMigTLS(dst)
		in.Src, in.Dst, in.SrcErr, in.DstErr = si.Status, di.Status, serr, derr
	}
	use, chk := encryptionCheck(in)
	plan.TLS, plan.TLSName = use, dst.Name
	*c = append(*c, chk)
}

// --- HTTP ---

var migTLSLocks = struct {
	sync.Mutex
	m map[int64]*sync.Mutex
}{m: map[int64]*sync.Mutex{}}

func lockNodeTLS(id int64) func() {
	migTLSLocks.Lock()
	l := migTLSLocks.m[id]
	if l == nil {
		l = &sync.Mutex{}
		migTLSLocks.m[id] = l
	}
	migTLSLocks.Unlock()
	l.Lock()
	return l.Unlock
}

func (s *Server) migTLSStatus(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	info, _, _, err := s.readMigTLS(h)
	if err != nil {
		fail(w, err)
		return
	}
	info.Node = id
	writeJSON(w, info)
}

type migTLSReq struct {
	// Force replaces certificate files this controller did not create (the old
	// ones are kept as *.virt-proj-orig and put back on removal) and removes
	// files that were changed after installation.
	Force bool `json:"force"`
	// AllowConfigChange is accepted for forward compatibility. Nothing needs a
	// change of qemu.conf, so nothing is ever edited; see configChanges in the
	// response.
	AllowConfigChange bool `json:"allowConfigChange"`
}

func readOptionalJSON(r *http.Request, v any) error {
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		return badRequest("invalid request body: " + err.Error())
	}
	return nil
}

func (s *Server) migTLSSetup(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req migTLSReq
	if err := readOptionalJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	defer lockNodeTLS(id)()
	var res migTLSInfo
	err = s.task(r, "migration-tls", h.Name, h.Name, func(progress func(float64)) error {
		var err error
		res, err = s.installMigTLS(id, h, req, progress)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	res.Node = id
	writeJSON(w, res)
}

func (s *Server) installMigTLS(id int64, h *hostconn.Host, req migTLSReq, progress func(float64)) (migTLSInfo, error) {
	ca, err := s.migCA(true)
	if err != nil {
		return migTLSInfo{}, err
	}
	info, env, st, err := s.readMigTLS(h)
	if err != nil {
		return info, err
	}
	if env.UID != "0" {
		return info, badRequest("setting up encrypted migration needs root on " + h.Name + " (the SSH user is not root)")
	}
	if !hostRE.MatchString(h.Name) {
		return info, badRequest("unsupported characters in the host name " + h.Name)
	}
	user, err := env.qemuUser()
	if err != nil {
		return info, badRequest(err.Error())
	}
	dir := info.Dir
	progress(0.1)

	// Which of the files are ours to replace?
	ours := st.snap.Marker != nil && st.snap.Marker.CAFingerprint == ca.Fingerprint()
	var foreign, edited []string
	for _, f := range migpki.Files {
		fs := st.snap.Files[f]
		switch {
		case !fs.Exists:
		case !ours:
			foreign = append(foreign, f)
		case st.snap.Marker.Installed[f] != fs.SHA256:
			edited = append(edited, f)
		}
	}
	if st.snap.MarkerBad || (st.snap.Marker != nil && !ours) {
		if !req.Force {
			return info, badRequest(dir + " on " + h.Name + " carries a marker from a different controller or CA; use force to take it over")
		}
	}
	if len(foreign) > 0 && !req.Force {
		return info, badRequest(fmt.Sprintf("%s on %s already contains %s, which this controller did not create. Nothing was changed; use force to replace them (the old files are kept as *%s)", dir, h.Name, strings.Join(foreign, ", "), migBackupSuffix))
	}
	if len(edited) > 0 && !req.Force {
		return info, badRequest(fmt.Sprintf("in %[2]s the file(s) %[1]s were changed after this controller installed them; nothing was changed. Use force to replace them", strings.Join(edited, ", "), h.Name))
	}

	marker := &migpki.Marker{CAFingerprint: ca.Fingerprint(), Installed: map[string]string{}, Backups: map[string]string{}, Time: time.Now().Unix()}
	if ours {
		for k, v := range st.snap.Marker.Backups {
			marker.Backups[k] = v
		}
		marker.CreatedDirs = st.snap.Marker.CreatedDirs
	} else {
		for _, a := range ancestors(dir) {
			if !st.dirExists[a] {
				marker.CreatedDirs = append(marker.CreatedDirs, a)
			}
		}
	}
	var backups [][2]string
	if !ours && (st.snap.Marker != nil || st.snap.MarkerBad) {
		foreign = append(foreign, migpki.FileMarker)
	}
	for _, f := range foreign {
		b := f + migBackupSuffix
		if st.listing[b] {
			return info, badRequest(dir + "/" + b + " already exists on " + h.Name + "; refusing to overwrite a backup")
		}
		marker.Backups[f] = b
		backups = append(backups, [2]string{f, b})
	}

	certPEM, keyPEM, err := ca.Issue(certNames(h.Name, env, s.nodeAddr(id)), time.Now())
	if err != nil {
		return info, err
	}
	content := map[string]string{
		migpki.FileCA: string(ca.CertPEM), migpki.FileServerCert: string(certPEM), migpki.FileClientCert: string(certPEM),
		migpki.FileServerKey: string(keyPEM), migpki.FileClientKey: string(keyPEM),
	}
	for f, c := range content {
		marker.Installed[f] = sha256hex(c)
	}
	mk, _ := json.MarshalIndent(marker, "", "  ")
	content[migpki.FileMarker] = string(mk) + "\n"

	// Phase 1: write every file under a temporary name. Nothing visible to
	// libvirt changes yet, so a failure here leaves the node as it was.
	var changes []string
	for _, d := range marker.CreatedDirs {
		if !st.dirExists[d] && !ours {
			changes = append(changes, "created directory "+d)
		}
	}
	if out, err := h.Run("umask 022; mkdir -p -- " + shq(dir)); err != nil {
		return info, fmt.Errorf("creating %s on %s: %w: %s", dir, h.Name, err, out)
	}
	cleanTmp := func() {
		var b strings.Builder
		b.WriteString("rm -f --")
		for f := range content {
			b.WriteString(" " + shq(path.Join(dir, f+tmpSuffix)))
		}
		h.Run(b.String())
		if !ours { // only remove directories this run created
			for i := len(marker.CreatedDirs) - 1; i >= 0; i-- {
				h.Run("rmdir -- " + shq(marker.CreatedDirs[i]) + " 2>/dev/null; true")
			}
		}
	}
	for i, f := range append(append([]string{}, migpki.Files...), migpki.FileMarker) {
		owner, mode := "0:0", "0644"
		if f == migpki.FileServerKey || f == migpki.FileClientKey {
			owner, mode = user+":0", "0400" // only QEMU's account reads the private key
		}
		tmp := shq(path.Join(dir, f+tmpSuffix))
		cmd := "umask 077; cat > " + tmp + " && chown " + shq(owner) + " " + tmp + " && chmod " + mode + " " + tmp
		if out, err := h.RunInput(cmd, content[f]); err != nil {
			cleanTmp()
			return info, fmt.Errorf("writing %s on %s: %w: %s", f, h.Name, err, out)
		}
		progress(0.2 + 0.5*float64(i+1)/6)
	}

	// Phase 2: move the files into place in one command.
	var sh strings.Builder
	sh.WriteString("set -e; ")
	for _, b := range backups {
		sh.WriteString("mv -- " + shq(path.Join(dir, b[0])) + " " + shq(path.Join(dir, b[1])) + "; ")
		changes = append(changes, "kept the existing "+path.Join(dir, b[0])+" as "+path.Join(dir, b[1]))
	}
	for _, f := range append(append([]string{}, migpki.Files...), migpki.FileMarker) {
		sh.WriteString("mv -f -- " + shq(path.Join(dir, f+tmpSuffix)) + " " + shq(path.Join(dir, f)) + "; ")
	}
	// SELinux hosts (RHEL family) need the file contexts libvirt expects.
	if env.Restore {
		sh.WriteString("restorecon -R " + shq(dir) + " >/dev/null 2>&1 || true; ")
	}
	sh.WriteString("true")
	if out, err := h.Run(sh.String()); err != nil {
		cleanTmp()
		return info, fmt.Errorf("installing the certificates in %s on %s failed: %w: %s (the directory may be partly changed; run the setup again or remove it)", dir, h.Name, err, out)
	}
	progress(0.9)
	for _, f := range migpki.Files {
		changes = append(changes, "wrote "+path.Join(dir, f))
	}
	changes = append(changes, "wrote "+path.Join(dir, migpki.FileMarker))
	if env.Restore {
		changes = append(changes, "restored SELinux contexts (restorecon)")
	}
	changes = append(changes, "/etc/libvirt/qemu.conf was not modified and no service was restarted")

	out, _, _, err := s.readMigTLS(h)
	if err != nil {
		return out, fmt.Errorf("installed, but reading the result back failed: %w", err)
	}
	out.Changes = changes
	out.Note = migTLSPublicMsg
	log.Printf("migration TLS: installed certificates for %s in %s", h.Name, dir)
	return out, nil
}

func sha256hex(s string) string {
	// The same digest the node's sha256sum prints.
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (s *Server) nodeAddr(id int64) string {
	if n, ok := s.nodeStatus(id); ok {
		return n.Addr
	}
	return ""
}

func (s *Server) migTLSRemove(w http.ResponseWriter, r *http.Request) {
	id, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req migTLSReq
	if err := readOptionalJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if r.URL.Query().Get("force") == "true" {
		req.Force = true
	}
	defer lockNodeTLS(id)()
	var res migTLSInfo
	err = s.task(r, "migration-tls-remove", h.Name, h.Name, func(progress func(float64)) error {
		var err error
		res, err = s.removeMigTLS(h, req)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	res.Node = id
	writeJSON(w, res)
}

func (s *Server) removeMigTLS(h *hostconn.Host, req migTLSReq) (migTLSInfo, error) {
	info, env, st, err := s.readMigTLS(h)
	if err != nil {
		return info, err
	}
	if env.UID != "0" {
		return info, badRequest("removing the certificates needs root on " + h.Name)
	}
	m := st.snap.Marker
	if m == nil {
		return info, badRequest("this controller installed nothing in " + info.Dir + " on " + h.Name + " (no marker file); nothing was removed")
	}
	if ca, err := s.migCA(false); err != nil || ca.Fingerprint() != m.CAFingerprint {
		if !req.Force {
			return info, badRequest("the certificates in " + info.Dir + " on " + h.Name + " were installed by a different controller or CA; use force to remove them anyway")
		}
	}
	dir := info.Dir
	var sh strings.Builder
	var changes, kept []string
	sh.WriteString("set -e; ")
	for _, f := range migpki.Files {
		fs := st.snap.Files[f]
		switch {
		case !fs.Exists:
		case fs.SHA256 != m.Installed[f] && !req.Force:
			kept = append(kept, f+" (changed after installation)")
			continue
		default:
			sh.WriteString("rm -f -- " + shq(path.Join(dir, f)) + "; ")
			changes = append(changes, "removed "+path.Join(dir, f))
		}
		if b := m.Backups[f]; b != "" && st.listing[b] {
			sh.WriteString("mv -- " + shq(path.Join(dir, b)) + " " + shq(path.Join(dir, f)) + "; ")
			changes = append(changes, "restored "+path.Join(dir, f)+" from "+path.Join(dir, b))
		}
	}
	sh.WriteString("rm -f -- " + shq(path.Join(dir, migpki.FileMarker)) + "; ")
	changes = append(changes, "removed "+path.Join(dir, migpki.FileMarker))
	if b := m.Backups[migpki.FileMarker]; b != "" && st.listing[b] {
		sh.WriteString("mv -- " + shq(path.Join(dir, b)) + " " + shq(path.Join(dir, migpki.FileMarker)) + "; ")
		changes = append(changes, "restored the previous marker file")
	}
	sh.WriteString("true")
	if out, err := h.Run(sh.String()); err != nil {
		return info, fmt.Errorf("removing the certificates on %s failed: %w: %s", h.Name, err, out)
	}
	for i := len(m.CreatedDirs) - 1; i >= 0; i-- {
		d := m.CreatedDirs[i]
		if !safePathRE.MatchString(d) {
			continue
		}
		if _, err := h.Run("rmdir -- " + shq(d)); err != nil {
			kept = append(kept, "directory "+d+" (not empty)")
			break
		}
		changes = append(changes, "removed directory "+d+" (created by the setup)")
	}
	out, _, _, err := s.readMigTLS(h)
	if err != nil {
		return out, err
	}
	out.Changes = changes
	if len(kept) > 0 {
		out.Note = "left in place: " + strings.Join(kept, ", ")
	}
	return out, nil
}

// --- alerts ---

// migCertExpiry reads a node's migration certificate for the alert pass. ok is
// false when the node has none of this controller's certificates.
func (s *Server) migCertExpiry(h *hostconn.Host) (notAfter time.Time, ok bool) {
	info, _, _, err := s.readMigTLS(h)
	if err != nil || info.Cert == nil || info.State == migpki.StateNotSetUp || info.State == migpki.StateMismatch {
		return time.Time{}, false
	}
	return info.Cert.NotAfter, true
}
