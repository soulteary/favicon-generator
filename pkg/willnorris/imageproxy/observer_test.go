package imageproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregjones/httpcache"
)

type stageEvent struct{ stage, result string }
type cacheEvent struct{ layer, result string }
type recordingObserver struct {
	mu     sync.Mutex
	stages []stageEvent
	caches []cacheEvent
}

func (o *recordingObserver) ObserveStage(stage, result string, elapsed time.Duration) {
	if elapsed < 0 {
		panic("negative stage duration")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stages = append(o.stages, stageEvent{stage, result})
}
func (o *recordingObserver) ObserveCache(layer, result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.caches = append(o.caches, cacheEvent{layer, result})
}
func (o *recordingObserver) stageCount(stage, result string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, event := range o.stages {
		if event.stage == stage && (result == "" || event.result == result) {
			n++
		}
	}
	return n
}
func (o *recordingObserver) cacheCount(layer, result string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, event := range o.caches {
		if event == (cacheEvent{layer, result}) {
			n++
		}
	}
	return n
}

type observerTimeout struct{}

func (observerTimeout) Error() string { return "private network diagnostic" }
func (observerTimeout) Timeout() bool { return true }

func TestObserverLabelsAndResults(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{nil, "success"}, {errors.New("https://secret.test/token"), "error"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "timeout"}, {observerTimeout{}, "timeout"},
		{ErrImageTooLarge, "limit"}, {ErrTooManyFrames, "limit"}, {ErrResponseTooLarge, "limit"},
		{ErrInvalidDimensions, "error"},
	} {
		if got := Result(tt.err); got != tt.want {
			t.Errorf("Result(%v)=%s, want %s", tt.err, got, tt.want)
		}
	}
	o := new(recordingObserver)
	ctx, cancel := context.WithCancel(WithObserver(context.Background(), o))
	finish := StartStage(ctx, "decode")
	cancel()
	finish(nil)
	finish(errors.New("duplicate"))
	StartStage(ctx, "https://secret.test/token")(nil)
	observeCache(ctx, "secret URL", "hit")
	observeCache(ctx, "source", "unbounded remote text")
	if o.stageCount("decode", "canceled") != 1 || len(o.stages) != 1 || o.cacheCount("source", "error") != 1 || len(o.caches) != 1 {
		t.Fatalf("unexpected labels or duplicate callbacks: %+v %+v", o.stages, o.caches)
	}
	if allocs := testing.AllocsPerRun(50, func() { StartStage(context.Background(), "decode")(nil) }); allocs != 0 {
		t.Fatalf("observer-free stage allocated: %v", allocs)
	}
}

func TestObserverTransformOperationsAndFailures(t *testing.T) {
	data := limitTestPNG(t, 20, 20)
	for _, tt := range []struct {
		name, stage, result string
		data                []byte
		opt                 Options
		limits              SourceLimits
	}{
		{"success", "encode", "success", data, Options{Format: "png"}, SourceLimits{}},
		{"malformed", "decode", "error", []byte("broken image"), Options{Format: "png"}, SourceLimits{}},
		{"pixel preflight", "decode", "limit", data, Options{Format: "png"}, SourceLimits{MaxPixels: 399}},
		{"transform pixels", "transform", "limit", data, Options{Width: 100, Height: 100, ScaleUp: true}, SourceLimits{MaxPixels: 500}},
		{"encoded bytes", "encode", "limit", data, Options{Format: "bmp"}, SourceLimits{MaxResponseBytes: int64(len(data))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := new(recordingObserver)
			_, err := TransformWithBudget(WithObserver(context.Background(), o), tt.data, tt.opt, tt.limits)
			if tt.result == "success" && err != nil || tt.result != "success" && err == nil {
				t.Fatalf("unexpected transform error: %v", err)
			}
			if o.stageCount(tt.stage, tt.result) != 1 {
				t.Fatalf("missing phase result: %+v", o.stages)
			}
			if tt.result != "success" && tt.stage != "encode" && o.stageCount("encode", "") != 0 {
				t.Fatalf("failure invented an encode: %+v", o.stages)
			}
		})
	}
	o := new(recordingObserver)
	ctx, cancel := context.WithCancel(WithObserver(context.Background(), o))
	cancel()
	_, err := TransformWithBudget(ctx, data, Options{Format: "png"}, SourceLimits{})
	if !errors.Is(err, context.Canceled) || len(o.stages) != 0 {
		t.Fatalf("already canceled operation did work: %v %+v", err, o.stages)
	}
}

func TestObserverGIFSeparatesDecodeTransformEncode(t *testing.T) {
	o := new(recordingObserver)
	_, err := TransformWithLimits(WithObserver(context.Background(), o), limitTestGIF(t, 4, 4, 3), Options{Width: 2}, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Metadata plus DecodeAll are independent decode operations. The frame
	// loop is one transform, and EncodeAll is exactly one encode operation.
	if o.stageCount("decode", "success") != 2 || o.stageCount("transform", "success") != 1 || o.stageCount("encode", "success") != 1 {
		t.Fatalf("GIF phases duplicated or conflated: %+v", o.stages)
	}
}

func serveObserved(p *Proxy, o Observer, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	p.ServeHTTP(w, req.WithContext(WithObserver(req.Context(), o)))
	return w
}

func cacheableObserverResponse(req *http.Request, data []byte) *http.Response {
	resp := policyResponse(req, data, "image/png")
	resp.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	resp.Header.Set("Cache-Control", "public, max-age=3600")
	resp.Header.Set("X-From-Cache", "1") // untrusted origin must not forge hits.
	return resp
}

func TestObserverColdWarmCacheLayersIgnoreOriginMarker(t *testing.T) {
	data := policyPNG(t)
	var calls atomic.Int32
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return cacheableObserverResponse(req, data), nil
	}), httpcache.NewMemoryCache())
	for _, tt := range []struct {
		path, sourceResult, transformedResult string
		fetches                               int
	}{
		{"/1,png/https://origin.test/image", "miss", "miss", 1},
		{"/1,png/https://origin.test/image", "", "hit", 0},
		{"/2,png/https://origin.test/image", "hit", "miss", 0},
	} {
		o := new(recordingObserver)
		if w := serveObserved(p, o, tt.path); w.Code != http.StatusOK {
			t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
		}
		if o.stageCount("fetch", "") != tt.fetches || o.cacheCount("transformed", tt.transformedResult) != 1 || tt.sourceResult != "" && o.cacheCount("source", tt.sourceResult) != 1 {
			t.Errorf("wrong cache decision: stages=%+v cache=%+v", o.stages, o.caches)
		}
		if o.stageCount("write", "success") != 1 {
			t.Errorf("missing actual response write: %+v", o.stages)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("network calls=%d, want one", calls.Load())
	}
}

func TestObserverSourceRevalidationClosesAbandonedBodies(t *testing.T) {
	for _, second := range []string{"304", "stale 500", "stale transport error"} {
		t.Run(second, func(t *testing.T) {
			data := policyPNG(t)
			var discarded *countedBody
			calls := 0
			p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					resp := cacheableObserverResponse(req, data)
					resp.Header.Set("Cache-Control", "max-age=0, stale-if-error=3600")
					resp.Header.Set("Etag", `"logo"`)
					return resp, nil
				}
				if req.Header.Get("If-None-Match") != `"logo"` {
					t.Error("stale cache did not revalidate")
				}
				if second == "stale transport error" {
					return nil, errors.New("private origin failure")
				}
				discarded = &countedBody{Reader: bytes.NewReader(nil)}
				resp := cacheableObserverResponse(req, nil)
				resp.StatusCode, resp.Body = 304, discarded
				if second == "stale 500" {
					resp.StatusCode = 500
				}
				return resp, nil
			}), httpcache.NewMemoryCache())
			u := sourceURL(t, "https://origin.test/image")
			if err := p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); err != nil {
				t.Fatal(err)
			}
			o := new(recordingObserver)
			err := p.WithSource(WithObserver(context.Background(), o), u, "", func(context.Context, []byte, SourceLimits) error {
				if discarded != nil && !discarded.closed {
					t.Error("cache replacement leaked its actual upstream body")
				}
				if o.stageCount("fetch", "") != 1 {
					t.Error("fetch remained open during downstream work")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			fetchResult, cacheResult := "success", "revalidated"
			if second != "304" {
				fetchResult, cacheResult = "error", "error"
			}
			if o.stageCount("fetch", fetchResult) != 1 || o.cacheCount("source", cacheResult) != 1 {
				t.Fatalf("wrong replacement metrics: %+v %+v", o.stages, o.caches)
			}
		})
	}
}

func TestObserverOnlyIfCachedMissingOrMalformedNeverHits(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		cache := httpcache.NewMemoryCache()
		const target = "https://origin.test/image"
		if malformed {
			cache.Set(target, []byte("not an HTTP response"))
		}
		p := NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("only-if-cached reached network")
			return nil, nil
		}), cache)
		o := new(recordingObserver)
		req, _ := http.NewRequestWithContext(WithObserver(context.Background(), o), http.MethodGet, target, nil)
		req.Header.Set("Cache-Control", "only-if-cached")
		resp, err := p.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readAndCloseResponse(req.Context(), resp, 1024); err != nil {
			t.Fatal(err)
		}
		want := "miss"
		if malformed {
			want = "error"
		}
		if resp.StatusCode != 504 || o.cacheCount("source", want) != 1 || o.stageCount("fetch", "") != 0 {
			t.Fatalf("malformed=%t status=%d events=%+v %+v", malformed, resp.StatusCode, o.stages, o.caches)
		}
	}
}

func TestObserverFreshCacheHitPrecedesRequestNoStore(t *testing.T) {
	data := policyPNG(t)
	calls := 0
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return cacheableObserverResponse(req, data), nil
	}), httpcache.NewMemoryCache())
	const target = "https://origin.test/image"
	for _, noStore := range []bool{false, true} {
		o := new(recordingObserver)
		req, err := http.NewRequestWithContext(WithObserver(context.Background(), o), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if noStore {
			req.Header.Set("Cache-Control", "no-store")
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readAndCloseResponse(req.Context(), resp, DefaultMaxResponseBytes)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("response=%q, error=%v", got, err)
		}
		if noStore && (o.cacheCount("source", "hit") != 1 || o.cacheCount("source", "bypass") != 0 || o.stageCount("fetch", "") != 0) {
			t.Fatalf("actual fresh reuse was misclassified: stages=%+v cache=%+v", o.stages, o.caches)
		}
	}
	if calls != 1 {
		t.Fatalf("fresh request no-store contacted upstream: calls=%d, want one initial fetch", calls)
	}
}

func TestObserverCold304IsNotRevalidated(t *testing.T) {
	for _, cached304 := range []bool{false, true} {
		cache := httpcache.NewMemoryCache()
		u := sourceURL(t, "https://origin.test/image")
		if cached304 {
			req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
			resp := cacheableObserverResponse(req, nil)
			resp.StatusCode, resp.Status = 304, "304 Not Modified"
			resp.Header.Set("Cache-Control", "max-age=0")
			data, err := httputil.DumpResponse(resp, true)
			if err != nil {
				t.Fatal(err)
			}
			cache.Set(u.String(), data)
		}
		p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp := cacheableObserverResponse(req, nil)
			resp.StatusCode = 304
			return resp, nil
		}), cache)
		o := new(recordingObserver)
		err := p.WithSource(WithObserver(context.Background(), o), u, "", func(context.Context, []byte, SourceLimits) error { t.Error("304 became an image"); return nil })
		if !errors.Is(err, ErrSourceUnavailable) || o.cacheCount("source", "revalidated") != 0 || o.stageCount("fetch", "") != 1 {
			t.Fatalf("cached304=%t err=%v events=%+v %+v", cached304, err, o.stages, o.caches)
		}
	}
}

func TestObserverBypassCancellationAndPrelookupPolicy(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) { return cacheableObserverResponse(req, data), nil }), nil)
	o := new(recordingObserver)
	u := sourceURL(t, "https://origin.test/image")
	err := p.WithSource(WithObserver(context.Background(), o), u, "", func(context.Context, []byte, SourceLimits) error { return nil })
	if err != nil || o.cacheCount("source", "bypass") != 1 || o.stageCount("fetch", "success") != 1 {
		t.Fatalf("no-cache events: %v %+v %+v", err, o.stages, o.caches)
	}
	p = NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := cacheableObserverResponse(req, data)
		resp.Header.Set("Cache-Control", "no-store")
		return resp, nil
	}), httpcache.NewMemoryCache())
	o = new(recordingObserver)
	for range 2 {
		if err := p.WithSource(WithObserver(context.Background(), o), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if o.cacheCount("source", "bypass") != 2 || o.stageCount("fetch", "success") != 2 {
		t.Fatalf("no-store response looked cached: %+v %+v", o.stages, o.caches)
	}
	p = NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, context.Canceled }), nil)
	o = new(recordingObserver)
	err = p.WithSource(WithObserver(context.Background(), o), u, "", func(context.Context, []byte, SourceLimits) error { return nil })
	if !errors.Is(err, context.Canceled) || o.stageCount("fetch", "canceled") != 1 || o.cacheCount("source", "error") != 1 {
		t.Fatalf("canceled fetch events: %v %+v %+v", err, o.stages, o.caches)
	}
	for _, restricted := range []string{"allowlist", "signature"} {
		cache := &redirectCache{Cache: httpcache.NewMemoryCache()}
		p = NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("forbidden request reached transport")
			return nil, nil
		}), cache)
		if restricted == "allowlist" {
			p.AllowHosts = []string{"other.test"}
		} else {
			p.SignatureKeys = [][]byte{[]byte("key")}
		}
		o = new(recordingObserver)
		err = p.WithSource(WithObserver(context.Background(), o), u, "", func(context.Context, []byte, SourceLimits) error { return nil })
		if !errors.Is(err, ErrSourceForbidden) || cache.reads != 0 || len(o.stages) != 0 || len(o.caches) != 0 {
			t.Fatalf("policy no longer precedes metrics/cache: %v %+v %+v", err, o.stages, o.caches)
		}
	}
}

func TestObserverFetchByteLimitsIncludeReadAndClose(t *testing.T) {
	for _, length := range []int64{4096, -1} {
		body := &countedBody{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 4096))}
		p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp := policyResponse(req, nil, "image/png")
			resp.Body, resp.ContentLength = body, length
			return resp, nil
		}), nil)
		p.MaxResponseBytes = 8
		o := new(recordingObserver)
		err := p.WithSource(WithObserver(context.Background(), o), sourceURL(t, "https://origin.test/image"), "", func(context.Context, []byte, SourceLimits) error {
			t.Error("oversized response entered callback")
			return nil
		})
		if !errors.Is(err, ErrResponseTooLarge) || !body.closed || o.stageCount("fetch", "limit") != 1 || o.stageCount("decode", "") != 0 {
			t.Fatalf("length=%d err=%v closed=%t stages=%+v", length, err, body.closed, o.stages)
		}
		if length > 0 && body.read != 0 || length < 0 && body.read > 9 {
			t.Fatalf("body read beyond budget: length=%d read=%d", length, body.read)
		}
	}
}

type observerWriteFailure struct{ *httptest.ResponseRecorder }

func (w observerWriteFailure) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestObserverLegacyShortWriteAndConcurrentRequests(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) { return cacheableObserverResponse(req, data), nil }), httpcache.NewMemoryCache())
	p.MaxConcurrentRequests = 32
	o := new(recordingObserver)
	req := httptest.NewRequest(http.MethodGet, "/1,png/https://origin.test/image", nil)
	p.ServeHTTP(observerWriteFailure{httptest.NewRecorder()}, req.WithContext(WithObserver(req.Context(), o)))
	if o.stageCount("write", "error") != 1 {
		t.Fatalf("short write not observed: %+v", o.stages)
	}
	var workers sync.WaitGroup
	for range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			serveObserved(p, o, "/1,png/https://origin.test/image")
		}()
	}
	workers.Wait()
	if o.stageCount("write", "success") != 24 || o.cacheCount("transformed", "hit") != 24 {
		t.Fatalf("concurrent events dropped: %+v %+v", o.stages, o.caches)
	}
}

var _ io.ReadCloser = (*observedBody)(nil)
