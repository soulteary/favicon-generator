package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// Recovery is driven by admitted requests, with one bounded connection attempt
// at a time and a cooldown after failures. No retry goroutine outlives a request.
type recoveringRedisArtifactStore struct {
	cfg        redisArtifactConfig
	maxBody    int64
	retryDelay time.Duration
	dial       func(context.Context, redisArtifactConfig, int64) (artifactStore, error)

	mu        sync.Mutex
	store     artifactStore
	retryAt   time.Time
	closed    bool
	connectMu sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

var _ artifactStore = (*recoveringRedisArtifactStore)(nil)

func newRecoveringRedisArtifactStore(cfg redisArtifactConfig, maxBody int64) (*recoveringRedisArtifactStore, error) {
	if err := cfg.validate(maxBody); err != nil {
		return nil, err
	}
	cfg.Client.Addrs = slices.Clone(cfg.Client.Addrs)
	if cfg.Client.TLSConfig != nil {
		cfg.Client.TLSConfig = cfg.Client.TLSConfig.Clone()
	}
	return &recoveringRedisArtifactStore{
		cfg: cfg, maxBody: maxBody, retryDelay: time.Second,
		dial: func(ctx context.Context, cfg redisArtifactConfig, maxBody int64) (artifactStore, error) {
			return newRedisArtifactStore(ctx, cfg, maxBody)
		},
	}, nil
}

func (s *recoveringRedisArtifactStore) ready(ctx context.Context) (artifactStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errRedisArtifactClosed
	}
	if time.Now().Before(s.retryAt) {
		s.mu.Unlock()
		return nil, errRedisArtifactUnavailable
	}
	store := s.store
	s.mu.Unlock()
	if store != nil {
		return store, nil
	}
	// Other admitted requests fall back immediately instead of accumulating
	// connection waiters or creating a second client while one is starting.
	if !s.connectMu.TryLock() {
		return nil, errRedisArtifactUnavailable
	}
	defer s.connectMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errRedisArtifactClosed
	}
	if time.Now().Before(s.retryAt) {
		s.mu.Unlock()
		return nil, errRedisArtifactUnavailable
	}
	store = s.store
	s.mu.Unlock()
	if store != nil {
		return store, nil
	}
	connectCtx, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	store, err := s.dial(connectCtx, s.cfg, s.maxBody)
	if err != nil || store == nil {
		if ctx.Err() == nil {
			s.failed()
		}
		return nil, redisArtifactFailure(ctx)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = store.Close()
		return nil, errRedisArtifactClosed
	}
	s.store = store
	s.mu.Unlock()
	return store, nil
}

func (s *recoveringRedisArtifactStore) failed() {
	s.mu.Lock()
	s.retryAt = time.Now().Add(s.retryDelay)
	s.mu.Unlock()
}

func (s *recoveringRedisArtifactStore) use(ctx context.Context, operation func(context.Context, artifactStore) error) error {
	bounded, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	store, err := s.ready(bounded)
	if err != nil {
		if bounded.Err() != nil {
			return redisArtifactFailure(ctx)
		}
		return err
	}
	err = operation(bounded, store)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if bounded.Err() != nil {
		err = redisArtifactFailure(ctx)
	}
	if errors.Is(err, errRedisArtifactUnavailable) {
		s.failed()
	}
	return err
}

func (s *recoveringRedisArtifactStore) Get(ctx context.Context, key string) (artifactEntry, error) {
	var entry artifactEntry
	err := s.use(ctx, func(operation context.Context, store artifactStore) error {
		var err error
		entry, err = store.Get(operation, key)
		return err
	})
	return entry, err
}

func (s *recoveringRedisArtifactStore) Put(ctx context.Context, entry artifactEntry) error {
	return s.use(ctx, func(operation context.Context, store artifactStore) error { return store.Put(operation, entry) })
}

func (s *recoveringRedisArtifactStore) Delete(ctx context.Context, key string) error {
	return s.use(ctx, func(operation context.Context, store artifactStore) error { return store.Delete(operation, key) })
}

func (s *recoveringRedisArtifactStore) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		// A concurrent starter is still owned by its admitted request. Join it
		// before releasing the backend; its attempt has an operation deadline.
		s.connectMu.Lock()
		defer s.connectMu.Unlock()
		s.mu.Lock()
		store := s.store
		s.store = nil
		s.mu.Unlock()
		if store != nil {
			s.closeErr = store.Close()
		}
	})
	return s.closeErr
}
