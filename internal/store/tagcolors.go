package store

// tagSchema is applied on every open, like profileSchema. A colour belongs to the tag NAME and so applies
// to every VM that carries the tag. Tag names themselves live in libvirt domain metadata.
const tagSchema = `
CREATE TABLE IF NOT EXISTS tag_colors (name TEXT PRIMARY KEY, color TEXT NOT NULL);
`

// TagColors returns every stored colour by tag name.
func (s *Store) TagColors() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, color FROM tag_colors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, c string
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

func (s *Store) SetTagColor(name, color string) error {
	_, err := s.db.Exec(`INSERT INTO tag_colors(name, color) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET color=excluded.color`, name, color)
	return err
}

// DeleteTagColor removes a colour; removing one that does not exist is not an error.
func (s *Store) DeleteTagColor(name string) error {
	_, err := s.db.Exec(`DELETE FROM tag_colors WHERE name=?`, name)
	return err
}
