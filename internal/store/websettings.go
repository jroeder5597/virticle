package store

// Controller-wide web access settings (listener, TLS mode, proxies) are kept as
// small JSON documents under a key. The table is created on first use.
const webSettingsSchema = `CREATE TABLE IF NOT EXISTS web_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`

// WebSetting returns the stored value; ok is false when the key was never set.
func (s *Store) WebSetting(key string) (value string, ok bool, err error) {
	if _, err = s.db.Exec(webSettingsSchema); err != nil {
		return "", false, err
	}
	rows, err := s.db.Query(`SELECT value FROM web_settings WHERE key=?`, key)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	if rows.Next() {
		err = rows.Scan(&value)
		return value, err == nil, err
	}
	return "", false, rows.Err()
}

// SetWebSetting stores value under key.
func (s *Store) SetWebSetting(key, value string) error {
	if _, err := s.db.Exec(webSettingsSchema); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO web_settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// DeleteWebSetting removes a key; a missing key is not an error.
func (s *Store) DeleteWebSetting(key string) error {
	if _, err := s.db.Exec(webSettingsSchema); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM web_settings WHERE key=?`, key)
	return err
}
