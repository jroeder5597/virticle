package webaccess

import (
	"net"
	"net/http"
	"strings"
)

// RedirectHandler sends every request to the HTTPS address. With a public URL
// that is its host; otherwise the host the client used, on the HTTPS port.
// The redirect is temporary (307) so a browser never caches it past a change
// of settings, and the method and body of a POST survive.
func RedirectHandler(s Settings) http.Handler {
	_, httpsPort, _ := net.SplitHostPort(s.HTTPSListen)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := s.PublicHost()
		if host == "" {
			h := r.Host
			if hh, _, err := net.SplitHostPort(h); err == nil {
				h = hh
			}
			h = strings.Trim(h, "[]")
			if strings.Contains(h, ":") {
				h = "[" + h + "]"
			}
			host = h
			if httpsPort != "" && httpsPort != "443" {
				host += ":" + httpsPort
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}
