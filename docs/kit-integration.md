# Kit integration validation

The integration suite exercises the real public middleware, source validation,
artifact service, bounded httpcache-kit memory backend and memfs/vfs ZIP path.
Its image transport is deterministic and local to the test process: no network,
DNS, listening sockets, Redis server or simulated latency is required.

## Security and lifecycle regressions

`TestKitIntegrationWarmArtifactKeepsCurrentAuthorizationAndBudgets` first fills a
real memory entry, then changes signature/allowlist policy or source/output
budgets. Rejected requests cannot reuse warm output. An already-canceled request
does not enter artifact lookup. Response text never contains source or signature
sentinels.

`TestKitIntegrationCurrentSourceRetentionRevokesOrShortensWarmEntry` changes the
current origin policy to no-store/private or a shorter public lifetime. The
source HTTP cache is disabled in this test so the new response is authoritative
immediately. Nonshareable responses bypass artifact lookup/storage; a shorter
source deadline invalidates and replaces the older entry. This does not claim
that a still-fresh source HTTP cache entry observes an unseen origin change.

The read/fill/write regressions hold real handlers at deterministic channel
boundaries. They verify that both legacy and icon routes reject extra requests
while an admitted handler reads an artifact, waits for a shared fill, or writes a
warm response. Canceling a waiter leaves its leader intact; canceling the leader
releases fill coordination. All test-owned requests finish before Close, which
is idempotent and leaves no active cache operation, fill or snapshot gate.
There is no background fill or cleanup worker to drain. Timeout timers in tests
bound failures; they do not simulate workload latency.

## Admission observations

`favicon_generator_pipeline_admission_rejected_total{pipeline,reason}` counts
only shared-admission rejections. `pipeline` is `image` or `icons`; `reason` is
`busy`. A rejection is counted before any stage runs, rather than inventing a
fetch/decode observation. The optional imageproxy `AdmissionObserver` extension
leaves the existing two-method `Observer` interface compatible. Collector labels
are fixed and use the private management registry. UI, assets, health checks and
metrics scraping do not increment this counter.

## Reproducible benchmark comparisons

The original `BenchmarkPipeline` keeps all four workloads and source-cache Cold/
Warm cases. The separate `BenchmarkArtifactPipeline` runs the same workloads with
source/transformed HTTP caches already warm in every case:

- `Off`: finished artifacts disabled, with the actual off service and observers.
- `MemoryCold`: clear only the artifact backend before each operation.
- `MemoryWarm`: retain artifacts primed by the identical workload.

Keeping the source cache warm isolates finished artifact reuse. The legacy PNG
endpoint is a control: toggling the artifact service does not affect its existing
transformed cache. PNG, ZIP and six-preview-plus-ZIP warm artifact operations
still perform source preflight/decode checks and write responses, while skipping
transformation, encoding and ZIP packing. Regular tests assert these production
stage histogram counts and exact cache outcomes.

Both series report ns/op, B/op, allocs/op, upstream_fetches/op,
cache_get_hits/op and the six observed stage duration sums. Artifact cases also
report artifact_hit/op, artifact_miss/op, artifact_bypass/op and
artifact_error/op from the actual production counters. Stage sums are not an
additive breakdown of total time. B/op is cumulative allocation, not retained or
peak memory. All warmup, cache resets, metric snapshots and counter assertions
run outside benchmark timing.

```sh
# Requires installed Go >=1.27.3 and pre-cached, checksum-verified dependencies.
# The script defaults to both series and retains offline, readonly operation.
BENCHTIME=1x BENCHCOUNT=1 ./scripts/benchmark.sh
GOMAXPROCS=2 BENCHTIME=1s BENCHCOUNT=3 ./scripts/benchmark.sh
BENCHMARK_PATTERN='^BenchmarkArtifactPipeline/BrowserFlow/' ./scripts/benchmark.sh
```

The single-iteration form is a smoke check, including in the existing CI step;
it is not a performance measurement or threshold. Record the exact revision,
Go version, CPU allocation and full repeated output for comparisons. The
historical source-cache baseline in [benchmarks.md](benchmarks.md) predates
finished artifact caching and remains a historical result.

## Real Redis and independent service instances

`TestRedisArtifactKitTwoInstancesShareValidatedBrowserProducts` parses the real
CLI flags and calls the production service/backend factory for two independent
service instances in one Go test process. They have separate public middleware,
registries, source proxies, admission state and Redis pools, and share only the
Redis namespace. Both redis-only and tiered modes exercise the six serial PNGs
and ZIP. The second instance must reuse all seven products without transform,
encode or pack observations, while still fetching/checking the current source
and writing all seven responses. Signed-source rejection, private registry
isolation and warm HEAD semantics remain checked.

`TestRedisArtifactKitRequestDrivenStartupRecovery` injects a deterministic first
dial failure into the lazy recovery wrapper, then recovers using actual Redis
commands. The complete public pipeline generates a valid bounded fallback,
publishes on the next admitted request, and exposes that output to a second
instance. `TestRedisArtifactKitCommandFailureRecoversWithoutRestart` pauses a
disposable Redis server, verifies fallback after the real command budget expires,
then waits for its PING response and verifies warm reuse without restarting the
application. These tests set the private cooldown test knob to zero to avoid
sleeping; separate backend tests cover the production cooldown. They do not
simulate or report performance latency.

These tests inherit the random-prefix cleanup and required-service policy from
`redisArtifactIntegration`. They skip locally without `REDIS_TEST_ADDR`; the
existing dedicated Redis CI job matches their `TestRedisArtifact` prefix and
requires the pinned official service. Only use a disposable Redis server because
the suite exercises ACL and CLIENT PAUSE, as described in [redis-cache.md](redis-cache.md).

```sh
REDIS_TEST_ADDR=127.0.0.1:6379 REDIS_TEST_REQUIRED=1 \
  go test -mod=readonly -race -count=1 -timeout=2m -run '^TestRedisArtifactKit' .
```

## Recorded integrated run

The assembled branch passed all three modules' race tests, vet and build, the
complete real-Redis suite, 18 frontend regressions, and a main-entry govulncheck
v1.9.0 scan with Go 1.27.3. The final ZIP64 guard then passed the ZIP and complete
Kit integration regressions, including real Redis, at measured revision
`d264652c75e3eaf8a9b16b99bec339373be19727`. After all parallel test work finished,
that revision ran the 20 benchmark cases three times with
`GOMAXPROCS=2 BENCHTIME=1s BENCHCOUNT=3`. All 60 samples passed.

[The measured artifact medians and complete raw output](benchmarks.md#integrated-kit-baseline-2026-10-25)
separate Off, MemoryCold and MemoryWarm for all four workloads. LegacyPNG remains
a control with no artifact-cache decisions. The warm browser flow recorded seven
artifact hits, source preflight and response writes, and no transform/encode/pack
operations. These memory-backend handler results are not Redis or browser-rendering
performance measurements. The earlier 24-sample source-cache baseline remains a
historical record of its own revision.
