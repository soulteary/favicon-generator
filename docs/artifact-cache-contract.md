# Artifact cache contract

Source HTTP caching and generated icon caching are separate layers. The source
transport continues to own redirects, HTTP freshness, Vary, and revalidation.
The application artifact contract is deliberately independent of its backend.
The contract alone does not enable artifact caching or allocate a backend.

## Authorization and resource boundaries

An artifact lookup belongs inside `Proxy.WithSourceMetadata`, after the original
URL/signature checks, shared admission, source retrieval and image preflight.
The callback retains the existing request deadline and image slot through the
final response write. A hit cannot bypass current source authorization, encoded
bytes, pixel/frame limits, or the output pixel plan. It can avoid subsequent
transformation, encoding and ZIP assembly; it does not avoid all source checks.

The original `WithSource` API remains compatible and does not compute the extra
content digest. The opt-in metadata API returns a value with read-only getters,
not mutable headers or a source URL. Neither metadata nor keys are HTTP output.

## Conservative source retention

Shared derived caching requires an explicit `public` response, a valid origin
Date, and a future max-age/s-maxage or Expires deadline. No heuristic freshness
is used. Missing or ambiguous metadata, no-store/private/no-cache, credentials,
cookies, Set-Cookie, or Vary other than Accept disables retention. The current
source policy must permit a lookup as well as a publication, including after a
previously cacheable origin changes its policy.

The deadline is derived from the original Date and shortened by Age, never
rebased on lookup time. This is conservative when Date and Age already describe
the same elapsed interval. Repeated source-cache hits cannot slide artifact
retention forward. Application TTL is positive, at most 24 hours, and clipped
to the allowed source deadline. Revalidation may establish a new deadline in
accordance with the source HTTP cache's existing protocol behavior.

## Identity and payload

A versioned structured key hashes the original source identity, actual source
bytes, Accept/content-type variant, renderer version, product, relevant manifest
metadata, and current resource limits. Keys contain neither raw URLs nor
signatures. Generator changes that affect output must bump the renderer version.
Different sources and byte versions cannot alias by URL normalization or fragment
stripping. Filename and query validation still happen before this contract.

PNG, ICO, and ZIP are eligible products. Standalone manifests embed the current
source URL and signature and are generated per request. ZIP manifests contain
relative filenames; their name and short name participate in the ZIP key.

Backends use one binary envelope: eight-byte schema, expiry, 32-byte key digest,
32-byte body digest, a fixed product identifier, payload length, and raw bytes.
The fixed overhead is 89 bytes. A remote backend must enforce the wire limit
before retrieving a payload, not merely inspect its length after a full GET.
The digest detects corruption; backend ACL and transport protection remain part
of the trusted deployment boundary.

Reads validate schema, binding, digest, length and logical expiry. Request-level
validation additionally checks the product shape and current pixel/byte budget.
ZIP directory offsets, size, and exactly eight actual records are scanned
without allocation before archive/zip allocates a directory; the advertised
EOCD count alone is insufficient because the standard reader accepts counts
modulo 65536. The fixed product directory and aggregate member bytes are bounded.
Returned bodies own their storage rather than aliasing a mutable backend buffer.

## Backend lifecycle

`artifactStore` carries the request context and errors on every Get, Put and
Delete. Context cancellation is preserved rather than converted into a miss.
Backends expose Close; the service closes them after admitted handlers drain.
Cache misses or corrupt storage may fall back to normal bounded generation;
source authorization and request resource errors must retain their HTTP result.

Capacity, per-entry limits, transient copies and active request admission are
separate budgets. TTL is not a memory limit. Source buffers, a retained entry,
an owned hit snapshot and a response can coexist, and cumulative allocation
bytes are not peak resident memory. Local or Redis implementations must preserve
these boundaries, their fixed-label observations, and the response's existing
private/no-store policy.
