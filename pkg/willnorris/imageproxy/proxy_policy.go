package imageproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultTimeout                     = 15 * time.Second
	DefaultMaxResponseBytes      int64 = 20 << 20
	DefaultMaxConcurrentRequests       = 4
)

var (
	ErrResponseTooLarge = errors.New("image response exceeds byte limit")
	errContentType      = errors.New("upstream content type is not allowed")
)

type fetchPolicyKey struct{}

type fetchPolicy struct {
	allowHosts      []string
	contentTypes    []string
	maxBytes        int64
	maxPixels       int64
	maxFrames       int
	followRedirects bool
}

func (p *Proxy) fetchPolicy() *fetchPolicy {
	policy := &fetchPolicy{
		allowHosts:      p.AllowHosts,
		contentTypes:    p.ContentTypes,
		maxBytes:        p.MaxResponseBytes,
		maxPixels:       p.MaxPixels,
		maxFrames:       p.MaxFrames,
		followRedirects: p.FollowRedirects,
	}
	if policy.maxBytes <= 0 {
		policy.maxBytes = DefaultMaxResponseBytes
	}
	if policy.maxPixels <= 0 {
		policy.maxPixels = DefaultMaxPixels
	}
	if policy.maxFrames <= 0 {
		policy.maxFrames = DefaultMaxFrames
	}
	return policy
}

func policyFromContext(ctx context.Context) *fetchPolicy {
	if policy, ok := ctx.Value(fetchPolicyKey{}).(*fetchPolicy); ok {
		return policy
	}
	return (&Proxy{FollowRedirects: true}).fetchPolicy()
}

func (p *fetchPolicy) checkURL(u *url.URL) error {
	if err := validateRemoteURL(u); err != nil {
		return err
	}
	if len(p.allowHosts) > 0 && !hostMatches(p.allowHosts, u) {
		return errUnsafeURL
	}
	return nil
}

func clientForRequest(original *http.Client, policy *fetchPolicy) *http.Client {
	client := *original
	// The per-request copy leaves the shared client's fields immutable.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !policy.followRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) >= maxRedirects {
			return errTooManyRedirects
		}
		if err := policy.checkURL(req.URL); err != nil {
			return err
		}
		if original.CheckRedirect != nil {
			return original.CheckRedirect(req, via)
		}
		return nil
	}
	return &client
}

// fetchTransport checks headers before the cache or decoder can buffer a body.
type fetchTransport struct {
	Transport http.RoundTripper
}

func (t *fetchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	policy := policyFromContext(req.Context())
	if err := policy.checkURL(req.URL); err != nil {
		return nil, err
	}
	resp, err := t.Transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: policy.maxBytes}
	if err := policy.checkResponse(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (p *fetchPolicy) checkResponse(resp *http.Response) error {
	if resp.ContentLength > p.maxBytes {
		return ErrResponseTooLarge
	}
	// Redirects and error statuses do not represent images. Their bodies are
	// still bounded, but a redirect must reach the client's redirect policy.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == http.StatusNoContent || resp.ContentLength == 0 {
		return nil
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType == "" || contentType == "application/octet-stream" || contentType == "binary/octet-stream" {
		reader := bufio.NewReaderSize(resp.Body, 512)
		resp.Body = &readerBody{Reader: reader, Closer: resp.Body}
		prefix, err := reader.Peek(512)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
		contentType = http.DetectContentType(prefix)
	}
	if !contentTypeMatches(p.contentTypes, contentType) {
		return errContentType
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Set("Content-Type", contentType)
	return nil
}

type readerBody struct {
	io.Reader
	io.Closer
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
	exceeded  bool
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.exceeded {
		return 0, ErrResponseTooLarge
	}
	if b.remaining < int64(len(p)) {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	if int64(n) > b.remaining {
		n = int(b.remaining)
		b.remaining = 0
		b.exceeded = true
		return n, ErrResponseTooLarge
	}
	b.remaining -= int64(n)
	return n, err
}

func readResponse(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(&boundedBody{ReadCloser: body, remaining: limit})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}
