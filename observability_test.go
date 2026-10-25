package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testObservability(t *testing.T, out io.Writer, level string) *observability {
	t.Helper()
	o, err := newObservability(out, level)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func observeRequest(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func observedMetric(t *testing.T, o *observability, name, kind string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := o.metrics.Registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "favicon_generator_http_"+name {
			continue
		}
		for _, metric := range family.Metric {
			values := make(map[string]string)
			for _, label := range metric.Label {
				values[label.GetName()] = label.GetValue()
			}
			match := len(values) == len(labels)
			for key, value := range labels {
				match = match && values[key] == value
			}
			if !match {
				continue
			}
			switch kind {
			case "counter":
				return metric.GetCounter().GetValue(), true
			case "gauge":
				return metric.GetGauge().GetValue(), true
			case "count":
				return float64(metric.GetHistogram().GetSampleCount()), true
			case "sum":
				return metric.GetHistogram().GetSampleSum(), true
			}
		}
	}
	return 0, false
}

func TestObservabilityReadinessAndManagementMethods(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	var proxyCalls atomic.Int32
	public := o.PublicHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	admin := o.AdminHandler()
	for _, route := range []string{"/health-check", "/livez", "/readyz"} {
		want := http.StatusOK
		if route == "/readyz" {
			want = http.StatusServiceUnavailable
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := observeRequest(public, method, route)
			if w.Code != want {
				t.Fatalf("%s %s: got %d, want %d", method, route, w.Code, want)
			}
			if method == http.MethodHead && w.Body.Len() != 0 {
				t.Fatalf("HEAD %s returned a body", route)
			}
			if strings.Contains(w.Body.String(), "checks") || strings.Contains(w.Body.String(), "service is not ready") {
				t.Fatalf("health details exposed: %s", w.Body.String())
			}
		}
		w := observeRequest(public, http.MethodPost, route)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("POST %s: got %d, Allow %q", route, w.Code, w.Header().Get("Allow"))
		}
	}
	if got := observeRequest(public, http.MethodGet, "/health-check").Body.String(); got != "OK" {
		t.Fatalf("legacy health response changed: %q", got)
	}
	for _, handler := range []http.Handler{public, admin} {
		if got := observeRequest(handler, http.MethodGet, "/readyz").Code; got != http.StatusServiceUnavailable {
			t.Fatalf("unready status = %d", got)
		}
	}
	o.SetReady(true)
	for _, handler := range []http.Handler{public, admin} {
		if got := observeRequest(handler, http.MethodGet, "/readyz").Code; got != http.StatusOK {
			t.Fatalf("ready status = %d", got)
		}
	}
	o.SetReady(false)
	if got := observeRequest(public, http.MethodGet, "/livez").Code; got != http.StatusOK {
		t.Fatalf("liveness depended on readiness: %d", got)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		if got := observeRequest(public, method, "/metrics?token=secret").Code; got != http.StatusNotFound {
			t.Fatalf("public metrics route status = %d", got)
		}
	}
	for _, target := range []string{"/", "/health-check", "/png/https://private.example/image"} {
		if got := observeRequest(admin, http.MethodGet, target).Code; got != http.StatusNotFound {
			t.Fatalf("admin accepted image/unknown route %q: %d", target, got)
		}
	}
	if got := observeRequest(admin, http.MethodGet, "/metrics").Code; got != http.StatusOK {
		t.Fatalf("admin metrics status = %d", got)
	}
	if w := observeRequest(admin, http.MethodHead, "/metrics"); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("admin metrics HEAD: %d, %d body bytes", w.Code, w.Body.Len())
	}
	if got := observeRequest(admin, http.MethodPost, "/metrics").Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("admin metrics POST status = %d", got)
	}
	if proxyCalls.Load() != 0 {
		t.Fatalf("management routes reached image proxy: %d", proxyCalls.Load())
	}
	headLabels := map[string]string{"method": "HEAD", "path": "readyz"}
	if count, _ := observedMetric(t, o, "response_size_bytes", "count", headLabels); count != 1 {
		t.Fatalf("HEAD response size was not observed: %v", count)
	}
	if size, _ := observedMetric(t, o, "response_size_bytes", "sum", headLabels); size != 0 {
		t.Fatalf("HEAD response included entity bytes: %v", size)
	}
}

func TestObservabilityRegistriesAreIsolated(t *testing.T) {
	first := testObservability(t, io.Discard, "info")
	second := testObservability(t, io.Discard, "info")
	handler := first.PublicHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	observeRequest(handler, http.MethodGet, "/image")
	labels := map[string]string{"method": "GET", "path": "image", "status": "201"}
	if value, found := observedMetric(t, first, "requests_total", "counter", labels); !found || value != 1 {
		t.Fatalf("first registry: found=%t value=%v", found, value)
	}
	if value, found := observedMetric(t, second, "requests_total", "counter", labels); found {
		t.Fatalf("second registry contains another instance's request: %v", value)
	}
}

func TestObservabilityDoesNotExposeRequestSecrets(t *testing.T) {
	var output bytes.Buffer
	o := testObservability(t, &output, "debug")
	const target = "/128,png,sSIGNED_SECRET/https://secret-origin.example/private-file?token=TOKEN_SECRET"
	var forwarded string
	handler := o.PublicHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.URL.RequestURI()
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "private-response")
	}))
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer HEADER_SECRET")
	r.Header.Set("Cookie", "session=COOKIE_SECRET")
	handler.ServeHTTP(httptest.NewRecorder(), r)
	if forwarded != target {
		t.Fatalf("proxy target was normalized: %q", forwarded)
	}
	observeRequest(handler, "METHOD_SECRET", target)
	o.ProxyLogger.Printf("upstream failure: %v", &url.Error{Op: "Get", URL: "https://secret-origin.example?token=TOKEN_SECRET", Err: errors.New("ERROR_SECRET")})
	o.ServerErrorLogger.Print("panic: PANIC_SECRET\nrequest URI: " + target)
	metrics := observeRequest(o.AdminHandler(), http.MethodGet, "/metrics").Body.String()
	for _, secret := range []string{"SIGNED_SECRET", "TOKEN_SECRET", "HEADER_SECRET", "COOKIE_SECRET", "METHOD_SECRET", "ERROR_SECRET", "PANIC_SECRET", "private-response", "secret-origin.example", "private-file"} {
		if strings.Contains(output.String(), secret) || strings.Contains(metrics, secret) {
			t.Errorf("observability exposed %q", secret)
		}
	}
	if !strings.Contains(metrics, `path="image"`) || !strings.Contains(metrics, `method="OTHER"`) {
		t.Fatalf("expected fixed labels missing: %s", metrics)
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("invalid structured log %q: %v", line, err)
		}
		for key := range fields {
			switch key {
			case "level", "time", "service", "route", "method", "status", "duration", "message", "category":
			default:
				t.Errorf("unexpected request log field %q", key)
			}
		}
	}
	if _, err := newObservability(io.Discard, "INVALID_LEVEL_SECRET"); err == nil || strings.Contains(err.Error(), "INVALID_LEVEL_SECRET") {
		t.Fatalf("unsafe log-level error: %v", err)
	}
}

func TestObservabilityConcurrentRequestsAndReadiness(t *testing.T) {
	var output bytes.Buffer
	o := testObservability(t, &output, "debug")
	const requests = 24
	started := make(chan struct{}, requests)
	release := make(chan struct{})
	handler := o.PublicHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		o.ServerErrorLogger.Print("CONCURRENT_SECRET")
		_, _ = io.WriteString(w, "image")
	}))
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			observeRequest(handler, http.MethodGet, "/https://source.example/random")
		}()
	}
	for i := 0; i < requests; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("requests did not enter the handler")
		}
	}
	value, found := observedMetric(t, o, "requests_in_flight", "gauge", nil)
	if !found || value != requests {
		t.Errorf("in-flight metric = %v, found=%t", value, found)
	}
	close(release)
	wg.Wait()
	labels := map[string]string{"method": "GET", "path": "image", "status": "200"}
	if value, _ := observedMetric(t, o, "requests_total", "counter", labels); value != requests {
		t.Fatalf("request count = %v, want %d", value, requests)
	}
	if value, _ := observedMetric(t, o, "requests_in_flight", "gauge", nil); value != 0 {
		t.Fatalf("requests remained in flight: %v", value)
	}
	sizeLabels := map[string]string{"method": "GET", "path": "image"}
	if value, _ := observedMetric(t, o, "response_size_bytes", "sum", sizeLabels); value != requests*5 {
		t.Fatalf("response bytes = %v", value)
	}
	if value, _ := observedMetric(t, o, "request_size_bytes", "count", sizeLabels); value != requests {
		t.Fatalf("zero-sized requests were not counted: %v", value)
	}
	if value, _ := observedMetric(t, o, "request_duration_seconds", "count", sizeLabels); value != requests {
		t.Fatalf("request durations were not counted: %v", value)
	}
	if strings.Contains(output.String(), "CONCURRENT_SECRET") {
		t.Fatal("diagnostic exposed request details")
	}
	ready := o.AdminHandler()
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o.SetReady(i%2 == 0)
			status := observeRequest(ready, http.MethodGet, "/readyz").Code
			if status != http.StatusOK && status != http.StatusServiceUnavailable {
				t.Errorf("readiness status = %d", status)
			}
		}(i)
	}
	wg.Wait()
}

type responseControllerProbe struct {
	header                      http.Header
	statuses                    []int
	body                        bytes.Buffer
	readDeadline, writeDeadline time.Time
	flushed, fullDuplex, pushed bool
	conn                        net.Conn
}

func (w *responseControllerProbe) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *responseControllerProbe) WriteHeader(status int)      { w.statuses = append(w.statuses, status) }
func (w *responseControllerProbe) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *responseControllerProbe) FlushError() error {
	w.flushed = true
	if len(w.statuses) == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return nil
}
func (w *responseControllerProbe) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return nil
}
func (w *responseControllerProbe) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}
func (w *responseControllerProbe) EnableFullDuplex() error { w.fullDuplex = true; return nil }
func (w *responseControllerProbe) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, nil, nil
}
func (w *responseControllerProbe) Push(string, *http.PushOptions) error { w.pushed = true; return nil }

func TestObservedResponseWriterStatusAndBytes(t *testing.T) {
	underlying := &responseControllerProbe{}
	w := &observedResponseWriter{ResponseWriter: underlying}
	w.WriteHeader(http.StatusEarlyHints)
	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusInternalServerError)
	w.WriteHeader(http.StatusEarlyHints)
	if _, err := io.Copy(w, strings.NewReader("image-data")); err != nil {
		t.Fatal(err)
	}
	if w.status != http.StatusCreated || w.bytes != 10 || len(underlying.statuses) != 2 || underlying.statuses[0] != 103 || underlying.statuses[1] != 201 {
		t.Fatalf("status=%d bytes=%d forwarded=%v", w.status, w.bytes, underlying.statuses)
	}
	implicit := &observedResponseWriter{ResponseWriter: httptest.NewRecorder()}
	_, _ = implicit.Write([]byte("ok"))
	if implicit.status != http.StatusOK || implicit.bytes != 2 {
		t.Fatalf("implicit response: status=%d bytes=%d", implicit.status, implicit.bytes)
	}
}

func TestObservedResponseWriterResponseController(t *testing.T) {
	underlying := &responseControllerProbe{}
	w := &observedResponseWriter{ResponseWriter: underlying}
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(time.Second)
	if err := controller.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := controller.EnableFullDuplex(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Flush(); err != nil {
		t.Fatal(err)
	}
	w.WriteHeader(http.StatusInternalServerError)
	if w.Unwrap() != underlying || !underlying.flushed || !underlying.fullDuplex || underlying.readDeadline != deadline || underlying.writeDeadline != deadline || w.status != http.StatusOK || len(underlying.statuses) != 1 {
		t.Fatalf("controller capabilities/status were lost: %+v", underlying)
	}
	if err := w.Push("/image", nil); err != nil || !underlying.pushed {
		t.Fatalf("Push: %v", err)
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	underlying.conn = server
	if conn, _, err := controller.Hijack(); err != nil || conn != server {
		t.Fatalf("Hijack returned %v, %v", conn, err)
	}
	unsupported := &observedResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := http.NewResponseController(unsupported).Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("unsupported Hijack: %v", err)
	}
}

func TestObservabilityPanicIsPropagatedWithoutLeakingDetails(t *testing.T) {
	var output bytes.Buffer
	o := testObservability(t, &output, "debug")
	handler := o.PublicHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("PANIC_SECRET") }))
	func() {
		defer func() {
			if got := recover(); got != "PANIC_SECRET" {
				t.Errorf("panic was swallowed or changed: %v", got)
			}
		}()
		observeRequest(handler, http.MethodGet, "/private")
	}()
	labels := map[string]string{"method": "GET", "path": "image", "status": "500"}
	if value, _ := observedMetric(t, o, "requests_total", "counter", labels); value != 1 {
		t.Fatalf("panic metric = %v", value)
	}
	if value, _ := observedMetric(t, o, "requests_in_flight", "gauge", nil); value != 0 {
		t.Fatalf("panic leaked in-flight request: %v", value)
	}
	if strings.Contains(output.String(), "PANIC_SECRET") {
		t.Fatal("panic details reached logs")
	}
}
