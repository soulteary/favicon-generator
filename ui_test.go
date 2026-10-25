package main

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

func TestUIAssetsAndHead(t *testing.T) {
	h, calls := newTestIconHandler(iconSourcePNG(t))
	h.proxy.DefaultBaseURL, _ = url.Parse("https://origin.test/")
	for path, contentType := range map[string]string{
		"/":                  "text/html; charset=utf-8",
		"/assets/app.js":     "text/javascript; charset=utf-8",
		"/assets/styles.css": "text/css; charset=utf-8",
	} {
		t.Run(path, func(t *testing.T) {
			get := httptest.NewRecorder()
			h.ServeHTTP(get, httptest.NewRequest("GET", path+"?url=URL_SECRET&s=SIGNATURE_SECRET", nil))
			if get.Code != 200 || get.Header().Get("Content-Type") != contentType || get.Body.Len() == 0 {
				t.Fatalf("asset response: %d, %s", get.Code, get.Header().Get("Content-Type"))
			}
			if get.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
				t.Fatal("incorrect asset length")
			}
			if strings.Contains(get.Body.String(), "URL_SECRET") || strings.Contains(get.Body.String(), "SIGNATURE_SECRET") {
				t.Fatal("UI reflects request parameters")
			}
			head := httptest.NewRecorder()
			h.ServeHTTP(head, httptest.NewRequest("HEAD", path, nil))
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
				t.Fatal("HEAD differs from GET")
			}
			for _, response := range []*httptest.ResponseRecorder{get, head} {
				for key, value := range map[string]string{"Cache-Control": "private, no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cross-Origin-Resource-Policy": "same-origin"} {
					if response.Header().Get(key) != value {
						t.Fatalf("asset missing %s", key)
					}
				}
				csp := response.Header().Get("Content-Security-Policy")
				for _, policy := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "img-src blob:", "connect-src 'self'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
					if !strings.Contains(csp, policy) {
						t.Fatalf("missing CSP directive %q", policy)
					}
				}
				if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
					t.Fatal("UI permits inline script execution")
				}
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("static UI reached configured image BaseURL")
	}
}

func TestUIReservesOnlyProductionAssetPaths(t *testing.T) {
	h, calls := newTestIconHandler(iconSourcePNG(t))
	h.proxy.DefaultBaseURL, _ = url.Parse("https://origin.test/")
	for _, path := range []string{"/assets", "/assets/", "/assets/index.html", "/assets/app.test.mjs", "/assets/package.json", "/assets/../go.mod", "/assets/%2e%2e/go.mod", "/assets/web/app.js", "/assets/https://origin.test/logo"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != 404 || (method == "HEAD" && w.Body.Len() != 0) {
				t.Fatalf("%s %s exposed an asset: %d", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/", "/assets/app.js", "/assets/styles.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("POST %s = %d", path, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unknown UI asset fell through to source proxy")
	}
	// Existing embedded source URLs still reach the proxy without path cleanup.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/32,png/https://origin.test/logo", nil))
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("legacy image route broken: %d", w.Code)
	}
}

func TestUIAndHealthRemainAvailableAtImageCapacity(t *testing.T) {
	h, _ := newTestIconHandler(iconSourcePNG(t))
	h.proxy.MaxConcurrentRequests = 1
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(release)
	source, _ := url.Parse("https://origin.test/logo")
	go func() {
		done <- h.proxy.WithSource(ctx, source, "", func(ctx context.Context, _ []byte, _ imageproxy.SourceLimits) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	lifecycleWait(t, started)
	obs := testObservability(t, io.Discard, "info")
	public := obs.PublicHandler(h)
	for _, path := range []string{"/", "/assets/app.js", "/assets/styles.css", "/health-check"} {
		w := httptest.NewRecorder()
		public.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("image capacity blocked %s: %d", path, w.Code)
		}
	}
	if w := iconRequest(h, "GET", "favicon-32x32.png", url.Values{"url": {"https://origin.test/logo"}}); w.Code != 503 {
		t.Fatalf("image capacity not occupied: %d", w.Code)
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("image worker did not cancel: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("image worker did not finish after cancellation")
	}
}

func TestUIObservabilityUsesFixedSecretFreeRoutes(t *testing.T) {
	var log bytes.Buffer
	obs := testObservability(t, &log, "debug")
	h, _ := newTestIconHandler(iconSourcePNG(t))
	public := obs.PublicHandler(h)
	for _, path := range []string{"/", "/assets/app.js", "/assets/styles.css"} {
		w := httptest.NewRecorder()
		public.ServeHTTP(w, httptest.NewRequest("GET", path+"?url=SOURCE_SECRET&s=SIGNATURE_SECRET", nil))
		if w.Code != 200 {
			t.Fatal("UI route failed")
		}
	}
	for path, count := range map[string]float64{"ui": 1, "assets": 2} {
		labels := map[string]string{"method": "GET", "path": path, "status": "200"}
		if value, found := observedMetric(t, obs, "requests_total", "counter", labels); !found || value != count {
			t.Fatalf("wrong %s request count: %v", path, value)
		}
	}
	metrics := observeRequest(obs.AdminHandler(), "GET", "/metrics").Body.String()
	for _, secret := range []string{"SOURCE_SECRET", "SIGNATURE_SECRET"} {
		if strings.Contains(log.String(), secret) || strings.Contains(metrics, secret) {
			t.Fatal("UI observation exposed query parameters")
		}
	}
}
