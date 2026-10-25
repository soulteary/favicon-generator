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

The service listens on `localhost:8080`. Request a remote image with optional
transforms in the path, for example:

```text
http://localhost:8080/32x32,png/https://example.com/logo.png
```

`/health-check` returns `OK`. The current implementation is an image processing
service; ICO generation and application icon bundles are not implemented yet.

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

Flags can also be configured through `IMAGEPROXY_` environment variables using
uppercase flag names, such as `IMAGEPROXY_MAXPIXELS` and `IMAGEPROXY_ALLOWHOSTS`.
Run `go run . -h` for all options. Configure embedded `imageproxy.Proxy` instances
before serving requests. Custom transports must enforce connection-time address
checks equivalent to the default transport.

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
Other image and cache libraries use versioned upstream Go modules.

The local `replace` directives for these two forks have no replacement version
metadata, so they are not fully covered by vulnerability version matching. A passing
scan does not establish that they have no known vulnerabilities; upstream changes
and the local security patches still need to be reviewed separately.
