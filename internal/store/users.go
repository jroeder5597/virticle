package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Roles in increasing order of power.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// RoleRank orders roles; unknown roles rank 0 (no access).
func RoleRank(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

const (
	sessionTTL    = 7 * 24 * time.Hour
	auditMaxRows  = 50000
	auditMaxAge   = 180 * 24 * time.Hour
	auditPruneGap = 200 // prune once per this many inserts
)

var (
	ErrConflict    = errors.New("conflict")
	ErrLastAdmin   = errors.New("this would leave no enabled local administrator")
	ErrBadPassword = errors.New("current password is wrong")
	dummyHash, _   = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)
)

// authSchema holds the tables added after the first release. Older databases
// get the new users columns from migrate.
const authSchema = `
CREATE TABLE IF NOT EXISTS api_tokens (
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, hash TEXT UNIQUE NOT NULL, role TEXT NOT NULL,
  created INTEGER NOT NULL, expires INTEGER NOT NULL DEFAULT 0, last_used INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY, time INTEGER NOT NULL, username TEXT NOT NULL, token TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS audit_time ON audit(time);
CREATE TABLE IF NOT EXISTS recovery_codes (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, hash TEXT NOT NULL, PRIMARY KEY(user_id, hash));
`

// migrate brings an existing database up to the current schema. It is safe to
// run on every start: each step checks before it changes anything.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cols := map[string]bool{}
	rows, err := tx.Query(`SELECT name FROM pragma_table_info('users')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		cols[n] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	// Every user that exists before roles were introduced was a full
	// administrator, so the column default keeps them that way.
	if !cols["role"] {
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin'`); err != nil {
			return err
		}
	}
	if !cols["disabled"] {
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	// Schema 3: sign-in sources and two-step sign-in. Existing users are local
	// and have it off.
	for _, c := range []struct{ name, def string }{
		{"source", "TEXT NOT NULL DEFAULT 'local'"},
		{"ext_id", "TEXT NOT NULL DEFAULT ''"},
		{"totp_secret", "TEXT NOT NULL DEFAULT ''"},
		{"totp_enabled", "INTEGER NOT NULL DEFAULT 0"},
		{"totp_step", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !cols[c.name] {
			if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(authSchema); err != nil {
		return err
	}
	if err := addColumn(tx, "audit", "detail", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := migrateNodes(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Where an account lives.
const (
	SourceLocal = "local"
	SourceOIDC  = "oidc"
	SourceLDAP  = "ldap"
)

// ExternalSessionTTL is how long a session of an account from an identity
// provider or directory lasts: the role is re-read from the provider at each sign-in.
const ExternalSessionTTL = 12 * time.Hour

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
	Created  int64  `json:"created"`
	// Source is where the account lives: local, oidc or ldap. External
	// accounts have no local password and take their role from the provider.
	Source string `json:"source"`
	// TwoStep is set when the user has the built-in two-step sign-in on.
	TwoStep bool `json:"twoStep"`
}

// userCols is the column list scanUser reads.
const userCols = `id, username, role, disabled, created, source, totp_enabled`

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(r rowScanner) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.Created, &u.Source, &u.TwoStep)
	return u, err
}

// Principal is who a request acts as. Role is the effective role: for an API
// token it is the lower of the token's and the user's role.
type Principal struct {
	User  User
	Role  string
	Token string // token name, empty for a cookie session
}

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func (s *Store) UserCount() (n int, err error) {
	err = s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}

func (s *Store) CreateUser(username, password, role string) (User, error) {
	h, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO users(username, pwhash, created, role) VALUES(?,?,?,?)`, username, h, now, role)
	if isUnique(err) {
		return User{}, fmt.Errorf("%w: user %q already exists", ErrConflict, username)
	}
	if err != nil {
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Username: username, Role: role, Created: now}, nil
}

// CreateFirstUser creates an administrator only while no user exists, in one
// statement so two concurrent setups cannot both succeed.
func (s *Store) CreateFirstUser(username, password string) (User, error) {
	h, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO users(username, pwhash, created, role) SELECT ?,?,?,'admin' WHERE NOT EXISTS (SELECT 1 FROM users)`, username, h, now)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrConflict
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Username: username, Role: RoleAdmin, Created: now}, nil
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) User(id int64) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
	if err != nil {
		return u, ErrNotFound
	}
	return u, nil
}

// UserUpdate lists the changes to apply; nil fields stay as they are.
type UserUpdate struct {
	Role     *string
	Password *string
	Disabled *bool
}

// UpdateUser applies u, refusing any change that would leave no enabled
// administrator. A role change, a disable or a password reset ends the user's
// sessions; tokens are cut down by the role check at use time.
func (s *Store) UpdateUser(id int64, u UserUpdate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role, source string
	var disabled bool
	if err := tx.QueryRow(`SELECT role, disabled, source FROM users WHERE id=?`, id).Scan(&role, &disabled, &source); err != nil {
		return ErrNotFound
	}
	newRole, newDisabled := role, disabled
	if u.Role != nil {
		newRole = *u.Role
	}
	if u.Disabled != nil {
		newDisabled = *u.Disabled
	}
	wasAdmin := role == RoleAdmin && !disabled && source == SourceLocal
	if wasAdmin && !(newRole == RoleAdmin && !newDisabled) {
		if err := requireOtherAdmin(tx, id); err != nil {
			return err
		}
	}
	if u.Password != nil {
		h, err := hashPassword(*u.Password)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET pwhash=? WHERE id=?`, h, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role=?, disabled=? WHERE id=?`, newRole, newDisabled, id); err != nil {
		return err
	}
	if u.Password != nil || newRole != role || newDisabled != disabled {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func requireOtherAdmin(tx *sql.Tx, id int64) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM users WHERE role='admin' AND disabled=0 AND source='local' AND id<>?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

func (s *Store) DeleteUser(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role, source string
	var disabled bool
	if err := tx.QueryRow(`SELECT role, disabled, source FROM users WHERE id=?`, id).Scan(&role, &disabled, &source); err != nil {
		return ErrNotFound
	}
	if role == RoleAdmin && !disabled && source == SourceLocal {
		if err := requireOtherAdmin(tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ChangePassword sets a new password after checking the current one, and ends
// every session of the user except keepSession.
func (s *Store) ChangePassword(id int64, current, next, keepSession string) error {
	var hash string
	if err := s.db.QueryRow(`SELECT pwhash FROM users WHERE id=?`, id).Scan(&hash); err != nil {
		return ErrNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		return ErrBadPassword
	}
	h, err := hashPassword(next)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET pwhash=? WHERE id=?`, h, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, id, keepSession); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckPassword verifies a local account's password. A missing, disabled,
// external or wrong account all give ErrNotFound, after comparable work.
func (s *Store) CheckPassword(username, password string) (User, error) {
	var hash string
	row := s.db.QueryRow(`SELECT `+userCols+`, pwhash FROM users WHERE username=? AND source='local'`, username)
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.Created, &u.Source, &u.TwoStep, &hash)
	if err != nil {
		// Burn comparable time so a missing user is not distinguishable.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return User{}, ErrNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || u.Disabled {
		return User{}, ErrNotFound
	}
	return u, nil
}

// NewSession starts a session that lasts ttl.
func (s *Store) NewSession(userID int64, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	now := time.Now()
	s.db.Exec(`DELETE FROM sessions WHERE expires<?`, now.Unix())
	_, err := s.db.Exec(`INSERT INTO sessions(token, user_id, expires) VALUES(?,?,?)`, token, userID, now.Add(ttl).Unix())
	return token, err
}

// Login checks a local password and returns a new full session.
func (s *Store) Login(username, password string) (string, User, error) {
	u, err := s.CheckPassword(username, password)
	if err != nil {
		return "", u, err
	}
	token, err := s.NewSession(u.ID, sessionTTL)
	return token, u, err
}

// SessionUser resolves a cookie session. Disabled users have no session.
func (s *Store) SessionUser(token string) (Principal, error) {
	var u User
	err := s.db.QueryRow(`SELECT u.id, u.username, u.role, u.disabled, u.created, u.source, u.totp_enabled FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token=? AND s.expires>? AND u.disabled=0`, token, time.Now().Unix()).
		Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.Created, &u.Source, &u.TwoStep)
	if err != nil {
		return Principal{}, ErrNotFound
	}
	return Principal{User: u, Role: u.Role}, nil
}

func (s *Store) Logout(token string) {
	s.db.Exec(`DELETE FROM sessions WHERE token=? OR expires<?`, token, time.Now().Unix())
}

// --- API tokens ---

type APIToken struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Created  int64  `json:"created"`
	Expires  int64  `json:"expires,omitempty"`
	LastUsed int64  `json:"lastUsed,omitempty"`
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateToken makes a token for the user and returns its secret, which is
// not stored and cannot be shown again.
func (s *Store) CreateToken(userID int64, name, role string, expires int64) (APIToken, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return APIToken{}, "", err
	}
	secret := "virticle_" + base64.RawURLEncoding.EncodeToString(b)
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO api_tokens(user_id, name, hash, role, created, expires) VALUES(?,?,?,?,?,?)`,
		userID, name, hashToken(secret), role, now, expires)
	if err != nil {
		return APIToken{}, "", err
	}
	id, _ := res.LastInsertId()
	return APIToken{ID: id, Name: name, Role: role, Created: now, Expires: expires}, secret, nil
}

func (s *Store) Tokens(userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT id, name, role, created, expires, last_used FROM api_tokens WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.Created, &t.Expires, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TokenUser resolves a bearer token. The effective role is the lower of the
// token's role and the user's current role.
func (s *Store) TokenUser(secret string) (Principal, error) {
	var u User
	var t APIToken
	err := s.db.QueryRow(`SELECT u.id, u.username, u.role, u.created, t.id, t.name, t.role, t.expires, t.last_used
		FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.hash=? AND u.disabled=0`, hashToken(secret)).
		Scan(&u.ID, &u.Username, &u.Role, &u.Created, &t.ID, &t.Name, &t.Role, &t.Expires, &t.LastUsed)
	if err != nil {
		return Principal{}, ErrNotFound
	}
	now := time.Now().Unix()
	if t.Expires != 0 && t.Expires <= now {
		return Principal{}, ErrNotFound
	}
	if now-t.LastUsed >= 60 {
		s.db.Exec(`UPDATE api_tokens SET last_used=? WHERE id=?`, now, t.ID)
	}
	role := t.Role
	if RoleRank(u.Role) < RoleRank(role) {
		role = u.Role
	}
	return Principal{User: u, Role: role, Token: t.Name}, nil
}

// --- audit log ---

type AuditEntry struct {
	ID     int64  `json:"id"`
	Time   int64  `json:"time"`
	User   string `json:"user"`
	Token  string `json:"token,omitempty"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	// Detail says what a sign-in or settings request did, where the path and status cannot (never a secret).
	Detail string `json:"detail,omitempty"`
}

func (s *Store) AddAudit(e AuditEntry) error {
	res, err := s.db.Exec(`INSERT INTO audit(time, username, token, ip, method, path, status, detail) VALUES(?,?,?,?,?,?,?,?)`,
		e.Time, e.User, e.Token, e.IP, e.Method, e.Path, e.Status, e.Detail)
	if err != nil {
		return err
	}
	if id, _ := res.LastInsertId(); id%auditPruneGap == 0 {
		return s.PruneAudit()
	}
	return nil
}

// PruneAudit drops entries older than the retention age or beyond the cap.
func (s *Store) PruneAudit() error {
	if _, err := s.db.Exec(`DELETE FROM audit WHERE time<?`, time.Now().Add(-auditMaxAge).Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM audit WHERE id <= (SELECT max(id) FROM audit) - ?`, auditMaxRows)
	return err
}

type AuditFilter struct {
	User, Method, Path string
	Status             int
	Since, Until       int64
	Before             int64 // entry id cursor: only older entries
	Limit              int
}

// Audit returns entries newest first.
func (s *Store) Audit(f AuditFilter) ([]AuditEntry, error) {
	q := `SELECT id, time, username, token, ip, method, path, status, detail FROM audit WHERE 1=1`
	var args []any
	add := func(cond string, v any) { q += " AND " + cond; args = append(args, v) }
	if f.User != "" {
		add("username=?", f.User)
	}
	if f.Method != "" {
		add("method=?", strings.ToUpper(f.Method))
	}
	if f.Path != "" {
		add("instr(path, ?)>0", f.Path)
	}
	if f.Status != 0 {
		add("status=?", f.Status)
	}
	if f.Since != 0 {
		add("time>=?", f.Since)
	}
	if f.Until != 0 {
		add("time<=?", f.Until)
	}
	if f.Before != 0 {
		add("id<?", f.Before)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Time, &e.User, &e.Token, &e.IP, &e.Method, &e.Path, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
