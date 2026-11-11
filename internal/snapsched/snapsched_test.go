package snapsched

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDueInterval(t *testing.T) {
	s := Schedule{Enabled: true, Mode: "interval", EveryMinutes: 60}
	now := ts("2026-01-01 12:00")
	for _, c := range []struct {
		last time.Time
		want bool
	}{
		{time.Time{}, true},
		{ts("2026-01-01 11:01"), false},
		{ts("2026-01-01 11:00"), true},
		{ts("2025-12-31 00:00"), true},
	} {
		if got := s.Due(now, c.last); got != c.want {
			t.Errorf("last %v: got %v want %v", c.last, got, c.want)
		}
	}
	s.Enabled = false
	if s.Due(now, time.Time{}) {
		t.Error("disabled schedule must never be due")
	}
}

func TestDueDaily(t *testing.T) {
	s := Schedule{Enabled: true, Mode: "daily", At: "03:30"}
	for _, c := range []struct {
		now, last string
		want      bool
	}{
		{"2026-01-02 03:29", "2026-01-01 03:31", false}, // today's slot not reached
		{"2026-01-02 03:30", "2026-01-01 03:31", true},
		{"2026-01-02 09:00", "2026-01-02 03:31", false}, // already done today
		{"2026-01-02 09:00", "2026-01-02 03:29", true},  // missed slot caught up
		{"2026-01-02 02:00", "2026-01-01 03:30", false}, // yesterday's slot satisfied
		{"2026-01-02 02:00", "2025-12-30 03:30", true},
	} {
		if got := s.Due(ts(c.now), ts(c.last)); got != c.want {
			t.Errorf("now %s last %s: got %v want %v", c.now, c.last, got, c.want)
		}
	}
}

func TestPrune(t *testing.T) {
	mk := func(name, desc, at string) Snap { return Snap{name, desc, ts(at)} }
	snaps := []Snap{
		mk("auto-20260101-000000", Marker, "2026-01-01 00:00"),
		mk("auto-20260103-000000", Marker, "2026-01-03 00:00"),
		mk("auto-20260102-000000", Marker, "2026-01-02 00:00"),
		mk("auto-20260104-000000", Marker, "2026-01-04 00:00"),
		mk("auto-20250101-000000", "made by hand", "2025-01-01 00:00"), // user-made lookalike
		mk("manual", "", "2024-01-01 00:00"),
		mk("other-20240101-000000", Marker, "2024-01-01 00:00"), // other prefix
		mk("auto-x", Marker, "2024-01-01 00:00"),                // not a timestamp name
	}
	got := Prune(snaps, "auto", 2)
	want := []string{"auto-20260101-000000", "auto-20260102-000000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := Prune(snaps, "auto", 4); got != nil {
		t.Errorf("nothing to prune, got %v", got)
	}
	if n := Newest(snaps, "auto"); !n.Equal(ts("2026-01-04 00:00")) {
		t.Errorf("newest %v", n)
	}
	// A prefix that is a prefix of another must not match it.
	if Ours(Snap{"auto-b-20260101-000000", Marker, time.Time{}}, "auto") {
		t.Error("auto must not claim auto-b-...")
	}
}

func TestName(t *testing.T) {
	if got := Name("auto", ts("2026-03-04 05:06")); got != "auto-20260304-050600" {
		t.Error(got)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	s := Schedule{Enabled: true, Mode: "interval", EveryMinutes: 15, Keep: 3, Prefix: "auto", Quiesce: true}
	m, err := Update("", &s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(m)
	if err != nil || got == nil || !reflect.DeepEqual(*got, s) {
		t.Fatalf("round trip: %+v %v\n%s", got, err, m)
	}
	s2 := Schedule{Enabled: false, Mode: "daily", At: "04:00", Keep: 7, Prefix: "nightly", Memory: true}
	m2, err := Update(m, &s2)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Parse(m2); got == nil || !reflect.DeepEqual(*got, s2) {
		t.Fatalf("replace: %+v\n%s", got, m2)
	}
	m3, err := Update(m2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Parse(m3); got != nil {
		t.Fatalf("remove left %+v", got)
	}
}

func TestOtherChildrenPreserved(t *testing.T) {
	// Shape of what libvirt returns: namespaced root, plain children.
	other := `<vp:meta xmlns:vp="` + NS + `"><migration state="x"> <a/> </migration>  <tags>a,b</tags></vp:meta>`
	s := Schedule{Enabled: true, Mode: "interval", EveryMinutes: 5, Keep: 2, Prefix: "p"}
	m, err := Update(other, &s)
	if err != nil {
		t.Fatal(err)
	}
	pre, _ := Update(`<vp:meta xmlns:vp="`+NS+`"><vp:tags>a</vp:tags></vp:meta>`, nil)
	if pre != `<meta><tags>a</tags></meta>` {
		t.Errorf("prefix not stripped: %s", pre)
	}
	if strings.Contains(m, "xmlns") {
		t.Errorf("libvirt rejects a namespace declaration, got:\n%s", m)
	}
	if !strings.Contains(m, `<migration state="x"> <a/> </migration>  <tags>a,b</tags>`) {
		t.Errorf("lost foreign content:\n%s", m)
	}
	if got, _ := Parse(m); got == nil || got.Prefix != "p" {
		t.Fatalf("parse: %+v", got)
	}
	m2, _ := Update(m, nil)
	if m2 != `<meta><migration state="x"> <a/> </migration>  <tags>a,b</tags></meta>` {
		t.Errorf("removal changed other content:\n%s", m2)
	}
	if Empty(m2) {
		t.Error("root with other content is not empty")
	}
	m3, _ := Update(`<vp:meta xmlns:vp="`+NS+`"><snapshot-schedule enabled="no" mode="daily" at="01:00" keep="1" prefix="x" memory="no" quiesce="no"/></vp:meta>`, nil)
	if !Empty(m3) {
		t.Errorf("only the schedule was there, got %s", m3)
	}
}

func TestValidate(t *testing.T) {
	ok := Schedule{Mode: "interval", EveryMinutes: 1, Keep: 1, Prefix: "a"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Schedule{
		{Mode: "interval", EveryMinutes: 0, Keep: 1, Prefix: "a"},
		{Mode: "interval", EveryMinutes: 1, Keep: 0, Prefix: "a"},
		{Mode: "interval", EveryMinutes: 1, Keep: 1, Prefix: "a/b"},
		{Mode: "interval", EveryMinutes: 1, Keep: 1, Prefix: ""},
		{Mode: "daily", At: "24:00", Keep: 1, Prefix: "a"},
		{Mode: "weekly", Keep: 1, Prefix: "a"},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be invalid", bad)
		}
	}
}

func TestNextRun(t *testing.T) {
	at := func(v string) time.Time {
		x, err := time.Parse("2006-01-02 15:04:05", v)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	hourly := Schedule{Enabled: true, Mode: "interval", EveryMinutes: 60, Keep: 3, Prefix: "p"}
	daily := Schedule{Enabled: true, Mode: "daily", At: "03:00", Keep: 3, Prefix: "p"}
	midnight := Schedule{Enabled: true, Mode: "daily", At: "00:00", Keep: 3, Prefix: "p"}
	cases := []struct {
		name      string
		s         Schedule
		now, last string
		want      string // "" = ok false
	}{
		{"hourly, last 20 min ago", hourly, "2026-03-01 10:20:00", "2026-03-01 10:00:00", "2026-03-01 11:00:00"},
		{"hourly, overdue is due now", hourly, "2026-03-01 12:30:00", "2026-03-01 10:00:00", "2026-03-01 12:30:00"},
		{"hourly, never taken is due now", hourly, "2026-03-01 12:30:00", "", "2026-03-01 12:30:00"},
		{"daily, taken at today's slot", daily, "2026-03-01 03:00:40", "2026-03-01 03:00:10", "2026-03-02 03:00:00"},
		{"daily, before today's slot, yesterday's taken", daily, "2026-03-01 02:00:00", "2026-02-28 03:00:10", "2026-03-01 03:00:00"},
		{"daily, slot passed and not taken", daily, "2026-03-01 09:00:00", "2026-02-28 03:00:10", "2026-03-01 09:00:00"},
		{"daily around midnight, just before", midnight, "2026-03-01 23:59:30", "2026-03-01 00:00:05", "2026-03-02 00:00:00"},
		{"daily around midnight, just after", midnight, "2026-03-02 00:00:10", "2026-03-02 00:00:05", "2026-03-03 00:00:00"},
		{"daily over the end of a month", daily, "2026-02-28 12:00:00", "2026-02-28 03:00:10", "2026-03-01 03:00:00"},
		{"disabled", Schedule{Mode: "daily", At: "03:00", Keep: 3, Prefix: "p"}, "2026-03-01 12:00:00", "", ""},
		{"incomplete", Schedule{Enabled: true, Mode: "daily", Keep: 3, Prefix: "p"}, "2026-03-01 12:00:00", "", ""},
	}
	for _, c := range cases {
		var last time.Time
		if c.last != "" {
			last = at(c.last)
		}
		got, ok := c.s.NextRun(at(c.now), last)
		if c.want == "" {
			if ok {
				t.Errorf("%s: want no next run, got %v", c.name, got)
			}
			continue
		}
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("%s: got %v %v, want %s", c.name, got, ok, c.want)
		}
		// The scheduler and the answer agree: nothing is due before the answer, and at the answer it is due.
		if got.After(at(c.now)) {
			if c.s.Due(got.Add(-time.Second), last) || !c.s.Due(got, last) {
				t.Errorf("%s: Due disagrees with NextRun at %v", c.name, got)
			}
		}
	}
}
