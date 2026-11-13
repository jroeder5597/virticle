package metrics

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// schemaVersion is bumped whenever the table layout changes. A file with
// another version is simply replaced: it only holds history.
const schemaVersion = 1

// DB is the metrics file.
type DB struct {
	db   *sql.DB
	path string
	now  func() time.Time

	mu    sync.Mutex // one writer at a time; guards the caches
	names map[string]int64
	label map[int64]string

	lastPrune time.Time
}

// Open opens or creates the file. A file that cannot be used is replaced.
func Open(path string) (*DB, error) {
	d, err := open(path)
	if err != nil {
		log.Printf("metrics: %s is not usable (%v); starting a new one", path, err)
		for _, s := range []string{"", "-wal", "-shm"} {
			os.Remove(path + s)
		}
		d, err = open(path)
	}
	return d, err
}

func open(path string) (*DB, error) {
	// Appends dominate: WAL, and a sync only at checkpoints. Losing the last
	// seconds on a power cut is fine for a performance history.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(INCREMENTAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	d := &DB{db: db, path: path, now: time.Now, names: map[string]int64{}, label: map[int64]string{}}
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		db.Close()
		return nil, err
	}
	if ver != 0 && ver != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("schema version %d", ver)
	}
	if _, err := db.Exec(ddl()); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.db.Close() }

// Path is the file's location.
func (d *DB) Path() string { return d.path }

// SetClock replaces the clock, for tests.
func (d *DB) SetClock(now func() time.Time) { d.now = now }

func colDefs(cols []col, tier int) string {
	var b strings.Builder
	for _, c := range cols {
		b.WriteString(", " + c.name + " REAL")
		if tier > 0 && c.kind == kGauge {
			b.WriteString(", " + c.name + "_max REAL")
		}
	}
	return b.String()
}

func devCols() []col {
	cs := make([]col, nDev)
	for i := range cs {
		cs[i] = col{"v" + strconv.Itoa(i+1), kGauge}
	}
	return cs
}

func ddl() string {
	var b strings.Builder
	b.WriteString(`CREATE TABLE IF NOT EXISTS names(id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, label TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v INTEGER NOT NULL);
`)
	for t := range Tiers {
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS vm_%d(ent INTEGER NOT NULL, t INTEGER NOT NULL%s, PRIMARY KEY(ent,t)) WITHOUT ROWID;\n", t, colDefs(vmCols, t))
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS nd_%d(ent INTEGER NOT NULL, t INTEGER NOT NULL%s, PRIMARY KEY(ent,t)) WITHOUT ROWID;\n", t, colDefs(nodeCols, t))
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS dv_%d(ent INTEGER NOT NULL, dev INTEGER NOT NULL, t INTEGER NOT NULL%s, PRIMARY KEY(ent,dev,t)) WITHOUT ROWID;\n", t, colDefs(devCols(), t))
	}
	return b.String()
}

func tableFor(entity string, tier int) string {
	return fmt.Sprintf("%s_%d", map[string]string{"vm": "vm", "node": "nd", "dev": "dv"}[entity], tier)
}

func colsFor(entity string) []col {
	switch entity {
	case "vm":
		return vmCols
	case "node":
		return nodeCols
	}
	return devCols()
}

// id interns a name; the caller holds d.mu and a transaction when writing.
func (d *DB) id(q interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}, name, label string) (int64, error) {
	if id, ok := d.names[name]; ok {
		if label != "" && d.label[id] != label {
			if _, err := q.Exec(`UPDATE names SET label=? WHERE id=?`, label, id); err != nil {
				return 0, err
			}
			d.label[id] = label
		}
		return id, nil
	}
	var id int64
	err := q.QueryRow(`SELECT id FROM names WHERE name=?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		res, err := q.Exec(`INSERT INTO names(name,label) VALUES(?,?)`, name, label)
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
		d.names[name], d.label[id] = id, label
		return id, nil
	}
	if err != nil {
		return 0, err
	}
	d.names[name] = id
	if label != "" && d.label[id] != label {
		if _, err := q.Exec(`UPDATE names SET label=? WHERE id=?`, label, id); err != nil {
			return 0, err
		}
	}
	d.label[id] = label
	return id, nil
}

func nullable(v float64) any {
	if isNaN(v) {
		return nil
	}
	return v
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Insert writes a batch in one transaction.
func (d *DB) Insert(b Batch) error { return d.InsertMany([]Batch{b}) }

// InsertMany writes several batches in one transaction.
func (d *DB) InsertMany(bs []Batch) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stm := map[string]*sql.Stmt{}
	prep := func(entity string, cols []col) (*sql.Stmt, error) {
		if s := stm[entity]; s != nil {
			return s, nil
		}
		names := []string{"ent"}
		if entity == "dev" {
			names = append(names, "dev")
		}
		names = append(names, "t")
		for _, c := range cols {
			names = append(names, c.name)
		}
		s, err := tx.Prepare(fmt.Sprintf("INSERT OR REPLACE INTO %s(%s) VALUES(%s)", tableFor(entity, 0), strings.Join(names, ","), placeholders(len(names))))
		stm[entity] = s
		return s, err
	}
	for _, b := range bs {
		for _, r := range b.Rows {
			ent, err := d.id(tx, r.Ent, r.Label)
			if err != nil {
				return err
			}
			entity := "vm"
			if strings.HasPrefix(r.Ent, "node:") {
				entity = "node"
			}
			s, err := prep(entity, colsFor(entity))
			if err != nil {
				return err
			}
			args := []any{ent, b.T}
			for _, v := range r.Vals {
				args = append(args, nullable(v))
			}
			if _, err := s.Exec(args...); err != nil {
				return err
			}
			for _, dv := range r.Devs {
				did, err := d.id(tx, dv.Kind+":"+dv.Name, "")
				if err != nil {
					return err
				}
				ds, err := prep("dev", colsFor("dev"))
				if err != nil {
					return err
				}
				args := []any{ent, did, b.T}
				for _, v := range dv.Vals {
					args = append(args, nullable(v))
				}
				if _, err := ds.Exec(args...); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// --- rollup and pruning ---

// rollupSQL builds the statement that folds tier-1 rows into tier "to"
// buckets for one table. Average of averages is exact here because every
// source row stands for the same span of time.
func rollupSQL(entity string, to int) string {
	from := to - 1
	cols := colsFor(entity)
	keys := "ent"
	if entity == "dev" {
		keys = "ent, dev"
	}
	names := []string{}
	exprs := []string{}
	for _, c := range cols {
		names = append(names, c.name)
		switch c.kind {
		case kLast:
			exprs = append(exprs, "max("+c.name+")")
		default:
			exprs = append(exprs, "avg("+c.name+")")
		}
		if c.kind == kGauge {
			names = append(names, c.name+"_max")
			if from == 0 {
				exprs = append(exprs, "max("+c.name+")")
			} else {
				exprs = append(exprs, "max("+c.name+"_max)")
			}
		}
	}
	return fmt.Sprintf("INSERT OR REPLACE INTO %s(%s, t, %s) SELECT %s, (t/%d)*%d, %s FROM %s WHERE t >= ? AND t < ? GROUP BY %s, t/%d",
		tableFor(entity, to), keys, strings.Join(names, ","), keys, Tiers[to].Step, Tiers[to].Step,
		strings.Join(exprs, ","), tableFor(entity, from), keys, Tiers[to].Step)
}

// Maintain rolls finished buckets up into the coarser tiers and deletes what
// is past its retention. It is cheap enough to run every half minute; pruning
// itself runs at most every ten minutes.
func (d *DB) Maintain() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().Unix()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for to := 1; to < len(Tiers); to++ {
		w := Tiers[to].Step
		complete := now / w * w // buckets before this are finished; the open one is rolled again each time
		key := fmt.Sprintf("rolled_%d", to)
		var start int64
		tx.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&start)
		if start == 0 {
			start = complete - Tiers[to-1].Retention
		}
		if start < complete-Tiers[to-1].Retention {
			start = complete - Tiers[to-1].Retention
		}
		start = start / w * w
		for _, e := range []string{"vm", "node", "dev"} {
			if _, err := tx.Exec(rollupSQL(e, to), start, now+1); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(k,v) VALUES(?,?)`, key, complete); err != nil {
			return err
		}
	}
	if d.lastPrune.IsZero() || d.now().Sub(d.lastPrune) >= 10*time.Minute {
		for t, tier := range Tiers {
			for _, e := range []string{"vm", "node", "dev"} {
				if _, err := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE t < ?", tableFor(e, t)), now-tier.Retention); err != nil {
					return err
				}
			}
		}
		d.lastPrune = d.now()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Free pages are reused by new rows, so the file stays flat. Give back
	// space only when much of the file is empty (many removed VMs).
	var pages, free int64
	d.db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	d.db.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	if pages > 2000 && free*2 > pages {
		d.db.Exec(`PRAGMA incremental_vacuum`)
	}
	return nil
}

// Run maintains the file every 30 seconds until stop is closed.
func (d *DB) Run(stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if err := d.Maintain(); err != nil {
			log.Printf("metrics: maintenance: %v", err)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
