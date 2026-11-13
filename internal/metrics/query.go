package metrics

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxPoints is about how many points one series gets in an answer.
const MaxPoints = 600

// Request is a range query for one VM or node.
type Request struct {
	Kind   string // "vm" or "node"
	ID     string // VM uuid or node id
	From   time.Time
	To     time.Time
	Groups []string // cpu, memory, disk, network, storage, gpu; empty means all
	Device string   // a device id from Result.Devices; empty means all (sum)
}

// Vals is a series; NaN is written as null.
type Vals []float64

func (v Vals) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, len(v)*6+2)
	b = append(b, '[')
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		if isNaN(x) || math.IsInf(x, 0) {
			b = append(b, "null"...)
		} else {
			b = strconv.AppendFloat(b, math.Round(x*1000)/1000, 'f', -1, 64)
		}
	}
	return append(b, ']'), nil
}

// Device is one disk, card, pool or GPU an entity has data for.
type Device struct {
	ID   string `json:"id"` // "kind:name", what Request.Device takes
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Event marks a change in a VM's life on the time axis.
type Event struct {
	T    int64  `json:"t"`
	Kind string `json:"kind"` // started, stopped, migrated
	Node int64  `json:"node,omitempty"`
}

// Result is the answer: one time array and one array per series.
type Result struct {
	Kind    string              `json:"kind"`
	ID      string              `json:"id"`
	From    int64               `json:"from"`
	To      int64               `json:"to"`
	Step    int64               `json:"step"`
	Peak    bool                `json:"peak"` // Max holds the highest value in each step
	T       []int64             `json:"t"`
	Series  map[string]Vals     `json:"series"`
	Max     map[string]Vals     `json:"max,omitempty"`
	Devices map[string][]Device `json:"devices"` // by group
	Events  []Event             `json:"events"`
}

// ChooseStep picks the tier and the step of an answer: the finest tier that
// still holds the start of the range, regrouped so that a series has at most
// about MaxPoints points.
func ChooseStep(now, from, to time.Time) (tier int, step int64) {
	span := to.Sub(from).Seconds()
	if span < 1 {
		span = 1
	}
	tier = len(Tiers) - 1
	for k := range Tiers {
		if from.Unix() >= now.Unix()-Tiers[k].Retention {
			tier = k
			break
		}
	}
	want := math.Ceil(span / MaxPoints)
	st := Tiers[tier].Step
	step = st
	if int64(want) > st {
		step = (int64(want) + st - 1) / st * st
	}
	return tier, step
}

var allGroups = []string{"cpu", "memory", "disk", "network", "storage", "gpu"}

func validGroup(kind, g string) bool {
	switch g {
	case "cpu", "memory", "disk", "network":
		return true
	case "storage", "gpu":
		return kind == "node"
	}
	return false
}

// kinds that make up "All" for a group of one entity kind.
func sumKinds(kind, group string) []string {
	switch {
	case kind == "vm" && group == "disk":
		return []string{"disk"}
	case kind == "vm" && group == "network":
		return []string{"vnet"}
	case group == "network":
		return []string{"nic"} // physical cards; bridges would count the same traffic twice
	case group == "disk":
		return []string{"bdev"}
	case group == "storage":
		return []string{"pool"}
	case group == "gpu":
		return []string{"gpu"}
	}
	return nil
}

func (d *DB) lookup(name string) (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.names[name]; ok {
		return id, true
	}
	var id int64
	if err := d.db.QueryRow(`SELECT id FROM names WHERE name=?`, name).Scan(&id); err != nil {
		return 0, false
	}
	d.names[name] = id
	return id, true
}

func selectExprs(cols []col, tier int, only func(string) bool) (exprs, names []string) {
	for _, c := range cols {
		if only != nil && !only(c.name) {
			continue
		}
		switch c.kind {
		case kLast:
			exprs = append(exprs, "max("+c.name+")", "max("+c.name+")")
		case kAvg:
			exprs = append(exprs, "avg("+c.name+")", "NULL")
		default:
			peak := "max(" + c.name + ")"
			if tier > 0 {
				peak = "max(" + c.name + "_max)"
			}
			exprs = append(exprs, "avg("+c.name+")", peak)
		}
		names = append(names, c.name)
	}
	return
}

// Query answers a range query.
func (d *DB) Query(req Request) (*Result, error) {
	if req.Kind != "vm" && req.Kind != "node" {
		return nil, fmt.Errorf("kind must be vm or node")
	}
	if !req.To.After(req.From) {
		return nil, fmt.Errorf("the end of the range must be after its start")
	}
	if req.To.Sub(req.From) > 8*24*time.Hour {
		return nil, fmt.Errorf("the range is longer than the 7 days that are kept")
	}
	groups := req.Groups
	if len(groups) == 0 {
		groups = allGroups
	}
	var use []string
	for _, g := range groups {
		if !validGroup(req.Kind, g) {
			if g == "storage" || g == "gpu" {
				continue
			}
			return nil, fmt.Errorf("unknown metric group %q", g)
		}
		use = append(use, g)
	}
	now := d.now()
	tier, step := ChooseStep(now, req.From, req.To)
	// A device was asked for: the per-device tables hold 15 s too, so the tier stays.
	first := req.From.Unix() / step * step
	last := req.To.Unix() / step * step
	n := int((last-first)/step) + 1
	res := &Result{Kind: req.Kind, ID: req.ID, From: req.From.Unix(), To: req.To.Unix(), Step: step, Peak: step > Tiers[0].Step,
		T: make([]int64, n), Series: map[string]Vals{}, Max: map[string]Vals{}, Devices: map[string][]Device{}, Events: []Event{}}
	for i := range res.T {
		res.T[i] = first + int64(i)*step
	}
	if !res.Peak {
		res.Max = nil
	}
	prefix := "vm:"
	if req.Kind == "node" {
		prefix = "node:"
	}
	ent, ok := d.lookup(prefix + req.ID)
	if !ok {
		return res, nil
	}
	lo, hi := first, last+step-1
	put := func(name string, avg, peak Vals) {
		res.Series[name] = avg
		if res.Max != nil && peak != nil {
			res.Max[name] = peak
		}
	}
	wide := "vm"
	wideCols := vmCols
	if req.Kind == "node" {
		wide, wideCols = "node", nodeCols
	}

	// The wide row: the groups that live there, plus the node column that
	// tells where a VM ran.
	inGroup := map[string]bool{}
	for _, g := range use {
		inGroup[g] = true
	}
	devGroup := func(g string) bool {
		return req.Kind == "node" && (g == "disk" || g == "network") || g == "storage" || g == "gpu"
	}
	wantCol := func(name string) bool {
		g := groupOf(name)
		return g != "" && inGroup[g] && !(devGroup(g) || (req.Device != "" && (g == "disk" || g == "network")))
	}
	exprs, names := selectExprs(wideCols, tier, func(s string) bool { return wantCol(s) || (s == "node" && req.Kind == "vm") })
	if len(names) > 0 {
		q := fmt.Sprintf("SELECT (t/%d)*%d, %s FROM %s WHERE ent=? AND t>=? AND t<=? GROUP BY t/%d ORDER BY 1",
			step, step, strings.Join(exprs, ","), tableFor(wide, tier), step)
		if err := d.scanSeries(q, []any{ent, lo, hi}, names, first, step, n, func(name string, avg, peak Vals) {
			if name == "node" {
				res.Series["\x00node"] = avg
				return
			}
			put(name, avg, peak)
		}); err != nil {
			return nil, err
		}
	}
	if nodes, ok := res.Series["\x00node"]; ok {
		delete(res.Series, "\x00node")
		res.Events = events(res.T, step, nodes, now, req.To)
	}

	// Devices present in the range, and the groups' series from them.
	devs, err := d.devices(ent, tier, lo, hi)
	if err != nil {
		return nil, err
	}
	for _, g := range allGroups {
		for _, dv := range devs {
			k := kindOf(dv.Kind)
			if k != nil && k.Entity == req.Kind && k.Group == g {
				res.Devices[g] = append(res.Devices[g], dv)
			}
		}
	}
	for _, g := range use {
		switch {
		case req.Device != "":
			kind, _, _ := strings.Cut(req.Device, ":")
			k := kindOf(kind)
			if k == nil || k.Entity != req.Kind || k.Group != g {
				continue
			}
			id, ok := d.lookup(req.Device)
			if !ok {
				continue
			}
			if err := d.deviceSeries(k, []int64{id}, "", ent, tier, step, lo, hi, first, n, put); err != nil {
				return nil, err
			}
		case devGroup(g):
			if g == "storage" || g == "gpu" {
				for _, dv := range res.Devices[g] {
					id, ok := d.lookup(dv.ID)
					if !ok {
						continue
					}
					if err := d.deviceSeries(kindOf(dv.Kind), []int64{id}, "/"+dv.Name, ent, tier, step, lo, hi, first, n, put); err != nil {
						return nil, err
					}
				}
				continue
			}
			// All (sum) of the cards or disks.
			for _, kn := range sumKinds(req.Kind, g) {
				var ids []int64
				for _, dv := range res.Devices[g] {
					if dv.Kind == kn {
						if id, ok := d.lookup(dv.ID); ok {
							ids = append(ids, id)
						}
					}
				}
				if len(ids) == 0 {
					continue
				}
				if err := d.deviceSeries(kindOf(kn), ids, "", ent, tier, step, lo, hi, first, n, put); err != nil {
					return nil, err
				}
			}
		}
	}
	return res, nil
}

// scanSeries runs a query that returns a bucket start followed by an
// average and a peak per name, and spreads the rows over the time grid.
func (d *DB) scanSeries(q string, args []any, names []string, first, step int64, n int, emit func(name string, avg, peak Vals)) error {
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	avg := make([]Vals, len(names))
	peak := make([]Vals, len(names))
	for i := range names {
		avg[i], peak[i] = make(Vals, n), make(Vals, n)
		for j := 0; j < n; j++ {
			avg[i][j], peak[i][j] = NaN, NaN
		}
	}
	for rows.Next() {
		var t int64
		vals := make([]sql.NullFloat64, 2*len(names))
		ptrs := make([]any, 1+len(vals))
		ptrs[0] = &t
		for i := range vals {
			ptrs[i+1] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		idx := int((t - first) / step)
		if idx < 0 || idx >= n {
			continue
		}
		for i := range names {
			if vals[2*i].Valid {
				avg[i][idx] = vals[2*i].Float64
			}
			if vals[2*i+1].Valid {
				peak[i][idx] = vals[2*i+1].Float64
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, nm := range names {
		emit(nm, avg[i], peak[i])
	}
	return nil
}

// devices lists the devices an entity has rows for in the range.
func (d *DB) devices(ent int64, tier int, lo, hi int64) ([]Device, error) {
	rows, err := d.db.Query(fmt.Sprintf(`SELECT n.name FROM names n WHERE n.id IN (SELECT DISTINCT dev FROM %s WHERE ent=? AND t>=? AND t<=?) ORDER BY n.name`, tableFor("dev", tier)), ent, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		kind, name, _ := strings.Cut(s, ":")
		out = append(out, Device{ID: s, Name: name, Kind: kind})
	}
	return out, rows.Err()
}

// deviceSeries reads the series of one or several devices of one kind; with
// several devices the values are combined as the kind says (sum or mean).
func (d *DB) deviceSeries(k *DevKind, ids []int64, suffix string, ent int64, tier int, step, lo, hi, first int64, n int, put func(string, Vals, Vals)) error {
	idList := make([]string, len(ids))
	for i, id := range ids {
		idList[i] = strconv.FormatInt(id, 10)
	}
	var inner, outer []string
	var names []string
	for i, s := range k.Series {
		if s == "" {
			continue
		}
		c := fmt.Sprintf("v%d", i+1)
		peak := "max(" + c + ")"
		if tier > 0 {
			peak = "max(" + c + "_max)"
		}
		inner = append(inner, fmt.Sprintf("avg(%s) a%d, %s p%d", c, i, peak, i))
		fn := "sum"
		if k.Agg[i] == aggAvg {
			fn = "avg"
		}
		outer = append(outer, fmt.Sprintf("%s(a%d), %s(p%d)", fn, i, fn, i))
		names = append(names, s)
	}
	q := fmt.Sprintf("SELECT b, %s FROM (SELECT dev, (t/%d)*%d b, %s FROM %s WHERE ent=? AND dev IN (%s) AND t>=? AND t<=? GROUP BY dev, t/%d) GROUP BY b ORDER BY b",
		strings.Join(outer, ","), step, step, strings.Join(inner, ","), tableFor("dev", tier), strings.Join(idList, ","), step)
	return d.scanSeries(q, []any{ent, lo, hi}, names, first, step, n, func(name string, avg, peak Vals) {
		put(name+suffix, avg, peak)
	})
}

// events finds starts, stops and moves of a VM from the node column of the
// answer: no value for two steps or more is a stop followed by a start.
func events(t []int64, step int64, node Vals, now, to time.Time) []Event {
	out := []Event{}
	lastSeen := -1
	missing := 0
	for i, v := range node {
		if isNaN(v) {
			missing++
			continue
		}
		if lastSeen < 0 {
			if missing >= 2 {
				out = append(out, Event{T: t[i], Kind: "started", Node: int64(v)})
			}
		} else if missing >= 2 {
			out = append(out, Event{T: t[lastSeen], Kind: "stopped"}, Event{T: t[i], Kind: "started", Node: int64(v)})
		} else if v != node[lastSeen] {
			out = append(out, Event{T: t[i], Kind: "migrated", Node: int64(v)})
		}
		lastSeen, missing = i, 0
	}
	// A gap that reaches the end of the range is a stop, unless it is only
	// the newest steps that were not written or rolled up yet.
	if lastSeen >= 0 {
		end := to.Unix()
		if now.Unix() < end {
			end = now.Unix()
		}
		if end-t[lastSeen] > 3*step {
			out = append(out, Event{T: t[lastSeen], Kind: "stopped"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}
