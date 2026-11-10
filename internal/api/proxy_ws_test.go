package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The WebSocket origin check behind a real reverse proxy that rewrites Host to
// the backend's address (the default of several proxies).
func TestWebSocketBehindProxy(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	backend := httptest.NewServer(e.h)
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(bu) // Host becomes the backend's
		pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
		pr.SetXForwarded()
		pr.Out.Header.Set("X-Forwarded-Host", "vm.example.org")
		pr.Out.Header.Set("X-Forwarded-Proto", "https")
	}})
	defer proxy.Close()
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/api/events"
	dial := func(origin string) error {
		h := http.Header{"Cookie": {cookieName + "=" + e.cookies["root"]}, "Origin": {origin}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
		if err == nil {
			c.CloseNow()
		}
		return err
	}
	// Not trusted: the rewritten Host decides, so the browser's origin fails.
	if err := dial("https://vm.example.org"); err == nil {
		t.Error("forwarded host believed from an untrusted proxy")
	}
	e.srv.TrustedProxies = mustProxies(t, "127.0.0.1,::1")
	if err := dial("https://vm.example.org"); err != nil {
		t.Errorf("origin of the proxied host refused: %v", err)
	}
	if err := dial("https://evil.example"); err == nil {
		t.Error("foreign origin accepted behind the proxy")
	}
}
