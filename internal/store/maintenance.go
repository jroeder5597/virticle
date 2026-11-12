package store

import (
	"database/sql"
)

// migrateNodes adds the maintenance flag to databases created before it
// existed. Like the rest of migrate it checks before it changes anything.
func migrateNodes(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info('nodes')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(have) > 0 && !have["maintenance"] {
		if _, err = tx.Exec(`ALTER TABLE nodes ADD COLUMN maintenance INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if len(have) > 0 && !have["migtls_off"] {
		if _, err = tx.Exec(`ALTER TABLE nodes ADD COLUMN migtls_off INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return migrateNodeConnection(tx, have)
}

// SetMigTLSOff records that encrypted migration was removed from a node by hand, so the
// controller does not set it up again until an administrator asks for it.
func (s *Store) SetMigTLSOff(id int64, off bool) error {
	v := 0
	if off {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE nodes SET migtls_off=? WHERE id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMaintenance marks or unmarks a node as being in maintenance.
func (s *Store) SetMaintenance(id int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE nodes SET maintenance=? WHERE id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TasksBy lists the tasks started by a user since a Unix time, oldest first.
func (s *Store) TasksBy(user string, since int64) ([]Task, error) {
	rows, err := s.db.Query(`SELECT id, kind, target, node, username, status, error, progress, started, finished
		FROM tasks WHERE username=? AND started>=? ORDER BY id`, user, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Kind, &t.Target, &t.Node, &t.User, &t.Status, &t.Error, &t.Progress, &t.Started, &t.Finished); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
