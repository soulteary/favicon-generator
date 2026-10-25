// Copyright 2013 The imageproxy authors.
// SPDX-License-Identifier: Apache-2.0

// Package imageproxy provides an image proxy server.  For typical use of
// creating and using a Proxy, see cmd/imageproxy/main.go.
package imageproxy // import "github.com/willnorris/imageproxy"

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gregjones/httpcache"
)

// Maximum number of redirection-followings allowed.
const maxRedirects = 10

// Proxy serves image requests.
// Configure a Proxy before serving requests; configuration must not be mutated
// while requests are in flight. Custom transports must enforce equivalent
// connection-time address checks to the default transport.
type Proxy struct {
	Client *http.Client // client used to fetch remote URLs
	Cache  Cache        // cache used to cache responses

	// Optional additional restrictions. Both apply when both are configured.
	AllowHosts    []string
	SignatureKeys [][]byte

	// Non-positive resource limits use the safe defaults.
	MaxResponseBytes      int64
	MaxPixels             int64
	MaxFrames             int
	MaxConcurrentRequests int

	initOnce sync.Once
	slots    chan struct{}
	client   *http.Client

	// FollowRedirects controls whether imageproxy will follow redirects or not.
	FollowRedirects bool

	// DefaultBaseURL is the URL that relative remote URLs are resolved in
	// reference to.  If nil, all remote URLs specified in requests must be
	// absolute.
	DefaultBaseURL *url.URL

	// The Logger used by the image proxy
	Logger *log.Logger

	// Allow images to scale beyond their original dimensions.
	ScaleUp bool

	// Timeout specifies a time limit for requests served by this Proxy.
	// If a call runs for longer than its time limit, a 504 Gateway Timeout
	// response is returned. Non-positive values use DefaultTimeout.
	Timeout time.Duration

	// If true, log additional debug messages
	Verbose bool

	// ContentTypes specifies a list of content types to allow. An empty
	// list means all content types are allowed.
	ContentTypes []string

	// PassRequestHeaders identifies HTTP headers to pass from inbound
	// requests to the proxied server.
	PassRequestHeaders []string
}

// NewProxy constructs a proxy with safe defaults. A nil transport uses a
// context-aware transport that only connects to public IP addresses.
func NewProxy(transport http.RoundTripper, cache Cache) *Proxy {
	if transport == nil {
		transport = newSafeTransport()
	}
	if cache == nil {
		cache = NopCache
	}
	proxy := &Proxy{
		Cache:                 cache,
		FollowRedirects:       true,
		Timeout:               DefaultTimeout,
		ContentTypes:          []string{"image/*"},
		MaxResponseBytes:      DefaultMaxResponseBytes,
		MaxPixels:             DefaultMaxPixels,
		MaxFrames:             DefaultMaxFrames,
		MaxConcurrentRequests: DefaultMaxConcurrentRequests,
	}
	client := new(http.Client)
	client.Transport = &requestPolicyTransport{
		Transport: &observedCacheTransport{Transport: &httpcache.Transport{
			Transport: &TransformingTransport{
				Transport:     &fetchTransport{Transport: transport},
				CachingClient: client,
			},
			Cache:               cache,
			MarkCachedResponses: true,
		}},
	}
	proxy.Client = client
	return proxy
}

func (p *Proxy) prepare() {
	p.initOnce.Do(func() {
		limit := p.MaxConcurrentRequests
		if limit <= 0 {
			limit = DefaultMaxConcurrentRequests
		}
		p.slots = make(chan struct{}, limit)
		p.client = p.Client
		if p.client == nil {
			p.client = NewProxy(nil, p.Cache).Client
		}
	})
}

// ServeHTTP handles incoming requests without starting detached background work.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/favicon.ico" {
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/health-check" {
		fmt.Fprint(w, "OK")
		return
	}
	p.prepare()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many active image requests", http.StatusServiceUnavailable)
		return
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	policy := p.fetchPolicy()
	ctx = context.WithValue(ctx, fetchPolicyKey{}, policy)
	p.serveImage(w, r.WithContext(ctx))
}

// serveImage handles incoming requests for proxied images.
func (p *Proxy) serveImage(w http.ResponseWriter, r *http.Request) {
	req, err := NewRequest(r, p.DefaultBaseURL)
	if err != nil {
		http.Error(w, "invalid request URL", http.StatusBadRequest)
		return
	}
	policy := policyFromContext(r.Context())
	if err := policy.checkURL(req.URL); err != nil {
		p.writeError(w, err)
		return
	}
	if len(p.SignatureKeys) > 0 {
		valid := false
		for _, key := range p.SignatureKeys {
			if len(key) > 0 && validSignature(key, req) {
				valid = true
				break
			}
		}
		if !valid {
			p.writeError(w, errUnsafeURL)
			return
		}
	}
	req.Options.ScaleUp = p.ScaleUp
	actualReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, req.String(), nil)
	if err != nil {
		http.Error(w, "invalid request URL", http.StatusBadRequest)
		return
	}
	if len(p.ContentTypes) != 0 {
		actualReq.Header.Set("Accept", strings.Join(p.ContentTypes, ", "))
	}
	copyHeader(actualReq.Header, r.Header, p.PassRequestHeaders...)
	client := clientForRequest(p.client, policy)
	resp, err := client.Do(actualReq)
	if err != nil {
		p.writeError(w, err)
		return
	}
	defer func() { resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if should304(r, resp) {
		copyHeader(w.Header(), resp.Header, "Cache-Control", "Last-Modified", "Expires", "Etag")
		finishWrite := StartStage(r.Context(), "write")
		w.WriteHeader(http.StatusNotModified)
		finishWrite(nil)
		return
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: policy.maxBytes}
	if err := policy.checkResponse(resp); err != nil {
		p.writeError(w, err)
		return
	}
	data, err := readAndCloseResponse(r.Context(), resp, policy.maxBytes)
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && len(data) > 0 {
		// Cached output must obey the current pixel/frame budget, too.
		_, err = transformWithLimits(r.Context(), data, Options{}, policy.maxPixels, policy.maxFrames, policy.maxBytes)
	}
	if err != nil {
		p.writeError(w, err)
		return
	}
	if err := r.Context().Err(); err != nil {
		p.writeError(w, err)
		return
	}
	copyHeader(w.Header(), resp.Header, "Cache-Control", "Last-Modified", "Expires", "Etag")
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	finishWrite := StartStage(r.Context(), "write")
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		finishWrite(err)
	} else {
		finishWrite(nil)
	}
}

func (p *Proxy) writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, errUnsafeURL), errors.Is(err, errContentType):
		status = http.StatusForbidden
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status = http.StatusGatewayTimeout
	case errors.Is(err, ErrResponseTooLarge), errors.Is(err, ErrImageTooLarge), errors.Is(err, ErrTooManyFrames):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrInvalidDimensions):
		status = http.StatusBadRequest
	}
	if p.Verbose {
		p.logf("image request failed: %v", err)
	}
	http.Error(w, http.StatusText(status), status)
}

// peekContentType peeks at the first 512 bytes of p, and attempts to detect
// the content type.  Returns empty string if error occurs.
func peekContentType(p *bufio.Reader) string {
	byt, err := p.Peek(512)
	if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
		return ""
	}
	return http.DetectContentType(byt)
}

// copyHeader copies values for specified headers from src to dst, adding to
// any existing values with the same header name.
func copyHeader(dst, src http.Header, headerNames ...string) {
	for _, name := range headerNames {
		k := http.CanonicalHeaderKey(name)
		for _, v := range src[k] {
			dst.Add(k, v)
		}
	}
}

var (
	errTooManyRedirects = errors.New("too many redirects")
	msgNotAllowed       = "requested URL is not allowed"
)

// contentTypeMatches returns whether contentType matches one of the allowed patterns.
func contentTypeMatches(patterns []string, contentType string) bool {
	if len(patterns) == 0 {
		return true
	}

	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, contentType); ok && err == nil {
			return true
		}
	}

	return false
}

// hostMatches returns whether the host in u matches one of hosts.
func hostMatches(hosts []string, u *url.URL) bool {
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if hostname == host {
			return true
		}
		if strings.HasPrefix(host, "*.") && (hostname == host[2:] || strings.HasSuffix(hostname, "."+host[2:])) {
			return true
		}
		// Checks whether the host in u is an IP
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			// Checks whether our current host is a CIDR
			if _, ipnet, err := net.ParseCIDR(host); err == nil {
				// Checks if our host contains the IP in u
				if ipnet.Contains(ip) {
					return true
				}
			}
		}
	}

	return false
}

// returns whether the referrer from the request is in the host list.
func referrerMatches(hosts []string, r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Referer"))
	if err != nil { // malformed or blank header, just deny
		return false
	}

	return hostMatches(hosts, u)
}

// validSignature returns whether the request signature is valid.
func validSignature(key []byte, r *Request) bool {
	sig := r.Options.Signature
	if m := len(sig) % 4; m != 0 { // add padding if missing
		sig += strings.Repeat("=", 4-m)
	}

	got, err := base64.URLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}

	// check signature with URL only
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(r.URL.String()))
	want := mac.Sum(nil)
	if hmac.Equal(got, want) {
		return true
	}

	// check signature with URL and options
	u, opt := *r.URL, r.Options // make copies
	opt.Signature = ""
	u.Fragment = opt.String()

	mac = hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(u.String()))
	want = mac.Sum(nil)
	return hmac.Equal(got, want)
}

// should304 returns whether we should send a 304 Not Modified in response to
// req, based on the response resp.  This is determined using the last modified
// time and the entity tag of resp.
func should304(req *http.Request, resp *http.Response) bool {
	// TODO(willnorris): if-none-match header can be a comma separated list
	// of multiple tags to be matched, or the special value "*" which
	// matches all etags
	etag := resp.Header.Get("Etag")
	if etag != "" && etag == req.Header.Get("If-None-Match") {
		return true
	}

	lastModified, err := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
	if err != nil {
		return false
	}
	ifModSince, err := time.Parse(time.RFC1123, req.Header.Get("If-Modified-Since"))
	if err != nil {
		return false
	}
	if lastModified.Before(ifModSince) || lastModified.Equal(ifModSince) {
		return true
	}

	return false
}

func (p *Proxy) log(v ...interface{}) {
	if p.Logger != nil {
		p.Logger.Print(v...)
	} else {
		log.Print(v...)
	}
}

func (p *Proxy) logf(format string, v ...interface{}) {
	if p.Logger != nil {
		p.Logger.Printf(format, v...)
	} else {
		log.Printf(format, v...)
	}
}

// TransformingTransport is an implementation of http.RoundTripper that
// optionally transforms images using the options specified in the request URL
// fragment.
type TransformingTransport struct {
	// Transport is the underlying http.RoundTripper used to satisfy
	// non-transform requests (those that do not include a URL fragment).
	Transport http.RoundTripper

	// CachingClient is used to fetch images to be resized.  This client is
	// used rather than Transport directly in order to ensure that
	// responses are properly cached.
	CachingClient *http.Client

	log func(format string, v ...interface{})
}

// RoundTrip implements the http.RoundTripper interface.
func (t *TransformingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	policy := policyFromContext(req.Context())
	if err := policy.checkURL(req.URL); err != nil {
		return nil, err
	}
	if req.URL.Fragment == "" || isRawFetch(req.Context()) {
		return t.Transport.RoundTrip(req)
	}

	// RoundTrippers must not mutate their caller's request or URL.
	rawContext := context.WithValue(req.Context(), rawFetchKey{}, true)
	rawContext = context.WithValue(rawContext, fetchPolicyKey{}, policy)
	rawReq := req.Clone(rawContext)
	rawReq.URL.Fragment = ""
	rawReq.URL.RawFragment = ""
	client := clientForRequest(t.CachingClient, policy)
	resp, err := client.Do(rawReq)
	if err != nil {
		return nil, err
	}
	defer func() { resp.Body.Close() }()
	if should304(req, resp) {
		return &http.Response{StatusCode: http.StatusNotModified, Header: resp.Header.Clone(), Body: http.NoBody, Request: req}, nil
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: policy.maxBytes}
	if err := policy.checkResponse(resp); err != nil {
		return nil, err
	}
	b, err := readAndCloseResponse(req.Context(), resp, policy.maxBytes)
	if err != nil {
		return nil, err
	}
	img := b
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(b) > 0 {
		img, err = transformWithLimits(req.Context(), b, ParseOptions(req.URL.Fragment), policy.maxPixels, policy.maxFrames, policy.maxBytes)
		if err != nil {
			return nil, err
		}
	}
	if int64(len(img)) > policy.maxBytes {
		return nil, ErrResponseTooLarge
	}
	result := new(http.Response)
	*result = *resp
	result.Header = resp.Header.Clone()
	result.Body = io.NopCloser(bytes.NewReader(img))
	result.ContentLength = int64(len(img))
	result.Header.Set("Content-Length", fmt.Sprint(len(img)))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(img) > 0 {
		finishDecode := StartStage(req.Context(), "decode")
		_, format, err := image.DecodeConfig(newContextReader(req.Context(), img))
		finishDecode(err)
		if err != nil {
			return nil, err
		}
		result.Header.Set("Content-Type", "image/"+format)
	}
	return result, nil
}
