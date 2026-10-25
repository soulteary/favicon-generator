package imageproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gregjones/httpcache"
)

// Each observed lookup gets a private copy of the cache transport and probes.
// Neither shared transports nor remote X-From-Cache headers are mutated or
// trusted. The policy transport remains outside this wrapper.
type observedCacheTransport struct{ Transport *httpcache.Transport }

func (t *observedCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	observing := observerFromContext(req.Context()) != nil
	if !observing && !cacheEnabled(t.Transport.Cache) {
		return t.Transport.RoundTrip(req)
	}
	layer := "source"
	if !isRawFetch(req.Context()) && req.URL.Fragment != "" {
		layer = "transformed"
	}
	probe := new(cacheAttempt)
	transport := *t.Transport
	if observing {
		transport.Cache = &cacheLookupProbe{Cache: transport.Cache, attempt: probe}
	}
	transport.Transport = &cacheTransportProbe{Transport: transport.Transport, attempt: probe}
	resp, err := transport.RoundTrip(req)
	// httpcache replaces validation/error responses with cached responses but
	// does not close the abandoned network body itself. Finish that real fetch
	// now, even when no observer is attached to this request.
	if probe.response != nil && probe.response != resp && probe.body != nil {
		probe.err = errors.Join(probe.err, probe.body.Close())
	}
	if !observing {
		return resp, err
	}
	result := "miss"
	switch {
	case err != nil || probe.err != nil || probe.status >= 500:
		// stale-if-error may return cached data even though the inner request
		// failed. That attempt is still an error, never a cache hit.
		result = "error"
	case !cacheEnabled(t.Transport.Cache):
		result = "bypass"
	case !probe.called && probe.hit && resp != nil && resp.StatusCode == http.StatusGatewayTimeout:
		// A malformed cache entry plus only-if-cached can produce a synthetic
		// 504 without calling the inner transport. Bytes existing is no hit.
		result = "error"
	case !probe.called && probe.hit:
		result = "hit"
	case probe.hit && probe.status == http.StatusNotModified && resp != nil && resp.StatusCode == http.StatusOK:
		result = "revalidated"
	case req.Method != http.MethodGet && req.Method != http.MethodHead || req.Header.Get("Range") != "" || hasNoStore(req.Header) || resp != nil && hasNoStore(resp.Header):
		result = "bypass"
	}
	if err != nil || resp == nil || resp.Body == nil {
		observeCache(req.Context(), layer, result)
		return resp, err
	}
	resp.Body = &observedBody{ReadCloser: resp.Body, finish: func(bodyErr error) {
		if bodyErr != nil {
			result = "error"
		}
		observeCache(req.Context(), layer, result)
	}}
	return resp, nil
}

func cacheEnabled(cache Cache) bool {
	switch cache.(type) {
	case nopCache, *nopCache:
		return false
	default:
		return true
	}
}

func hasNoStore(header http.Header) bool {
	for _, value := range header.Values("Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(strings.SplitN(directive, "=", 2)[0]), "no-store") {
				return true
			}
		}
	}
	return false
}

type cacheAttempt struct {
	hit, called bool
	status      int
	err         error
	response    *http.Response
	body        io.ReadCloser
}

type cacheLookupProbe struct {
	Cache
	attempt *cacheAttempt
}

func (p *cacheLookupProbe) Get(key string) ([]byte, bool) {
	data, ok := p.Cache.Get(key)
	p.attempt.hit = ok
	return data, ok
}

type cacheTransportProbe struct {
	Transport http.RoundTripper
	attempt   *cacheAttempt
}

func (p *cacheTransportProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	p.attempt.called = true
	resp, err := p.Transport.RoundTrip(req)
	p.attempt.err = err
	p.attempt.response = resp
	if resp != nil {
		p.attempt.status = resp.StatusCode
		p.attempt.body = resp.Body
	}
	return resp, err
}

// observedBody finishes a fetch/cache attempt only after the real body is
// closed, including all reading and closure errors. Close may be concurrent
// with cancellation, and repeated Close calls do not duplicate observations.
type observedBody struct {
	io.ReadCloser
	finish    func(error)
	mu        sync.Mutex
	readErr   error
	closeErr  error
	closeOnce sync.Once
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.mu.Lock()
		b.readErr = errors.Join(b.readErr, err)
		b.mu.Unlock()
	}
	return n, err
}

func (b *observedBody) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.mu.Lock()
		err := errors.Join(b.readErr, b.closeErr)
		b.mu.Unlock()
		b.finish(err)
	})
	return b.closeErr
}

func readAndCloseResponse(ctx context.Context, resp *http.Response, limit int64) ([]byte, error) {
	data, err := readResponse(ctx, resp.Body, limit)
	closeErr := resp.Body.Close()
	resp.Body = http.NoBody
	return data, errors.Join(err, closeErr)
}
