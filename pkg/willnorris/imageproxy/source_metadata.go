package imageproxy

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SourceMetadata is an immutable snapshot for deciding whether an application
// may retain derived artifacts. It exposes no URL, credentials, or raw headers.
// Its zero value forbids shared caching. HTTP caching remains the transport's
// responsibility; this deliberately conservative policy is only for artifacts.
type SourceMetadata struct {
	digest  [sha256.Size]byte
	variant [sha256.Size]byte
	until   time.Time
}

func (m SourceMetadata) ContentDigest() [sha256.Size]byte { return m.digest }
func (m SourceMetadata) VariantDigest() [sha256.Size]byte { return m.variant }
func (m SourceMetadata) FreshUntil() time.Time            { return m.until }
func (m SourceMetadata) SharedCacheAllowed() bool         { return !m.until.IsZero() }

type sourceMetadataRequestKey struct{}
type sourceMetadataKey struct{}

// WithSourceMetadata has the same authorization, admission, deadline and image
// checks as WithSource. The callback and any artifact hit remain inside them.
// Legacy WithSource callers do not pay the hashing/metadata cost.
func (p *Proxy) WithSourceMetadata(ctx context.Context, source *url.URL, signature string, fn func(context.Context, []byte, SourceLimits, SourceMetadata) error) error {
	if fn == nil {
		return p.WithSource(ctx, source, signature, nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, sourceMetadataRequestKey{}, true)
	return p.WithSource(ctx, source, signature, func(ctx context.Context, data []byte, limits SourceLimits) error {
		metadata, _ := ctx.Value(sourceMetadataKey{}).(SourceMetadata)
		return fn(ctx, data, limits, metadata)
	})
}

func newSourceMetadata(resp *http.Response, data []byte, now time.Time) SourceMetadata {
	m := SourceMetadata{digest: sha256.Sum256(data)}
	if resp == nil || resp.Request == nil || resp.StatusCode != http.StatusOK {
		return m
	}
	request := resp.Request
	if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || len(resp.Header.Values("Set-Cookie")) != 0 {
		return m
	}
	for _, value := range resp.Header.Values("Vary") {
		for _, field := range strings.Split(value, ",") {
			field = strings.TrimSpace(field)
			if field != "" && !strings.EqualFold(field, "Accept") {
				return m
			}
		}
	}
	m.variant = sha256.Sum256([]byte(request.Header.Get("Accept") + "\x00" + resp.Header.Get("Content-Type")))
	directives := map[string]string{}
	for _, value := range resp.Header.Values("Cache-Control") {
		for _, field := range strings.Split(value, ",") {
			parts := strings.SplitN(strings.TrimSpace(field), "=", 2)
			name := strings.ToLower(strings.TrimSpace(parts[0]))
			if name == "" {
				continue
			}
			if _, duplicate := directives[name]; duplicate {
				return m
			}
			parameter := ""
			if len(parts) == 2 {
				parameter = strings.TrimSpace(parts[1])
			}
			directives[name] = parameter
		}
	}
	if value, public := directives["public"]; !public || value != "" {
		return m
	}
	for _, forbidden := range []string{"no-store", "private", "no-cache"} {
		if _, found := directives[forbidden]; found {
			return m
		}
	}
	// Require a stable origin Date. Deriving expiry from lookup time would let
	// repeated source-cache hits extend the lifetime of an old artifact.
	if len(resp.Header.Values("Date")) != 1 {
		return m
	}
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil || date.After(now) {
		return m
	}
	var age time.Duration
	if values := resp.Header.Values("Age"); len(values) != 0 {
		if len(values) != 1 {
			return m
		}
		var ok bool
		age, ok = cacheSeconds(values[0])
		if !ok {
			return m
		}
	}
	var until time.Time
	lifetime, hasLifetime := directives["s-maxage"]
	if !hasLifetime {
		lifetime, hasLifetime = directives["max-age"]
	}
	if hasLifetime {
		duration, ok := cacheSeconds(lifetime)
		if !ok {
			return m
		}
		until = date.Add(duration)
	} else {
		if len(resp.Header.Values("Expires")) != 1 {
			return m
		}
		until, err = http.ParseTime(resp.Header.Get("Expires"))
		if err != nil {
			return m
		}
	}
	// Subtracting Age from the Date-based deadline is conservative if both
	// already describe the same elapsed time, and never refreshes it on a hit.
	until = until.Add(-age)
	if until.After(now) {
		m.until = until
	}
	return m
}

func cacheSeconds(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	if value == "" {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds > int64((1<<63-1)/time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
