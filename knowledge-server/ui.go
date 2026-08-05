// Embedded web UI: vanilla HTML/JS/CSS, zero dependencies, served
// from the same binary at "/" and "/ui/*". The UI is a client of
// /api/* — it never touches the store directly. The shell is served
// without authentication (the static code needed to bootstrap login
// carries no data); /api/* auth is untouched by registerUI. See
// docs/plans/knowledge-server.md, build-order step 4.
package main

import (
	"embed"
	"net/http"
	"strings"
)

// uiFS embeds EXACTLY the production UI assets. Test-only files
// (lib_test.mjs, …) are not embedded and therefore cannot be
// served under /ui/ even if a request names them.
//
//go:embed ui/index.html ui/app.js ui/lib.mjs ui/style.css
var uiFS embed.FS

// uiHardeningHeaders are set on every UI (non-API) response. The CSP
// pins the same-origin / no-inline / no-eval contract that the JS and
// HTML are written against; the three extra headers are belt-and-
// suspenders against browser caching, MIME confusion, and click-
// jacking framing.
var uiHardeningHeaders = []struct{ name, value string }{
	{"Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"},
	{"X-Frame-Options", "DENY"},
	{"Cache-Control", "no-store"},
	{"X-Content-Type-Options", "nosniff"},
}

// registerUI mounts the embedded UI assets on mux. It registers the
// exact root ("/") for index.html and an exact allow-list for the
// three other asset paths; any other /ui/* request 404s, including
// paths to files that are NOT in the //go:embed list above (test
// sources, future scratch files). registerUI MUST NOT touch the
// /api/* routes.
func registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		serveUIFile(w, r, "ui/index.html", "text/html; charset=utf-8")
	})
	for asset, ctype := range uiAllowList() {
		path := "/ui/" + asset
		asset, ctype := asset, ctype
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			serveUIFile(w, r, "ui/"+asset, ctype)
		})
	}
}

// uiAllowList pins the exact production asset set. Test files and
// any other files present in the ui/ directory are intentionally
// absent — they must not be served.
func uiAllowList() map[string]string {
	return map[string]string{
		"app.js":    "text/javascript; charset=utf-8",
		"lib.mjs":   "text/javascript; charset=utf-8",
		"style.css": "text/css; charset=utf-8",
	}
}

// serveUIFile writes one embedded UI asset with the hardening headers.
func serveUIFile(w http.ResponseWriter, r *http.Request, embedName, ctype string) {
	for _, h := range uiHardeningHeaders {
		w.Header().Set(h.name, h.value)
	}
	w.Header().Set("Content-Type", ctype)
	f, err := uiFS.Open(embedName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.IsDir() {
		http.NotFound(w, r)
		return
	}
	// Sanity: refuse to serve anything whose embed path doesn't match
	// the allow-list (defense in depth against future routing changes).
	if !strings.HasPrefix(embedName, "ui/") {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, embedName, stat.ModTime(), f.(readSeeker))
}

// readSeeker lets serveUIFile hand a *embed.File (which implements
// io.ReadSeeker) to http.ServeContent.
type readSeeker interface {
	Read(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
}
