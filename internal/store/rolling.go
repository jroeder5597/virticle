package store

import "database/sql"

// Rolling updates (internal/api/rolling.go): one row per run. The state of the run is one JSON text that the API
// layer owns; the columns next to it only let the store find the runs that are unfinished.
const rollSchema = `
CREATE TABLE IF NOT EXISTS rolling_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  started INTEGER NOT NULL,
  finished INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  state TEXT NOT NULL);
`

// RollRow is one stored rolling update run.
type RollRow struct {
	ID       int64
	Started  int64
	Finished int64
	Status   string // running | ok | failed | cancelled
	State    string // JSON
}

// CreateRoll stores a new run and returns its id.
func (s *Store) CreateRoll(started int64, status, state string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO rolling_runs(started, status, state) VALUES(?,?,?)`, started, status, state)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SaveRoll replaces the status and state of a run.
func (s *Store) SaveRoll(id, finished int64, status, state string) error {
	_, err := s.db.Exec(`UPDATE rolling_runs SET finished=?, status=?, state=? WHERE id=?`, finished, status, state, id)
	return err
}

// LatestRoll returns the newest run, if there is one.
func (s *Store) LatestRoll() (RollRow, bool, error) {
	var r RollRow
	err := s.db.QueryRow(`SELECT id, started, finished, status, state FROM rolling_runs ORDER BY id DESC LIMIT 1`).
		Scan(&r.ID, &r.Started, &r.Finished, &r.Status, &r.State)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

// PruneRolls keeps the newest keep runs.
func (s *Store) PruneRolls(keep int) {
	s.db.Exec(`DELETE FROM rolling_runs WHERE id NOT IN (SELECT id FROM rolling_runs ORDER BY id DESC LIMIT ?)`, keep)
}
