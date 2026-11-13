package api

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"virticle/internal/buildinfo"
	"virticle/internal/cluster"
	"virticle/internal/metrics"
	"virticle/internal/stats"
)

// History and Prometheus. The collector (stats.go) feeds both; nothing here
// calls libvirt, so a scrape or a chart costs the nodes nothing.

// parseTime reads RFC 3339 or unix seconds (milliseconds are recognised by
// size).
func parseTime(s string) (time.Time, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		if n > 1e11 {
			n /= 1000
		}
		return time.Unix(int64(n), 0), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, badRequest(fmt.Sprintf("%q is not a time: use RFC 3339 or unix seconds", s))
	}
	return t, nil
}

func rangeOf(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	to = time.Now()
	if v := q.Get("to"); v != "" {
		if to, err = parseTime(v); err != nil {
			return
		}
	}
	from = to.Add(-time.Hour)
	if v := q.Get("from"); v != "" {
		if from, err = parseTime(v); err != nil {
			return
		}
	}
	return
}

func (s *Server) metricsQuery(w http.ResponseWriter, r *http.Request) {
	if hub.db == nil {
		fail(w, badRequest("the performance history is not available on this controller"))
		return
	}
	q := r.URL.Query()
	from, to, err := rangeOf(r)
	if err != nil {
		fail(w, err)
		return
	}
	req := metrics.Request{Kind: q.Get("kind"), ID: q.Get("id"), From: from, To: to, Device: q.Get("device")}
	switch req.Kind {
	case "vm":
		u, err := cluster.ParseUUID(req.ID)
		if err != nil {
			fail(w, badRequest("id must be the VM's uuid"))
			return
		}
		req.ID = cluster.FormatUUID(u)
	case "node":
		if _, err := strconv.ParseInt(req.ID, 10, 64); err != nil {
			fail(w, badRequest("id must be the node's number"))
			return
		}
	default:
		fail(w, badRequest("kind must be vm or node"))
		return
	}
	if m := q.Get("metrics"); m != "" {
		req.Groups = strings.Split(m, ",")
	}
	res, err := hub.db.Query(req)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if q.Get("format") == "csv" {
		name := fmt.Sprintf("virticle-%s-%s-%s.csv", req.Kind, strings.NewReplacer(":", "-").Replace(req.ID), from.UTC().Format("20060102T1504Z"))
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		res.WriteCSV(w)
		return
	}
	writeJSON(w, res)
}

func (s *Server) metricsOverview(w http.ResponseWriter, r *http.Request) {
	if hub.db == nil {
		fail(w, badRequest("the performance history is not available on this controller"))
		return
	}
	from, to, err := rangeOf(r)
	if err != nil {
		fail(w, err)
		return
	}
	var node int64
	if v := r.URL.Query().Get("node"); v != "" {
		if node, err = strconv.ParseInt(v, 10, 64); err != nil {
			fail(w, badRequest("node must be the node's number"))
			return
		}
	}
	top := 5
	if v := r.URL.Query().Get("top"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			top = n
		}
	}
	o, err := hub.db.Rank(from, to, node, top)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, o)
}

// promMetrics serves the current values from the collector's memory.
func (s *Server) promMetrics(w http.ResponseWriter, r *http.Request) {
	var nodes []metrics.PromNode
	for _, n := range s.Nodes.Status() {
		pn := metrics.PromNode{Name: nodeLabel(n), Up: n.State == "online"}
		hub.mu.Lock()
		ns := hub.nodes[n.ID]
		hub.mu.Unlock()
		if ns != nil {
			ns.mu.Lock()
			if pn.Up && time.Since(ns.lastTick) <= 3*stats.Interval {
				pn.Tracker = ns.trk.Snapshot()
			}
			ns.mu.Unlock()
		}
		nodes = append(nodes, pn)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metrics.WriteProm(w, nodes, buildinfo.Version, runtime.Version())
}
