# Pipeline and cache benchmarks

The [historical source-cache baseline](#recorded-baseline-2026-10-25) includes raw
samples, environment details, and its measured source revision. The current
source/artifact cache comparisons use the same workloads and production observers.
The [integrated kit run](#integrated-kit-baseline-2026-10-25) records all 60 current
samples and the artifact-mode medians.
[Kit integration validation](kit-integration.md) describes the security, lifecycle
and real-Redis checks behind these comparisons.

Run the benchmarks with an installed Go 1.27.3 or later toolchain. Download the
root module's dependencies once in a network-enabled environment, then run
offline:

```sh
go mod download
./scripts/benchmark.sh
```

The script disables toolchain switching and module downloads, uses
`-mod=readonly`, and retains Go's normal checksum verification. Dependencies and
their required checksum data must already be cached; missing prerequisites
produce an error. The workloads do not fetch images, bind listeners or make
DNS/network requests. No Node.js runtime or running service is required.

Use the same machine, Go version, CPU allocation and flags when comparing runs:

```sh
GOMAXPROCS=2 BENCHTIME=2s BENCHCOUNT=5 ./scripts/benchmark.sh | tee /tmp/favicon-generator-bench.txt
BENCHTIME=1x BENCHCOUNT=1 ./scripts/benchmark.sh
BENCHMARK_PATTERN='^BenchmarkPipeline/BrowserFlow/' ./scripts/benchmark.sh -cpu=1
```

The default runs both benchmark series: eight source-cache Cold/Warm cases and
twelve artifact-cache mode cases, for 20 cases. Three runs of each case produce
60 samples with Go's one-second benchmark target. `BENCHTIME`, `BENCHCOUNT` and
`BENCHMARK_PATTERN` map to the corresponding Go benchmark flags; additional
arguments are passed to `go test`. Use a single iteration for smoke checks, not
to compare performance. GitHub Actions runs that smoke check on stable Go without
an absolute timing threshold.

## Workloads and timing

All cases use the same generated 512 × 320 RGBA PNG with deterministic pixels
and partial transparency, a fake `http.RoundTripper`, and the real 100 MB LRU
cache. The origin response has a fresh `Date`, an ETag and
`Cache-Control: public, max-age=86400`. This is a cacheable source, unlike an
origin response that forbids caching. No artificial latency or sleeps are added.

One benchmark operation means:

- `LegacyPNG`: one `/32x32,png/https://benchmark.test/logo.png` request.
- `IconPNG`: one `/icons/favicon-32x32.png` request.
- `ZIP`: one `/icons/icons.zip` request.
- `BrowserFlow`: the actual six PNG preview requests, serially, followed by one
  ZIP request, all using the same source URL.

Requests run through the actual public observability middleware and icon/proxy
handlers with `httptest.ResponseRecorder`. The default `info` log level and
production HTTP/pipeline observers are enabled, with logs directed to
`io.Discard`; successful requests do not emit access logs at that level.
Constructing loggers, registries and handlers is outside the timed region.
The reported work includes HTTP request/response allocation, metric observation
and lightweight result validation: every response must have status 200 and the
expected nonempty output type; PNG headers must have the requested dimensions,
and ZIPs must have the eight fixed, unique, nonempty members. Existing functional
tests separately decode complete image streams and ZIP contents.

These are handler benchmarks rather than end-to-end network or browser-rendering
measurements. They exclude listener/socket overhead and management metric
scraping. They do include the pipeline observer installed by `PublicHandler`.

The `BenchmarkPipeline` source-cache cases initialize the proxy and execute the
same workload twice before timing: the first pass exercises cache misses and
the second exercises hits.
This removes lazy transport/admission and both metric-series initializations
from the comparison. Warmup is excluded from elapsed time, allocations and
custom counters.

- `Cold`: replace only the LRU with an empty one before **each operation**. A
  browser-flow operation starts empty, then retains its source entry across the
  remaining six requests.
- `Warm`: retain the cache populated by the identical warmup operation.

LRU replacement, counter resets and fetch-count assertions run with the
benchmark timer stopped. Timing and allocation metrics include only the handler
work and its result validation. The benchmarks are sequential, not a concurrency
or admission-capacity test.

## Reading the results

Go reports `ns/op`, `B/op` and `allocs/op`. Custom metrics are:

- `upstream_fetches/op`: calls to the fake origin transport, excluding warmup.
  Every `BenchmarkPipeline` cold case must fetch exactly once; every warm case
  must fetch zero times. All artifact-series cases keep their source cache warm
  and must fetch zero times.
- `cache_get_hits/op`: successful calls to the underlying cache's `Get` method.
  This counts stored entries found, rather than the HTTP cache's freshness
  decision. It is distinct from the management metrics' `hit` or `revalidated`
  outcomes described in [metrics.md](metrics.md).
- `fetch_ns/op`, `decode_ns/op`, `transform_ns/op`, `encode_ns/op`,
  `pack_ns/op`, and `write_ns/op`: differences in the actual production
  `stage_duration_seconds_sum` histograms, converted to nanoseconds and divided
  by benchmark operations. Snapshots are gathered after warmup and after the
  measured iterations with timing stopped, across all result labels in the
  fixture's private registry. An unexecuted stage reports zero. These operation
  sums are not an additive breakdown of overall `ns/op`: bookkeeping and result
  validation are omitted, and a stage can execute multiple times per request.

In the source-cache series, single-request cold cases expect zero successful
cache gets, and their warm cases expect one. A cold `BrowserFlow` expects six successful gets and one
origin fetch; a warm flow expects seven gets and zero fetches. A regular test
(`TestPipelineBenchmarkCacheBehavior`) checks these counts for repeated
operations, and every benchmark iteration checks them again.
The regular test also checks that warm source reuse removes the fetch-stage
observation while icon decoding and encoding still execute. Warm legacy
transformed-cache hits do not repeat encoding.

The source cache reduces upstream fetching. In `BenchmarkPipeline`, and with
the default `-artifactCache=off`, generated icon outputs are not retained: each
PNG or ZIP request still validates and decodes the source and repeats the required
resize/encode work; ZIP requests also regenerate ICO, manifest and ZIP bytes. A warm ZIP therefore need not be much faster or use much less memory.
The legacy imageproxy endpoint can also cache its transformed response, so its
warm behavior should not be generalized to the icon endpoints.

This fixed fixture reveals reproducible pipeline and cache behavior. It does
not predict production latency or maximum throughput: source size, entropy,
format, animation, cache directives, network latency, CPU and concurrent load
all change the result. Record the code revision and complete command alongside
measurements, and compare repeated runs rather than using one sample as a
performance guarantee.

## Recorded baseline: 2026-10-25

This is the historical 24-sample source-cache baseline, measured before the
current kit integrations and opt-in finished artifact caching. It is retained
for reference and does not describe the current artifact-cache modes.

Measured source revision:
[`fc7bf69`](https://github.com/soulteary/favicon-generator/commit/fc7bf69aa7f317a1d028a4c69e37f17b6efceb1c).
Environment: Go 1.27.3, macOS 26.5.1, darwin/arm64, Apple M5,
`GOMAXPROCS=2`. The observer is enabled and the race detector is disabled.
The documentation commit containing these results does not change measured code.

```sh
GOMAXPROCS=2 BENCHTIME=1s BENCHCOUNT=3 ./scripts/benchmark.sh
```

[Raw output from all 24 samples](benchmarks/2026-10-25-go1.27.3-darwin-arm64.txt).
The following values are the independent medians of three samples for each case:

- `LegacyPNG`: cold **1.578 ms/op**, 2,587,346 B/op, 405 allocs/op;
  warm **0.0124 ms/op**, 34,923 B/op, 137 allocs/op.
- `IconPNG`: cold **1.755 ms/op**, 3,310,763 B/op, 352 allocs/op;
  warm **1.853 ms/op**, 3,201,585 B/op, 305 allocs/op.
- `ZIP`: cold **28.483 ms/op**, 31,583,545 B/op, 1,178 allocs/op;
  warm **27.869 ms/op**, 31,464,545 B/op, 1,131 allocs/op.
- `BrowserFlow`: cold **56.225 ms/op**, 60,996,541 B/op, 3,089 allocs/op;
  warm **55.856 ms/op**, 60,877,458 B/op, 3,040 allocs/op.

Every cold case made exactly one upstream call, and every warm case made zero.
Browser-flow cache gets were six cold and seven warm. All per-iteration output
and count assertions passed. `B/op` is cumulative allocated bytes, not peak or
retained memory.

In the warm browser flow, median stage totals per operation were 13.673 ms of
decode/check work, 21.770 ms of transformation, 20.243 ms of encoding, 0.0449 ms
of packing, 0.0381 ms of writing to the recorder, and zero fetching. Source
caching therefore removes origin retrieval but leaves the repeated icon
generation work visible. The much cheaper warm legacy case reuses transformed
output and still runs metadata/resource checks.

The fake origin has no network latency, so these results isolate handler work.
The warm icon PNG sample was slightly slower than cold, and one warm browser
sample reached 61.669 ms: the small differences between cold and warm icon
operations do not establish a speedup. The robust distinction here is one
versus zero origin calls, and continued transform/encode work. Real network
latency can make source caching substantially more valuable; measure production
traffic with the stage and cache metrics before selecting an output-cache policy.

## Opt-in artifact caching

The original `BenchmarkPipeline` keeps the default artifact cache disabled.
Compare the finished-cache paths separately:

```sh
BENCHMARK_PATTERN='^BenchmarkArtifactPipeline$' ./scripts/benchmark.sh
```

The source HTTP cache stays warm in all modes. `Off` performs normal generation,
`MemoryCold` removes finished entries before each measured workload, and
`MemoryWarm` reuses previously generated products. These modes cover icon PNG,
ZIP and six serial previews followed by ZIP. `LegacyPNG` is a fourth, unchanged
control workload: artifact-cache selection does not affect the legacy endpoint
and its existing transformed cache. They use the production handler,
private stage histograms, and the actual capacity-bounded httpcache-kit store.
Warm icon artifacts must show no additional transform, encode or pack
observations; source validation and final writes remain part of the measured
work. The legacy control keeps its existing warm transformed-cache behavior in
all three modes. Clear and setup operations are outside timing.

The artifact series additionally reports `artifact_hit/op`, `artifact_miss/op`,
`artifact_bypass/op` and `artifact_error/op` from the production artifact counters.
A warm single PNG or ZIP expects one artifact hit; a warm browser flow expects
seven. Off cases bypass the artifact store and cold-memory cases miss it. Legacy
control cases have no artifact decisions. Every artifact-series operation has
zero upstream fetches because its source/transformed HTTP cache stays warm.
Per-operation allocation totals are not peak live memory or a promise about
process RSS.

## Integrated kit baseline: 2026-10-25

Measured code revision:
[`d264652`](https://github.com/soulteary/favicon-generator/commit/d264652c75e3eaf8a9b16b99bec339373be19727).
Environment: Go 1.27.3, macOS 26.5.1 (25F80), darwin/arm64, Apple M5,
`GOMAXPROCS=2`; race detector disabled. The documentation commit does not change
measured code. The complete run passed in 143.969 seconds.

```sh
GOMAXPROCS=2 BENCHTIME=1s BENCHCOUNT=3 ./scripts/benchmark.sh
```

[Complete raw output with metadata: all 60 samples](benchmarks/2026-10-25-kits-go1.27.3-darwin-arm64.txt).
There are three samples for each of 20 cases. The following values are independent
medians of ns/op, B/op and allocs/op for all 12 artifact-series cases. The source
HTTP cache is warm in every mode; these are local memory-artifact measurements,
not Redis latency measurements.

- `LegacyPNG`:
  `Off` **12.915 µs/op**, 34,894 B/op, 137 allocs/op;
  `MemoryCold` **13.094 µs/op**, 34,922 B/op, 137 allocs/op;
  `MemoryWarm` **13.130 µs/op**, 34,892 B/op, 137 allocs/op.
- `IconPNG`:
  `Off` **1.784205 ms/op**, 3,204,949 B/op, 327 allocs/op;
  `MemoryCold` **1.863843 ms/op**, 3,288,422 B/op, 529 allocs/op;
  `MemoryWarm` **0.030759 ms/op**, 92,767 B/op, 205 allocs/op.
- `ZIP`:
  `Off` **28.520797 ms/op**, 31,466,734 B/op, 1,087 allocs/op;
  `MemoryCold` **28.709514 ms/op**, 31,742,299 B/op, 1,328 allocs/op;
  `MemoryWarm` **0.120120 ms/op**, 395,252 B/op, 298 allocs/op.
- `BrowserFlow`:
  `Off` **56.359202 ms/op**, 60,900,639 B/op, 3,128 allocs/op;
  `MemoryCold` **56.386060 ms/op**, 61,784,730 B/op, 4,608 allocs/op;
  `MemoryWarm` **0.350023 ms/op**, 1,173,483 B/op, 1,558 allocs/op.

Every artifact-series sample made zero upstream calls. A warm icon PNG or ZIP
recorded one artifact hit per operation; the warm browser flow recorded seven.
Memory-cold icon cases recorded misses and Off icon cases recorded bypasses.
The legacy control recorded no artifact decisions and retained one transformed
cache get in every mode; its small timing differences do not demonstrate an
artifact-cache benefit.

Warm browser-flow median observations were 4.709 µs/op of source decode/preflight
checks and 13.899 µs/op of writes to the response recorder. Fetch, transform,
encode and pack observations were zero in all three samples. These stage sums
are not additive components of the 0.350023 ms overall result. Source identity,
authorization, resource checks, cache validation/copies, middleware and response
handling still run on the warm path.

Cold artifact cases still generate output and include storage/copy work. The
observed warm reduction depends on reusable, fresh artifacts for this fixed
fixture; it does not predict production latency, network or Redis costs,
concurrent throughput, cache hit rate, or process memory. The 1,173,483 B/op warm
browser-flow median is cumulative allocation, not the resident size of its cache.
Compare repeated runs on the same environment and retain their full raw output.
