package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
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

// Obtain opaque metadata through the production source pipeline, including its
// sharing policy, instead of manufacturing privileged metadata in tests.
func artifactTestSource(t testing.TB, policy string) (*url.URL, []byte, imageproxy.SourceLimits, imageproxy.SourceMetadata) {
	t.Helper()
	var encoded bytes.Buffer
	// A small deterministic source; the generated product remains a real PNG.
	data := iconSourcePNGForArtifact(t)
	encoded.Write(data)
	source, _ := url.Parse("https://origin.test/logo.png?token=secret")
	p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{
			"Content-Type": {"image/png"}, "Cache-Control": {policy},
			"Date": {time.Now().UTC().Format(http.TimeFormat)},
		}, Body: io.NopCloser(bytes.NewReader(encoded.Bytes())), ContentLength: int64(encoded.Len()), Request: r}, nil
	}), nil)
	var metadata imageproxy.SourceMetadata
	var limits imageproxy.SourceLimits
	if err := p.WithSourceMetadata(context.Background(), source, "", func(_ context.Context, _ []byte, l imageproxy.SourceLimits, m imageproxy.SourceMetadata) error {
		limits, metadata = l, m
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return source, data, limits, metadata
}

func iconSourcePNGForArtifact(t testing.TB) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := range 2 {
		for x := range 4 {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type artifactTestStore struct {
	mu                    sync.Mutex
	entries               map[string]artifactEntry
	getErr, putErr        error
	puts, deletes, closes int
}

func (s *artifactTestStore) Get(ctx context.Context, key string) (artifactEntry, error) {
	if err := ctx.Err(); err != nil {
		return artifactEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return artifactEntry{}, s.getErr
	}
	entry, ok := s.entries[key]
	if !ok {
		return artifactEntry{}, errArtifactMiss
	}
	return entry, nil
}
func (s *artifactTestStore) Put(ctx context.Context, e artifactEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.putErr != nil {
		return s.putErr
	}
	if s.entries == nil {
		s.entries = map[string]artifactEntry{}
	}
	e.Body = bytes.Clone(e.Body)
	s.entries[e.Key] = e
	return nil
}
func (s *artifactTestStore) Delete(_ context.Context, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, k)
	s.deletes++
	return nil
}
func (s *artifactTestStore) Close() error { s.mu.Lock(); defer s.mu.Unlock(); s.closes++; return nil }

func TestArtifactMemoryOwnershipExpiryBoundsAndClose(t *testing.T) {
	store, err := newMemoryArtifactStore(4096, 512, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	body := []byte("owned artifact snapshot")
	entry := newArtifactEntry(key, "favicon-32x32.png", body, time.Now().Add(time.Minute))
	if err := store.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	body[0] = 'X'
	got, err := store.Get(context.Background(), key)
	if err != nil || string(got.Body) != "owned artifact snapshot" {
		t.Fatalf("get = %q, %v", got.Body, err)
	}
	got.Body[0] = 'Y'
	got, err = store.Get(context.Background(), key)
	if err != nil || got.Body[0] != 'o' {
		t.Fatal("reader mutated stored snapshot")
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, errArtifactMiss) {
		t.Fatalf("delete = %v", err)
	}
	if err := store.Put(context.Background(), newArtifactEntry(key, "favicon.ico", make([]byte, 513), time.Now().Add(time.Minute))); !errors.Is(err, errArtifactTooLarge) {
		t.Fatalf("oversize = %v", err)
	}
	// No sweeper runs; a read must enforce the encoded logical deadline.
	entry = newArtifactEntry(key, "favicon.ico", []byte("expires"), time.Now().Add(10*time.Millisecond))
	if err := store.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := store.Get(context.Background(), key); !errors.Is(err, errArtifactMiss) {
		t.Fatalf("expired = %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := store.Put(context.Background(), newArtifactEntry(strings.Repeat("0", 62)+string("0123456789abcdef"[i%16])+string("0123456789abcdef"[i/16]), "favicon.ico", make([]byte, 512), time.Now().Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if stats := store.cache.Stats(); stats.TotalSize > 4096 {
		t.Fatalf("capacity escaped: %+v", stats)
	}
	// An operation waiting for temporary-copy ownership is cancelable.
	store.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Get(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	<-store.gate
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, errArtifactStoreClosed) {
		t.Fatalf("closed = %v", err)
	}
}

func TestArtifactServiceWarmAndPolicyBypass(t *testing.T) {
	source, data, limits, metadata := artifactTestSource(t, "public, max-age=3600")
	for _, filename := range []string{"favicon-32x32.png", "icons.zip"} {
		t.Run(filename, func(t *testing.T) {
			store := &artifactTestStore{}
			var outcomes []string
			service := newArtifactService(store, "memory", time.Minute, 1<<20, func(_, result string) { outcomes = append(outcomes, result) })
			calls := 0
			generate := func(ctx context.Context) ([]byte, error) {
				calls++
				return generateIcon(ctx, data, limits, filename, "Name", "Short", nil)
			}
			first, err := service.GetOrGenerate(context.Background(), source, metadata, filename, "Name", "Short", limits, generate)
			if err != nil {
				t.Fatal(err)
			}
			second, err := service.GetOrGenerate(context.Background(), source, metadata, filename, "Name", "Short", limits, generate)
			if err != nil || !bytes.Equal(first, second) || calls != 1 || strings.Join(outcomes, ",") != "miss,hit" {
				t.Fatalf("warm calls=%d outcomes=%v err=%v", calls, outcomes, err)
			}
		})
	}
	for _, policy := range []string{"no-store", "private, max-age=3600", "max-age=3600", "public, no-cache"} {
		t.Run(policy, func(t *testing.T) {
			u, _, l, m := artifactTestSource(t, policy)
			store := &artifactTestStore{}
			service := newArtifactService(store, "memory", time.Minute, 1<<20, nil)
			calls := 0
			for range 2 {
				if _, err := service.GetOrGenerate(context.Background(), u, m, "favicon-32x32.png", "", "", l, func(context.Context) ([]byte, error) { calls++; return []byte("not stored"), nil }); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 || store.puts != 0 {
				t.Fatalf("private source cached: %d/%d", calls, store.puts)
			}
		})
	}
	store := &artifactTestStore{}
	service := newArtifactService(store, "memory", time.Minute, 1<<20, nil)
	for range 2 {
		if _, err := service.GetOrGenerate(context.Background(), source, metadata, "site.webmanifest", "Name", "Short", limits, func(context.Context) ([]byte, error) { return []byte("manifest"), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if store.puts != 0 {
		t.Fatal("standalone manifest was cached")
	}
}

func TestArtifactServiceConcurrentFillAndCanceledLeader(t *testing.T) {
	source, data, limits, metadata := artifactTestSource(t, "public, max-age=3600")
	store := &artifactTestStore{}
	service := newArtifactService(store, "memory", time.Minute, 1<<20, nil)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	generate := func(ctx context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return generateIcon(ctx, data, limits, "favicon-32x32.png", "", "", nil)
	}
	leaderCtx, cancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() {
		_, err := service.GetOrGenerate(leaderCtx, source, metadata, "favicon-32x32.png", "", "", limits, generate)
		leader <- err
	}()
	<-entered
	var wg sync.WaitGroup
	errorsOut := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := service.GetOrGenerate(context.Background(), source, metadata, "favicon-32x32.png", "", "", limits, generate)
			errorsOut <- err
		})
	}
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader = %v", err)
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || store.puts != 1 {
		t.Fatalf("fills=%d puts=%d", calls.Load(), store.puts)
	}
	// A waiting caller has its own deadline and must not cancel the leader.
	store = &artifactTestStore{}
	service = newArtifactService(store, "memory", time.Minute, 1<<20, nil)
	entered, release = make(chan struct{}), make(chan struct{})
	calls.Store(0)
	go func() {
		_, err := service.GetOrGenerate(context.Background(), source, metadata, "favicon-32x32.png", "", "", limits, generate)
		leader <- err
	}()
	<-entered
	ctx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelWait()
	if _, err := service.GetOrGenerate(ctx, source, metadata, "favicon-32x32.png", "", "", limits, generate); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter = %v", err)
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
}

func TestArtifactServiceRejectsPollutionBudgetsAndBackendFailure(t *testing.T) {
	source, data, limits, metadata := artifactTestSource(t, "public, max-age=3600")
	key, _ := artifactKey(source, metadata, "favicon-32x32.png", "", "", limits)
	body, err := generateIcon(context.Background(), data, limits, "favicon-32x32.png", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"expired", "future-source", "invalid", "backend", "oversize-wire", "put-error", "generation-error"} {
		t.Run(kind, func(t *testing.T) {
			store := &artifactTestStore{entries: map[string]artifactEntry{key: newArtifactEntry(key, "favicon-32x32.png", body, time.Now().Add(time.Minute))}}
			switch kind {
			case "expired":
				e := store.entries[key]
				e.ExpiresAt = time.Now().Add(-time.Second)
				store.entries[key] = e
			case "future-source":
				e := store.entries[key]
				e.ExpiresAt = metadata.FreshUntil().Add(time.Second)
				store.entries[key] = e
			case "invalid":
				e := store.entries[key]
				e.Body = []byte("corrupt")
				store.entries[key] = e
			case "backend":
				store.getErr = errors.New("internal unavailable")
			case "oversize-wire":
				store.getErr = errArtifactTooLarge
			case "put-error":
				store.entries = nil
				store.putErr = errors.New("internal unavailable")
			case "generation-error":
				store.entries = nil
			}
			service := newArtifactService(store, "memory", time.Minute, 1<<20, nil)
			calls := 0
			_, err := service.GetOrGenerate(context.Background(), source, metadata, "favicon-32x32.png", "", "", limits, func(context.Context) ([]byte, error) {
				calls++
				if kind == "generation-error" {
					return nil, errors.New("generation failed")
				}
				return body, nil
			})
			if calls != 1 || (err != nil) != (kind == "generation-error") {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if kind == "generation-error" && store.puts != 0 {
				t.Fatal("cached error")
			}
		})
	}
	// An attacker-controlled backend cannot turn a current budget violation
	// into a miss that silently regenerates a response over that budget.
	small := limits
	small.MaxResponseBytes = int64(len(body) - 1)
	smallKey, _ := artifactKey(source, metadata, "favicon-32x32.png", "", "", small)
	store := &artifactTestStore{entries: map[string]artifactEntry{smallKey: newArtifactEntry(smallKey, "favicon-32x32.png", body, time.Now().Add(time.Minute))}}
	service := newArtifactService(store, "memory", time.Minute, 1<<20, nil)
	if _, err := service.GetOrGenerate(context.Background(), source, metadata, "favicon-32x32.png", "", "", small, func(context.Context) ([]byte, error) { t.Fatal("regenerated hard budget rejection"); return nil, nil }); !errors.Is(err, imageproxy.ErrResponseTooLarge) {
		t.Fatalf("budget = %v", err)
	}
}

func TestArtifactHandlerWarmStillAuthorizesAndObserves(t *testing.T) {
	data := iconSourcePNG(t)
	var upstream atomic.Int32
	p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}, "Cache-Control": {"public, max-age=3600"}, "Date": {time.Now().UTC().Format(http.TimeFormat)}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	}), nil)
	obs, err := newObservability(io.Discard, "info")
	if err != nil {
		t.Fatal(err)
	}
	store, err := newMemoryArtifactStore(1<<20, 1<<16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := newArtifactService(store, "memory", time.Minute, 1<<16, obs.artifactCache.Observe)
	defer service.Close()
	h := obs.PublicHandler(&iconHandler{proxy: p, artifacts: service})
	u := "https://origin.test/logo.png?token=secret"
	q := url.Values{"url": {u}}
	for range 2 {
		if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
	if upstream.Load() != 2 {
		t.Fatal("artifact lookup bypassed the source pipeline")
	}
	metrics := httptest.NewRecorder()
	obs.AdminHandler().ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), `favicon_generator_artifact_cache_requests_total{backend="memory",result="hit"} 1`) || strings.Contains(metrics.Body.String(), "secret") {
		t.Fatal("missing bounded artifact metric")
	}
	// The warm request still decodes the source but must not encode again.
	if !strings.Contains(metrics.Body.String(), `favicon_generator_pipeline_stage_duration_seconds_count{pipeline="icons",result="success",stage="encode"} 2`) {
		t.Fatalf("warm encoded again: %s", metrics.Body.String())
	}
	p.SignatureKeys = [][]byte{[]byte("server-key")}
	if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 403 {
		t.Fatalf("warm unsigned = %d", w.Code)
	}
	mac := hmac.New(sha256.New, []byte("server-key"))
	mac.Write([]byte(u))
	q.Set("s", base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 200 {
		t.Fatalf("valid signature = %d", w.Code)
	}
	p.AllowHosts = []string{"other.test"}
	if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 403 {
		t.Fatalf("warm allowlist = %d", w.Code)
	}
	if upstream.Load() != 3 {
		t.Fatalf("forbidden request reached source: %d", upstream.Load())
	}
}

func TestArtifactCLIResolutionAndBoundaries(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	cfg, err := parseConfig(nil, none)
	if err != nil || cfg.ArtifactCache != "off" || cfg.ArtifactCacheBytes != 64<<20 || cfg.ArtifactCacheTTL != 5*time.Minute {
		t.Fatalf("defaults=%+v %v", cfg, err)
	}
	for _, args := range [][]string{{"-artifactCache=redis"}, {"-artifactCache="}, {"-artifactCacheBytes=0"}, {"-artifactCacheEntryBytes=-1"}, {"-artifactCacheTTL=0"}, {"-artifactCacheTTL=24h1ns"}, {"-artifactCache=memory", "-artifactCacheBytes=2048"}} {
		if _, err := parseConfig(args, none); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	cfg, err = parseConfig([]string{"-artifactCache=off", "-artifactCacheTTL=24h", "-maxResponseBytes=100"}, func(key string) (string, bool) {
		if key == "IMAGEPROXY_ARTIFACTCACHE" {
			t.Fatal("read losing ENV")
		}
		return "", false
	})
	if err != nil || cfg.artifactEntryLimit() != 100 {
		t.Fatalf("strict resolution=%+v %v", cfg, err)
	}
	if _, err := parseConfig([]string{"-artifactCache=memory", "-maxResponseBytes=9223372036854775806"}, none); err != nil {
		t.Fatalf("default entry rejected huge legacy response limit: %v", err)
	}
	if _, err := parseConfig([]string{"-version"}, func(string) (string, bool) { t.Fatal("version read unrelated environment"); return "", false }); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(nil, func(key string) (string, bool) {
		if key == "IMAGEPROXY_ARTIFACTCACHE" {
			return "secret-invalid-value", true
		}
		return "", false
	}); err == nil || strings.Contains(err.Error(), "secret-invalid-value") {
		t.Fatalf("error leaked env: %v", err)
	}
}

func TestArtifactMetricsPrivateAndFixedLabels(t *testing.T) {
	for range 2 {
		obs, err := newObservability(io.Discard, "info")
		if err != nil {
			t.Fatal(err)
		}
		obs.artifactCache.Observe("memory", "hit")
		obs.artifactCache.Observe("https://secret", "hit")
		obs.artifactCache.Observe("memory", "secret")
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				for range 10 {
					obs.artifactCache.Observe("memory", "hit")
				}
			})
		}
		wg.Wait()
		w := httptest.NewRecorder()
		obs.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if !strings.Contains(w.Body.String(), `favicon_generator_artifact_cache_requests_total{backend="memory",result="hit"} 101`) || strings.Contains(w.Body.String(), "secret") {
			t.Fatal("metrics conflict or unbounded labels")
		}
	}
}
