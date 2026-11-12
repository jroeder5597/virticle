package store

import (
	"database/sql"
	"fmt"
)

// migrateNodeConnection adds the connection settings columns to a nodes table
// made before they existed. Existing nodes keep working: an empty privilege
// means "detect on the next connect", an empty migration address means "use
// the address the controller uses".
func migrateNodeConnection(tx *sql.Tx, have map[string]bool) error {
	for _, c := range []struct{ name, def string }{
		{"privilege", "''"},
		{"migration_addr", "''"},
		{"host_uuid", "''"},
	} {
		if have[c.name] {
			continue
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE nodes ADD COLUMN %s TEXT NOT NULL DEFAULT %s`, c.name, c.def)); err != nil {
			return err
		}
	}
	// "sudo" was the privilege of a login with broad passwordless sudo, a mode
	// that no longer exists. Such a node is not prepared until detected again
	// ("" means detect on the next connect).
	_, err := tx.Exec(`UPDATE nodes SET privilege='' WHERE privilege='sudo'`)
	return err
}

// UpdateNodeConnection changes how a node is reached. The id, the maintenance
// flag and everything keyed to the id stay.
func (s *Store) UpdateNodeConnection(id int64, n Node) error {
	res, err := s.db.Exec(`UPDATE nodes SET addr=?, user=?, jump=?, privilege=?, migration_addr=?, host_uuid=? WHERE id=?`,
		n.Addr, n.User, n.Jump, n.Privilege, n.MigrationAddr, n.HostUUID, id)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodePrivilege stores the detected privilege of a node.
func (s *Store) SetNodePrivilege(id int64, p string) error {
	_, err := s.db.Exec(`UPDATE nodes SET privilege=? WHERE id=?`, p, id)
	return err
}

// SetNodeHostUUID stores the libvirt host UUID seen on a node.
func (s *Store) SetNodeHostUUID(id int64, u string) error {
	_, err := s.db.Exec(`UPDATE nodes SET host_uuid=? WHERE id=?`, u, id)
	return err
}

// GetNode returns one node.
func (s *Store) GetNode(id int64) (Node, error) {
	all, err := s.Nodes()
	if err != nil {
		return Node{}, err
	}
	for _, n := range all {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, ErrNotFound
}
