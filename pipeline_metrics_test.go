package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

func pipelineMetric(t *testing.T, o *observability, name, pipeline, label, value, result string) (float64, float64) {
	t.Helper()
	families, err := o.metrics.Registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "favicon_generator_pipeline_"+name {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, pair := range metric.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			if len(labels) == 3 && labels["pipeline"] == pipeline && labels[label] == value && labels["result"] == result {
				if name == "cache_requests_total" {
					return metric.GetCounter().GetValue(), 0
				}
				return float64(metric.GetHistogram().GetSampleCount()), metric.GetHistogram().GetSampleSum()
			}
		}
	}
	return 0, 0
}

func stageCount(t *testing.T, o *observability, pipeline, stage, result string) float64 {
	t.Helper()
	count, _ := pipelineMetric(t, o, "stage_duration_seconds", pipeline, "stage", stage, result)
	return count
}

func TestPipelineMetricsEndToEndAndIsolation(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	other := testObservability(t, io.Discard, "info")
	h, _ := newTestIconHandler(iconSourcePNG(t))
	handler := o.PublicHandler(h)
	q := url.Values{"url": {"https://origin.test/logo?token=SOURCE_SECRET"}, "s": {"SIGNATURE_SECRET"}, "name": {"NAME_SECRET"}}
	if w := iconRequest(handler, "GET", "icons.zip", q); w.Code != 200 {
		t.Fatalf("ZIP request failed: %d %s", w.Code, w.Body.String())
	}
	for _, stage := range []string{"fetch", "decode", "transform", "encode", "pack", "write"} {
		count, duration := pipelineMetric(t, o, "stage_duration_seconds", "icons", "stage", stage, "success")
		if count < 1 || duration <= 0 {
			t.Fatalf("%s count=%v duration=%v", stage, count, duration)
		}
		if stageCount(t, other, "icons", stage, "success") != 0 {
			t.Fatalf("%s metric leaked into a second service registry", stage)
		}
	}
	if w := observeRequest(handler, "GET", "/32,png/https://origin.test/logo"); w.Code != 200 {
		t.Fatalf("legacy image request failed: %d", w.Code)
	}
	if stageCount(t, o, "image", "write", "success") != 1 || stageCount(t, o, "image", "pack", "success") != 0 {
		t.Fatal("legacy image observations were not separated from icons")
	}
	if count, _ := pipelineMetric(t, o, "cache_requests_total", "icons", "layer", "source", "bypass"); count != 1 {
		t.Fatalf("disabled source cache should bypass once, got %v", count)
	}
	first := observeRequest(o.AdminHandler(), "GET", "/metrics").Body.String()
	for _, target := range []string{"/", "/assets/app.js", "/livez", "/readyz", "/health-check"} {
		observeRequest(handler, "GET", target)
	}
	second := observeRequest(o.AdminHandler(), "GET", "/metrics").Body.String()
	if pipelineLines(first) != pipelineLines(second) {
		t.Fatal("UI, assets, health, or scraping created pipeline observations")
	}
	for _, secret := range []string{"origin.test", "SOURCE_SECRET", "SIGNATURE_SECRET", "NAME_SECRET", "logo?", "icons.zip"} {
		if strings.Contains(second, secret) {
			t.Fatalf("metrics exposed %q", secret)
		}
	}
	if w := observeRequest(handler, "GET", "/metrics"); w.Code != 404 {
		t.Fatal("metrics became available on public listener")
	}
}

func pipelineLines(scrape string) string {
	var out strings.Builder
	for line := range strings.SplitSeq(scrape, "\n") {
		if strings.Contains(line, "favicon_generator_pipeline_") {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestPipelineObserverRejectsUnboundedLabels(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	o.imagePipeline.ObserveStage("https://SECRET", "success", time.Second)
	o.imagePipeline.ObserveStage("decode", "ERROR_SECRET", time.Second)
	o.imagePipeline.ObserveStage("decode", "success", -time.Second)
	o.iconsPipeline.ObserveCache("CACHE_KEY_SECRET", "hit")
	o.iconsPipeline.ObserveCache("source", "ERROR_SECRET")
	if got := pipelineLines(observeRequest(o.AdminHandler(), "GET", "/metrics").Body.String()); got != "" {
		t.Fatalf("invalid observations created series: %s", got)
	}
}

func TestPipelineDecodeAndFetchFailures(t *testing.T) {
	for _, test := range []struct {
		name, stage, result string
		data                []byte
		maxBytes, maxPixels int64
		status              int
	}{
		{"bad-image", "decode", "error", []byte("not a PNG"), 0, 0, 502},
		{"pixel-limit", "decode", "limit", iconSourcePNG(t), 0, 1, 413},
		{"download-limit", "fetch", "limit", iconSourcePNG(t), 1, 0, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := testObservability(t, io.Discard, "info")
			h, _ := newTestIconHandler(test.data)
			if test.maxBytes != 0 {
				h.proxy.MaxResponseBytes = test.maxBytes
			}
			if test.maxPixels != 0 {
				h.proxy.MaxPixels = test.maxPixels
			}
			w := iconRequest(o.PublicHandler(h), "GET", "favicon-32x32.png", url.Values{"url": {"https://origin.test/logo"}})
			if w.Code != test.status || stageCount(t, o, "icons", test.stage, test.result) != 1 {
				t.Fatalf("status=%d stage %s/%s count=%v", w.Code, test.stage, test.result, stageCount(t, o, "icons", test.stage, test.result))
			}
			if stageCount(t, o, "icons", "encode", "success") != 0 || stageCount(t, o, "icons", "pack", "success") != 0 {
				t.Fatal("failed source request fabricated later pipeline stages")
			}
		})
	}
}

type pipelineFailWriter struct {
	*httptest.ResponseRecorder
	err    error
	cancel context.CancelFunc
}

func (w pipelineFailWriter) Write(p []byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
		return w.ResponseRecorder.Write(p)
	}
	n, _ := w.ResponseRecorder.Write(p[:min(8, len(p))])
	return n, w.err
}

type pipelineTimeoutError struct{}

func (pipelineTimeoutError) Error() string   { return "WRITE_SECRET" }
func (pipelineTimeoutError) Timeout() bool   { return true }
func (pipelineTimeoutError) Temporary() bool { return false }

func TestPipelineResponseWriteResults(t *testing.T) {
	for _, test := range []struct {
		name, result string
		err          error
		cancel       bool
	}{
		{"closed-pipe", "error", io.ErrClosedPipe, false},
		{"short-write", "error", nil, false},
		{"timeout", "timeout", pipelineTimeoutError{}, false},
		{"canceled", "canceled", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := testObservability(t, io.Discard, "info")
			h, _ := newTestIconHandler(iconSourcePNG(t))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := pipelineFailWriter{ResponseRecorder: httptest.NewRecorder(), err: test.err}
			if test.cancel {
				w.cancel = cancel
			}
			r := httptest.NewRequest("GET", "/icons/favicon-32x32.png?"+url.Values{"url": {"https://origin.test/logo"}}.Encode(), nil).WithContext(ctx)
			o.PublicHandler(h).ServeHTTP(w, r)
			if stageCount(t, o, "icons", "write", test.result) != 1 || stageCount(t, o, "icons", "write", "success") != 0 {
				t.Fatalf("incorrect write result: %s", pipelineLines(observeRequest(o.AdminHandler(), "GET", "/metrics").Body.String()))
			}
			if w.Code != 200 || (w.Body.Len() != 8 && !test.cancel) || strings.Contains(w.Body.String(), "WRITE_SECRET") {
				t.Fatal("write failure changed the committed image response")
			}
		})
	}
}

func TestPipelineSlowResponseCompletesOnRelease(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	h, _ := newTestIconHandler(iconSourcePNG(t))
	w := &blockedIconResponse{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(w.release) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		iconRequest := httptest.NewRequest("GET", "/icons/favicon-32x32.png?"+url.Values{"url": {"https://origin.test/logo"}}.Encode(), nil)
		o.PublicHandler(h).ServeHTTP(w, iconRequest)
	}()
	lifecycleWait(t, w.started)
	if stageCount(t, o, "icons", "write", "success") != 0 {
		t.Fatal("write completed before the blocked response was released")
	}
	once.Do(func() { close(w.release) })
	lifecycleWait(t, done)
	if stageCount(t, o, "icons", "write", "success") != 1 {
		t.Fatal("released response did not complete its write observation")
	}
}

func TestPipelineHEADAndNonBundleStages(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	h, _ := newTestIconHandler(iconSourcePNG(t))
	w := iconRequest(o.PublicHandler(h), "HEAD", "favicon-32x32.png", url.Values{"url": {"https://origin.test/logo"}})
	if w.Code != 200 || w.Body.Len() != 0 || stageCount(t, o, "icons", "write", "success") != 1 || stageCount(t, o, "icons", "pack", "success") != 0 {
		t.Fatal("HEAD body or stage observations are incorrect")
	}
	if size, _ := observedMetric(t, o, "response_size_bytes", "sum", map[string]string{"method": "HEAD", "path": "icons"}); size != 0 {
		t.Fatal("HEAD recorded response body bytes")
	}
}

func TestPipelineWarmCacheStillValidatesPolicy(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	data := iconSourcePNG(t)
	cache := (Config{Cache: []CacheConfig{{maxBytes: cacheMegabyte}}}).NewCache()
	p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{
			"Content-Type": {"image/png"}, "Cache-Control": {"public, max-age=3600"}, "Date": {time.Now().UTC().Format(http.TimeFormat)},
		}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	}), cache)
	h := o.PublicHandler(&iconHandler{proxy: p})
	q := url.Values{"url": {"https://origin.test/logo"}}
	for range 2 {
		if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 200 {
			t.Fatalf("icon response %d", w.Code)
		}
	}
	if got, _ := pipelineMetric(t, o, "cache_requests_total", "icons", "layer", "source", "hit"); got != 1 {
		t.Fatalf("fresh source cache hits=%v", got)
	}
	p.SignatureKeys = [][]byte{[]byte("required-key")}
	if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 403 {
		t.Fatal("warm cache bypassed signature validation")
	}
	if got, _ := pipelineMetric(t, o, "cache_requests_total", "icons", "layer", "source", "hit"); got != 1 {
		t.Fatal("unauthorized request reached cache observation")
	}
	if stageCount(t, o, "icons", "fetch", "success") != 1 {
		t.Fatal("cache hit performed or recorded a network fetch")
	}
	if stageCount(t, o, "icons", "write", "success") != 2 {
		t.Fatal("warm cache skipped output generation/write")
	}
}

// Keep errors.Is behavior explicit: metric classification must accept wrapped
// sentinel errors rather than requiring an encoder to return an exact value.
func TestPipelineStageClassifiesWrappedLimit(t *testing.T) {
	o := testObservability(t, io.Discard, "info")
	finish := imageproxy.StartStage(imageproxy.WithObserver(context.Background(), o.iconsPipeline), "encode")
	finish(errors.Join(errors.New("ENCODER_SECRET"), imageproxy.ErrResponseTooLarge))
	if stageCount(t, o, "icons", "encode", "limit") != 1 {
		t.Fatal("wrapped resource limit was not classified")
	}
}
