# Favicon Generator / 图标生成器

Favicon.ico &amp; App Icon Generator.

网站图标，应用图标生成器。

![](.github/banner.png)

## Run

Requires Go 1.27.3 or later. Use the latest patch release of a supported Go
version for production builds.

```sh
go run . -cache memory:100:1h
```

The image service listens on `localhost:8080`; a separate management listener
uses `localhost:9090`. Request a remote image with optional transforms in the
path, for example:

```text
http://localhost:8080/32x32,png/https://example.com/logo.png
```

The current implementation is an image processing service; ICO generation and
application icon bundles are not implemented yet.

Print build information without starting either listener:

```sh
go run . -version
```

Normal development builds report `dev`. Release builds can set the fields used
by version-kit:

```sh
go build -ldflags '-X github.com/soulteary/version-kit/v4.Version=1.0.0 -X github.com/soulteary/version-kit/v4.Commit=COMMIT -X github.com/soulteary/version-kit/v4.BuildDate=BUILD_DATE' -o favicon-generator .
```

## Configuration

Explicit command-line flags take precedence over their `IMAGEPROXY_`
environment variables; unset values use defaults. Environment names use the
uppercase flag name without added separators, for example
`IMAGEPROXY_ADMINADDR`, `IMAGEPROXY_SHUTDOWNTIMEOUT` and `IMAGEPROXY_LOGLEVEL`.
An explicitly empty value is applied, so `-adminAddr=''` or an empty
`IMAGEPROXY_ADMINADDR` disables the management listener. CLI cache flags replace
the environment's cache configuration rather than adding tiers to it.

Malformed flags, positional arguments, invalid environment values, unsupported
log levels, invalid listener/base URL/allowlist syntax and unsafe resource
limits fail startup. Configuration errors omit input values, including credentials.
An invalid lower-priority environment value is ignored when that option was
explicitly supplied on the command line. `-version` does not consult unrelated
environment settings or start the service.

Caching is disabled unless configured. `-cache memory` selects 100 MB;
`-cache memory:100:1h` sets a capacity of 100,000,000 bytes and a one-hour age.
Repeat `-cache` for additional memory tiers, or put space-separated tiers in
`IMAGEPROXY_CACHE`. Capacities must be positive integer MB values without
overflow. An omitted or zero age disables expiration; positive fractional
seconds are rounded up to whole seconds. Unsupported backends, negative ages
and malformed cache specifications are rejected.

Run `go run . -h` for all options and their environment names.

## Health, metrics and logs

The public listener accepts `GET` and `HEAD` on these health routes:

- `/health-check`: the compatible plain-text `OK` liveness response.
- `/livez`: a JSON liveness response.
- `/readyz`: a JSON readiness response; returns `503` until startup completes
  and when shutdown begins.

Readiness becomes healthy only after every configured listener has bound. It
reflects the service lifecycle, rather than fetching an external image. The
management listener serves `/metrics`, `/livez` and `/readyz`; `/metrics` is
unavailable on the public listener. Keep the management listener on a trusted
network if changing its default loopback address. It has no application-level
authentication and no runtime log-level mutation endpoint.

```sh
curl http://localhost:8080/readyz
curl http://localhost:9090/metrics
```

Prometheus metrics use a service-owned registry with the
`favicon_generator_http_` prefix. They include request counts, latency, in-flight
requests and request/response sizes. Labels use fixed route names, bounded HTTP
method categories and response status codes; source URLs, query values and
signatures are never label values. Scraping `/metrics` does not record another
HTTP observation.

Logs are structured JSON written to standard error. `-logLevel info` is the
default; successful access events require `debug` or `trace`, while HTTP errors
are logged at `warn`. `-verbose` promotes `info` to `debug`; other selected log
levels are retained.
Request logs contain only a fixed route, method, status and duration. Internal
proxy and HTTP-server diagnostic text is omitted because it can contain source
URLs, signatures or panic values.

## Shutdown

`SIGINT` and `SIGTERM` first clear readiness and stop admitting requests. Both
HTTP servers drain concurrently within `-shutdownTimeout 15s`. If that grace
period expires, the service cancels active request contexts and closes the
connections, then waits for admitted handlers and server workers to finish.
An image operation already running may need to reach its cancellation boundary;
the grace period is not a hard process-exit deadline. If either listener fails
to bind, any listeners already opened are closed and startup fails.

## Security and resource limits

Remote images must use HTTP or HTTPS and resolve entirely to public IP addresses.
Private, loopback, link-local, reserved and transition addresses are blocked,
including after redirects. DNS results are checked at connection time and the
connection uses the checked IP directly. Environment HTTP proxies and automatic
AIA certificate downloads are not used; HTTPS uses normal certificate validation.
Fragments in upstream `Location` URLs are ignored during source retrieval and
cannot override the requested transform options. One redirect budget covers
cache hits, network responses and nested fetch clients. Request cancellation is
checked before cache lookups and redirect handling.

Use `-allowHosts example.com,*.example.org` to further restrict origins. Wildcards
match the named domain and its subdomains. Redirect targets must also match.
Use `IMAGEPROXY_SIGNATUREKEY` to require HMAC signatures. When both restrictions
are configured, both must pass; a signature cannot permit a private address.
Signatures use URL-safe base64 HMAC-SHA256 over the remote URL, optionally with
the normalized transform options in its fragment, and are passed as an
`s{signature}` transform option.

Defaults apply to each image request:

- `-timeout 15s`: deadline propagated through DNS, connection, response reading,
  decoding and encoding. Image operations check cancellation at their boundaries.
- `-maxResponseBytes 20971520`: 20 MiB input and output limit, including unknown
  response lengths, decompressed downloads, cache hits and encoding.
- `-maxPixels 16000000`: decoded, output and resize intermediate pixel limit.
  GIFs also share this budget across their full-canvas frames.
- `-maxFrames 100`: GIF frame limit, checked before decoding all frames.
- `-maxConcurrent 4`: active image request limit; excess requests receive `503`
  with `Retry-After`. Health checks remain available.

All limits must be positive. Images also have a fixed maximum side length of
32,768 pixels to bound resampling allocations. Oversized images return `413`,
disallowed origins or content types return `403`, invalid dimensions return
`400`, upstream failures return `502`, and expired requests return `504`.
Only supported raster image formats are accepted; invalid image data fails
instead of silently returning an untransformed image.

Image decoding uses `golang.org/x/image` v0.47.0, which includes fixes for known
TIFF metadata, tile and decompression allocation issues and excessive VP8L
Huffman table allocation. Pixel limits alone do not protect against allocations
inside an older decoder. WebP inputs also undergo strict RIFF and frame-header
validation before image decoding: actual VP8/VP8L dimensions must fit the pixel
budget and agree with the VP8X canvas. Malformed, truncated or inconsistent
headers are rejected, including when no transformation was requested.

Configure embedded `imageproxy.Proxy` instances before serving requests. Custom
transports must enforce connection-time address checks equivalent to the default
transport.

## Test

```sh
./scripts/test.sh
```

The script checks the installed Go toolchain and rejects versions below 1.27.3.
Automatic toolchain downloads and switching are disabled for these checks.
It runs race-enabled tests, static checks and builds for all three modules:
the root module, `pkg/willnorris/imageproxy` and `pkg/willnorris/gifresize`.
Running only `go test ./...` at the root excludes the two local forks' regression
suites. Tests use generated images, fixtures and controlled HTTP transports;
they do not require public network services.

GitHub Actions runs the same script on pushes and pull requests using Go 1.27.3
and stable Go. The stable job also uses the pinned
`govulncheck` v1.9.0 to check the actual main program and its reachable dependencies
against the official Go vulnerability database. This includes `golang.org/x/image`
and the build toolchain's standard library. Test-only paths and APIs unused by
the program are outside that scan.

## Dependencies

Only `imageproxy` and `gifresize` remain as local forks: they contain the proxy
security controls and cancellable GIF processing maintained in this repository.
Their regression suites run independently in CI alongside the main program.
Other image and cache libraries use versioned upstream Go modules. The executable
uses version-kit/v4 for build information, health-kit/v4 for lifecycle probes,
logger-kit/v3 for structured logs and metrics-kit/v3 for its private Prometheus
registry. These service integrations leave the image proxy's security and
resource-limit chain in place.

The local `replace` directives for these two forks have no replacement version
metadata, so they are not fully covered by vulnerability version matching. A passing
scan does not establish that they have no known vulnerabilities; upstream changes
and the local security patches still need to be reviewed separately.
