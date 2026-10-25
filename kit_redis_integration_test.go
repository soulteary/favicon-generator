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
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

type kitRedisInstance struct {
	obs      *observability
	service  *artifactService
	proxy    *imageproxy.Proxy
	handler  http.Handler
	query    url.Values
	upstream atomic.Int64
}

// Parse real flags and use the production backend factory. Each instance owns
// its registry, admission state, service and Redis connection pool.
func newKitRedisInstance(t *testing.T, remote redisArtifactConfig, backend string) *kitRedisInstance {
	t.Helper()
	cfg, err := parseConfig([]string{
		"-artifactCache=" + backend, "-artifactRedisAddr=" + remote.Client.Addr,
		"-artifactRedisUsername=" + remote.Client.Username, "-artifactRedisPassword=" + remote.Client.Password,
		"-artifactRedisDB=" + strconv.Itoa(remote.Client.DB), "-artifactRedisPrefix=" + remote.Prefix,
		"-artifactRedisTimeout=" + remote.OperationTimeout.String(), "-signatureKey=KIT_KEY_SECRET", "-allowHosts=origin.test",
	}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal("Redis integration flags failed validation")
	}
	f := &kitRedisInstance{obs: testObservability(t, io.Discard, "info")}
	f.service, err = cfg.newArtifactService(f.obs.artifactCache.Observe)
	if err != nil {
		t.Fatal("Redis integration backend factory failed")
	}
	t.Cleanup(func() {
		if err := f.service.Close(); err != nil {
			t.Error(err)
		}
	})
	data := iconSourcePNGForArtifact(t)
	date := time.Now().UTC().Format(http.TimeFormat)
	source := "https://origin.test/logo.png?token=SOURCE_SECRET"
	mac := hmac.New(sha256.New, []byte(cfg.SignatureKey))
	_, _ = mac.Write([]byte(source))
	f.query = url.Values{"url": {source}, "s": {base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}}
	f.proxy = imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != source {
			return nil, errors.New("unexpected integration source request")
		}
		f.upstream.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}, "Cache-Control": {"public, max-age=3600"}, "Date": {date}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	}), cfg.NewCache())
	f.proxy.AllowHosts, f.proxy.SignatureKeys = cfg.AllowHosts, [][]byte{[]byte(cfg.SignatureKey)}
	f.proxy.MaxConcurrentRequests, f.proxy.Timeout = cfg.MaxConcurrent, cfg.Timeout
	f.proxy.MaxResponseBytes, f.proxy.MaxPixels, f.proxy.MaxFrames = cfg.MaxResponseBytes, cfg.MaxPixels, cfg.MaxFrames
	f.proxy.Logger = f.obs.ProxyLogger
	f.handler = f.obs.PublicHandler(&iconHandler{proxy: f.proxy, artifacts: f.service})
	return f
}

func (f *kitRedisInstance) png(t *testing.T) {
	t.Helper()
	w := iconRequest(f.handler, "GET", "favicon-32x32.png", f.query)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.Len() == 0 {
		t.Fatalf("Redis-backed pipeline PNG status=%d", w.Code)
	}
}

func (f *kitRedisInstance) workload() pipelineBenchmarkWorkload {
	workload := pipelineBenchmarkWorkload{name: "RedisBrowserFlow"}
	for _, item := range []struct {
		name string
		size int
	}{
		{"favicon-16x16.png", 16}, {"favicon-32x32.png", 32}, {"favicon-48x48.png", 48},
		{"apple-touch-icon.png", 180}, {"android-chrome-192x192.png", 192}, {"android-chrome-512x512.png", 512},
	} {
		workload.requests = append(workload.requests, pipelineBenchmarkRequest{"/icons/" + item.name + "?" + f.query.Encode(), "image/png", item.size})
	}
	workload.requests = append(workload.requests, pipelineBenchmarkRequest{"/icons/icons.zip?" + f.query.Encode(), "application/zip", 0})
	return workload
}

func TestRedisArtifactKitTwoInstancesShareValidatedBrowserProducts(t *testing.T) {
	_, _, remote := redisArtifactIntegration(t)
	for _, backend := range []string{"redis", "tiered"} {
		t.Run(backend, func(t *testing.T) {
			cfg := remote
			cfg.Prefix += backend + ":"
			first, second := newKitRedisInstance(t, cfg, backend), newKitRedisInstance(t, cfg, backend)
			workload := first.workload()
			runPipelineBenchmarkWorkload(t, &pipelineBenchmarkFixture{handler: first.handler}, workload)
			if integrationCounter(t, first.obs, "artifact_cache_requests_total", map[string]string{"backend": backend, "result": "miss"}) != 7 {
				t.Fatal("first instance did not fill all real browser products")
			}
			if stageCount(t, first.obs, "icons", "pack", "success") != 1 || stageCount(t, first.obs, "icons", "encode", "success") == 0 {
				t.Fatal("cold instance did not use the production rendering/ZIP path")
			}
			runPipelineBenchmarkWorkload(t, &pipelineBenchmarkFixture{handler: second.handler}, workload)
			if integrationCounter(t, second.obs, "artifact_cache_requests_total", map[string]string{"backend": backend, "result": "hit"}) != 7 {
				t.Fatal("second instance did not reuse shared artifacts")
			}
			for _, stage := range []string{"transform", "encode", "pack"} {
				if stageCount(t, second.obs, "icons", stage, "success") != 0 {
					t.Fatalf("second instance repeated %s", stage)
				}
			}
			if stageCount(t, second.obs, "icons", "decode", "success") != 7 || stageCount(t, second.obs, "icons", "write", "success") != 7 || first.upstream.Load() != 7 || second.upstream.Load() != 7 {
				t.Fatal("shared artifacts bypassed current source preflight or response writes")
			}
			if integrationCounter(t, first.obs, "artifact_cache_requests_total", map[string]string{"backend": backend, "result": "hit"}) != 0 {
				t.Fatal("registry values leaked between service instances")
			}
			q := url.Values{"url": {second.query.Get("url")}, "s": {"SIGNATURE_SECRET"}}
			if w := iconRequest(second.handler, "GET", "favicon-32x32.png", q); w.Code != 403 || w.Header().Get(iconErrorCodeHeader) != "source_forbidden" {
				t.Fatal("distributed warm cache bypassed signed source policy")
			}
			if second.upstream.Load() != 7 {
				t.Fatal("invalid signature reached origin in second instance")
			}
			if w := iconRequest(second.handler, "HEAD", "favicon-32x32.png", second.query); w.Code != 200 || w.Body.Len() != 0 {
				t.Fatal("distributed warm HEAD response changed binary semantics")
			}
		})
	}
}

func TestRedisArtifactKitRequestDrivenStartupRecovery(t *testing.T) {
	_, _, remote := redisArtifactIntegration(t)
	f := newKitRedisInstance(t, remote, "redis")
	recovering, ok := f.service.store.(*recoveringRedisArtifactStore)
	if !ok {
		t.Fatal("factory did not select lazy Redis recovery")
	}
	// A deterministic dial failure exercises startup fail-open through the
	// complete source/handler path. Successful recovery uses real Redis.
	var unavailable atomic.Bool
	unavailable.Store(true)
	var attempts atomic.Int64
	recovering.retryDelay = 0 // Cooldown itself has separate deterministic tests.
	recovering.dial = func(ctx context.Context, cfg redisArtifactConfig, maxBody int64) (artifactStore, error) {
		attempts.Add(1)
		if unavailable.Load() {
			return nil, errRedisArtifactUnavailable
		}
		return newRedisArtifactStore(ctx, cfg, maxBody)
	}
	f.png(t)
	encoded := stageCount(t, f.obs, "icons", "encode", "success")
	if attempts.Load() != 1 || encoded == 0 || integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "redis", "result": "error"}) != 1 {
		t.Fatal("unavailable Redis did not fall back to bounded generation")
	}
	unavailable.Store(false)
	f.png(t)
	if attempts.Load() != 2 || stageCount(t, f.obs, "icons", "encode", "success") != 2*encoded {
		t.Fatal("next admitted request did not recover and publish")
	}
	f.png(t)
	if attempts.Load() != 2 || stageCount(t, f.obs, "icons", "encode", "success") != 2*encoded || integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "redis", "result": "hit"}) != 1 {
		t.Fatal("recovered Redis did not reuse its output")
	}
	second := newKitRedisInstance(t, remote, "redis")
	second.png(t)
	if stageCount(t, second.obs, "icons", "encode", "success") != 0 || second.upstream.Load() != 1 {
		t.Fatal("recovery was not visible through a second real service instance")
	}
}

func TestRedisArtifactKitCommandFailureRecoversWithoutRestart(t *testing.T) {
	_, admin, remote := redisArtifactIntegration(t)
	remote.OperationTimeout = 100 * time.Millisecond
	f := newKitRedisInstance(t, remote, "redis")
	recovering := f.service.store.(*recoveringRedisArtifactStore)
	recovering.retryDelay = 0 // Avoid a sleep; recovery remains request-driven.
	f.png(t)
	f.png(t)
	encoded := stageCount(t, f.obs, "icons", "encode", "success")
	// This is a disposable Redis test service. A real paused command must
	// exhaust its bounded Redis budget while the HTTP pipeline still succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.Do(ctx, "CLIENT", "PAUSE", 400, "ALL").Err(); err != nil {
		t.Fatal("cannot pause disposable integration Redis")
	}
	f.png(t)
	if stageCount(t, f.obs, "icons", "encode", "success") != 2*encoded || integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "redis", "result": "error"}) != 1 {
		t.Fatal("command outage did not preserve successful bounded generation")
	}
	// PING waits for the real server to resume. No artificial workload sleep
	// or background reconnect loop is used by the application or this test.
	if err := admin.Ping(ctx).Err(); err != nil {
		t.Fatal("disposable Redis did not resume")
	}
	f.png(t)
	if stageCount(t, f.obs, "icons", "encode", "success") != 2*encoded || integrationCounter(t, f.obs, "artifact_cache_requests_total", map[string]string{"backend": "redis", "result": "hit"}) != 2 {
		t.Fatal("resumed Redis required a restart or repeated rendering")
	}
	if f.upstream.Load() != 4 || stageCount(t, f.obs, "icons", "write", "success") != 4 {
		t.Fatal("outage or recovery bypassed source checks or final writes")
	}
}
