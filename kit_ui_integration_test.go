package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestKitIntegrationLocalizedUIAndErrorsKeepCacheBoundary(t *testing.T) {
	f := newKitIntegrationFixture(t, 2)
	f.prime(t)
	fetches, gets := f.upstream.Load(), f.store.gets.Load()
	for _, locale := range []string{"zh-CN", "en"} {
		for _, path := range []string{"/?lang=" + locale, "/assets/locales/" + locale + ".json"} {
			for _, method := range []string{"GET", "HEAD"} {
				w := observeRequest(f.handler, method, path)
				if w.Code != 200 || w.Header().Get("Content-Language") != locale || w.Header().Get("Content-Security-Policy") != uiContentSecurityPolicy || w.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatalf("localized resource %s %s lost its response contract", method, path)
				}
				if method == "HEAD" && w.Body.Len() != 0 || strings.Contains(w.Body.String(), "SECRET") {
					t.Fatal("localized resource exposed a body or input outside its contract")
				}
			}
		}
	}
	if f.upstream.Load() != fetches || f.store.gets.Load() != gets {
		t.Fatal("language page or dictionary accessed source/artifact storage")
	}
	// A warm binary download remains language-neutral and HEAD-compatible.
	w := iconRequest(f.handler, "HEAD", "favicon-32x32.png", f.query)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Language") != "" || w.Header().Get(iconErrorCodeHeader) != "" || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("localized UI changed warm binary response semantics")
	}
	fetches, gets = f.upstream.Load(), f.store.gets.Load()
	for _, invalid := range []struct{ name, value, code, body string }{
		{"s", "SIGNATURE_SECRET", "source_forbidden", "source not permitted\n"},
		{"lang", "en", "invalid_parameters", "invalid icon parameters\n"},
	} {
		q := url.Values{"url": {f.query.Get("url")}, "s": {f.query.Get("s")}}
		q.Set(invalid.name, invalid.value)
		w := iconRequest(f.handler, "GET", "favicon-32x32.png", q)
		if w.Code < 400 || w.Header().Get(iconErrorCodeHeader) != invalid.code || w.Body.String() != invalid.body || w.Header().Get("Content-Language") != "" {
			t.Fatal("warm cache changed fixed error-code or English API protocol")
		}
	}
	if f.upstream.Load() != fetches || f.store.gets.Load() != gets {
		t.Fatal("localized invalid/unauthorized request entered source or artifact lookup")
	}
	// Switching the page language has left the successful source identity intact.
	w = httptest.NewRecorder()
	f.request(context.Background(), w)
	if w.Code != 200 || integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "memory", "result": "hit"}) != 3 {
		t.Fatal("language resources invalidated or aliased the warm artifact")
	}
}
