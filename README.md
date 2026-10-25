# Favicon Generator / 图标生成器

Favicon.ico &amp; App Icon Generator.

网站图标，应用图标生成器。

![](.github/banner.png)

## Run

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

Flags can also be configured through `IMAGEPROXY_` environment variables using
uppercase flag names, such as `IMAGEPROXY_MAXPIXELS` and `IMAGEPROXY_ALLOWHOSTS`.
Run `go run . -h` for all options. Configure embedded `imageproxy.Proxy` instances
before serving requests. Custom transports must enforce connection-time address
checks equivalent to the default transport.

## Test

```sh
./scripts/test.sh
```

The script runs race-enabled tests, static checks and builds for the root module
and every nested module under `pkg`. Running only `go test ./...` at the root
does not test those modules. Tests use generated images and local HTTP/TLS
servers and do not require public network services. Optional tests against a
real memcached server retain their existing availability checks.

GitHub Actions runs the same script on pushes and pull requests using stable Go.
