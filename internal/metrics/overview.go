package metrics

import (
	"fmt"
	"time"
)

// Busy is one VM in a ranking.
type Busy struct {
	UUID string  `json:"uuid"`
	Name string  `json:"name"`
	Node int64   `json:"node"`
	Avg  float64 `json:"avg"`
	Max  float64 `json:"max"`
}

// Overview ranks the busiest VMs in a range by CPU (percent), memory used
// (KiB), disk (bytes per second, read plus write) and network (bits per
// second, received plus sent), each by average.
type Overview struct {
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Step    int64  `json:"step"`
	Cpu     []Busy `json:"cpu"`
	Memory  []Busy `json:"memory"`
	Disk    []Busy `json:"disk"`
	Network []Busy `json:"network"`
}

var rankings = []struct {
	name string
	cols []string
}{
	{"cpu", []string{"cpu"}},
	{"memory", []string{"mem_used_kib"}},
	{"disk", []string{"disk_rd_bps", "disk_wr_bps"}},
	{"network", []string{"net_rx_bps", "net_tx_bps"}},
}

// Rank answers "what was busy in this range". top is the length of each
// list; node 0 means all nodes. The peak of a sum is the sum of the peaks,
// which can overstate when read and write peak at different moments.
func (d *DB) Rank(from, to time.Time, node int64, top int) (*Overview, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("the end of the range must be after its start")
	}
	if top < 1 {
		top = 5
	}
	tier, _ := ChooseStep(d.now(), from, to)
	o := &Overview{From: from.Unix(), To: to.Unix(), Step: Tiers[tier].Step}
	out := map[string]*[]Busy{"cpu": &o.Cpu, "memory": &o.Memory, "disk": &o.Disk, "network": &o.Network}
	for _, r := range rankings {
		sumAvg, sumPeak := "", ""
		for i, c := range r.cols {
			p := "coalesce(max(" + c + "), 0)"
			if tier > 0 {
				p = "coalesce(max(" + c + "_max), 0)"
			}
			if i > 0 {
				sumAvg += "+"
				sumPeak += "+"
			}
			sumAvg += "coalesce(avg(" + c + "), 0)"
			sumPeak += p
		}
		where := "t>=? AND t<=?"
		args := []any{from.Unix(), to.Unix()}
		if node > 0 {
			where += " AND node=?"
			args = append(args, node)
		}
		q := fmt.Sprintf(`SELECT ent, %s a, %s m FROM %s WHERE %s GROUP BY ent HAVING a > 0 ORDER BY a DESC LIMIT %d`, sumAvg, sumPeak, tableFor("vm", tier), where, top)
		rows, err := d.db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		list := []Busy{}
		var ents []int64
		for rows.Next() {
			var id int64
			var b Busy
			if err := rows.Scan(&id, &b.Avg, &b.Max); err != nil {
				rows.Close()
				return nil, err
			}
			ents = append(ents, id)
			list = append(list, b)
		}
		rows.Close()
		for i, id := range ents {
			var name, label string
			d.db.QueryRow(`SELECT name, label FROM names WHERE id=?`, id).Scan(&name, &label)
			if len(name) > 3 {
				list[i].UUID = name[3:]
			}
			list[i].Name = label
			d.db.QueryRow(fmt.Sprintf(`SELECT node FROM %s WHERE ent=? AND t>=? AND t<=? AND node IS NOT NULL ORDER BY t DESC LIMIT 1`, tableFor("vm", tier)), id, from.Unix(), to.Unix()).Scan(&list[i].Node)
		}
		*out[r.name] = list
	}
	return o, nil
}
