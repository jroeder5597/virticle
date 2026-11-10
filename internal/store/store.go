// Package store holds the controller's own state. VM, storage and network
// state is never stored here: libvirt on each node is the source of truth.
package store

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, pwhash TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS nodes (
  id INTEGER PRIMARY KEY, addr TEXT UNIQUE NOT NULL, user TEXT NOT NULL, jump TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tasks (
  id INTEGER PRIMARY KEY, kind TEXT NOT NULL, target TEXT NOT NULL, node TEXT NOT NULL, username TEXT NOT NULL,
  status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', progress REAL NOT NULL DEFAULT 0,
  started INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0);
`

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

type Node struct {
	ID   int64  `json:"id"`
	Addr string `json:"addr"`
	User string `json:"user"`
	Jump string `json:"jump,omitempty"`
	// Maintenance keeps the node out of the targets the planners pick.
	Maintenance bool `json:"maintenance"`
}

type Task struct {
	ID       int64   `json:"id"`
	Kind     string  `json:"kind"`
	Target   string  `json:"target"`
	Node     string  `json:"node"`
	User     string  `json:"user"`
	Status   string  `json:"status"` // running | ok | failed
	Error    string  `json:"error,omitempty"`
	Progress float64 `json:"progress"`
	Started  int64   `json:"started"`
	Finished int64   `json:"finished,omitempty"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	// Tasks still marked running belong to a previous process that is gone.
	if _, err := db.Exec(`UPDATE tasks SET status='failed', error='controller restarted', finished=? WHERE status='running'`, time.Now().Unix()); err != nil {
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- nodes ---

func (s *Store) Nodes() ([]Node, error) {
	rows, err := s.db.Query(`SELECT id, addr, user, jump, maintenance FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Addr, &n.User, &n.Jump, &n.Maintenance); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) AddNode(n Node) (Node, error) {
	res, err := s.db.Exec(`INSERT INTO nodes(addr, user, jump, created) VALUES(?,?,?,?)`, n.Addr, n.User, n.Jump, time.Now().Unix())
	if err != nil {
		return n, err
	}
	n.ID, _ = res.LastInsertId()
	return n, nil
}

func (s *Store) DeleteNode(id int64) error {
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, id)
	return err
}

// --- tasks ---

func (s *Store) StartTask(kind, target, node, user string) (Task, error) {
	t := Task{Kind: kind, Target: target, Node: node, User: user, Status: "running", Started: time.Now().Unix()}
	res, err := s.db.Exec(`INSERT INTO tasks(kind, target, node, username, status, started) VALUES(?,?,?,?,?,?)`,
		kind, target, node, user, t.Status, t.Started)
	if err != nil {
		return t, err
	}
	t.ID, _ = res.LastInsertId()
	return t, nil
}

func (s *Store) TaskProgress(id int64, p float64) {
	s.db.Exec(`UPDATE tasks SET progress=? WHERE id=?`, p, id)
}

func (s *Store) FinishTask(t *Task, err error) {
	t.Finished = time.Now().Unix()
	if err != nil {
		t.Status, t.Error = "failed", err.Error()
	} else {
		t.Status, t.Progress = "ok", 1
	}
	s.db.Exec(`UPDATE tasks SET status=?, error=?, progress=?, finished=? WHERE id=?`, t.Status, t.Error, t.Progress, t.Finished, t.ID)
}

func (s *Store) Tasks(limit int) ([]Task, error) {
	rows, err := s.db.Query(`SELECT id, kind, target, node, username, status, error, progress, started, finished FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Kind, &t.Target, &t.Node, &t.User, &t.Status, &t.Error, &t.Progress, &t.Started, &t.Finished); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
