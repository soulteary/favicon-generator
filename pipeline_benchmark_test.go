package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/die-net/lrucache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/willnorris/imageproxy"
)

const pipelineBenchmarkSource = "https://benchmark.test/logo.png"

var pipelineBenchmarkStages = [...]string{"fetch", "decode", "transform", "encode", "pack", "write"}

type pipelineBenchmarkRequest struct {
	path, contentType string
	pngSize           int
}

type pipelineBenchmarkWorkload struct {
	name     string
	requests []pipelineBenchmarkRequest
}

func pipelineBenchmarkWorkloads() []pipelineBenchmarkWorkload {
	query := url.Values{"url": {pipelineBenchmarkSource}}.Encode()
	iconPNG := func(filename string, size int) pipelineBenchmarkRequest {
		return pipelineBenchmarkRequest{"/icons/" + filename + "?" + query, "image/png", size}
	}
	zipRequest := pipelineBenchmarkRequest{"/icons/icons.zip?" + query + "&name=Benchmark&shortName=Bench", "application/zip", 0}
	previews := []pipelineBenchmarkRequest{
		iconPNG("favicon-16x16.png", 16), iconPNG("favicon-32x32.png", 32), iconPNG("favicon-48x48.png", 48),
		iconPNG("apple-touch-icon.png", 180), iconPNG("android-chrome-192x192.png", 192), iconPNG("android-chrome-512x512.png", 512),
	}
	return []pipelineBenchmarkWorkload{
		{"LegacyPNG", []pipelineBenchmarkRequest{{"/32x32,png/" + pipelineBenchmarkSource, "image/png", 32}}},
		{"IconPNG", []pipelineBenchmarkRequest{previews[1]}},
		{"ZIP", []pipelineBenchmarkRequest{zipRequest}},
		{"BrowserFlow", append(previews, zipRequest)},
	}
}

// Replacing only the LRU leaves the proxy's initialized transports and admission
// state intact. Calls are serial; no reset occurs while a handler is active.
// Successful Get calls are distinct from HTTP cache freshness outcomes.
type pipelineBenchmarkCache struct {
	imageproxy.Cache
	hits atomic.Int64
}

func (c *pipelineBenchmarkCache) Get(key string) ([]byte, bool) {
	value, ok := c.Cache.Get(key)
	if ok {
		c.hits.Add(1)
	}
	return value, ok
}

func (c *pipelineBenchmarkCache) clear() { c.Cache = lrucache.New(100_000_000, 3600) }

type pipelineBenchmarkFixture struct {
	handler   http.Handler
	cache     *pipelineBenchmarkCache
	fetches   atomic.Int64
	metrics   prometheus.Gatherer
	artifacts *memoryArtifactStore
}

func newPipelineBenchmarkFixture(tb testing.TB, artifactEnabled ...bool) *pipelineBenchmarkFixture {
	tb.Helper()
	// The deterministic rectangular RGBA source exercises fitting and square
	// padding. Source encoding and fixture setup are outside benchmark timing.
	source := image.NewNRGBA(image.Rect(0, 0, 512, 320))
	for y := range 320 {
		for x := range 512 {
			source.SetNRGBA(x, y, color.NRGBA{
				R: byte((x*7 + y*3) % 256), G: byte((x + y*11) % 256), B: byte((x*5 + y) % 256),
				A: byte(128 + (x+y)%128),
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		tb.Fatal(err)
	}
	data := encoded.Bytes()
	fixture := &pipelineBenchmarkFixture{cache: new(pipelineBenchmarkCache)}
	fixture.cache.clear()
	date := time.Now().UTC().Format(http.TimeFormat)
	transport := iconTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != pipelineBenchmarkSource {
			return nil, errors.New("unexpected benchmark upstream request")
		}
		fixture.fetches.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{
				"Content-Type": {"image/png"}, "Cache-Control": {"public, max-age=86400"},
				"Date": {date}, "Etag": {`"benchmark-source-v1"`},
			},
			Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r,
		}, nil
	})
	observability, err := newObservability(io.Discard, "info")
	if err != nil {
		tb.Fatal(err)
	}
	proxy := imageproxy.NewProxy(transport, fixture.cache)
	proxy.Logger = observability.ProxyLogger
	handler := &iconHandler{proxy: proxy}
	if len(artifactEnabled) > 0 && artifactEnabled[0] {
		store, err := newMemoryArtifactStore(defaultArtifactCacheBytes, defaultArtifactCacheEntryBytes, defaultArtifactCacheTTL)
		if err != nil {
			tb.Fatal(err)
		}
		service := newArtifactService(store, "memory", defaultArtifactCacheTTL, defaultArtifactCacheEntryBytes, observability.artifactCache.Observe)
		fixture.artifacts = store
		handler.artifacts = service
		tb.Cleanup(func() {
			if err := service.Close(); err != nil {
				tb.Error(err)
			}
		})
	}
	fixture.handler = observability.PublicHandler(handler)
	fixture.metrics = observability.metrics.Registry.Gatherer()
	return fixture
}

// Read the actual production histograms rather than timing a parallel copy of
// their stage boundaries. Gathering occurs only while benchmark timing is off.
func (f *pipelineBenchmarkFixture) stageSums(tb testing.TB) map[string]float64 {
	tb.Helper()
	sums := make(map[string]float64, len(pipelineBenchmarkStages))
	for _, stage := range pipelineBenchmarkStages {
		sums[stage] = 0
	}
	families, err := f.metrics.Gather()
	if err != nil {
		tb.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "favicon_generator_pipeline_stage_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "stage" {
					stage := label.GetValue()
					if _, ok := sums[stage]; ok {
						sums[stage] += metric.GetHistogram().GetSampleSum()
					}
				}
			}
		}
	}
	return sums
}

func (f *pipelineBenchmarkFixture) resetCounters() {
	f.fetches.Store(0)
	f.cache.hits.Store(0)
}

func runPipelineBenchmarkWorkload(tb testing.TB, fixture *pipelineBenchmarkFixture, workload pipelineBenchmarkWorkload) {
	tb.Helper()
	for _, item := range workload.requests {
		response := httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, item.path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != item.contentType || response.Body.Len() == 0 {
			tb.Fatalf("%s: status=%d, type=%q, bytes=%d", workload.name, response.Code, response.Header().Get("Content-Type"), response.Body.Len())
		}
		if item.pngSize > 0 {
			config, err := png.DecodeConfig(bytes.NewReader(response.Body.Bytes()))
			if err != nil || config.Width != item.pngSize || config.Height != item.pngSize {
				tb.Fatalf("%s: invalid PNG dimensions %dx%d: %v", workload.name, config.Width, config.Height, err)
			}
			continue
		}
		archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
		if err != nil || len(archive.File) != 8 {
			tb.Fatalf("%s: invalid ZIP directory: %v", workload.name, err)
		}
		seen := make(map[string]bool, len(archive.File))
		for _, member := range archive.File {
			if !validIconBundleName(member.Name) || seen[member.Name] || member.UncompressedSize64 == 0 {
				tb.Fatalf("%s: invalid or duplicate ZIP member", workload.name)
			}
			seen[member.Name] = true
		}
	}
}

func checkPipelineBenchmarkCounts(tb testing.TB, fixture *pipelineBenchmarkFixture, workload pipelineBenchmarkWorkload, cold bool) {
	tb.Helper()
	wantFetches, wantHits := int64(0), int64(len(workload.requests))
	if cold {
		wantFetches = 1
		wantHits--
	}
	if got := fixture.fetches.Load(); got != wantFetches {
		tb.Fatalf("%s: upstream fetches=%d, want %d (cold=%v)", workload.name, got, wantFetches, cold)
	}
	if got := fixture.cache.hits.Load(); got != wantHits {
		tb.Fatalf("%s: cache Get hits=%d, want %d (cold=%v)", workload.name, got, wantHits, cold)
	}
}

func TestPipelineBenchmarkCacheBehavior(t *testing.T) {
	for _, workload := range pipelineBenchmarkWorkloads() {
		for _, mode := range []string{"Cold", "Warm"} {
			t.Run(workload.name+"/"+mode, func(t *testing.T) {
				fixture := newPipelineBenchmarkFixture(t)
				// Prime both miss and hit metric series using this operation.
				// Priming calls never contribute to the checked counters.
				runPipelineBenchmarkWorkload(t, fixture, workload)
				runPipelineBenchmarkWorkload(t, fixture, workload)
				for range 2 {
					if mode == "Cold" {
						fixture.cache.clear()
					}
					fixture.resetCounters()
					before := fixture.stageSums(t)
					runPipelineBenchmarkWorkload(t, fixture, workload)
					checkPipelineBenchmarkCounts(t, fixture, workload, mode == "Cold")
					after := fixture.stageSums(t)
					if (after["fetch"] > before["fetch"]) != (mode == "Cold") {
						t.Fatal("fetch histogram does not match actual upstream calls")
					}
					for _, stage := range []string{"decode", "write"} {
						if after[stage] <= before[stage] {
							t.Fatalf("missing actual %s observation", stage)
						}
					}
					wantEncode := workload.name != "LegacyPNG" || mode == "Cold"
					if (after["encode"] > before["encode"]) != wantEncode {
						t.Fatal("encode histogram no longer distinguishes source and transformed caching")
					}
				}
			})
		}
	}
}

func BenchmarkPipeline(b *testing.B) {
	for _, workload := range pipelineBenchmarkWorkloads() {
		for _, mode := range []string{"Cold", "Warm"} {
			b.Run(workload.name+"/"+mode, func(b *testing.B) {
				b.StopTimer()
				fixture := newPipelineBenchmarkFixture(b)
				// The first pass primes miss series; the second primes hit series.
				runPipelineBenchmarkWorkload(b, fixture, workload)
				runPipelineBenchmarkWorkload(b, fixture, workload)
				before := fixture.stageSums(b)
				b.ReportAllocs()
				b.ResetTimer()
				var fetches, hits int64
				for range b.N {
					if mode == "Cold" {
						fixture.cache.clear()
					}
					fixture.resetCounters()
					b.StartTimer()
					runPipelineBenchmarkWorkload(b, fixture, workload)
					b.StopTimer()
					checkPipelineBenchmarkCounts(b, fixture, workload, mode == "Cold")
					fetches += fixture.fetches.Load()
					hits += fixture.cache.hits.Load()
				}
				b.ReportMetric(float64(fetches)/float64(b.N), "upstream_fetches/op")
				b.ReportMetric(float64(hits)/float64(b.N), "cache_get_hits/op")
				after := fixture.stageSums(b)
				for _, stage := range pipelineBenchmarkStages {
					b.ReportMetric((after[stage]-before[stage])*1e9/float64(b.N), stage+"_ns/op")
				}
			})
		}
	}
}

// Source HTTP caching stays warm in all three modes. Only the finished artifact
// cache changes, so the comparison measures real avoided rendering and packing.
func BenchmarkArtifactPipeline(b *testing.B) {
	for _, workload := range pipelineBenchmarkWorkloads()[1:] {
		for _, mode := range []string{"Off", "MemoryCold", "MemoryWarm"} {
			b.Run(workload.name+"/"+mode, func(b *testing.B) {
				b.StopTimer()
				fixture := newPipelineBenchmarkFixture(b, mode != "Off")
				runPipelineBenchmarkWorkload(b, fixture, workload)
				before := fixture.stageSums(b)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if mode == "MemoryCold" {
						if err := fixture.artifacts.cache.Purge(); err != nil {
							b.Fatal(err)
						}
					}
					fixture.resetCounters()
					b.StartTimer()
					runPipelineBenchmarkWorkload(b, fixture, workload)
					b.StopTimer()
					checkPipelineBenchmarkCounts(b, fixture, workload, false)
				}
				after := fixture.stageSums(b)
				for _, stage := range pipelineBenchmarkStages {
					b.ReportMetric((after[stage]-before[stage])*1e9/float64(b.N), stage+"_ns/op")
				}
			})
		}
	}
}

func TestArtifactBenchmarkWarmSkipsRenderingAndPacking(t *testing.T) {
	for _, workload := range pipelineBenchmarkWorkloads()[1:] {
		t.Run(workload.name, func(t *testing.T) {
			fixture := newPipelineBenchmarkFixture(t, true)
			runPipelineBenchmarkWorkload(t, fixture, workload)
			before := fixture.stageSums(t)
			fixture.resetCounters()
			runPipelineBenchmarkWorkload(t, fixture, workload)
			after := fixture.stageSums(t)
			checkPipelineBenchmarkCounts(t, fixture, workload, false)
			for _, stage := range []string{"transform", "encode", "pack"} {
				if after[stage] != before[stage] {
					t.Fatalf("warm artifact repeated %s", stage)
				}
			}
			if after["write"] <= before["write"] || after["decode"] <= before["decode"] {
				t.Fatal("warm artifact skipped source validation or response write")
			}
		})
	}
}
