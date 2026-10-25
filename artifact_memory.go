package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	httpcache "github.com/soulteary/httpcache-kit/v4"
)

var errArtifactStoreClosed = errors.New("artifact store closed")

// The kit counts serialized headers as well as the envelope and payload. Keep
// headroom for our fixed headers; no source headers or URLs enter this store.
const artifactResourceOverhead int64 = 1024

type memoryArtifactStore struct {
	cache   httpcache.ExtendedCache
	maxBody int64
	gate    chan struct{}
	closed  bool // protected by gate
}

func newMemoryArtifactStore(capacity, maxBody int64, ttl time.Duration) (*memoryArtifactStore, error) {
	if capacity <= 0 || maxBody <= 0 || artifactWireLimit(maxBody) > capacity-artifactResourceOverhead || ttl <= 0 || ttl > maxArtifactTTL {
		return nil, errors.New("invalid artifact memory cache limits")
	}
	return &memoryArtifactStore{
		cache: httpcache.NewMemoryCacheWithConfig(&httpcache.CacheConfig{
			Integrity: httpcache.IntegritySHA256, MaxSize: capacity, TTL: ttl,
			// Expiration is checked on every read. No background worker owns
			// entries or outlives the server's admitted handlers.
			CleanupInterval: 0,
		}), maxBody: maxBody, gate: make(chan struct{}, 1),
	}, nil
}

func (s *memoryArtifactStore) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		if s.closed {
			<-s.gate
			return errArtifactStoreClosed
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *memoryArtifactStore) unlock() { <-s.gate }

func validArtifactStorageKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}

func (s *memoryArtifactStore) Get(ctx context.Context, key string) (artifactEntry, error) {
	if !validArtifactStorageKey(key) {
		return artifactEntry{}, errInvalidArtifact
	}
	if err := s.lock(ctx); err != nil {
		return artifactEntry{}, err
	}
	defer s.unlock()
	resource, err := s.cache.Retrieve(key)
	if errors.Is(err, httpcache.ErrNotFoundInCache) {
		return artifactEntry{}, errArtifactMiss
	}
	if err != nil {
		return artifactEntry{}, err
	}
	if resource.Status() == http.StatusGone {
		return artifactEntry{}, errors.Join(errArtifactMiss, resource.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(artifactContextReader{ctx, resource}, artifactWireLimit(s.maxBody)+1))
	closeErr := resource.Close()
	if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
		return artifactEntry{}, err
	}
	entry, err := decodeArtifact(ctx, data, key, s.maxBody)
	if err != nil {
		// A bounded same-key tombstone replaces corrupt payloads. Invalidate
		// would create a separate stale marker outside the kit's LRU budget.
		_ = s.tombstone(ctx, key)
		return artifactEntry{}, err
	}
	if !entry.ExpiresAt.After(time.Now()) {
		_ = s.tombstone(ctx, key)
		return artifactEntry{}, errArtifactMiss
	}
	return entry, ctx.Err()
}

func (s *memoryArtifactStore) Put(ctx context.Context, entry artifactEntry) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	wire, err := encodeArtifact(ctx, entry, s.maxBody)
	if err != nil {
		return err
	}
	return s.store(ctx, entry.Key, http.StatusOK, wire)
}

func (s *memoryArtifactStore) store(ctx context.Context, key string, status int, wire []byte) error {
	resource := httpcache.NewResourceBytes(status, wire, http.Header{
		"Content-Type":   {"application/octet-stream"},
		"Content-Length": {strconv.Itoa(len(wire))},
	})
	// The memory VFS copies during StoreContext, not in an asynchronous fill.
	// This gate bounds temporary envelope/read snapshots to one operation.
	err := s.cache.(httpcache.ContextStore).StoreContext(ctx, resource, key)
	return errors.Join(err, resource.Close(), ctx.Err())
}

func (s *memoryArtifactStore) tombstone(ctx context.Context, key string) error {
	return s.store(ctx, key, http.StatusGone, nil)
}

func (s *memoryArtifactStore) Delete(ctx context.Context, key string) error {
	if !validArtifactStorageKey(key) {
		return errInvalidArtifact
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	return s.tombstone(ctx, key)
}

func (s *memoryArtifactStore) Close() error {
	// Called after server drain; concurrent Close remains safe and idempotent.
	s.gate <- struct{}{}
	defer s.unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := errors.Join(s.cache.Close(), s.cache.Purge())
	s.cache = nil
	return err
}

type artifactContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r artifactContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err == nil {
		err = r.ctx.Err()
	}
	return n, err
}
