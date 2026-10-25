package imageproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"
	"time"

	"github.com/gregjones/httpcache"
)

func redirectResponse(req *http.Request, target string) *http.Response {
	resp := policyResponse(req, nil, "")
	resp.StatusCode, resp.Status = http.StatusFound, "302 Found"
	resp.Header.Set("Location", target)
	resp.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	resp.Header.Set("Cache-Control", "public, max-age=3600")
	return resp
}

type redirectCache struct {
	Cache
	reads int
	onGet func(string)
}

func (c *redirectCache) Get(key string) ([]byte, bool) {
	c.reads++
	if c.onGet != nil {
		c.onGet(key)
	}
	// Bound the old recursive implementation, so a regression fails without
	// letting the test process run out of stack or memory.
	if c.reads > 2*maxRedirects+2 {
		return nil, false
	}
	return c.Cache.Get(key)
}

func cacheRedirect(t *testing.T, cache Cache, origin, target string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := httputil.DumpResponse(redirectResponse(req, target), true)
	if err != nil {
		t.Fatal(err)
	}
	cache.Set(origin, data)
}

func TestProxyIgnoresUpstreamRedirectFragments(t *testing.T) {
	data := policyPNG(t)
	for _, fragment := range []string{"", "#1", "#NaN", "#100x100,png", "#%31"} {
		t.Run(fragment, func(t *testing.T) {
			calls := 0
			p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Fragment != "" || req.URL.RawFragment != "" {
					t.Errorf("upstream request retained fragment: %s", req.URL)
				}
				if req.URL.Host == "origin.test" {
					return redirectResponse(req, "http://cdn.test/image"+fragment), nil
				}
				if req.URL.Host != "cdn.test" {
					t.Fatalf("unexpected destination: %s", req.URL)
				}
				return policyResponse(req, data, "image/png"), nil
			}), nil)
			p.AllowHosts = []string{"origin.test", "cdn.test"}
			w := servePolicy(p, "/1,png/http://origin.test/image")
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, body=%q", w.Code, w.Body.String())
			}
			config, format, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
			if err != nil || format != "png" || config.Width != 1 || config.Height != 1 {
				t.Fatalf("requested transform lost: %s %dx%d, error=%v", format, config.Width, config.Height, err)
			}
			if calls != 2 {
				t.Fatalf("upstream calls=%d, want 2", calls)
			}
		})
	}
}

func TestProxyBoundsRedirectLoopsWithFragments(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached=%t", cached), func(t *testing.T) {
			const origin = "http://origin.test/image"
			cache := &redirectCache{Cache: httpcache.NewMemoryCache()}
			var selectedCache Cache
			if cached {
				cacheRedirect(t, cache, origin, origin+"#1")
				selectedCache = cache
			}
			calls := 0
			p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if cached || calls > maxRedirects {
					return nil, errors.New("redirect escaped its budget or cache")
				}
				return redirectResponse(req, origin+"#1"), nil
			}), selectedCache)
			w := servePolicy(p, "/1/"+origin)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("redirect loop returned %d", w.Code)
			}
			if cached {
				if calls != 0 || cache.reads > maxRedirects+1 {
					t.Fatalf("cached loop: upstream calls=%d, cache reads=%d", calls, cache.reads)
				}
			} else if calls != maxRedirects {
				t.Fatalf("upstream calls=%d, want %d", calls, maxRedirects)
			}
		})
	}
}

func TestRedirectBudgetIsSharedAcrossClients(t *testing.T) {
	policy := (&Proxy{FollowRedirects: true}).fetchPolicy()
	first, second := clientForRequest(&http.Client{}, policy), clientForRequest(&http.Client{}, policy)
	req := httptest.NewRequest(http.MethodGet, "http://origin.test/image", nil)
	// Each independent client sees a short local chain. Their combined count
	// must still reach the request's shared limit.
	for i := 1; i < maxRedirects; i++ {
		if err := first.CheckRedirect(req, []*http.Request{req}); err != nil {
			t.Fatalf("redirect %d: %v", i, err)
		}
	}
	if err := second.CheckRedirect(req, []*http.Request{req}); !errors.Is(err, errTooManyRedirects) {
		t.Fatalf("second client's redirect = %v, want errTooManyRedirects", err)
	}
}

func TestProxyRedirectFragmentsStillEnforceURLPolicy(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/image#1", "http://blocked.test/image#NaN"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			p := NewProxy(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return redirectResponse(req, target), nil
			}), nil)
			p.AllowHosts = []string{"origin.test"}
			if got := servePolicy(p, "/1/http://origin.test/image").Code; got != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", got)
			}
			if calls != 1 {
				t.Fatalf("blocked target reached transport: calls=%d", calls)
			}
		})
	}
}

func TestProxyCancelledRequestSkipsCacheAndNetwork(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if expired {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()
			cache := &redirectCache{Cache: httpcache.NewMemoryCache()}
			p := NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("cancelled request reached upstream")
				return nil, nil
			}), cache)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/1/http://origin.test/image", nil).WithContext(ctx))
			if w.Code != http.StatusGatewayTimeout || cache.reads != 0 {
				t.Fatalf("status=%d, cache reads=%d; want 504 and no reads", w.Code, cache.reads)
			}
		})
	}
}

func TestProxyStopsCachedRedirectAfterDeadline(t *testing.T) {
	const origin = "http://origin.test/image"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	cache := &redirectCache{
		Cache: httpcache.NewMemoryCache(),
		onGet: func(key string) {
			if key == origin {
				<-ctx.Done() // The first cached redirect arrives after expiry.
			}
		},
	}
	cacheRedirect(t, cache, origin, origin+"#1")
	p := NewProxy(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("expired cached redirect reached upstream")
		return nil, nil
	}), cache)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/1/"+origin, nil).WithContext(ctx))
	if w.Code != http.StatusGatewayTimeout || cache.reads > 2 {
		t.Fatalf("status=%d, cache reads=%d; want 504 and at most 2 reads", w.Code, cache.reads)
	}
}
