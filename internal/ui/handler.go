// Package ui serves the daemon's read-only task dashboard: one HTML file
// embedded next to this source, mounted on the MCP listener at /ui/.
package ui

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// contentSecurityPolicy keeps the page on this origin. script-src
// 'unsafe-inline' is the page's own script: default-src 'self' does not allow
// an inline script, and the dashboard is one file, so there is no script URL
// to load. 'unsafe-inline' does not include 'self', so a script URL — even a
// same-origin one — is still refused.
const contentSecurityPolicy = "default-src 'self'; script-src 'unsafe-inline'; style-src 'self' 'unsafe-inline'"

// Handler serves index.html for GET /ui and GET /ui/. Anything else is 404,
// and any other method is 405. The page polls the MCP endpoint on this same
// origin; it has no mutating controls.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ui" && r.URL.Path != "/ui/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
}
