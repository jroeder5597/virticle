package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"virticle/internal/cloudinit"
	"virticle/internal/store"
)

// Cloud-init profiles are controller settings (SQLite), not VM state: they
// only describe what to put on a seed disk when a VM is created or cloned.
// Nothing about the profile is recorded on the VM afterwards, so deleting or
// editing a profile never touches a VM made from it.

const (
	maxProfileName = 64
	maxProfileDesc = 500
	maxProfileKeys = 20
	maxKeyLen      = 8 << 10
	maxProfilePkgs = 100
	maxProfileRaw  = 64 << 10
)

type profileRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	User        string   `json:"user"`
	Password    string   `json:"password"`      // plaintext, hashed here and dropped
	ClearPass   bool     `json:"clearPassword"` // on update: remove the stored hash
	SSHKeys     []string `json:"sshKeys"`
	Packages    []string `json:"packages"`
	RawUserData string   `json:"rawUserData"`
	Default     bool     `json:"default"`
}

type profileView struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	User        string   `json:"user"`
	HasPassword bool     `json:"hasPassword"`
	SSHKeys     []string `json:"sshKeys"`
	Packages    []string `json:"packages"`
	RawUserData string   `json:"rawUserData"`
	Default     bool     `json:"default"`
	Created     int64    `json:"created"`
	Updated     int64    `json:"updated"`
}

// view is the only way a profile leaves the server: the hash is reduced to a flag.
func view(p store.CloudProfile) profileView {
	return profileView{ID: p.ID, Name: p.Name, Description: p.Description, User: p.User, HasPassword: p.PwHash != "",
		SSHKeys: nz(p.SSHKeys), Packages: nz(p.Packages), RawUserData: p.RawUserData, Default: p.Default, Created: p.Created, Updated: p.Updated}
}

func nz(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// cleanList trims entries and drops empty ones and duplicates, keeping order.
func cleanList(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// validateProfile normalises and checks a request. It returns the profile to
// store, without a password hash: the caller decides what happens to that.
func validateProfile(q profileRequest) (store.CloudProfile, error) {
	p := store.CloudProfile{
		Name: strings.TrimSpace(q.Name), Description: strings.TrimSpace(q.Description), User: strings.TrimSpace(q.User),
		SSHKeys: cleanList(q.SSHKeys), Packages: cleanList(q.Packages), RawUserData: q.RawUserData, Default: q.Default,
	}
	switch {
	case p.Name == "":
		return p, badRequest("a profile needs a name")
	case len([]rune(p.Name)) > maxProfileName:
		return p, badRequest(fmt.Sprintf("the name may be at most %d characters", maxProfileName))
	case len([]rune(p.Description)) > maxProfileDesc:
		return p, badRequest(fmt.Sprintf("the description may be at most %d characters", maxProfileDesc))
	case len(p.SSHKeys) > maxProfileKeys:
		return p, badRequest(fmt.Sprintf("at most %d SSH keys per profile", maxProfileKeys))
	case len(p.Packages) > maxProfilePkgs:
		return p, badRequest(fmt.Sprintf("at most %d packages per profile", maxProfilePkgs))
	case len(p.RawUserData) > maxProfileRaw:
		return p, badRequest(fmt.Sprintf("raw user-data may be at most %d KiB", maxProfileRaw>>10))
	}
	for _, k := range p.SSHKeys {
		if len(k) > maxKeyLen {
			return p, badRequest("an SSH key is too long")
		}
		if strings.ContainsAny(k, "\r\n") {
			return p, badRequest("an SSH key must be on a single line")
		}
		if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(k)); err != nil || len(strings.TrimSpace(string(rest))) > 0 {
			return p, badRequest(fmt.Sprintf("not an SSH public key: %.40q", k))
		}
	}
	// The raw text is checked as far as is possible without a YAML parser
	// (the project has none): it must be something cloud-init accepts as a
	// first line.
	if raw := strings.TrimSpace(p.RawUserData); raw != "" && !strings.HasPrefix(raw, "#cloud-config") && !strings.HasPrefix(raw, "#!") {
		return p, badRequest("raw user-data must start with #cloud-config or a #! script line")
	}
	if strings.ContainsRune(p.RawUserData, 0) {
		return p, badRequest("raw user-data contains a NUL byte")
	}
	if len(q.Password) > 256 {
		return p, badRequest("the password is too long")
	}
	// Field rules (user name syntax, package names) are the seed builder's own.
	if err := (cloudinit.Config{User: p.User, SSHKeys: p.SSHKeys, Packages: p.Packages}).Validate(); err != nil {
		return p, badRequest(err.Error())
	}
	return p, nil
}

func (s *Server) listCloudProfiles(w http.ResponseWriter, r *http.Request) {
	ps, err := s.Store.CloudProfiles()
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]profileView, 0, len(ps))
	for _, p := range ps {
		out = append(out, view(p))
	}
	writeJSON(w, map[string]any{"profiles": out})
}

func (s *Server) saveCloudProfile(w http.ResponseWriter, r *http.Request) {
	var q profileRequest
	if err := readJSON(r, &q); err != nil {
		fail(w, err)
		return
	}
	p, err := validateProfile(q)
	if err != nil {
		fail(w, err)
		return
	}
	if id := r.PathValue("id"); id != "" {
		if p.ID, err = strconv.ParseInt(id, 10, 64); err != nil {
			fail(w, store.ErrNotFound)
			return
		}
		old, err := s.Store.CloudProfile(p.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if !q.ClearPass {
			p.PwHash = old.PwHash
		}
		p.Created = old.Created
	}
	if q.Password != "" {
		if p.PwHash, err = cloudinit.HashPassword(q.Password); err != nil {
			fail(w, err)
			return
		}
	}
	if p.PwHash != "" && p.User == "" {
		fail(w, badRequest("a password needs a user name"))
		return
	}
	saved, err := s.Store.SaveCloudProfile(p)
	if errors.Is(err, store.ErrNameTaken) {
		fail(w, badRequest(err.Error()))
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, view(saved))
}

func (s *Server) deleteCloudProfile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return
	}
	if err := s.Store.DeleteCloudProfile(id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ciInput is the cloud-init part of a create or clone request.
type ciInput struct {
	Hostname    string
	User        string
	Password    string
	SSHKeys     []string
	Packages    []string
	RawUserData string
}

// mergeProfile combines a profile (nil for none) with what the request itself
// sets. A value in the request wins; SSH keys and packages are added to the
// profile's. The profile's password only travels as its hash. Pure: no
// storage, no defaults for the hostname.
func mergeProfile(p *store.CloudProfile, in ciInput) cloudinit.Config {
	cfg := cloudinit.Config{Hostname: in.Hostname, User: in.User, Password: in.Password,
		SSHKeys: cleanList(in.SSHKeys), Packages: cleanList(in.Packages), RawUserData: in.RawUserData}
	if p == nil {
		return cfg
	}
	if cfg.User == "" {
		cfg.User = p.User
	}
	if cfg.Password == "" {
		cfg.PasswordHash = p.PwHash
	}
	if strings.TrimSpace(cfg.RawUserData) == "" {
		cfg.RawUserData = p.RawUserData
	}
	cfg.SSHKeys = cleanList(append(append([]string{}, p.SSHKeys...), cfg.SSHKeys...))
	cfg.Packages = cleanList(append(append([]string{}, p.Packages...), cfg.Packages...))
	return cfg
}

// resolveProfile loads the profile a request names; id 0 means none.
func (s *Server) resolveProfile(id int64) (*store.CloudProfile, error) {
	if id == 0 {
		return nil, nil
	}
	p, err := s.Store.CloudProfile(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, badRequest(fmt.Sprintf("cloud-init profile %d does not exist (deleted meanwhile?)", id))
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
