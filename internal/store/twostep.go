package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// addColumn adds a column unless the table already has it.
func addColumn(tx *sql.Tx, table, col, def string) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('`+table+`') WHERE name=?`, col).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, def))
	return err
}

// HashRecoveryCode is how a recovery code is stored. The codes carry 50 bits
// of randomness and are single use, so a plain salted hash is enough; the
// user id is the salt.
func HashRecoveryCode(userID int64, code string) string {
	code = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	h := sha256.Sum256([]byte(fmt.Sprintf("virticle-recovery:%d:%s", userID, code)))
	return hex.EncodeToString(h[:])
}

// TwoStep is a user's two-step sign-in state. Secret is sealed.
type TwoStep struct {
	Enabled bool
	Secret  string
	Step    int64 // the last time step accepted
}

func (s *Store) TwoStep(userID int64) (TwoStep, error) {
	var t TwoStep
	err := s.db.QueryRow(`SELECT totp_enabled, totp_secret, totp_step FROM users WHERE id=?`, userID).Scan(&t.Enabled, &t.Secret, &t.Step)
	if err != nil {
		return t, ErrNotFound
	}
	return t, nil
}

// EnableTwoStep stores the sealed secret and replaces the recovery codes (given
// as hashes) in one step. step is the time step of the code that confirmed the
// secret, so that same code cannot be used again to sign in.
func (s *Store) EnableTwoStep(userID int64, sealed string, step int64, codeHashes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET totp_secret=?, totp_enabled=1, totp_step=? WHERE id=? AND totp_enabled=0`, sealed, step, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: two-step sign-in is already on", ErrConflict)
	}
	if err := putCodes(tx, userID, codeHashes); err != nil {
		return err
	}
	return tx.Commit()
}

func putCodes(tx *sql.Tx, userID int64, hashes []string) error {
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes(user_id, hash) VALUES(?,?)`, userID, h); err != nil {
			return err
		}
	}
	return nil
}

// DisableTwoStep turns two-step sign-in off and forgets the secret and codes.
func (s *Store) DisableTwoStep(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET totp_secret='', totp_enabled=0, totp_step=0 WHERE id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetTwoStep is DisableTwoStep for an administrator or the shell: it also
// ends the user's sessions.
func (s *Store) ResetTwoStep(userID int64) error {
	if err := s.DisableTwoStep(userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

// UseTwoStepStep records step as used, and reports false when that step or a
// later one was already used (a replay). The check and the write are one
// statement, so two simultaneous requests with the same code cannot both pass.
func (s *Store) UseTwoStepStep(userID, step int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE users SET totp_step=? WHERE id=? AND totp_enabled=1 AND totp_step<?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UseRecoveryCode consumes a recovery code (given as its hash).
func (s *Store) UseRecoveryCode(userID int64, hash string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM recovery_codes WHERE user_id=? AND hash=?`, userID, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RecoveryCodesLeft counts the unused recovery codes.
func (s *Store) RecoveryCodesLeft(userID int64) (n int, err error) {
	err = s.db.QueryRow(`SELECT count(*) FROM recovery_codes WHERE user_id=?`, userID).Scan(&n)
	return
}

// ReplaceRecoveryCodes swaps in a fresh set (as hashes).
func (s *Store) ReplaceRecoveryCodes(userID int64, hashes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := putCodes(tx, userID, hashes); err != nil {
		return err
	}
	return tx.Commit()
}

// UserByName finds an account of any source by its sign-in name.
func (s *Store) UserByName(name string) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, name))
	if err != nil {
		return u, ErrNotFound
	}
	return u, nil
}

// UpsertExternal creates the account of someone signing in through a
// provider on first sign-in, and at every sign-in sets the role from the
// provider. extID is the provider's stable identifier (OIDC: issuer and
// subject; LDAP: the directory entry's DN, which can change, so the sign-in
// name is the key there and extID only records the last one). A name already
// used by a local account, or by an account of another provider, is refused:
// the local account always wins.
func (s *Store) UpsertExternal(source, extID, username, role string) (User, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var id int64
	var src, ext string
	err = tx.QueryRow(`SELECT id, source, ext_id FROM users WHERE username=?`, username).Scan(&id, &src, &ext)
	switch {
	case err == sql.ErrNoRows:
		if source == SourceOIDC {
			// Same person, renamed at the provider: keep the account.
			if e := tx.QueryRow(`SELECT id FROM users WHERE source=? AND ext_id=?`, source, extID).Scan(&id); e == nil {
				if _, err := tx.Exec(`UPDATE users SET username=?, role=? WHERE id=?`, username, role, id); err != nil {
					return User{}, err
				}
				break
			}
		}
		res, err := tx.Exec(`INSERT INTO users(username, pwhash, created, role, source, ext_id) VALUES(?,?,?,?,?,?)`,
			username, "!", time.Now().Unix(), role, source, extID)
		if err != nil {
			return User{}, err
		}
		id, _ = res.LastInsertId()
	case err != nil:
		return User{}, err
	default:
		if src != source || (source == SourceOIDC && ext != extID) {
			return User{}, fmt.Errorf("%w: the name %q is already used by another account", ErrConflict, username)
		}
		if _, err := tx.Exec(`UPDATE users SET role=?, ext_id=? WHERE id=?`, role, extID, id); err != nil {
			return User{}, err
		}
	}
	u, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// EndSessions signs the user out everywhere.
func (s *Store) EndSessions(userID int64) {
	s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
}
