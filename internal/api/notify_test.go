package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"virticle/internal/store"
	"virticle/internal/webaccess"
)

// fakeClock runs the waits of the retry logic without waiting and records them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err() == nil
}
func (c *fakeClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// hookSink is a stand-in for a service's webhook endpoint: it records the bodies and answers
// with whatever answer(n) says for the n-th request.
type hookSink struct {
	mu     sync.Mutex
	bodies []string
	heads  []http.Header
	answer func(n int, w http.ResponseWriter)
	srv    *httptest.Server
}

func newRecorder(t *testing.T, tlsOn bool, answer func(n int, w http.ResponseWriter)) *hookSink {
	rc := &hookSink{answer: answer}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, string(b))
		rc.heads = append(rc.heads, r.Header.Clone())
		n := len(rc.bodies)
		rc.mu.Unlock()
		if rc.answer == nil {
			w.WriteHeader(200)
			return
		}
		rc.answer(n, w)
	})
	if tlsOn {
		rc.srv = httptest.NewTLSServer(h)
	} else {
		rc.srv = httptest.NewServer(h)
	}
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *hookSink) count() int { rc.mu.Lock(); defer rc.mu.Unlock(); return len(rc.bodies) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testNotifier(t *testing.T, dests ...notifyDest) (*notifier, *fakeClock) {
	t.Helper()
	n := newNotifier(func() []notifyDest { return dests })
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	n.clock = clk
	n.testClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	n.sync()
	t.Cleanup(func() {
		n.mu.Lock()
		for _, w := range n.workers {
			w.cancel()
		}
		n.mu.Unlock()
	})
	return n, clk
}

func oneAlert(sev string) []Alert {
	return []Alert{{ID: "pool:1:data", Kind: "pool-usage", Severity: sev, Title: "Pool data is 97% full", Message: "m", Node: 1, Link: "/storage"}}
}

func manyAlerts(n int) []Alert {
	var out []Alert
	for i := 0; i < n; i++ {
		out = append(out, Alert{ID: "a" + string(rune('a'+i)), Severity: "warning", Title: "t" + string(rune('a'+i))})
	}
	return out
}

func TestDeliveryRetriesWithBackoff(t *testing.T) {
	rc := newRecorder(t, false, func(n int, w http.ResponseWriter) {
		if n < 4 {
			w.WriteHeader(503)
		}
	})
	n, clk := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	n.dispatch(oneAlert("critical"), nil, "", nil)
	waitFor(t, "the fourth try", func() bool { return rc.count() == 4 })
	waitFor(t, "ok state", func() bool { return !n.worker("1").status().LastOK.IsZero() })
	want := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}
	if got := clk.waits(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("waits %v, want %v", got, want)
	}
	if st := n.worker("1").status(); st.Failing {
		t.Errorf("still failing after success: %+v", st)
	}
}

func TestDeliveryGivesUpAndShowsTheReason(t *testing.T) {
	rc := newRecorder(t, false, func(n int, w http.ResponseWriter) { w.WriteHeader(502) })
	n, _ := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	n.dispatch(oneAlert("critical"), nil, "", nil)
	waitFor(t, "four tries", func() bool { return rc.count() == 4 })
	waitFor(t, "failing state", func() bool {
		st := n.worker("1").status()
		return st.Failing && strings.Contains(st.Error, "502") && !st.At.IsZero()
	})
	time.Sleep(30 * time.Millisecond)
	if rc.count() != 4 {
		t.Errorf("tried %d times, want 4 (one try and three retries)", rc.count())
	}
}

func TestPermanentAnswersAreNotRetried(t *testing.T) {
	rc := newRecorder(t, false, func(n int, w http.ResponseWriter) { http.Error(w, "no_service", 404) })
	n, clk := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	n.dispatch(oneAlert("critical"), nil, "", nil)
	waitFor(t, "failing", func() bool { return n.worker("1").status().Failing })
	if rc.count() != 1 || len(clk.waits()) != 0 {
		t.Errorf("tries %d waits %v", rc.count(), clk.waits())
	}
	if e := n.worker("1").status().Error; !strings.Contains(e, "404") || !strings.Contains(e, "no_service") {
		t.Errorf("reason %q", e)
	}
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	rc := newRecorder(t, false, func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		}
	})
	n, clk := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	n.dispatch(oneAlert("critical"), nil, "", nil)
	waitFor(t, "second try", func() bool { return rc.count() == 2 })
	if got := clk.waits(); len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("waits %v, want [7s] and not the 5 s backoff", got)
	}
	// Discord sends fractions; an hour is capped.
	if r := describeStatus(429, "1.5", ""); r.RetryAfter != 1500*time.Millisecond || r.OK || r.Permanent {
		t.Errorf("%+v", r)
	}
}

func TestBurstsBecomeOneSummary(t *testing.T) {
	slack := newRecorder(t, false, nil)
	hook := newRecorder(t, false, nil)
	n, _ := testNotifier(t,
		notifyDest{ID: "1", Kind: "slack", Name: "S", URL: slack.srv.URL, Severity: "warning", Cleared: true},
		notifyDest{ID: "2", Kind: "webhook", Name: "H", URL: hook.srv.URL, Severity: "warning", Cleared: true})
	n.dispatch(manyAlerts(6), nil, "", nil)
	waitFor(t, "messages", func() bool { return slack.count() == 1 && hook.count() == 6 })
	if !strings.Contains(slack.bodies[0], "6 new alerts") {
		t.Errorf("slack: %s", slack.bodies[0])
	}
	// Exactly five are sent one by one.
	n.dispatch(manyAlerts(5), nil, "", nil)
	waitFor(t, "five more", func() bool { return slack.count() == 6 })
	// The raw webhook is read by programs: always one alert per message, in the old shape.
	var m webhookMsg
	if err := json.Unmarshal([]byte(hook.bodies[0]), &m); err != nil || m.Event != "raise" || m.Alert.ID == "" {
		t.Errorf("webhook body %s", hook.bodies[0])
	}
}

func TestSeverityAndClearedChoices(t *testing.T) {
	crit := newRecorder(t, false, nil)
	noClear := newRecorder(t, false, nil)
	n, _ := testNotifier(t,
		notifyDest{ID: "1", Kind: "webhook", Name: "C", URL: crit.srv.URL, Severity: "critical", Cleared: true},
		notifyDest{ID: "2", Kind: "webhook", Name: "N", URL: noClear.srv.URL, Severity: "warning", Cleared: false})
	n.dispatch(oneAlert("warning"), oneAlert("critical"), "", nil)
	waitFor(t, "messages", func() bool { return crit.count() == 1 && noClear.count() == 1 })
	time.Sleep(30 * time.Millisecond)
	if !strings.Contains(crit.bodies[0], `"event":"clear"`) {
		t.Errorf("critical-only destination got %s (warning must be left out, the cleared critical sent)", crit.bodies[0])
	}
	if !strings.Contains(noClear.bodies[0], `"event":"raise"`) {
		t.Errorf("no-clear destination got %s", noClear.bodies[0])
	}
}

func TestDispatchNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	rc := newRecorder(t, false, func(n int, w http.ResponseWriter) { <-release })
	defer close(release)
	n, _ := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	start := time.Now()
	for i := 0; i < 3*notifyQueue; i++ {
		n.dispatch(oneAlert("critical"), nil, "", nil)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("dispatch blocked for %v", time.Since(start))
	}
	waitFor(t, "overflow reported", func() bool { return strings.Contains(n.worker("1").status().Error, "dropped") })
}

func TestSignatureHeaderOnlyWithSecret(t *testing.T) {
	signed := newRecorder(t, false, nil)
	plain := newRecorder(t, false, nil)
	n, _ := testNotifier(t,
		notifyDest{ID: "1", Kind: "webhook", Name: "S", URL: signed.srv.URL, Severity: "warning", Cleared: true, Secret: "s3cret"},
		notifyDest{ID: "2", Kind: "webhook", Name: "P", URL: plain.srv.URL, Severity: "warning", Cleared: true})
	n.dispatch(oneAlert("critical"), nil, "", nil)
	waitFor(t, "both", func() bool { return signed.count() == 1 && plain.count() == 1 })
	if got, want := signed.heads[0].Get("X-Virticle-Signature"), signBody("s3cret", []byte(signed.bodies[0])); got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	if plain.heads[0].Get("X-Virticle-Signature") != "" {
		t.Error("signature without a secret")
	}
	if signed.heads[0].Get("Content-Type") != "application/json" {
		t.Error("content type")
	}
}

func TestTestMessageReportsInPlainWords(t *testing.T) {
	code := 200
	rc := newRecorder(t, false, func(n int, w http.ResponseWriter) { w.WriteHeader(code) })
	n, clk := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	w := n.worker("1")
	if r := w.sendTest(context.Background()); !r.OK || !strings.Contains(r.Message, "accepted") {
		t.Errorf("%+v", r)
	}
	if !strings.Contains(rc.bodies[0], `"event":"test"`) {
		t.Errorf("test body %s", rc.bodies[0])
	}
	code = 404
	r := w.sendTest(context.Background())
	if r.OK || r.Status != 404 || !strings.Contains(r.Message, "does not know this address") {
		t.Errorf("%+v", r)
	}
	if !w.status().Failing {
		t.Error("a failed test shows on the page")
	}
	if len(clk.waits()) != 0 {
		t.Error("a test is sent once, without retries")
	}
	// A connection error does not repeat the (secret) address.
	rc.srv.Close()
	r = w.sendTest(context.Background())
	if r.OK || strings.Contains(r.Message, rc.srv.URL) || strings.Contains(r.Message, "http") {
		t.Errorf("error leaks the address: %q", r.Message)
	}
}

func TestCheckDestURL(t *testing.T) {
	old := lookupIPs
	defer func() { lookupIPs = old }()
	lookupIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "hooks.slack.com":
			return []netip.Addr{netip.MustParseAddr("52.1.2.3")}, nil
		case "chat.lan":
			return []netip.Addr{netip.MustParseAddr("192.168.1.20")}, nil
		case "sneaky.example":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "meta.example":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, &net.DNSError{Name: host, Err: "no such host"}
	}
	cases := []struct {
		kind, url string
		ok        bool
	}{
		{"slack", "https://hooks.slack.com/services/T0/B0/XXXXXXXX", true},
		{"slack", "http://hooks.slack.com/services/T0/B0/XXXXXXXX", false}, // not https
		{"discord", "http://hooks.slack.com/x", false},
		{"mattermost", "https://chat.lan/hooks/abcdef", true}, // private (LAN) address
		{"mattermost", "https://192.168.1.20:8065/hooks/abcdef", true},
		{"slack", "https://127.0.0.1/x/yyyy", false}, // loopback
		{"slack", "https://[::1]/x/yyyy", false},
		{"discord", "https://localhost/x/yyyy", false},
		{"slack", "https://sneaky.example/x/yyyy", false}, // a name that resolves to loopback
		{"slack", "https://169.254.169.254/x/yyyy", false},
		{"slack", "https://meta.example/x/yyyy", false},
		{"slack", "https://[fe80::1]/x/yyyy", false},
		{"slack", "https://0.0.0.0/x/yyyy", false},
		{"slack", "https://nowhere.invalid/x/yyyy", false}, // cannot be found
		{"slack", "https://user:pw@hooks.slack.com/x/yyyy", false},
		{"slack", "ftp://hooks.slack.com/x/yyyy", false},
		{"slack", "hooks.slack.com/x/yyyy", false},
		{"webhook", "http://127.0.0.1:9000/hook", true}, // raw webhooks may be local and plain
		{"webhook", "https://chat.lan/hook", true},
		{"webhook", "http://10.0.0.5/hook", true},
		{"webhook", "http://169.254.169.254/latest/meta-data/", false}, // the metadata address
		{"webhook", "http://meta.example/hook", false},
		{"webhook", "http://[fe80::1]/hook", false},
		{"webhook", "ftp://chat.lan/hook", false},
	}
	for _, c := range cases {
		host, tail, err := checkDestURL(c.kind, c.url)
		if (err == nil) != c.ok {
			t.Errorf("%s %s: err = %v, want ok=%v", c.kind, c.url, err, c.ok)
		}
		if err == nil && (host == "" || tail != c.url[len(c.url)-4:]) {
			t.Errorf("%s: host %q tail %q", c.url, host, tail)
		}
	}
	// The connection itself refuses what the saved address no longer allows (a name that moved).
	if !forbiddenAddr("slack", netip.MustParseAddr("::ffff:127.0.0.1")) || forbiddenAddr("slack", netip.MustParseAddr("10.1.1.1")) ||
		!forbiddenAddr("webhook", netip.MustParseAddr("169.254.1.1")) || forbiddenAddr("webhook", netip.MustParseAddr("127.0.0.1")) {
		t.Error("forbiddenAddr")
	}
}

// The real client refuses to connect to a forbidden address even when it was reached by name.
func TestClientRefusesLoopbackForChatTypes(t *testing.T) {
	rc := newRecorder(t, true, nil)
	n := newNotifier(func() []notifyDest { return nil })
	req, _ := http.NewRequest("POST", rc.srv.URL, strings.NewReader("{}"))
	_, err := n.client("slack").Do(req)
	if err == nil || !strings.Contains(describeError(err), "may not send") {
		t.Fatalf("slack client reached loopback: %v", err)
	}
	if strings.Contains(describeError(err), "127.0.0.1") {
		t.Error("the message names the address")
	}
	if rc.count() != 0 {
		t.Error("request arrived")
	}
	plain := newRecorder(t, false, nil)
	req, _ = http.NewRequest("POST", plain.srv.URL, strings.NewReader("{}"))
	resp, err := n.client("webhook").Do(req)
	if err != nil {
		t.Fatalf("webhook client: %v", err)
	}
	resp.Body.Close()
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := newRecorder(t, false, nil)
	redir := newRecorder(t, false, func(n int, w http.ResponseWriter) {
		w.Header().Set("Location", target.srv.URL)
		w.WriteHeader(302)
	})
	n := newNotifier(func() []notifyDest { return nil })
	req, _ := http.NewRequest("POST", redir.srv.URL, strings.NewReader("{}"))
	resp, err := n.client("webhook").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if target.count() != 0 {
		t.Error("redirect followed")
	}
}

// --- the stored destinations ---

func newStoreServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{Store: st, DataDir: dir}
}

func TestOldWebhookBecomesADestination(t *testing.T) {
	s := newStoreServer(t)
	const old = "http://hook.example:8080/old-path-token"
	if err := s.Store.SetWebSetting(legacyWebhookKey, old); err != nil {
		t.Fatal(err)
	}
	n := s.notifier()
	rows, _ := s.Store.NotifyDests()
	if len(rows) != 1 || rows[0].Kind != "webhook" || rows[0].Name != "Webhook" || rows[0].Severity != "warning" || !rows[0].Cleared {
		t.Fatalf("destinations after the upgrade: %+v", rows)
	}
	if strings.Contains(rows[0].URLEnc, "hook.example") || strings.Contains(rows[0].URLEnc, "token") {
		t.Errorf("address stored in the clear: %q", rows[0].URLEnc)
	}
	if rows[0].Host != "http://hook.example:8080" || rows[0].Tail != "oken" {
		t.Errorf("host %q tail %q", rows[0].Host, rows[0].Tail)
	}
	if _, ok, _ := s.Store.WebSetting(legacyWebhookKey); ok {
		t.Error("the old setting is still there")
	}
	if ds := s.loadDests(); len(ds) != 1 || ds[0].URL != old {
		t.Errorf("loaded %+v", ds)
	}
	_ = n
	// A second start does nothing.
	s2 := &Server{Store: s.Store, DataDir: s.DataDir}
	s2.notifier()
	if rows, _ := s.Store.NotifyDests(); len(rows) != 1 {
		t.Errorf("migrated twice: %d", len(rows))
	}
	// An empty old setting leaves no destination.
	s3 := newStoreServer(t)
	s3.Store.SetWebSetting(legacyWebhookKey, "")
	s3.notifier()
	if rows, _ := s3.Store.NotifyDests(); len(rows) != 0 {
		t.Errorf("empty setting made %d", len(rows))
	}
}

func TestFlagWebhookIsOneMoreReadOnlyDestination(t *testing.T) {
	e := newEnv(t)
	e.user("root", "admin")
	e.srv.Alerts.Webhook = "http://flag.example/hooktoken"
	w := e.do("GET", "/api/notify/destinations", nil, cookieOf(e, "root"))
	var out struct {
		Destinations []destJSON `json:"destinations"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Destinations) != 1 || out.Destinations[0].ID != "flag" || !out.Destinations[0].ReadOnly || out.Destinations[0].Type != "webhook" {
		t.Fatalf("%s", w.Body)
	}
	if strings.Contains(w.Body.String(), "hooktoken") {
		t.Error("the flag address is returned")
	}
	if w := e.do("PUT", "/api/notify/destinations/flag", map[string]any{"name": "x"}, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("edit flag destination: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/notify/destinations/flag", nil, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("delete flag destination: %d", w.Code)
	}
}

func TestDestinationAPI(t *testing.T) {
	old := lookupIPs
	defer func() { lookupIPs = old }()
	lookupIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("52.1.2.3")}, nil
	}
	e := newEnv(t)
	e.srv.DataDir = t.TempDir()
	e.user("root", "admin")
	e.user("eve", "operator")
	const secretURL = "https://hooks.slack.com/services/T000/B000/SECRETSECRETXYZ9"
	body := map[string]any{"type": "slack", "name": "Ops", "url": secretURL}
	if w := e.do("POST", "/api/notify/destinations", body, cookieOf(e, "eve")); w.Code != 403 {
		t.Errorf("operator added a destination: %d", w.Code)
	}
	if w := e.do("GET", "/api/notify/destinations", nil, cookieOf(e, "eve")); w.Code != 403 {
		t.Errorf("operator listed destinations: %d", w.Code)
	}
	if w := e.do("POST", "/api/notify/destinations", map[string]any{"type": "slack", "name": "x", "url": "http://hooks.slack.com/x/yyyy"}, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("http slack accepted: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/notify/destinations", map[string]any{"type": "slack", "name": "x", "url": secretURL, "secret": "s"}, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("a secret on slack: %d", w.Code)
	}
	if w := e.do("POST", "/api/notify/destinations", map[string]any{"type": "teams", "url": secretURL}, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("unknown type: %d", w.Code)
	}
	if w := e.do("POST", "/api/notify/destinations", body, cookieOf(e, "root")); w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	hw := map[string]any{"type": "webhook", "url": "https://hook.lan/path/WEBHOOKTOKEN", "secret": "topsecretvalue", "severity": "critical", "cleared": false}
	if w := e.do("POST", "/api/notify/destinations", hw, cookieOf(e, "root")); w.Code != 200 {
		t.Fatalf("add webhook: %d %s", w.Code, w.Body)
	}

	list := func() []destJSON {
		w := e.do("GET", "/api/notify/destinations", nil, cookieOf(e, "root"))
		for _, secret := range []string{"SECRETSECRETXYZ9", "services/T000", "WEBHOOKTOKEN", "topsecretvalue"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("the list contains a secret (%s): %s", secret, w.Body)
			}
		}
		var out struct {
			Destinations []destJSON `json:"destinations"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Destinations
	}
	ds := list()
	if len(ds) != 2 {
		t.Fatalf("%+v", ds)
	}
	if d := ds[0]; d.Name != "Ops" || d.Host != "hooks.slack.com" || d.Tail != "XYZ9" || d.Severity != "warning" || !d.Cleared || d.HasSecret || d.State != "idle" {
		t.Errorf("slack destination: %+v", d)
	}
	if d := ds[1]; d.Name != "Webhook" || !d.HasSecret || d.Severity != "critical" || d.Cleared || d.Tail != "KEN"[:0]+"OKEN" {
		t.Errorf("webhook destination: %+v", d)
	}

	// What is in the database is encrypted.
	rows, _ := e.srv.Store.NotifyDests()
	for _, r := range rows {
		if strings.Contains(r.URLEnc, "hooks") || strings.Contains(r.URLEnc, "services") || strings.Contains(r.SecretEnc, "topsecret") {
			t.Errorf("stored in the clear: %+v", r)
		}
	}
	if ds2 := e.srv.loadDests(); len(ds2) != 2 || ds2[0].URL != secretURL || ds2[1].Secret != "topsecretvalue" {
		t.Errorf("decrypted: %+v", ds2)
	}

	// Change the name only: the address stays. Replace the address: the tail changes.
	id := ds[0].ID
	if w := e.do("PUT", "/api/notify/destinations/"+id, map[string]any{"name": "Night shift", "severity": "critical"}, cookieOf(e, "root")); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	if d := list()[0]; d.Name != "Night shift" || d.Severity != "critical" || d.Tail != "XYZ9" {
		t.Errorf("after edit: %+v", d)
	}
	if w := e.do("PUT", "/api/notify/destinations/"+id, map[string]any{"url": "https://hooks.slack.com/services/T000/B000/NEWNEWNEWabcd"}, cookieOf(e, "root")); w.Code != 200 {
		t.Fatalf("replace: %d %s", w.Code, w.Body)
	}
	if d := list()[0]; d.Tail != "abcd" {
		t.Errorf("after replace: %+v", d)
	}
	// Remove the signing secret of the webhook.
	if w := e.do("PUT", "/api/notify/destinations/"+ds[1].ID, map[string]any{"clearSecret": true}, cookieOf(e, "root")); w.Code != 200 {
		t.Fatalf("clear secret: %d", w.Code)
	}
	if list()[1].HasSecret {
		t.Error("secret still set")
	}
	if w := e.do("PUT", "/api/notify/destinations/"+id, map[string]any{"severity": "loud"}, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("bad severity: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/notify/destinations/"+id, nil, cookieOf(e, "root")); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/notify/destinations/"+id, nil, cookieOf(e, "root")); w.Code != 404 {
		t.Errorf("delete again: %d", w.Code)
	}
	if len(list()) != 1 {
		t.Error("not deleted")
	}
	if n := e.srv.notifier(); n.worker(id) != nil {
		t.Error("worker of a deleted destination is still running")
	}
}

// End to end through the HTTP API with stand-ins for the three services: destinations are added
// as an administrator would, an alert is raised and cleared, and what arrives is checked
// against what each service's webhook accepts.
func TestEndToEndAgainstServiceStandIns(t *testing.T) {
	old := lookupIPs
	defer func() { lookupIPs = old }()
	lookupIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.9.8.7")}, nil // "a LAN address"
	}
	slack := newRecorder(t, true, func(n int, w http.ResponseWriter) { io.WriteString(w, "ok") })
	mm := newRecorder(t, true, func(n int, w http.ResponseWriter) { io.WriteString(w, "ok") })
	dc := newRecorder(t, true, func(n int, w http.ResponseWriter) { w.WriteHeader(204) })
	hook := newRecorder(t, false, nil)

	e := newEnv(t)
	e.srv.DataDir = t.TempDir()
	e.user("root", "admin")
	e.srv.UseWebSettings(webaccess.Settings{PublicURL: "https://virt.example.org"})
	nf := e.srv.notifier()
	nf.clock = &fakeClock{now: time.Now()}
	// The stand-ins listen on loopback, which the real client refuses for these types: every
	// request goes to the stand-in that belongs to its host name instead.
	byHost := map[string]string{"slack.test": slack.srv.Listener.Addr().String(), "mm.test": mm.srv.Listener.Addr().String(), "dc.test": dc.srv.Listener.Addr().String()}
	nf.testClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, _, _ := net.SplitHostPort(addr)
			if to, ok := byHost[h]; ok {
				addr = to
			} else {
				_, p, _ := net.SplitHostPort(addr)
				addr = "127.0.0.1:" + p
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	add := func(m map[string]any) {
		t.Helper()
		if w := e.do("POST", "/api/notify/destinations", m, cookieOf(e, "root")); w.Code != 200 {
			t.Fatalf("add %v: %d %s", m["type"], w.Code, w.Body)
		}
	}
	add(map[string]any{"type": "slack", "name": "S", "url": "https://slack.test/services/T0/B0/abcdefgh"})
	add(map[string]any{"type": "mattermost", "name": "M", "url": "https://mm.test/hooks/abcdefgh"})
	add(map[string]any{"type": "discord", "name": "D", "url": "https://dc.test/api/webhooks/1/abcdefgh"})
	add(map[string]any{"type": "webhook", "name": "W", "url": hook.srv.URL + "/in", "secret": "k"})

	a := Alert{ID: "node-down:1", Kind: "node-down", Severity: "critical", Title: "kvm1 is offline", Message: "kvm1 has been unreachable since 10:00:00.", Node: 1, Link: "/nodes", Since: 1}
	e.srv.dispatchAlerts([]Alert{a}, nil)
	all := []*hookSink{slack, mm, dc, hook}
	waitFor(t, "the raise on all four", func() bool {
		for _, r := range all {
			if r.count() != 1 {
				return false
			}
		}
		return true
	})
	e.srv.dispatchAlerts(nil, []Alert{a})
	waitFor(t, "the clear on all four", func() bool {
		for _, r := range all {
			if r.count() != 2 {
				return false
			}
		}
		return true
	})

	var sl struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type     string `json:"type"`
			Elements []struct {
				URL string `json:"url"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	json.Unmarshal([]byte(slack.bodies[0]), &sl)
	if sl.Text != "Critical: kvm1 is offline" || len(sl.Blocks) != 4 || sl.Blocks[3].Elements[0].URL != "https://virt.example.org/nodes" {
		t.Errorf("slack raise: %s", slack.bodies[0])
	}
	if !strings.Contains(slack.bodies[1], `"text":"Cleared"`) {
		t.Errorf("slack clear: %s", slack.bodies[1])
	}
	var mmb struct {
		Attachments []struct {
			Title     string
			TitleLink string `json:"title_link"`
		} `json:"attachments"`
	}
	json.Unmarshal([]byte(mm.bodies[0]), &mmb)
	if len(mmb.Attachments) != 1 || mmb.Attachments[0].Title != "Critical: kvm1 is offline" || mmb.Attachments[0].TitleLink != "https://virt.example.org/nodes" {
		t.Errorf("mattermost raise: %s", mm.bodies[0])
	}
	if !strings.Contains(mm.bodies[1], "Cleared: kvm1 is offline") {
		t.Errorf("mattermost clear: %s", mm.bodies[1])
	}
	if !strings.Contains(dc.bodies[0], `"content":"Critical: kvm1 is offline"`) || !strings.Contains(dc.bodies[1], `"content":"Cleared: kvm1 is offline"`) {
		t.Errorf("discord: %s / %s", dc.bodies[0], dc.bodies[1])
	}
	var wm webhookMsg
	json.Unmarshal([]byte(hook.bodies[1]), &wm)
	if wm.Event != "clear" || wm.Alert.ID != "node-down:1" {
		t.Errorf("webhook clear: %s", hook.bodies[1])
	}
	if hook.heads[0].Get("X-Virticle-Signature") != signBody("k", []byte(hook.bodies[0])) {
		t.Error("signature")
	}

	// A test message through the API, and the status shown afterwards.
	var out struct {
		Destinations []destJSON `json:"destinations"`
	}
	json.Unmarshal(e.do("GET", "/api/notify/destinations", nil, cookieOf(e, "root")).Body.Bytes(), &out)
	w := e.do("POST", "/api/notify/destinations/"+out.Destinations[2].ID+"/test", nil, cookieOf(e, "root"))
	if !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(dc.bodies[2], "Test") {
		t.Errorf("test message: %s / %s", w.Body, dc.bodies[len(dc.bodies)-1])
	}
	json.Unmarshal(e.do("GET", "/api/notify/destinations", nil, cookieOf(e, "root")).Body.Bytes(), &out)
	if out.Destinations[0].State != "ok" || out.Destinations[0].LastOK == 0 {
		t.Errorf("status after delivery: %+v", out.Destinations[0])
	}
}
