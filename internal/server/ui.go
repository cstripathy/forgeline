package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
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
	assets = secure(http.StripPrefix("/ui/", etagged(sub, files)))
	return index, assets
}

// etagged adds a content hash as the ETag of every embedded file and answers a matching
// If-None-Match with 304. The files carry no modification time, so without this a browser has
// nothing to revalidate against and may keep serving an old script after an upgrade.
func etagged(sub fs.FS, next http.Handler) http.Handler {
	tags := map[string]string{}
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := fs.ReadFile(sub, p); err == nil {
			sum := sha256.Sum256(b)
			tags[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		}
		return nil
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := tags[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			w.Header().Set("ETag", tag)
			if r.Header.Get("If-None-Match") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
