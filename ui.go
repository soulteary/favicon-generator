package main

import (
	_ "embed"
	"net/http"
	"strconv"
	"strings"
)

// Only production assets are embedded and exposed. Tests and package metadata
// in web/ are never reachable through the public asset namespace.
//
//go:embed web/index.html
var uiHTML string

//go:embed web/app.js
var uiJS string

//go:embed web/styles.css
var uiCSS string

type uiAsset struct {
	content     string
	contentType string
}

var uiAssets = map[string]uiAsset{
	"/":                  {uiHTML, "text/html; charset=utf-8"},
	"/assets/app.js":     {uiJS, "text/javascript; charset=utf-8"},
	"/assets/styles.css": {uiCSS, "text/css; charset=utf-8"},
}

const uiContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src blob:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

// serveUI reserves the root and the entire asset namespace before imageproxy's
// BaseURL fallback, without ServeMux's source-URL path canonicalization.
func serveUI(w http.ResponseWriter, r *http.Request) bool {
	asset, found := uiAssets[r.URL.Path]
	if !found && r.URL.Path != "/assets" && !strings.HasPrefix(r.URL.Path, "/assets/") {
		return false
	}
	if r.Method == http.MethodHead {
		w = &headResponseWriter{ResponseWriter: w}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", uiContentSecurityPolicy)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if !found {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.content)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(asset.content))
	}
	return true
}
