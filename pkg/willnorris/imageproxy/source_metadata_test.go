package imageproxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gregjones/httpcache"
)

func metadataResponse(now time.Time) *http.Response {
	req, _ := http.NewRequest("GET", "https://origin.test/logo", nil)
	req.Header.Set("Accept", "image/png")
	return &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Cache-Control": {"public, max-age=120"}, "Date": {now.UTC().Format(http.TimeFormat)}, "Content-Type": {"image/png"}}}
}

func TestSourceMetadataRetentionPolicy(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		mutate  func(*http.Response)
		allowed bool
	}{
		{"public", func(*http.Response) {}, true},
		{"accept variant", func(r *http.Response) { r.Header.Set("Vary", "Accept") }, true},
		{"implicit public", func(r *http.Response) { r.Header.Set("Cache-Control", "max-age=120") }, false},
		{"private", func(r *http.Response) { r.Header.Set("Cache-Control", "public, private, max-age=120") }, false},
		{"no-store", func(r *http.Response) { r.Header.Set("Cache-Control", "public, no-store, max-age=120") }, false},
		{"no-cache", func(r *http.Response) { r.Header.Set("Cache-Control", "public, no-cache, max-age=120") }, false},
		{"unknown freshness", func(r *http.Response) { r.Header.Set("Cache-Control", "public") }, false},
		{"expires", func(r *http.Response) {
			r.Header.Set("Cache-Control", "public")
			r.Header.Set("Expires", now.Add(time.Minute).Format(http.TimeFormat))
		}, true},
		{"zero", func(r *http.Response) { r.Header.Set("Cache-Control", "public, max-age=0") }, false},
		{"negative", func(r *http.Response) { r.Header.Set("Cache-Control", "public, max-age=-1") }, false},
		{"overflow", func(r *http.Response) { r.Header.Set("Cache-Control", "public, max-age=9223372036854775807") }, false},
		{"duplicate", func(r *http.Response) { r.Header.Add("Cache-Control", "max-age=900") }, false},
		{"missing date", func(r *http.Response) { r.Header.Del("Date") }, false},
		{"future date", func(r *http.Response) { r.Header.Set("Date", now.Add(time.Second).Format(http.TimeFormat)) }, false},
		{"expired date", func(r *http.Response) { r.Header.Set("Date", now.Add(-time.Hour).Format(http.TimeFormat)) }, false},
		{"bad age", func(r *http.Response) { r.Header.Set("Age", "SECRET") }, false},
		{"authorization", func(r *http.Response) { r.Request.Header.Set("Authorization", "SECRET") }, false},
		{"cookie", func(r *http.Response) { r.Request.Header.Set("Cookie", "SECRET") }, false},
		{"set-cookie", func(r *http.Response) { r.Header.Set("Set-Cookie", "SECRET") }, false},
		{"vary star", func(r *http.Response) { r.Header.Set("Vary", "*") }, false},
		{"vary cookie", func(r *http.Response) { r.Header.Set("Vary", "Cookie") }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := metadataResponse(now)
			test.mutate(r)
			m := newSourceMetadata(r, []byte("source"), now)
			if m.SharedCacheAllowed() != test.allowed {
				t.Fatalf("allowed=%v want %v", m.SharedCacheAllowed(), test.allowed)
			}
			if m.ContentDigest() != sha256.Sum256([]byte("source")) {
				t.Fatal("missing source digest")
			}
		})
	}
	if (SourceMetadata{}).SharedCacheAllowed() {
		t.Fatal("zero metadata permits retention")
	}
}

func TestSourceMetadataDeadlineDoesNotSlideOnCacheHit(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	r := metadataResponse(now)
	r.Header.Set("Age", "10")
	first := newSourceMetadata(r, []byte("source"), now)
	later := newSourceMetadata(r, []byte("source"), now.Add(time.Minute))
	if !first.FreshUntil().Equal(now.Add(110*time.Second)) || !later.FreshUntil().Equal(first.FreshUntil()) {
		t.Fatal("derived deadline slid with lookup time")
	}
	r.Header.Set("Cache-Control", "public, max-age=120, s-maxage=30")
	if got := newSourceMetadata(r, nil, now).FreshUntil(); !got.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("s-maxage deadline=%v", got)
	}
	r.Request.Header.Set("Accept", "image/gif")
	if newSourceMetadata(r, nil, now).VariantDigest() == first.VariantDigest() {
		t.Fatal("Accept variants share identity")
	}
}

func TestWithSourceMetadataRetainsSecurityAndSlot(t *testing.T) {
	data := policyPNG(t)
	calls := 0
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		r := cacheableObserverResponse(req, data)
		r.Header.Set("Cache-Control", "public, max-age=3600")
		return r, nil
	}), httpcache.NewMemoryCache())
	p.MaxConcurrentRequests = 1
	p.AllowHosts = []string{"origin.test"}
	p.SignatureKeys = [][]byte{[]byte("key")}
	u := sourceURL(t, "https://origin.test/logo?token=SECRET")
	signature := sourceSignature([]byte("key"), u.String())
	var deadline time.Time
	for range 2 {
		err := p.WithSourceMetadata(context.Background(), u, signature, func(ctx context.Context, got []byte, limits SourceLimits, m SourceMetadata) error {
			if m.ContentDigest() != sha256.Sum256(data) || !m.SharedCacheAllowed() {
				t.Fatal("metadata missing on fresh source or cache hit")
			}
			if deadline.IsZero() {
				deadline = m.FreshUntil()
			} else if !deadline.Equal(m.FreshUntil()) {
				t.Fatal("source-cache hit extended artifact retention")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("callback lost deadline")
			}
			if err := p.WithSource(ctx, u, signature, func(context.Context, []byte, SourceLimits) error { return nil }); !errors.Is(err, ErrSourceBusy) {
				t.Fatalf("metadata callback lost slot: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("origin fetches=%d", calls)
	}
	if err := p.WithSourceMetadata(context.Background(), u, "wrong", func(context.Context, []byte, SourceLimits, SourceMetadata) error {
		t.Fatal("invalid signature reached metadata callback")
		return nil
	}); !errors.Is(err, ErrSourceForbidden) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.WithSourceMetadata(ctx, u, signature, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
