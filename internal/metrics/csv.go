package metrics

import (
	"encoding/csv"
	"io"
	"math"
	"sort"
	"strconv"
	"time"
)

// WriteCSV writes a result as a table: one row per time step, a column per
// series, and (when the answer has peaks) a column "<series> peak" next to it.
func (r *Result) WriteCSV(w io.Writer) error {
	names := make([]string, 0, len(r.Series))
	for n := range r.Series {
		names = append(names, n)
	}
	sort.Strings(names)
	head := []string{"time"}
	for _, n := range names {
		head = append(head, n)
		if _, ok := r.Max[n]; ok {
			head = append(head, n+" peak")
		}
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(head); err != nil {
		return err
	}
	cell := func(v float64) string {
		if isNaN(v) || math.IsInf(v, 0) {
			return ""
		}
		return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
	}
	for i, t := range r.T {
		row := []string{time.Unix(t, 0).UTC().Format(time.RFC3339)}
		for _, n := range names {
			row = append(row, cell(r.Series[n][i]))
			if m, ok := r.Max[n]; ok {
				row = append(row, cell(m[i]))
			}
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
