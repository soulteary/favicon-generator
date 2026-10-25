# Pipeline metrics

The management listener exposes these collectors alongside the existing HTTP
metrics. The default scrape URL is `http://localhost:9090/metrics`; the public
listener does not expose them. Each service instance owns its registry.

`favicon_generator_pipeline_stage_duration_seconds` is a histogram with
`pipeline`, `stage`, and `result` labels. Its `_count` series is also the number
of completed operations. `pipeline` is `image` for the legacy image proxy or
`icons` for the icon API. The stages are:

- `fetch`: a real outbound HTTP exchange, including reading and closing the
  response body. Redirects can produce multiple fetches. A fresh cache hit
  produces no fetch observation.
- `decode`: image metadata/resource checks and pixel decoding. GIF frame checks,
  decoding the normalized PNG, and validating ICO frame PNGs are included. These
  are separate operations, so a request can produce several observations.
- `transform`: output-size planning/resource checks, resizing, cropping,
  rotation, padding, and GIF frame processing.
- `encode`: PNG and other image encoders, ICO layout/frame preparation and
  container assembly, and manifest JSON serialization. These are separate
  operations; an encode count is not solely a PNG encoder call count.
- `pack`: building the bounded in-memory ZIP archive, including validation and
  the independent virtual file tree. Only ZIP requests have this stage.
- `write`: committing generated response headers and writing the response body
  to the HTTP writer. This includes blocked writes and write errors. HEAD records
  the header operation and has zero response body bytes in the HTTP metrics.

`result` is `success`, `error`, `canceled`, `timeout`, or `limit`. Resource-limit
errors retain their classification when wrapped. Error strings, source URLs,
signatures, query values, filenames, and cache keys never become labels. Unknown
label values are rejected. UI, static assets, health checks, and scrapes do not
create pipeline observations.

Stages that did not execute are absent. A request rejected by authorization or
admission does not invent a decode or encode observation. A malformed source can
have a successful fetch followed by a failed decode. HTTP status totals remain
the source of truth for the final response status; a download can commit HTTP 200
and subsequently fail during `write`. That failure is recorded without appending
an error body to the partial artifact.

These are operation timings, not an additive decomposition of request latency.
They omit admission, request parsing, HTTP cache work, and other bookkeeping;
there may be multiple operations of each kind. Compare end-to-end latency with
`favicon_generator_http_request_duration_seconds` and response bytes with
`favicon_generator_http_response_size_bytes`. A write observation ends when the
server's HTTP writer returns, not when the remote browser has received all bytes.

## HTTP cache decisions

`favicon_generator_pipeline_cache_requests_total{pipeline,layer,result}` counts
HTTP cache decisions. `layer` is `source` for raw origin responses or
`transformed` for the legacy proxy's transformed response cache. The icon API
currently caches only source HTTP responses; PNG, ICO, manifest, and ZIP output
is regenerated per request.

- `hit`: a reusable cached response returned without calling the inner
  transport, according to HTTP cache freshness/request rules.
- `miss`: the inner transport was called and returned a new response. This also
  includes stale entries that required a replacement and an `only-if-cached`
  lookup without an entry.
- `revalidated`: the inner transport returned HTTP 304 and the stored response
  was reused. Source revalidation incurs an outbound exchange; transformed
  revalidation may reuse the source cache. Actual exchanges are counted by
  `fetch`.
- `bypass`: caching is disabled (`NopCache`), the request cannot use the HTTP
  cache, or a transport-path request/response specifies `no-store` and therefore
  prevents storage. An already reusable entry may still produce a `hit` when a
  later request specifies `no-store`; the observer reports the cache's actual
  behavior rather than changing its reuse policy.
- `error`: transport, response-body, or cache processing failed, or the inner
  transport returned HTTP 5xx. A stale fallback after a failed network exchange
  is not a `hit`. A corrupt entry that produces a synthetic cache-only 504 is
  also an error.

The observer tracks whether the inner transport ran; it does not trust the
upstream-controlled `X-From-Cache` header. Decisions finish when the response body
is closed, so body-read/close failures are included; real outbound failures also
appear in the fetch stage. A `miss` is not a promise that an entry was stored.
Multi-tier cache backend lookups are not additional HTTP cache decisions.
Signature, origin policy, and request cancellation checks still run before cache
access. Discarded 304/5xx response bodies are closed when revalidation or stale
fallback substitutes a cached response, so those fetch operations also finish.

Caching is disabled by default. Enable a bounded cache, for example
`-cache memory:100`, to measure source reuse. A fresh transformed legacy response
can avoid the nested source lookup entirely. A fresh icon source still requires
resource checks, decoding, generation, and response writing.

## Example queries

Per-stage p95 over five minutes (keep `result` if failures need separate views):

```promql
histogram_quantile(0.95,
  sum by (le, pipeline, stage) (
    rate(favicon_generator_pipeline_stage_duration_seconds_bucket[5m])
  )
)
```

Fresh source hit ratio for the icon API among hit, miss, and revalidation
decisions. Bypasses and errors are deliberately excluded; inspect them separately.
No traffic gives an undefined ratio rather than a fabricated 100% hit rate.

```promql
sum(rate(favicon_generator_pipeline_cache_requests_total{pipeline="icons",layer="source",result="hit"}[5m]))
/
sum(rate(favicon_generator_pipeline_cache_requests_total{pipeline="icons",layer="source",result=~"hit|miss|revalidated"}[5m]))
```

If a result has never occurred, its label series may not exist yet. Dashboards
that require zero should use their usual zero-fill convention. Inspect write
failures separately from HTTP statuses:

```promql
sum by (pipeline, result) (
  rate(favicon_generator_pipeline_stage_duration_seconds_count{stage="write",result!="success"}[5m])
)
```

See [the benchmark guide](benchmarks.md) for reproducible cold/warm workloads,
including the browser's six PNG requests followed by a ZIP request. The recorded
baseline predates optional finished-artifact caching; compare the same workload
and cache configuration explicitly rather than treating source-cache hits as
finished-artifact reuse. Response privacy policies are unchanged.

## Finished artifacts

`favicon_generator_artifact_cache_requests_total{backend,result}` uses fixed
backend labels (`off`, `memory`, `redis`, `tiered`) and outcomes (`hit`, `miss`,
`bypass`, `error`). Configurable backends are `off`, `memory`, `redis` and
`tiered`. Each application request records one overall decision after the source has
passed authorization and image preflight. A hit avoids rendering/encoding and
ZIP packing, but still records the source checks and final response write.
There are no raw URLs, cache keys, source metadata or filenames in these labels.

- `hit`: a valid, unexpired finished artifact was reused, including after waiting
  for another request's successful synchronous fill.
- `miss`: an eligible product was generated after a missing entry. A generation
  failure still describes a cache miss and is never published.
- `bypass`: disabled caching, unsupported product/source policy, expired source
  sharing horizon, per-entry storage cap, or saturated fill coordination. The
  normal bounded generator handles the request.
- `error`: backend read/write failure, invalid stored content, a request canceled
  while accessing/waiting for the cache, or a current cached-output budget
  rejection. Availability failures generate uncached; caller cancellation and
  resource errors retain their normal HTTP result.

The memory backend owns a bounded LRU using httpcache-kit resources. A per-store
context-aware gate bounds simultaneous temporary wire/read copies. Logical
expiry and current source retention are checked on every read, independent of
any sweeper; there is no cache background worker. Deletion replaces a key with a
small, capacity-accounted tombstone instead of accumulating stale markers.
Response bodies retain `private, no-store` even when an internal artifact is reused.

The Redis backend bounds each operation, including lazy connection startup, by
its command budget and the parent request deadline. A cache-only timeout is a
backend failure; a parent cancellation is propagated. Redis connections recover
on later requests with a one-second failure cooldown and no reconnect worker.

In tiered mode, local hits avoid Redis. A remote miss or failure still permits
local warming during generation; a remote hit is copied into the bounded local
tier without changing its absolute expiry. Failed remote store operations also
record `backend="redis",result="error"` in addition to the overall
`backend="tiered"` decision. These layer errors can occur for lookup, write or
delete, so summing backends does not produce an HTTP request count. The HTTP
request metric remains the source of truth for request totals and status.
