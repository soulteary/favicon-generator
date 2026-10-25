# Redis artifact backend

The Redis backend implements the same context-aware `artifactStore` contract as
local artifact storage. It stores generated PNG, ICO and ZIP artifacts rather
than raw origin HTTP responses. Source caching and its HTTP freshness, redirect,
signature and resource checks remain in the existing image proxy. The artifact
service must authorize and validate the source before it looks up an artifact.
Standalone signed manifests are excluded from the artifact contract.

## Client and lifetime

`newRedisArtifactStore` uses redis-kit v1.8.0's
`client.NewUniversalClientContext`. It accepts standalone, Sentinel and Cluster
client configuration, including distinct Redis and Sentinel ACL credentials and
certificate-verified TLS. TLS configurations are cloned and require TLS 1.2 or
later; insecure certificate verification is rejected. Cluster database selection
must remain zero.

The default artifact client has eight pooled connections per Redis node and no
minimum idle connections. Every command has a 250 ms budget by default, capped
by the request context; configuration accepts positive budgets up to five
seconds. Command context deadlines are enabled, retries are disabled, and socket
read/write and pool-wait timeouts use the same operation budget. Startup also
observes the caller context and the configured dial timeout.

An internal command-budget expiration is a backend-unavailable error so the
artifact service can fall back to local storage or generation. Caller
cancellation or deadline expiration retains its context error and must not
become a cache miss or start a replacement generation. Errors do not include
addresses, credentials, keys or raw server messages. `Close` is idempotent and
is called after admitted handlers have drained; the backend starts no
application-owned background workers.

## Storage bounds and consistency

Keys contain only a validated namespace and the artifact contract's lowercase
SHA256 identity. Values use its versioned binary envelope, avoiding JSON/base64
copies. One atomic Lua script checks Redis type and `STRLEN` before calling GET.
An oversized value returns only a refusal code, so the client does not first
allocate and download the oversized payload. A separate STRLEN followed by GET
would not provide this guarantee during concurrent writes.

After the bounded read, the backend validates envelope shape, key binding, body
length and checksum, and expiry. The artifact service still validates filename,
format and current output limits. Unknown schema, wrong Redis types, corruption
and oversized values remain distinct from misses. An expired valid envelope is
a miss even if Redis still retains it. A checksum detects corruption rather
than authenticity against an operator who can rewrite the value and checksum.

SET writes a complete value and absolute expiry in one command; expiry is
rounded down to milliseconds and checked again in the envelope. Queueing cannot
extend the stored lifetime. No automatic read-then-delete eviction is performed:
a late reader must not delete a newer replacement. Explicit Delete affects only
the requested namespaced key.

Content-addressed artifact keys make concurrent valid fills interchangeable;
fill coordination belongs to the common artifact service. This backend does not
claim a distributed lease or exactly-once generation guarantee. Set a Redis
`maxmemory` and eviction policy appropriate to a cache deployment: per-item
limits and TTL do not bound total Redis storage, and an instance-wide maxmemory
is not a quota for one key prefix. Do not expose Redis to untrusted clients.

## Integration tests

The dedicated Redis CI job requires a real service, pinned to the official
Redis 8.4.7 multi-platform index:

```
docker.io/library/redis@sha256:6a12a187547eb2eac4da4528f9d9787d69f3848e513480baedfd75e3b2ac393e
```

Against a disposable local instance, run:

```sh
REDIS_TEST_ADDR=127.0.0.1:6379 REDIS_TEST_REQUIRED=1 \
  go test -mod=readonly -race -count=1 -timeout=2m -run '^TestRedisArtifact' .
```

`REDIS_TEST_USERNAME` and `REDIS_TEST_PASSWORD` optionally authenticate the test
administrator. The suite needs ACL administration and CLIENT PAUSE permission;
it must not run against a production server. Tests use random private prefixes
and clean only their own keys and ACL user. They exercise actual Lua refusal,
TTL, envelope corruption, snapshots, concurrent operations, named ACL users,
command deadlines and recovery. TLS tests carry real Redis commands through a
joined local TLS terminator and verify both trusted and untrusted certificates.
Without an address, the real-service tests skip locally; the CI job sets
`REDIS_TEST_REQUIRED=1`, making a missing address or unavailable service fail.
