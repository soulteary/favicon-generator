package imageproxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregjones/httpcache"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func policyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func policyResponse(r *http.Request, data []byte, contentType string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {contentType}},
		Body:   io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r,
	}
}

func servePolicy(p *Proxy, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestProxyBlocksUnsafeDestinations(t *testing.T) {
	var calls atomic.Int32
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return policyResponse(r, nil, "image/png"), nil
	}), nil)
	for _, target := range []string{
		"http://127.0.0.1/private.png", "http://10.0.0.1/private.png",
		"http://169.254.169.254/latest", "http://[::1]/private.png",
		"http://[::ffff:127.0.0.1]/private.png", "http://localhost/private.png",
		"http://user:secret@example.com/private.png",
	} {
		if got := servePolicy(p, "/"+target).Code; got != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", target, got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe destination reached transport")
	}
}

func TestProxyChecksEveryRedirect(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/secret", "http://other.test/image"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: http.NoBody, Request: r}, nil
			}), nil)
			p.AllowHosts = []string{"origin.test"}
			if got := servePolicy(p, "/16,png/http://origin.test/image").Code; got != http.StatusForbidden {
				t.Fatalf("redirect: got %d, want 403", got)
			}
			if calls.Load() != 1 {
				t.Fatalf("redirect reached transport: calls=%d", calls.Load())
			}
		})
	}
}

func TestProxyRequiresConfiguredSignatureAndAllowlist(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return policyResponse(r, data, "image/png"), nil
	}), nil)
	p.SignatureKeys = [][]byte{[]byte("secret")}
	p.AllowHosts = []string{"origin.test"}
	if got := servePolicy(p, "/http://origin.test/image").Code; got != 403 {
		t.Fatalf("missing signature: got %d", got)
	}
	for _, host := range []string{"origin.test", "other.test"} {
		url := "http://" + host + "/image"
		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write([]byte(url))
		path := "/s" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "/" + url
		want := 200
		if host == "other.test" {
			want = 403
		}
		if got := servePolicy(p, path).Code; got != want {
			t.Errorf("signed %s: got %d, want %d", host, got, want)
		}
	}
}

type countedBody struct {
	*bytes.Reader
	read   int
	closed bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *countedBody) Close() error { b.closed = true; return nil }

func TestProxyRejectsBodiesBeforeBuffering(t *testing.T) {
	for _, test := range []struct {
		name, contentType   string
		length              int64
		limit               int64
		wantStatus, maxRead int
	}{
		{"declared oversize", "image/png", 4096, 64, 413, 0},
		{"unknown oversize", "image/png", -1, 64, 413, 65},
		{"sniff oversize", "", -1, 4, 413, 5},
		{"forbidden type", "text/plain", -1, 64, 403, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &countedBody{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 4096))}
			p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp := policyResponse(r, nil, test.contentType)
				resp.Body, resp.ContentLength = body, test.length
				return resp, nil
			}), nil)
			p.MaxResponseBytes = test.limit
			if got := servePolicy(p, "/http://origin.test/image").Code; got != test.wantStatus {
				t.Errorf("got %d, want %d", got, test.wantStatus)
			}
			if body.read > test.maxRead || !body.closed {
				t.Errorf("read=%d (max %d), closed=%t", body.read, test.maxRead, body.closed)
			}
		})
	}
}

func TestProxyAcceptsExactByteLimit(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return policyResponse(r, data, "image/png"), nil
	}), nil)
	p.MaxResponseBytes = int64(len(data))
	w := servePolicy(p, "/http://origin.test/image")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("exact limit: status=%d, bytes=%d", w.Code, w.Body.Len())
	}
}

func TestProxyDoesNotFallBackOnPixelLimit(t *testing.T) {
	data := policyPNG(t)
	for _, path := range []string{"/http://origin.test/image", "/100x100/http://origin.test/image"} {
		p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return policyResponse(r, data, "image/png"), nil
		}), nil)
		p.MaxPixels = 3
		if got := servePolicy(p, path).Code; got != 413 {
			t.Errorf("%s: got %d, want 413", path, got)
		}
	}
}

func TestProxyReportsConvertedFormat(t *testing.T) {
	data := policyPNG(t)
	for _, format := range []string{"png", "jpeg", "tiff"} {
		t.Run(format, func(t *testing.T) {
			p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return policyResponse(r, data, "image/png"), nil
			}), nil)
			w := servePolicy(p, "/1,"+format+"/http://origin.test/image")
			if w.Code != 200 || w.Header().Get("Content-Type") != "image/"+format {
				t.Fatalf("status=%d type=%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			config, actualFormat, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
			if err != nil || actualFormat != format || config.Width != 1 || config.Height != 1 {
				t.Fatalf("decoded %s %dx%d, error=%v", actualFormat, config.Width, config.Height, err)
			}
		})
	}
}

func TestProxyBoundsEncodedOutput(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return policyResponse(r, data, "image/png"), nil
	}), nil)
	p.MaxResponseBytes = int64(len(data))
	// TIFF encoding is larger than this tiny PNG, even at the same dimensions.
	if got := servePolicy(p, "/2,tiff/http://origin.test/image").Code; got != 413 {
		t.Fatalf("encoded output limit: got %d, want 413", got)
	}
}

func TestProxyPropagatesTimeoutAndCancellation(t *testing.T) {
	for _, cancelIncoming := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelIncoming), func(t *testing.T) {
			stopped := make(chan struct{})
			started := make(chan struct{})
			p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				close(started)
				<-r.Context().Done()
				close(stopped)
				return nil, r.Context().Err()
			}), nil)
			p.Timeout = 30 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelIncoming {
				p.Timeout = time.Second
				go func() { <-started; cancel() }()
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest("GET", "/http://origin.test/image", nil).WithContext(ctx))
			if w.Code != 504 {
				t.Fatalf("got %d, want 504", w.Code)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("upstream work outlived handler")
			}
		})
	}
}

func TestProxyBoundsConcurrency(t *testing.T) {
	data := policyPNG(t)
	started, release := make(chan struct{}), make(chan struct{})
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return policyResponse(r, data, "image/png"), nil
	}), nil)
	p.MaxConcurrentRequests = 1
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- servePolicy(p, "/http://origin.test/image") }()
	<-started
	second := servePolicy(p, "/http://origin.test/image")
	if second.Code != 503 || second.Header().Get("Retry-After") == "" {
		t.Errorf("saturated proxy: %d, headers=%v", second.Code, second.Header())
	}
	if got := servePolicy(p, "/health-check").Code; got != 200 {
		t.Errorf("health check: %d", got)
	}
	close(release)
	if got := (<-done).Code; got != 200 {
		t.Errorf("first request: %d", got)
	}
}

func TestProxyConcurrentRequestsLeaveSharedClientUnchanged(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return policyResponse(r, data, "image/png"), nil
	}), nil)
	p.MaxConcurrentRequests = 32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := servePolicy(p, "/1/http://origin.test/image").Code; got != 200 {
				t.Errorf("got %d", got)
			}
		}()
	}
	wg.Wait()
	if p.Client.CheckRedirect != nil {
		t.Fatal("shared client was modified")
	}
}

func TestProxyRechecksCachedImageLimits(t *testing.T) {
	data := policyPNG(t)
	cache := httpcache.NewMemoryCache()
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := policyResponse(r, data, "image/png")
		resp.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
		resp.Header.Set("Cache-Control", "public, max-age=3600")
		return resp, nil
	})
	first := NewProxy(transport, cache)
	if got := servePolicy(first, "/http://origin.test/image").Code; got != 200 {
		t.Fatalf("first: %d", got)
	}
	second := NewProxy(transport, cache)
	second.MaxPixels = 3
	if got := servePolicy(second, "/http://origin.test/image").Code; got != 413 {
		t.Fatalf("cached: %d", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("cache missed: upstream calls=%d", calls.Load())
	}
}

func TestTransformingTransportPreservesRequestURL(t *testing.T) {
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return policyResponse(r, data, "image/png"), nil
	}), nil)
	req, err := http.NewRequest("GET", "http://origin.test/image#1", nil)
	before := req.URL.String()
	resp, err := p.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if got := req.URL.String(); got != before {
		t.Errorf("URL changed: %q -> %q", before, got)
	}
}

func TestBoundedBodyRetainsLimitError(t *testing.T) {
	body := &boundedBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("12345"))), remaining: 2}
	buf := make([]byte, 8)
	if n, err := body.Read(buf); n != 2 || !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("first read: %d %v", n, err)
	}
	if _, err := body.Read(buf); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("second read: %v", err)
	}
}
