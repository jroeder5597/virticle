package api

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"virticle/internal/metrics"
)

func withHistory(t *testing.T) *metrics.DB {
	t.Helper()
	db, err := metrics.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := hub.db
	hub.db = db
	t.Cleanup(func() { hub.db = old; db.Close() })
	return db
}

func TestMetricsAPI(t *testing.T) {
	db := withHistory(t)
	e := newEnv(t)
	e.user("ann", admin)
	w := e.do("POST", "/api/account/tokens", map[string]any{"name": "prom", "role": "viewer"}, cookieOf(e, "ann"))
	var created struct{ Secret string }
	decode(t, w, &created)

	now := time.Now().Unix() / 15 * 15
	uuid := "11111111-2222-3333-4444-555555555555"
	vals := make([]float64, 15)
	for i := range vals {
		vals[i] = metrics.NaN
	}
	vals[0], vals[1] = 1, 25 // node, cpu
	var bs []metrics.Batch
	for i := 0; i < 40; i++ {
		bs = append(bs, metrics.Batch{T: now - int64(i)*15, Rows: []metrics.Row{{Ent: "vm:" + uuid, Label: "web", Vals: vals}}})
	}
	if err := db.InsertMany(bs); err != nil {
		t.Fatal(err)
	}

	q := "/api/metrics/query?kind=vm&id=" + strings.ToUpper(uuid) + "&metrics=cpu&from=" + strconv.FormatInt(now-600, 10) + "&to=" + strconv.FormatInt(now, 10)
	var res struct {
		Step   int64
		T      []int64
		Series map[string][]*float64
	}
	w = e.do("GET", q, nil, bearer(created.Secret))
	if w.Code != 200 {
		t.Fatalf("query: %d %s", w.Code, w.Body)
	}
	decode(t, w, &res)
	if res.Step != 15 || len(res.T) != 41 || len(res.Series["cpu"]) != 41 || res.Series["cpu"][10] == nil || *res.Series["cpu"][10] != 25 {
		t.Errorf("shape: step %d, %d times, %v", res.Step, len(res.T), res.Series)
	}
	if _, ok := res.Series["mem_used_kib"]; ok {
		t.Error("an unrequested group came back")
	}

	w = e.do("GET", q+"&format=csv", nil, bearer(created.Secret))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") || !strings.HasPrefix(w.Body.String(), "time,cpu,") {
		t.Errorf("csv: %d %q %.40q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Error("csv is not a download")
	}

	for _, bad := range []string{
		"/api/metrics/query?kind=vm&id=nope",
		"/api/metrics/query?kind=disk&id=1",
		"/api/metrics/query?kind=node&id=x",
		"/api/metrics/query?kind=node&id=1&from=yesterday",
		"/api/metrics/query?kind=node&id=1&metrics=bogus",
		"/api/metrics/query?kind=node&id=1&from=1&to=" + strconv.FormatInt(now, 10),
	} {
		if w := e.do("GET", bad, nil, bearer(created.Secret)); w.Code != 400 {
			t.Errorf("%s: %d, want 400", bad, w.Code)
		}
	}

	w = e.do("GET", "/api/metrics/overview?from="+strconv.FormatInt(now-600, 10)+"&to="+strconv.FormatInt(now, 10), nil, bearer(created.Secret))
	var ov struct {
		Cpu []struct {
			UUID, Name string
			Avg        float64
		}
	}
	decode(t, w, &ov)
	if w.Code != 200 || len(ov.Cpu) != 1 || ov.Cpu[0].Name != "web" || ov.Cpu[0].Avg != 25 {
		t.Errorf("overview: %d %s", w.Code, w.Body)
	}

	// Prometheus: nothing without a token; with one, the text format.
	if w := e.do("GET", "/metrics", nil); w.Code != 401 {
		t.Errorf("/metrics without a token: %d", w.Code)
	}
	if w := e.do("GET", "/metrics", nil, bearer("virticle_nonsense")); w.Code != 401 {
		t.Errorf("/metrics with a wrong token: %d", w.Code)
	}
	w = e.do("GET", "/metrics", nil, bearer(created.Secret))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") || !strings.Contains(w.Body.String(), "virticle_build_info") {
		t.Errorf("/metrics: %d %q %.80q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
}
