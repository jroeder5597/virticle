package store

// prefSchema is applied on every open. A preference belongs to a user, so it follows them across
// browsers; the allowed keys and values are checked by the API layer, not here.
const prefSchema = `
CREATE TABLE IF NOT EXISTS user_prefs (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key TEXT NOT NULL, value TEXT NOT NULL,
  PRIMARY KEY (user_id, key));
`

// UserPrefs returns every stored preference of one user.
func (s *Store) UserPrefs(userID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM user_prefs WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetUserPrefs stores the given preferences, all or none; keys not mentioned are left alone.
func (s *Store) SetUserPrefs(userID int64, prefs map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range prefs {
		if _, err := tx.Exec(`INSERT INTO user_prefs(user_id, key, value) VALUES(?,?,?) ON CONFLICT(user_id, key) DO UPDATE SET value=excluded.value`, userID, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
