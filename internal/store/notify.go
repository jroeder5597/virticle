package store

import "database/sql"

// Notification destinations: where alerts are sent. The address and the signing
// secret are stored in the form the API layer gives (encrypted); Host and Tail
// are the harmless parts shown on the page.
const notifySchema = `
CREATE TABLE IF NOT EXISTS notify_destinations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL, name TEXT NOT NULL,
  url_enc TEXT NOT NULL, host TEXT NOT NULL, tail TEXT NOT NULL,
  severity TEXT NOT NULL DEFAULT 'warning',
  cleared INTEGER NOT NULL DEFAULT 1,
  secret_enc TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL);
`

type NotifyDest struct {
	ID        int64
	Kind      string // slack | mattermost | discord | webhook
	Name      string
	URLEnc    string
	Host      string
	Tail      string // last characters of the address
	Severity  string // warning (warnings and critical) | critical
	Cleared   bool
	SecretEnc string
	Created   int64
}

const notifyCols = `id, kind, name, url_enc, host, tail, severity, cleared, secret_enc, created`

func scanDest(r interface{ Scan(...any) error }) (NotifyDest, error) {
	var d NotifyDest
	var cl int
	err := r.Scan(&d.ID, &d.Kind, &d.Name, &d.URLEnc, &d.Host, &d.Tail, &d.Severity, &cl, &d.SecretEnc, &d.Created)
	d.Cleared = cl != 0
	return d, err
}

func (s *Store) NotifyDests() ([]NotifyDest, error) {
	rows, err := s.db.Query(`SELECT ` + notifyCols + ` FROM notify_destinations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyDest
	for rows.Next() {
		d, err := scanDest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) NotifyDest(id int64) (NotifyDest, error) {
	d, err := scanDest(s.db.QueryRow(`SELECT `+notifyCols+` FROM notify_destinations WHERE id=?`, id))
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return d, err
}

func (s *Store) CreateNotifyDest(d NotifyDest, now int64) (int64, error) {
	r, err := s.db.Exec(`INSERT INTO notify_destinations(kind,name,url_enc,host,tail,severity,cleared,secret_enc,created) VALUES(?,?,?,?,?,?,?,?,?)`,
		d.Kind, d.Name, d.URLEnc, d.Host, d.Tail, d.Severity, b2i(d.Cleared), d.SecretEnc, now)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// UpdateNotifyDest replaces everything except the kind and the creation time.
func (s *Store) UpdateNotifyDest(d NotifyDest) error {
	r, err := s.db.Exec(`UPDATE notify_destinations SET name=?, url_enc=?, host=?, tail=?, severity=?, cleared=?, secret_enc=? WHERE id=?`,
		d.Name, d.URLEnc, d.Host, d.Tail, d.Severity, b2i(d.Cleared), d.SecretEnc, d.ID)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteNotifyDest(id int64) error {
	r, err := s.db.Exec(`DELETE FROM notify_destinations WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveLegacyWebhook turns the single webhook address of earlier versions (the
// web setting under key) into a destination, once: the setting is removed in the
// same transaction. seal turns the address into the stored form and the parts
// shown on the page. It reports whether a destination was created.
func (s *Store) MoveLegacyWebhook(key string, seal func(url string) (enc, host, tail string, err error), now int64) (bool, error) {
	if _, err := s.db.Exec(webSettingsSchema); err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var u string
	switch err := tx.QueryRow(`SELECT value FROM web_settings WHERE key=?`, key).Scan(&u); err {
	case nil:
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, err
	}
	if u != "" {
		enc, host, tail, err := seal(u)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(`INSERT INTO notify_destinations(kind,name,url_enc,host,tail,severity,cleared,secret_enc,created) VALUES('webhook','Webhook',?,?,?,'warning',1,'',?)`,
			enc, host, tail, now); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM web_settings WHERE key=?`, key); err != nil {
		return false, err
	}
	return u != "", tx.Commit()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
