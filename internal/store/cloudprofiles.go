package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// profileSchema is applied on every open (CREATE IF NOT EXISTS), so a database
// from before profiles existed simply gains the table.
const profileSchema = `
CREATE TABLE IF NOT EXISTS cloudinit_profiles (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, description TEXT NOT NULL DEFAULT '',
  user TEXT NOT NULL DEFAULT '', pwhash TEXT NOT NULL DEFAULT '',
  ssh_keys TEXT NOT NULL DEFAULT '[]', packages TEXT NOT NULL DEFAULT '[]',
  raw_user_data TEXT NOT NULL DEFAULT '', is_default INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL, updated INTEGER NOT NULL);
`

// ErrNameTaken is returned when a profile name already exists.
var ErrNameTaken = errors.New("a profile with this name already exists")

// CloudProfile is a saved set of cloud-init settings. The password is only
// ever kept as a SHA-512 crypt hash, which never leaves the server.
type CloudProfile struct {
	ID          int64
	Name        string
	Description string
	User        string
	PwHash      string
	SSHKeys     []string
	Packages    []string
	RawUserData string
	Default     bool
	Created     int64
	Updated     int64
}

const profileCols = `id, name, description, user, pwhash, ssh_keys, packages, raw_user_data, is_default, created, updated`

func scanProfile(sc interface{ Scan(...any) error }) (CloudProfile, error) {
	var p CloudProfile
	var keys, pkgs string
	if err := sc.Scan(&p.ID, &p.Name, &p.Description, &p.User, &p.PwHash, &keys, &pkgs, &p.RawUserData, &p.Default, &p.Created, &p.Updated); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(keys), &p.SSHKeys); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(pkgs), &p.Packages); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Store) CloudProfiles() ([]CloudProfile, error) {
	rows, err := s.db.Query(`SELECT ` + profileCols + ` FROM cloudinit_profiles ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudProfile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CloudProfile(id int64) (CloudProfile, error) {
	p, err := scanProfile(s.db.QueryRow(`SELECT `+profileCols+` FROM cloudinit_profiles WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func nz(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// SaveCloudProfile inserts the profile (ID 0) or replaces the one with that
// ID. A profile marked default un-marks every other one.
func (s *Store) SaveCloudProfile(p CloudProfile) (CloudProfile, error) {
	keys, _ := json.Marshal(nz(p.SSHKeys))
	pkgs, _ := json.Marshal(nz(p.Packages))
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if p.ID == 0 {
		res, err := tx.Exec(`INSERT INTO cloudinit_profiles(name, description, user, pwhash, ssh_keys, packages, raw_user_data, is_default, created, updated) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			p.Name, p.Description, p.User, p.PwHash, string(keys), string(pkgs), p.RawUserData, p.Default, now, now)
		if err != nil {
			return p, profileErr(err)
		}
		p.ID, _ = res.LastInsertId()
		p.Created = now
	} else {
		res, err := tx.Exec(`UPDATE cloudinit_profiles SET name=?, description=?, user=?, pwhash=?, ssh_keys=?, packages=?, raw_user_data=?, is_default=?, updated=? WHERE id=?`,
			p.Name, p.Description, p.User, p.PwHash, string(keys), string(pkgs), p.RawUserData, p.Default, now, p.ID)
		if err != nil {
			return p, profileErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return p, ErrNotFound
		}
	}
	p.Updated = now
	if p.Default {
		if _, err := tx.Exec(`UPDATE cloudinit_profiles SET is_default=0 WHERE id<>?`, p.ID); err != nil {
			return p, err
		}
	}
	return p, tx.Commit()
}

func profileErr(err error) error {
	if strings.Contains(err.Error(), "UNIQUE") {
		return ErrNameTaken
	}
	return err
}

func (s *Store) DeleteCloudProfile(id int64) error {
	res, err := s.db.Exec(`DELETE FROM cloudinit_profiles WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
