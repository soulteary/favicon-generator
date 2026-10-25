# Pipeline and cache benchmarks

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

The default is three runs of every cold/warm case with Go's one-second benchmark
target. `BENCHTIME`, `BENCHCOUNT` and `BENCHMARK_PATTERN` map to the corresponding
Go benchmark flags; additional arguments are passed to `go test`. Use a single
iteration for smoke checks, not to compare performance. GitHub Actions runs that
smoke check on stable Go without an absolute timing threshold.

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

Both modes initialize the proxy and execute the same workload twice before
timing: the first pass exercises cache misses and the second exercises hits.
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
  Every cold case must fetch exactly once; every warm case must fetch zero times.
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

The single-request cold cases expect zero successful cache gets, and their warm
cases expect one. A cold `BrowserFlow` expects six successful gets and one
origin fetch; a warm flow expects seven gets and zero fetches. A regular test
(`TestPipelineBenchmarkCacheBehavior`) checks these counts for repeated
operations, and every benchmark iteration checks them again.
The regular test also checks that warm source reuse removes the fetch-stage
observation while icon decoding and encoding still execute. Warm legacy
transformed-cache hits do not repeat encoding.

The source cache reduces upstream fetching. Icon outputs are **not** cached:
each PNG or ZIP request still validates and decodes the source and repeats the
required resize/encode work; ZIP requests also regenerate ICO, manifest and ZIP
bytes. A warm ZIP therefore need not be much faster or use much less memory.
The legacy imageproxy endpoint can also cache its transformed response, so its
warm behavior should not be generalized to the icon endpoints.

This fixed fixture reveals reproducible pipeline and cache behavior. It does
not predict production latency or maximum throughput: source size, entropy,
format, animation, cache directives, network latency, CPU and concurrent load
all change the result. Record the code revision and complete command alongside
measurements, and compare repeated runs rather than using one sample as a
performance guarantee.
