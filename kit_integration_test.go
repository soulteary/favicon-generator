package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

// Gates wait on caller cancellation rather than creating background workers.
// They wrap the real bounded memory backend, not a replacement cache model.
type integrationArtifactStore struct {
	inner                               *memoryArtifactStore
	gets, puts, deletes, active, closes atomic.Int64
	mu                                  sync.RWMutex
	getHook, putHook                    func(context.Context) error
	getEvents                           chan struct{}
}

func (s *integrationArtifactStore) hooks(get, put func(context.Context) error) {
	s.mu.Lock()
	s.getHook, s.putHook = get, put
	s.mu.Unlock()
}

func (s *integrationArtifactStore) Get(ctx context.Context, key string) (artifactEntry, error) {
	s.gets.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)
	s.mu.RLock()
	hook := s.getHook
	s.mu.RUnlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return artifactEntry{}, err
		}
	}
	e, err := s.inner.Get(ctx, key)
	select {
	case s.getEvents <- struct{}{}:
	default:
	}
	return e, err
}

func (s *integrationArtifactStore) Put(ctx context.Context, entry artifactEntry) error {
	s.puts.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)
	s.mu.RLock()
	hook := s.putHook
	s.mu.RUnlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	return s.inner.Put(ctx, entry)
}

func (s *integrationArtifactStore) Delete(ctx context.Context, key string) error {
	s.deletes.Add(1)
	return s.inner.Delete(ctx, key)
}
func (s *integrationArtifactStore) Close() error { s.closes.Add(1); return s.inner.Close() }

type kitIntegrationFixture struct {
	proxy        *imageproxy.Proxy
	service      *artifactService
	store        *integrationArtifactStore
	obs          *observability
	handler      http.Handler
	data         []byte
	query        url.Values
	upstream     atomic.Int64
	mu           sync.RWMutex
	policy, date string
}

func newKitIntegrationFixture(t *testing.T, slots int) *kitIntegrationFixture {
	t.Helper()
	f := &kitIntegrationFixture{data: iconSourcePNGForArtifact(t), policy: "public, max-age=3600", date: time.Now().UTC().Format(http.TimeFormat)}
	f.obs = testObservability(t, io.Discard, "info")
	store, err := newMemoryArtifactStore(2<<20, 1<<20, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.store = &integrationArtifactStore{inner: store, getEvents: make(chan struct{}, 32)}
	f.service = newArtifactService(f.store, "memory", time.Minute, 1<<20, f.obs.artifactCache.Observe)
	t.Cleanup(func() {
		if err := f.service.Close(); err != nil {
			t.Error(err)
		}
	})
	f.proxy = imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		f.upstream.Add(1)
		f.mu.RLock()
		policy, date := f.policy, f.date
		f.mu.RUnlock()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}, "Cache-Control": {policy}, "Date": {date}}, Body: io.NopCloser(bytes.NewReader(f.data)), ContentLength: int64(len(f.data)), Request: r}, nil
	}), nil) // Every request sees the current origin retention policy.
	f.proxy.MaxConcurrentRequests = slots
	f.proxy.Timeout = 5 * time.Second
	f.proxy.AllowHosts = []string{"origin.test"}
	f.proxy.SignatureKeys = [][]byte{[]byte("INTEGRATION_KEY_SECRET")}
	source := "https://origin.test/logo.png?token=SOURCE_SECRET"
	mac := hmac.New(sha256.New, f.proxy.SignatureKeys[0])
	_, _ = mac.Write([]byte(source))
	f.query = url.Values{"url": {source}, "s": {base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}}
	f.handler = f.obs.PublicHandler(&iconHandler{proxy: f.proxy, artifacts: f.service})
	return f
}

func (f *kitIntegrationFixture) request(ctx context.Context, writer http.ResponseWriter) {
	r := httptest.NewRequest("GET", "/icons/favicon-32x32.png?"+f.query.Encode(), nil).WithContext(ctx)
	f.handler.ServeHTTP(writer, r)
}

func (f *kitIntegrationFixture) prime(t *testing.T) {
	t.Helper()
	for range 2 {
		w := httptest.NewRecorder()
		f.request(context.Background(), w)
		if w.Code != 200 {
			t.Fatalf("prime status=%d: %s", w.Code, w.Body)
		}
	}
	if got := integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "memory", "result": "hit"}); got != 1 {
		t.Fatalf("prime hits=%v", got)
	}
}

func integrationAwait[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("owned request did not complete its expected lifecycle transition")
	}
	var zero T
	return zero
}

func integrationCounter(t *testing.T, o *observability, name string, want map[string]string) float64 {
	t.Helper()
	families, err := o.metrics.Registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "favicon_generator_"+name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if len(metric.Label) != len(want) {
				continue
			}
			match := true
			for _, label := range metric.GetLabel() {
				match = match && want[label.GetName()] == label.GetValue()
			}
			if match {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestKitIntegrationWarmArtifactKeepsCurrentAuthorizationAndBudgets(t *testing.T) {
	for _, scenario := range []string{"signature", "allowlist", "source-pixels", "source-bytes", "product-pixels", "already-canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newKitIntegrationFixture(t, 2)
			f.prime(t)
			gets := f.store.gets.Load()
			fetches := f.upstream.Load()
			encoded := stageCount(t, f.obs, "icons", "encode", "success")
			want := 413
			ctx := context.Background()
			switch scenario {
			case "signature":
				f.query.Set("s", "SIGNATURE_SECRET")
				want = 403
			case "allowlist":
				f.proxy.AllowHosts = []string{"other.test"}
				want = 403
			case "source-pixels":
				f.proxy.MaxPixels = 7
			case "source-bytes":
				f.proxy.MaxResponseBytes = int64(len(f.data) - 1)
			case "product-pixels":
				f.proxy.MaxPixels = 100
			case "already-canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = 504
			}
			w := httptest.NewRecorder()
			f.request(ctx, w)
			if w.Code != want {
				t.Fatalf("warm rejection=%d, want %d", w.Code, want)
			}
			wantFetches := int64(1)
			if scenario == "signature" || scenario == "allowlist" || scenario == "already-canceled" {
				wantFetches = 0
			}
			if f.upstream.Load()-fetches != wantFetches {
				t.Fatal("warm rejection entered or skipped the origin contrary to its authorization/preflight boundary")
			}
			if scenario != "product-pixels" && f.store.gets.Load() != gets {
				t.Fatal("rejected request entered artifact lookup")
			}
			if stageCount(t, f.obs, "icons", "encode", "success") != encoded {
				t.Fatal("rejected request encoded output")
			}
			if integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "memory", "result": "hit"}) != 1 {
				t.Fatal("rejected request reused warm output")
			}
			if strings.Contains(w.Body.String(), "SECRET") {
				t.Fatal("error response exposed input or secret")
			}
		})
	}
}

func TestKitIntegrationCurrentSourceRetentionRevokesOrShortensWarmEntry(t *testing.T) {
	for _, policy := range []string{"public, no-store, max-age=3600", "private, max-age=3600", "public, max-age=30"} {
		t.Run(policy, func(t *testing.T) {
			f := newKitIntegrationFixture(t, 2)
			f.prime(t)
			gets, puts := f.store.gets.Load(), f.store.puts.Load()
			encoded := stageCount(t, f.obs, "icons", "encode", "success")
			f.mu.Lock()
			f.policy = policy
			f.mu.Unlock()
			w := httptest.NewRecorder()
			f.request(context.Background(), w)
			if w.Code != 200 || stageCount(t, f.obs, "icons", "encode", "success") <= encoded {
				t.Fatal("changed source policy reused warm output")
			}
			if integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "memory", "result": "hit"}) != 1 {
				t.Fatal("policy change recorded an artifact hit")
			}
			if policy == "public, max-age=30" {
				if f.store.deletes.Load() == 0 || f.store.puts.Load() != puts+1 {
					t.Fatal("shorter source deadline did not replace long-lived entry")
				}
				var metadata imageproxy.SourceMetadata
				u, _ := url.Parse(f.query.Get("url"))
				if err := f.proxy.WithSourceMetadata(context.Background(), u, f.query.Get("s"), func(_ context.Context, _ []byte, _ imageproxy.SourceLimits, m imageproxy.SourceMetadata) error {
					metadata = m
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				limits := imageproxy.SourceLimits{MaxResponseBytes: imageproxy.DefaultMaxResponseBytes, MaxPixels: imageproxy.DefaultMaxPixels, MaxFrames: imageproxy.DefaultMaxFrames}
				key, ok := artifactKey(u, metadata, "favicon-32x32.png", "App", "App", limits)
				entry, err := f.store.inner.Get(context.Background(), key)
				if !ok || err != nil || entry.ExpiresAt.After(metadata.FreshUntil()) {
					t.Fatal("replacement entry exceeds the current source deadline", err)
				}
			} else if f.store.gets.Load() != gets || f.store.puts.Load() != puts {
				t.Fatal("nonshareable current source consulted or published artifact cache")
			}
		})
	}
}

func assertIntegrationBusy(t *testing.T, f *kitIntegrationFixture) {
	t.Helper()
	for _, pipeline := range []string{"image", "icons"} {
		before := integrationCounter(t, f.obs, "pipeline_admission_rejected_total", map[string]string{"pipeline": pipeline, "reason": "busy"})
		w := httptest.NewRecorder()
		if pipeline == "image" {
			f.handler.ServeHTTP(w, httptest.NewRequest("GET", "/32,png/https://origin.test/logo.png", nil))
		} else {
			f.request(context.Background(), w)
		}
		if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("%s did not reject occupied shared admission", pipeline)
		}
		if after := integrationCounter(t, f.obs, "pipeline_admission_rejected_total", map[string]string{"pipeline": pipeline, "reason": "busy"}); after != before+1 {
			t.Fatalf("%s admission counter=%v, want %v", pipeline, after, before+1)
		}
	}
}

func assertIntegrationDrained(t *testing.T, f *kitIntegrationFixture) {
	t.Helper()
	f.service.mu.Lock()
	fills := len(f.service.fills)
	f.service.mu.Unlock()
	if fills != 0 || f.store.active.Load() != 0 || len(f.store.inner.gate) != 0 {
		t.Fatal("completed requests retained a fill, cache operation or owned snapshot gate")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Close(); err != nil || f.store.closes.Load() != 1 {
		t.Fatal("cache lifetime did not close once after drain", err)
	}
	if _, err := f.store.inner.Get(context.Background(), strings.Repeat("a", 64)); !errors.Is(err, errArtifactStoreClosed) {
		t.Fatal("closed backend accepts reads", err)
	}
}

func TestKitIntegrationCacheReadAndFillWaitRetainAdmissionUntilCancellation(t *testing.T) {
	t.Run("cache-read", func(t *testing.T) {
		f := newKitIntegrationFixture(t, 1)
		f.prime(t)
		entered := make(chan struct{})
		var once sync.Once
		// Block the real backend's snapshot ownership gate, so cancellation
		// exercises its context-aware lock rather than just a fake Get.
		f.store.inner.gate <- struct{}{}
		var release sync.Once
		defer release.Do(func() { <-f.store.inner.gate })
		f.store.hooks(func(context.Context) error { once.Do(func() { close(entered) }); return nil }, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w, done := httptest.NewRecorder(), make(chan struct{})
		go func() { f.request(ctx, w); close(done) }()
		integrationAwait(t, entered)
		assertIntegrationBusy(t, f)
		cancel()
		integrationAwait(t, done)
		if w.Code != 504 {
			t.Fatalf("canceled cache read=%d", w.Code)
		}
		release.Do(func() { <-f.store.inner.gate })
		assertIntegrationDrained(t, f)
	})
	t.Run("fill-wait", func(t *testing.T) {
		f := newKitIntegrationFixture(t, 2)
		entered := make(chan struct{})
		var once sync.Once
		f.store.hooks(nil, func(ctx context.Context) error { once.Do(func() { close(entered) }); <-ctx.Done(); return ctx.Err() })
		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		defer cancelLeader()
		leader, leaderDone := httptest.NewRecorder(), make(chan struct{})
		go func() { f.request(leaderCtx, leader); close(leaderDone) }()
		integrationAwait(t, entered)
		leaderEncodes := stageCount(t, f.obs, "icons", "encode", "success")
		if leaderEncodes == 0 {
			t.Fatal("leader did not generate a real product before publication")
		}
		waiterCtx, cancelWaiter := context.WithCancel(context.Background())
		defer cancelWaiter()
		waiter, waiterDone := httptest.NewRecorder(), make(chan struct{})
		go func() { f.request(waiterCtx, waiter); close(waiterDone) }()
		// Two leader lookups plus the waiter's miss prove both requests were
		// admitted. The encode/put checks below establish that the waiter then
		// waited for the leader rather than independently generating output.
		for range 3 {
			integrationAwait(t, f.store.getEvents)
		}
		assertIntegrationBusy(t, f)
		cancelWaiter()
		integrationAwait(t, waiterDone)
		if waiter.Code != 504 {
			t.Fatalf("canceled fill waiter=%d", waiter.Code)
		}
		if stageCount(t, f.obs, "icons", "encode", "success") != leaderEncodes || f.store.puts.Load() != 1 {
			t.Fatal("fill waiter generated or published a duplicate product")
		}
		f.service.mu.Lock()
		fills := len(f.service.fills)
		f.service.mu.Unlock()
		if fills != 1 {
			t.Fatal("waiter cancellation disturbed the leader fill")
		}
		cancelLeader()
		integrationAwait(t, leaderDone)
		if leader.Code != 504 {
			t.Fatalf("canceled fill leader=%d", leader.Code)
		}
		f.store.hooks(nil, nil)
		w := httptest.NewRecorder()
		f.request(context.Background(), w)
		if w.Code != 200 {
			t.Fatal("canceled fill left stale coordination or admission behind")
		}
		if stageCount(t, f.obs, "icons", "encode", "success") != 2*leaderEncodes || f.store.puts.Load() != 2 {
			t.Fatal("fill recovery did not perform exactly one replacement generation")
		}
		assertIntegrationDrained(t, f)
	})
}

func TestKitIntegrationWarmResponseWriteRetainsAdmission(t *testing.T) {
	f := newKitIntegrationFixture(t, 1)
	f.prime(t)
	encoded := stageCount(t, f.obs, "icons", "encode", "success")
	w := &blockedIconResponse{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(w.release) })
	done := make(chan struct{})
	go func() { f.request(context.Background(), w); close(done) }()
	integrationAwait(t, w.started)
	assertIntegrationBusy(t, f)
	once.Do(func() { close(w.release) })
	integrationAwait(t, done)
	if w.Code != 200 || stageCount(t, f.obs, "icons", "encode", "success") != encoded {
		t.Fatal("warm write regenerated or failed output")
	}
	assertIntegrationDrained(t, f)
}

func TestKitIntegrationAdmissionCounterUsesPrivateFixedLabels(t *testing.T) {
	o, other := testObservability(t, io.Discard, "info"), testObservability(t, io.Discard, "info")
	o.imagePipeline.ObserveAdmissionRejected("busy")
	o.iconsPipeline.ObserveAdmissionRejected("busy")
	o.iconsPipeline.ObserveAdmissionRejected("SOURCE_SIGNATURE_SECRET")
	for _, pipeline := range []string{"image", "icons"} {
		labels := map[string]string{"pipeline": pipeline, "reason": "busy"}
		if integrationCounter(t, o, "pipeline_admission_rejected_total", labels) != 1 || integrationCounter(t, other, "pipeline_admission_rejected_total", labels) != 0 {
			t.Fatal("counter leaked across registries or labels")
		}
	}
	before := pipelineLines(observeRequest(o.AdminHandler(), "GET", "/metrics").Body.String())
	for _, path := range []string{"/", "/health-check", "/livez", "/readyz", "/assets/app.js"} {
		observeRequest(o.PublicHandler(http.NotFoundHandler()), "GET", path)
	}
	after := pipelineLines(observeRequest(o.AdminHandler(), "GET", "/metrics").Body.String())
	if before != after || strings.Contains(after, "SECRET") {
		t.Fatal("management/UI/scraping created a rejection or exposed unbounded labels")
	}
}
