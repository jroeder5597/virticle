package api

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"virt-proj/internal/store"
)

func mustProxies(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseTrustedProxies(t *testing.T) {
	p := mustProxies(t, "10.0.0.0/8, 192.0.2.9 ,::1,")
	if len(p) != 3 {
		t.Fatalf("got %v", p)
	}
	if !inRanges(p, netip.MustParseAddr("10.9.8.7")) || !inRanges(p, netip.MustParseAddr("::ffff:192.0.2.9")) || inRanges(p, netip.MustParseAddr("192.0.2.10")) {
		t.Errorf("ranges wrong: %v", p)
	}
	if _, err := ParseTrustedProxies("10.0.0.0/8,nonsense"); err == nil {
		t.Error("garbage accepted")
	}
	if p, err := ParseTrustedProxies(""); err != nil || len(p) != 0 {
		t.Error("empty must mean no proxies")
	}
}

func TestForwardedClient(t *testing.T) {
	tr := mustProxies(t, "10.0.0.0/8")
	peer := netip.MustParseAddr("10.0.0.1")
	cases := []struct {
		xff  []string
		want string
	}{
		{nil, "10.0.0.1"},
		{[]string{"203.0.113.5"}, "203.0.113.5"},
		// the client may prepend anything; the right-most untrusted hop wins
		{[]string{"1.2.3.4, 203.0.113.5"}, "203.0.113.5"},
		{[]string{"1.2.3.4", "203.0.113.5, 10.0.0.7"}, "203.0.113.5"},
		{[]string{"10.0.0.5, 10.0.0.7"}, "10.0.0.5"},
		{[]string{"1.2.3.4, garbage"}, "10.0.0.1"},
		{[]string{"1.2.3.4, garbage, 10.0.0.2"}, "10.0.0.2"},
		{[]string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		if got := forwardedClient(tr, c.xff, peer); got != c.want {
			t.Errorf("%q: got %s, want %s", c.xff, got, c.want)
		}
	}
}

func lastAudit(t *testing.T, e *env) store.AuditEntry {
	t.Helper()
	a, err := e.srv.Store.Audit(store.AuditFilter{Limit: 1})
	if err != nil || len(a) == 0 {
		t.Fatalf("no audit entry: %v", err)
	}
	return a[0]
}

// A client that is not a trusted proxy cannot influence its address, scheme
// or host with forwarding headers.
func TestForwardedHeadersIgnoredFromUntrustedPeer(t *testing.T) {
	for _, trusted := range []string{"", "10.0.0.0/8"} {
		e := newEnv(t)
		e.srv.TrustedProxies = mustProxies(t, trusted)
		e.user("alice", "admin")
		spoof := []opt{header("X-Forwarded-For", "198.51.100.99"), header("X-Forwarded-Proto", "https"), header("X-Forwarded-Host", "evil.example")}

		w := e.do("POST", "/api/login", credentials{"alice", testPW}, spoof...)
		if w.Code != 200 {
			t.Fatalf("login: %d %s", w.Code, w.Body)
		}
		if got := lastAudit(t, e).IP; got != "192.0.2.7" {
			t.Errorf("trusted=%q: audit IP %s, want the peer 192.0.2.7", trusted, got)
		}
		if c := w.Result().Cookies(); len(c) == 0 || c[0].Secure {
			t.Errorf("trusted=%q: cookie must not be Secure for a plain-HTTP direct client", trusted)
		}
		if h := w.Header().Get("Strict-Transport-Security"); h != "" {
			t.Errorf("HSTS set from a spoofed header: %q", h)
		}
		if csp := w.Header().Get("Content-Security-Policy"); strings.Contains(csp, "evil.example") {
			t.Errorf("CSP follows a spoofed host: %s", csp)
		}
		// A spoofed host must not make a cross-origin POST look same-origin.
		w = e.do("POST", "/api/logout", nil, header("Origin", "https://evil.example"), header("X-Forwarded-Host", "evil.example"))
		if w.Code != 403 {
			t.Errorf("trusted=%q: spoofed X-Forwarded-Host passed the CSRF check (%d)", trusted, w.Code)
		}

		// Rate limiting keys on the peer: failures with ever-changing
		// X-Forwarded-For values still lock the one address.
		locked := false
		for i := 0; i < 40 && !locked; i++ {
			w = e.do("POST", "/api/login", credentials{"nobody", "x"}, header("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i)))
			locked = w.Code == 429
		}
		if !locked {
			t.Errorf("trusted=%q: rotating X-Forwarded-For evaded the rate limit", trusted)
		}
	}
}

func TestForwardedHeadersFromTrustedPeer(t *testing.T) {
	e := newEnv(t)
	e.srv.TrustedProxies = mustProxies(t, "192.0.2.0/24")
	e.user("alice", "admin")
	fwd := []opt{header("X-Forwarded-For", "9.9.9.9, 203.0.113.5"), header("X-Forwarded-Proto", "https"), header("X-Forwarded-Host", "vm.example.org")}

	w := e.do("POST", "/api/login", credentials{"alice", testPW}, append(fwd, header("Origin", "https://vm.example.org"))...)
	if w.Code != 200 {
		t.Fatalf("login via proxy: %d %s", w.Code, w.Body)
	}
	if got := lastAudit(t, e).IP; got != "203.0.113.5" {
		t.Errorf("audit IP %s, want 203.0.113.5", got)
	}
	if c := w.Result().Cookies(); len(c) == 0 || !c[0].Secure {
		t.Error("cookie must be Secure when the proxy says https")
	}
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing behind an https proxy")
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "wss://vm.example.org") || strings.Contains(csp, "ws://vm.example.org") {
		t.Errorf("CSP connect-src should name wss://vm.example.org only: %s", csp)
	}
	// Origin is checked against the forwarded host, not the rewritten Host.
	if w = e.do("POST", "/api/logout", nil, append(fwd, header("Origin", "https://example.test"))...); w.Code != 403 {
		t.Errorf("foreign origin accepted: %d", w.Code)
	}
	// Plain-http proxy: no Secure, no HSTS.
	w = e.do("POST", "/api/login", credentials{"alice", testPW}, header("X-Forwarded-For", "203.0.113.6"))
	if c := w.Result().Cookies(); len(c) == 0 || c[0].Secure || w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("http via proxy must not be Secure/HSTS")
	}
	// The rate limit key is the forwarded client, so one bad client does not lock the proxy.
	for i := 0; i < 6; i++ {
		e.do("POST", "/api/login", credentials{"alice", "wrong-wrong"}, header("X-Forwarded-For", "203.0.113.77"))
	}
	if w = e.do("POST", "/api/login", credentials{"alice", testPW}, header("X-Forwarded-For", "203.0.113.78")); w.Code != 200 {
		t.Errorf("another client behind the proxy is locked out: %d", w.Code)
	}
}

func TestGlobEscape(t *testing.T) {
	if got := globEscape("[::1]:8080"); got != `\[::1\]:8080` {
		t.Error(got)
	}
}
