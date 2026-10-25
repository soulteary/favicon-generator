package main

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/willnorris/imageproxy"
)

const maxArtifactFills = 64

// The service runs synchronously inside WithSourceMetadata. Waiting for an
// existing fill retains the caller's proxy slot and complete request deadline.
type artifactService struct {
	store    artifactStore
	backend  string
	ttl      time.Duration
	maxEntry int64
	observe  func(string, string)
	now      func() time.Time
	mu       sync.Mutex
	fills    map[string]chan struct{}
	closed   bool
}

func newArtifactService(store artifactStore, backend string, ttl time.Duration, maxEntry int64, observe func(string, string)) *artifactService {
	return &artifactService{store: store, backend: backend, ttl: ttl, maxEntry: maxEntry, observe: observe, now: time.Now, fills: make(map[string]chan struct{})}
}

func (s *artifactService) GetOrGenerate(ctx context.Context, source *url.URL, metadata imageproxy.SourceMetadata, filename, name, shortName string, limits imageproxy.SourceLimits, generate func(context.Context) ([]byte, error)) (body []byte, resultErr error) {
	result := "bypass"
	defer func() {
		if s.observe != nil {
			s.observe(s.backend, result)
		}
	}()
	if err := ctx.Err(); err != nil {
		result = "error"
		return nil, err
	}
	key, eligible := artifactKey(source, metadata, filename, name, shortName, limits)
	if s.store == nil || !eligible || s.maxEntry <= 0 || artifactExpiry(s.now(), metadata.FreshUntil(), s.ttl).IsZero() {
		return generate(ctx)
	}
	backendError := false
	lookup := func() ([]byte, bool, error) {
		entry, err := s.store.Get(ctx, key)
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if errors.Is(err, errArtifactMiss) {
			return nil, false, nil
		}
		if err != nil {
			backendError = true
			if errors.Is(err, errInvalidArtifact) || errors.Is(err, errArtifactTooLarge) {
				_ = s.store.Delete(ctx, key)
			}
			return nil, false, nil // local/remote availability fails open
		}
		now := s.now()
		if err := validateArtifact(ctx, entry, key, filename, limits, now); err != nil {
			if errors.Is(err, imageproxy.ErrResponseTooLarge) || errors.Is(err, imageproxy.ErrImageTooLarge) || ctx.Err() != nil {
				return nil, false, err
			}
			backendError = true
			_ = s.store.Delete(ctx, key)
			return nil, false, nil
		}
		// The source's current sharing horizon is authoritative on every hit;
		// neither source revalidation nor a new local TTL may extend a value.
		if !metadata.FreshUntil().After(now) || entry.ExpiresAt.After(metadata.FreshUntil()) || entry.ExpiresAt.After(now.Add(s.ttl)) || int64(len(entry.Body)) > s.maxEntry {
			_ = s.store.Delete(ctx, key)
			return nil, false, nil
		}
		return entry.Body, true, nil
	}
	for {
		cached, hit, err := lookup()
		if err != nil {
			result = "error"
			return nil, err
		}
		if hit {
			result = "hit"
			return cached, nil
		}
		if backendError {
			result = "error"
			return generate(ctx)
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			result = "error"
			return generate(ctx)
		}
		if done, waiting := s.fills[key]; waiting {
			s.mu.Unlock()
			select {
			case <-done:
				continue // failed/canceled leaders are retried
			case <-ctx.Done():
				result = "error"
				return nil, ctx.Err()
			}
		}
		if len(s.fills) >= maxArtifactFills {
			s.mu.Unlock()
			return generate(ctx)
		}
		done := make(chan struct{})
		s.fills[key] = done
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.fills, key); close(done); s.mu.Unlock() }()
		// A previous leader can publish between our miss and taking this key.
		cached, hit, err = lookup()
		if err != nil {
			result = "error"
			return nil, err
		}
		if hit {
			result = "hit"
			return cached, nil
		}
		result = "miss"
		if backendError {
			result = "error"
		}
		body, err = generate(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if backendError {
			return body, nil
		}
		expires := artifactExpiry(s.now(), metadata.FreshUntil(), s.ttl)
		validationNow := s.now()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Retention can end between rendering and publication, especially for
		// a nanosecond TTL. Return the completed product without caching it.
		if int64(len(body)) > s.maxEntry || !expires.After(validationNow) {
			result = "bypass"
			return body, nil
		}
		entry := newArtifactEntry(key, filename, body, expires)
		// Validate before publication too. A successful generator must never
		// publish a product inconsistent with the current resource contract.
		if err := validateArtifact(ctx, entry, key, filename, limits, validationNow); err != nil {
			return nil, err
		}
		if err := s.store.Put(ctx, entry); err != nil {
			result = "error"
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return body, nil
	}
}

func (s *artifactService) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}
