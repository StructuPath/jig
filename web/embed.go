// Package web serves the embedded operator UI (U8, R18).
//
// dist/ is built by `npm run build` and COMMITTED, so every jig binary
// carries the same reviewed production bundle and an operator never needs
// Node — `go build` is the whole build. CI rebuilds it and fails on a diff,
// which is what keeps the committed artifact honest.
//
// Serving rules, following factory:web/embed.go:
//
//   - SPA fallback applies only to app routes. A request for a path with an
//     extension that is not in dist stays a 404: returning index.html to a
//     script or stylesheet request turns a missing asset into a confusing
//     parse error instead of an honest miss.
//   - Hashed assets under assets/ are immutable for a year; index.html is
//     never cached, so a new binary's UI appears on the next reload.
//   - GET and HEAD only, with browser hardening headers on every response.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var assets embed.FS

type handler struct {
	files http.Handler
	dist  fs.FS
	index []byte
}

// Handler serves the embedded single-page app. It is mounted at "/" beside
// the control-plane API on the same origin, which is what lets the UI's
// state-changing requests pass the API's Origin check as same-origin (R20)
// without carrying the per-process token.
//
// It panics if the embedded bundle is missing or unreadable: a jig binary
// whose UI did not build is a build error, not a runtime surprise.
func Handler() http.Handler {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		panic("open embedded UI: " + err.Error())
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		panic("read embedded UI index: " + err.Error())
	}
	return &handler{files: http.FileServer(http.FS(dist)), dist: dist, index: index}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setBrowserHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		h.serveIndex(w, r)
		return
	}
	if info, err := fs.Stat(h.dist, name); err == nil && !info.IsDir() {
		if strings.HasPrefix(name, "assets/") {
			// Vite hashes these names, so the content behind one can never
			// change: cache it as long as a browser will.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		h.files.ServeHTTP(w, r)
		return
	}
	if path.Ext(name) != "" {
		// A missing asset stays a missing asset.
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodGet {
		_, _ = w.Write(h.index)
	}
}

// setBrowserHeaders is the UI's half of the local-first security posture
// (R20, R21): everything the page loads or talks to must be this origin, and
// nothing may frame it.
func setBrowserHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; font-src 'self'; "+
			"base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
