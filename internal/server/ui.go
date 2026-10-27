package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui
var uiFiles embed.FS

// contentSecurityPolicy lets the page load only its own script and styles and talk only to
// this server. There is no inline script or style, and pages cannot be framed. Pipeline
// names, step names and log output are untrusted, so the UI also inserts them as text only.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// uiHandler serves the embedded web UI: the page at "/" and its assets under "/ui/".
func uiHandler() (index, assets http.Handler) {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // the embed directive guarantees the directory exists
	}
	files := http.FileServerFS(sub)
	secure := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", contentSecurityPolicy)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cache-Control", "no-cache")
			next.ServeHTTP(w, r)
		})
	}
	// Served from memory: http.ServeFile redirects any request for index.html to "./".
	page, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic(err)
	}
	index = secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	}))
	assets = secure(http.StripPrefix("/ui/", files))
	return index, assets
}
