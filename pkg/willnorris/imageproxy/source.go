// Copyright 2026 The imageproxy authors.
// SPDX-License-Identifier: Apache-2.0

package imageproxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SourceLimits bounds both an encoded image and its decoded representation.
// Non-positive values select the corresponding safe defaults.
type SourceLimits struct {
	MaxResponseBytes int64
	MaxPixels        int64
	MaxFrames        int
}

var (
	ErrSourceForbidden   = errors.New("image source is not allowed")
	ErrSourceNotFound    = errors.New("image source was not found")
	ErrSourceUnavailable = errors.New("image source is unavailable")
	ErrSourceBusy        = errors.New("too many active image requests")
)

// WithSource fetches an original image using the same security policy, cache,
// concurrency slots, and timeout as ServeHTTP. It validates encoded-image
// metadata, including all GIF frame budgets, before calling fn. The callback
// remains within that slot and deadline, and must observe its context during
// further work. No background worker is started to enforce the deadline.
//
// source must be an absolute HTTP(S) URL without user information or a fragment.
// When SignatureKeys is configured, signature must be a URL-safe base64 HMAC
// SHA-256 of source.String(), using one of those keys. The signature covers the
// original source URL, before any redirects.
func (p *Proxy) WithSource(ctx context.Context, source *url.URL, signature string, fn func(context.Context, []byte, SourceLimits) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == nil || !source.IsAbs() || source.Fragment != "" || source.RawFragment != "" {
		return ErrSourceForbidden
	}
	// Do not retain the caller's mutable URL while a request is in progress.
	sourceCopy := *source
	policy := p.fetchPolicy()
	if err := policy.checkURL(&sourceCopy); err != nil {
		return ErrSourceForbidden
	}
	if !p.validSourceSignature(&sourceCopy, signature) {
		return ErrSourceForbidden
	}
	if fn == nil {
		return ErrSourceUnavailable
	}
	p.prepare()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		return ErrSourceBusy
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx = context.WithValue(ctx, fetchPolicyKey{}, policy)
	ctx = context.WithValue(ctx, rawFetchKey{}, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceCopy.String(), nil)
	if err != nil {
		return ErrSourceForbidden
	}
	if len(policy.contentTypes) > 0 {
		req.Header.Set("Accept", strings.Join(policy.contentTypes, ", "))
	}
	resp, err := clientForRequest(p.client, policy).Do(req)
	if err != nil {
		return sourceFetchError(err)
	}
	defer func() { resp.Body.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrSourceNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrSourceUnavailable
	}
	// The same checks apply to cached responses, which may have been populated
	// under less restrictive limits or different permitted content types.
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: policy.maxBytes}
	if err := policy.checkResponse(resp); err != nil {
		return sourceFetchError(err)
	}
	data, err := readAndCloseResponse(ctx, resp, policy.maxBytes)
	if err != nil {
		return sourceFetchError(err)
	}
	limits := SourceLimits{policy.maxBytes, policy.maxPixels, policy.maxFrames}
	if _, err := TransformWithBudget(ctx, data, Options{}, limits); err != nil {
		return sourceFetchError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if requested, _ := ctx.Value(sourceMetadataRequestKey{}).(bool); requested {
		ctx = context.WithValue(ctx, sourceMetadataKey{}, newSourceMetadata(resp, data, time.Now()))
	}
	err = fn(ctx, data, limits)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (p *Proxy) validSourceSignature(source *url.URL, signature string) bool {
	if len(p.SignatureKeys) == 0 {
		return true
	}
	if pad := len(signature) % 4; pad != 0 {
		signature += strings.Repeat("=", 4-pad)
	}
	sig, err := base64.URLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	for _, key := range p.SignatureKeys {
		if len(key) == 0 {
			continue
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(source.String()))
		if hmac.Equal(sig, mac.Sum(nil)) {
			return true
		}
	}
	return false
}

// sourceFetchError preserves public error classifications without exposing a
// remote URL, credential, or arbitrary upstream error text to callers.
func sourceFetchError(err error) error {
	for _, public := range []error{
		context.Canceled, context.DeadlineExceeded,
		ErrResponseTooLarge, ErrImageTooLarge, ErrTooManyFrames, ErrInvalidDimensions,
	} {
		if errors.Is(err, public) {
			return public
		}
	}
	if errors.Is(err, errUnsafeURL) || errors.Is(err, errContentType) {
		return ErrSourceForbidden
	}
	return ErrSourceUnavailable
}

// TransformWithBudget applies the supplied source limits to input validation,
// image operations, and encoded output. It is suitable for transformations
// performed inside a WithSource callback, including configurations with a byte
// limit lower than TransformWithLimits' default.
func TransformWithBudget(ctx context.Context, img []byte, opt Options, limits SourceLimits) ([]byte, error) {
	return transformWithLimits(ctx, img, opt, limits.MaxPixels, limits.MaxFrames, limits.MaxResponseBytes)
}
