package migpki

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The five files libvirt/QEMU look for in the certificate directory, plus the
// marker that records what this controller installed.
const (
	FileCA         = "ca-cert.pem"
	FileServerCert = "server-cert.pem"
	FileServerKey  = "server-key.pem"
	FileClientCert = "client-cert.pem"
	FileClientKey  = "client-key.pem"
	FileMarker     = ".virt-proj-migration.json"
)

// Files lists the certificate files in install order (keys before certs).
var Files = []string{FileServerKey, FileClientKey, FileCA, FileServerCert, FileClientCert}

// Marker is stored next to the certificates on the node. Its presence with the
// controller's CA fingerprint is what says "this controller created these
// files"; the hashes detect later changes by someone else.
type Marker struct {
	CAFingerprint string            `json:"caFingerprint"`
	Installed     map[string]string `json:"installed"`             // file name -> sha256 of the content installed
	Backups       map[string]string `json:"backups,omitempty"`     // file name -> name its previous content was moved to (force)
	CreatedDirs   []string          `json:"createdDirs,omitempty"` // directories made for the install, outermost first
	Time          int64             `json:"time"`
}

// FileState is what was read for one file on the node.
type FileState struct {
	Exists bool
	SHA256 string
	Owner  string // user:group
	Mode   string // octal, e.g. 640
}

// Snapshot is everything read from a node that the status depends on.
type Snapshot struct {
	Dir       string
	Files     map[string]FileState
	Marker    *Marker // nil when absent
	MarkerBad bool    // a marker exists but cannot be parsed
	CACert    []byte  // content of ca-cert.pem
	Server    []byte  // content of server-cert.pem
	Client    []byte  // content of client-cert.pem
	Hostname  string  // the node's name as libvirt reports it
}

// Status states.
const (
	StateNotSetUp = "not set up"
	StateOK       = "ok"
	StateExpiring = "expiring"
	StateExpired  = "expired"
	StateMismatch = "mismatch"
)

// Status is the evaluated state of one node.
type Status struct {
	State    string   `json:"state"`
	Problems []string `json:"problems,omitempty"`
	Cert     *Info    `json:"cert,omitempty"`
	DaysLeft int      `json:"daysLeft,omitempty"`
}

// Usable reports whether the node can take part in an encrypted migration
// right now (expiring certificates still work).
func (s Status) Usable() bool { return s.State == StateOK || s.State == StateExpiring }

// Evaluate judges a snapshot against the controller's CA.
func (ca *CA) Evaluate(sn Snapshot, now time.Time) Status {
	var present []string
	for _, f := range Files {
		if sn.Files[f].Exists {
			present = append(present, f)
		}
	}
	sort.Strings(present)
	if sn.MarkerBad {
		return Status{State: StateMismatch, Problems: []string{"the marker file " + FileMarker + " in " + sn.Dir + " cannot be read"}}
	}
	if sn.Marker == nil {
		if len(present) == 0 {
			return Status{State: StateNotSetUp}
		}
		return Status{State: StateMismatch, Problems: []string{fmt.Sprintf("%s already contains %s, which this controller did not create", sn.Dir, strings.Join(present, ", "))}}
	}

	var problems []string
	if sn.Marker.CAFingerprint != ca.Fingerprint() {
		problems = append(problems, "set up by a different controller or CA (fingerprint "+short(sn.Marker.CAFingerprint)+", this controller's is "+short(ca.Fingerprint())+")")
	}
	for _, f := range Files {
		st := sn.Files[f]
		switch {
		case !st.Exists:
			problems = append(problems, f+" is missing")
		case sn.Marker.Installed[f] != "" && st.SHA256 != sn.Marker.Installed[f]:
			problems = append(problems, f+" was changed after installation")
		}
	}
	if len(sn.CACert) > 0 && !bytes.Equal(bytes.TrimSpace(sn.CACert), bytes.TrimSpace(ca.CertPEM)) {
		problems = append(problems, FileCA+" on the node is not this controller's CA")
	}

	var info *Info
	var days int
	expired, expiring := false, false
	if len(sn.Server) > 0 {
		crt, in, err := ParseCert(sn.Server)
		if err != nil {
			problems = append(problems, FileServerCert+": "+err.Error())
		} else {
			info = &in
			if err := ca.Verify(crt, sn.Hostname, now); err != nil {
				problems = append(problems, FileServerCert+": "+err.Error())
			}
			left := in.NotAfter.Sub(now)
			days = int(left.Hours() / 24)
			expired, expiring = left <= 0, left < RenewLeft
		}
	}
	if len(sn.Client) > 0 {
		if crt, in, err := ParseCert(sn.Client); err != nil {
			problems = append(problems, FileClientCert+": "+err.Error())
		} else {
			if err := ca.Verify(crt, sn.Hostname, now); err != nil {
				problems = append(problems, FileClientCert+": "+err.Error())
			}
			if left := in.NotAfter.Sub(now); left <= 0 {
				expired = true
			} else if left < RenewLeft {
				expiring = true
			}
		}
	}
	st := Status{State: StateOK, Problems: problems, Cert: info, DaysLeft: days}
	switch {
	case len(problems) > 0:
		st.State = StateMismatch
	case expired:
		st.State = StateExpired
		st.Problems = []string{"the certificate has expired; issue it again"}
	case expiring:
		st.State = StateExpiring
		st.Problems = []string{fmt.Sprintf("the certificate expires in %d days; issue it again", days)}
	}
	return st
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}
