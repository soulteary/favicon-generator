package imageproxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregjones/httpcache"
)

func sourceURL(t *testing.T, value string) *url.URL {
	t.Helper()
	u, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func sourceSignature(key []byte, value string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func sourceResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("source operation did not finish")
		return nil
	}
}

func sourceEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("source operation did not start")
	}
}

func TestWithSourceUsesRawFetchPolicyAndOriginalSignature(t *testing.T) {
	data := policyPNG(t)
	u := sourceURL(t, "https://origin.test/image?version=1")
	key := []byte("test-signing-key")
	calls := 0
	var fetchDeadline time.Time
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if !isRawFetch(req.Context()) || req.URL.Fragment != "" || req.URL.RawFragment != "" {
			t.Error("source fetch was not marked as an original image")
		}
		if got := req.Header.Get("Accept"); got != "image/png, image/gif" {
			t.Errorf("Accept=%q", got)
		}
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("fetch has no deadline")
		}
		fetchDeadline = deadline
		if req.URL.Host == "origin.test" {
			return redirectResponse(req, "https://cdn.test/image#10000x10000,png"), nil
		}
		return policyResponse(req, data, "image/png"), nil
	}), nil)
	p.AllowHosts = []string{"origin.test", "cdn.test"}
	p.SignatureKeys = [][]byte{nil, []byte("old-key"), key}
	p.ContentTypes = []string{"image/png", "image/gif"}
	p.MaxResponseBytes, p.MaxPixels, p.MaxFrames = int64(len(data)), 4, 2
	called := false
	err := p.WithSource(context.Background(), u, sourceSignature(key, u.String()), func(ctx context.Context, got []byte, limits SourceLimits) error {
		called = true
		if !bytes.Equal(got, data) {
			t.Error("callback did not receive the unmodified source")
		}
		if limits != (SourceLimits{int64(len(data)), 4, 2}) {
			t.Errorf("limits=%+v", limits)
		}
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(fetchDeadline) {
			t.Error("callback does not share the fetch deadline")
		}
		return nil
	})
	if err != nil || !called || calls != 2 {
		t.Fatalf("error=%v, callback=%t, upstream calls=%d", err, called, calls)
	}
}

func TestWithSourceRejectsInvalidSourcesAndSignatures(t *testing.T) {
	var calls atomic.Int32
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return policyResponse(req, policyPNG(t), "image/png"), nil
	}), nil)
	p.AllowHosts = []string{"origin.test"}
	p.SignatureKeys = [][]byte{[]byte("key")}
	for _, value := range []string{
		"/relative.png", "ftp://origin.test/image", "http://user:secret@origin.test/image",
		"http://origin.test/image#1", "http://127.0.0.1/image", "http://[::ffff:127.0.0.1]/image",
		"http://other.test/image",
	} {
		t.Run(value, func(t *testing.T) {
			u := sourceURL(t, value)
			err := p.WithSource(context.Background(), u, sourceSignature([]byte("key"), u.String()), func(context.Context, []byte, SourceLimits) error {
				t.Error("forbidden source entered callback")
				return nil
			})
			if !errors.Is(err, ErrSourceForbidden) {
				t.Fatalf("error=%v, want ErrSourceForbidden", err)
			}
		})
	}
	for _, signature := range []string{"", "not base64!", sourceSignature([]byte("wrong-key"), "http://origin.test/image"), sourceSignature([]byte("key"), "http://origin.test/image#0x0")} {
		err := p.WithSource(context.Background(), sourceURL(t, "http://origin.test/image"), signature, func(context.Context, []byte, SourceLimits) error {
			t.Error("invalid signature entered callback")
			return nil
		})
		if !errors.Is(err, ErrSourceForbidden) {
			t.Fatalf("signature error=%v", err)
		}
	}
	for _, u := range []*url.URL{nil, {Scheme: "https", Host: "origin.test", Path: "/image", RawFragment: "1"}} {
		if err := p.WithSource(context.Background(), u, "", nil); !errors.Is(err, ErrSourceForbidden) {
			t.Errorf("invalid URL error=%v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}

func TestWithSourceRejectsUnusableResponses(t *testing.T) {
	data := policyPNG(t)
	for _, tt := range []struct {
		name, contentType string
		status            int
		data              []byte
		want              error
	}{
		{"missing", "image/png", 404, data, ErrSourceNotFound},
		{"upstream failure", "image/png", 500, data, ErrSourceUnavailable},
		{"not modified", "image/png", 304, nil, ErrSourceUnavailable},
		{"empty", "image/png", 204, nil, ErrSourceUnavailable},
		{"wrong type", "text/plain", 200, data, ErrSourceForbidden},
		{"invalid image", "image/png", 200, []byte("not an image"), ErrSourceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &countedBody{Reader: bytes.NewReader(tt.data)}
			p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp := policyResponse(req, tt.data, tt.contentType)
				resp.StatusCode, resp.Body = tt.status, body
				return resp, nil
			}), nil)
			err := p.WithSource(context.Background(), sourceURL(t, "https://origin.test/image?token=private"), "", func(context.Context, []byte, SourceLimits) error {
				t.Error("unusable response entered callback")
				return nil
			})
			if !errors.Is(err, tt.want) || !body.closed {
				t.Errorf("error=%v, want %v; body closed=%t", err, tt.want, body.closed)
			}
		})
	}
	p := NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream secret diagnostics")
	}), nil)
	err := p.WithSource(context.Background(), sourceURL(t, "https://origin.test/image?token=private"), "", func(context.Context, []byte, SourceLimits) error { return nil })
	if !errors.Is(err, ErrSourceUnavailable) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe upstream error=%v", err)
	}
}

func TestWithSourceRechecksCachedPolicyAndBudgets(t *testing.T) {
	for _, tt := range []struct {
		name  string
		data  []byte
		setup func(*Proxy)
		want  error
	}{
		{"bytes", policyPNG(t), func(p *Proxy) { p.MaxResponseBytes = 8 }, ErrResponseTooLarge},
		{"pixels", policyPNG(t), func(p *Proxy) { p.MaxPixels = 3 }, ErrImageTooLarge},
		{"frames", limitTestGIF(t, 1, 1, 3), func(p *Proxy) { p.MaxFrames = 2 }, ErrTooManyFrames},
		{"aggregate GIF pixels", limitTestGIF(t, 2, 2, 3), func(p *Proxy) { p.MaxPixels = 8 }, ErrImageTooLarge},
		{"content type", policyPNG(t), func(p *Proxy) { p.ContentTypes = []string{"image/jpeg"} }, ErrSourceForbidden},
		{"allowlist", policyPNG(t), func(p *Proxy) { p.AllowHosts = []string{"other.test"} }, ErrSourceForbidden},
		{"signature", policyPNG(t), func(p *Proxy) { p.SignatureKeys = [][]byte{[]byte("key")} }, ErrSourceForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := httpcache.NewMemoryCache()
			calls := 0
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				contentType := http.DetectContentType(tt.data)
				resp := policyResponse(req, tt.data, contentType)
				resp.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
				resp.Header.Set("Cache-Control", "public, max-age=3600")
				return resp, nil
			})
			u := sourceURL(t, "https://origin.test/image")
			warm := NewProxy(transport, cache)
			if err := warm.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); err != nil {
				t.Fatal(err)
			}
			p := NewProxy(transport, cache)
			tt.setup(p)
			err := p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error {
				t.Error("cached response bypassed current policy")
				return nil
			})
			if !errors.Is(err, tt.want) || calls != 1 {
				t.Errorf("error=%v, want %v; upstream calls=%d, want 1", err, tt.want, calls)
			}
		})
	}
}

func TestWithSourceChecksCachedRedirectsBeforeTargetCache(t *testing.T) {
	const origin = "https://origin.test/image"
	cache := &redirectCache{Cache: httpcache.NewMemoryCache()}
	cacheRedirect(t, cache, origin, "https://blocked.test/image#1")
	p := NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("blocked cached redirect reached upstream")
		return nil, nil
	}), cache)
	p.AllowHosts = []string{"origin.test"}
	err := p.WithSource(context.Background(), sourceURL(t, origin), "", func(context.Context, []byte, SourceLimits) error {
		t.Error("blocked redirect entered callback")
		return nil
	})
	if !errors.Is(err, ErrSourceForbidden) || cache.reads != 1 {
		t.Fatalf("error=%v, cache reads=%d; want forbidden before reading target cache", err, cache.reads)
	}
}

func TestWithSourceBoundsCachedRedirectLoop(t *testing.T) {
	const origin = "https://origin.test/image"
	cache := &redirectCache{Cache: httpcache.NewMemoryCache()}
	cacheRedirect(t, cache, origin, origin+"#10000x10000,png")
	p := NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("cached redirect loop reached upstream")
		return nil, nil
	}), cache)
	err := p.WithSource(context.Background(), sourceURL(t, origin), "", func(context.Context, []byte, SourceLimits) error {
		t.Error("redirect loop entered callback")
		return nil
	})
	if !errors.Is(err, ErrSourceUnavailable) || cache.reads > maxRedirects+1 {
		t.Fatalf("error=%v, cache reads=%d", err, cache.reads)
	}
}

func TestWithSourceSharesProxyConcurrencySlot(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return policyResponse(req, data, "image/png"), nil
	}), nil)
	p.MaxConcurrentRequests = 1
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	u := sourceURL(t, "https://origin.test/image")
	go func() {
		result <- p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error {
			close(entered)
			<-release
			return nil
		})
	}()
	sourceEntered(t, entered)
	if err := p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); !errors.Is(err, ErrSourceBusy) {
		t.Errorf("second source request error=%v, want busy", err)
	}
	if got := servePolicy(p, "/https://origin.test/image").Code; got != http.StatusServiceUnavailable {
		t.Errorf("proxy request status=%d, want 503", got)
	}
	close(release)
	if err := sourceResult(t, result); err != nil {
		t.Fatal(err)
	}
	if got := servePolicy(p, "/https://origin.test/image").Code; got != http.StatusOK {
		t.Errorf("released slot status=%d, want 200", got)
	}

	entered, release = make(chan struct{}), make(chan struct{})
	p = NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return policyResponse(req, data, "image/png"), nil
	}), nil)
	p.MaxConcurrentRequests = 1
	go func() {
		if status := servePolicy(p, "/https://origin.test/image").Code; status != http.StatusOK {
			result <- errors.New("proxy request failed")
		} else {
			result <- nil
		}
	}()
	sourceEntered(t, entered)
	if err := p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); !errors.Is(err, ErrSourceBusy) {
		t.Errorf("source request while proxy active: %v, want busy", err)
	}
	close(release)
	if err := sourceResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestWithSourceCallbackTimeoutKeepsSlotUntilReturn(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return policyResponse(req, data, "image/png"), nil
	}), nil)
	p.MaxConcurrentRequests, p.Timeout = 1, 20*time.Millisecond
	expired, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	u := sourceURL(t, "https://origin.test/image")
	go func() {
		result <- p.WithSource(context.Background(), u, "", func(ctx context.Context, _ []byte, _ SourceLimits) error {
			<-ctx.Done()
			close(expired)
			<-release
			return nil
		})
	}()
	sourceEntered(t, expired)
	if err := p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); !errors.Is(err, ErrSourceBusy) {
		t.Errorf("expired callback released its slot early: %v", err)
	}
	close(release)
	if err := sourceResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("callback error=%v, want deadline", err)
	}
}

func TestWithSourceCancellationAndFetchTimeout(t *testing.T) {
	data := policyPNG(t)
	u := sourceURL(t, "https://origin.test/image")
	ctx, cancel := context.WithCancel(context.Background())
	p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return policyResponse(req, data, "image/png"), nil
	}), nil)
	callbackError := errors.New("callback result")
	err := p.WithSource(ctx, u, "", func(context.Context, []byte, SourceLimits) error {
		cancel()
		return callbackError
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation error=%v", err)
	}
	if err := p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error { return callbackError }); !errors.Is(err, callbackError) {
		t.Fatalf("callback error was lost: %v", err)
	}
	p = NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}), nil)
	p.Timeout = 10 * time.Millisecond
	err = p.WithSource(context.Background(), u, "", func(context.Context, []byte, SourceLimits) error {
		t.Error("timed out fetch entered callback")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch error=%v, want deadline", err)
	}
	cache := &redirectCache{Cache: httpcache.NewMemoryCache()}
	p = NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("cancelled source reached upstream")
		return nil, nil
	}), cache)
	if err := p.WithSource(ctx, u, "", nil); !errors.Is(err, context.Canceled) || cache.reads != 0 {
		t.Fatalf("cancelled request: error=%v, cache reads=%d", err, cache.reads)
	}
}

func TestTransformWithBudgetUsesActualByteAndAnimationLimits(t *testing.T) {
	data := limitTestPNG(t, 20, 20)
	limits := SourceLimits{int64(len(data)), 1000, 10}
	if _, err := TransformWithBudget(context.Background(), data, Options{Format: "bmp"}, limits); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("encoded output error=%v, want configured byte limit", err)
	}
	limits.MaxResponseBytes--
	if _, err := TransformWithBudget(context.Background(), data, Options{}, limits); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("original image error=%v, want configured byte limit", err)
	}
	animation := limitTestGIF(t, 2, 2, 3)
	if _, err := TransformWithBudget(context.Background(), animation, Options{Format: "png"}, SourceLimits{MaxFrames: 2}); !errors.Is(err, ErrTooManyFrames) {
		t.Fatalf("flattened GIF skipped source frame budget: %v", err)
	}
	out, err := TransformWithBudget(context.Background(), animation, Options{Format: "png"}, SourceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "png" || config.Width != 2 || config.Height != 2 {
		t.Fatalf("static output: %s %dx%d, error=%v", format, config.Width, config.Height, err)
	}
}
