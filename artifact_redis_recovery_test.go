package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedisArtifactRecoveryStartsOfflineThenRecovers(t *testing.T) {
	store, err := newRecoveringRedisArtifactStore(defaultRedisArtifactConfig(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	store.retryDelay = 10 * time.Millisecond
	fake := new(artifactRedisFake)
	backend := redisArtifactTestStore(fake)
	var attempts atomic.Int32
	store.dial = func(context.Context, redisArtifactConfig, int64) (artifactStore, error) {
		if attempts.Add(1) == 1 {
			return nil, errRedisArtifactUnavailable
		}
		return backend, nil
	}
	key := redisArtifactTestEntry().Key
	if _, err := store.Get(context.Background(), key); !errors.Is(err, errRedisArtifactUnavailable) {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, errRedisArtifactUnavailable) || attempts.Load() != 1 {
		t.Fatalf("cooldown made another connection attempt: %v, %d", err, attempts.Load())
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := store.Get(context.Background(), key); !errors.Is(err, errArtifactMiss) || attempts.Load() != 2 {
		t.Fatalf("later request did not recover: %v, %d", err, attempts.Load())
	}
	if err := store.Put(context.Background(), redisArtifactTestEntry()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil || fake.closes.Load() != 1 {
		t.Fatalf("recovered client was not closed: %v", err)
	}
	if err := store.Close(); err != nil || fake.closes.Load() != 1 {
		t.Fatalf("recovered client closed repeatedly: %v", err)
	}
}

func TestRedisArtifactRecoveryHasOneBoundedStarter(t *testing.T) {
	cfg := defaultRedisArtifactConfig()
	cfg.OperationTimeout = 20 * time.Millisecond
	store, err := newRecoveringRedisArtifactStore(cfg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var attempts atomic.Int32
	store.dial = func(ctx context.Context, _ redisArtifactConfig, _ int64) (artifactStore, error) {
		attempts.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := make(chan error, 1)
	go func() { _, err := store.Get(context.Background(), redisArtifactTestEntry().Key); result <- err }()
	<-entered
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := store.Get(context.Background(), redisArtifactTestEntry().Key); !errors.Is(err, errRedisArtifactUnavailable) {
				t.Errorf("concurrent start: %v", err)
			}
		}()
	}
	workers.Wait()
	if err := <-result; !errors.Is(err, errRedisArtifactUnavailable) || attempts.Load() != 1 {
		t.Fatalf("starter budget classified as request timeout or retried: %v, %d", err, attempts.Load())
	}
	if _, err := store.Get(context.Background(), redisArtifactTestEntry().Key); !errors.Is(err, errRedisArtifactUnavailable) || attempts.Load() != 1 {
		t.Fatalf("timed-out starter did not enter cooldown: %v, %d", err, attempts.Load())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRedisArtifactRecoveryCancellationAndConcurrentClose(t *testing.T) {
	store, err := newRecoveringRedisArtifactStore(defaultRedisArtifactConfig(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store.dial = func(context.Context, redisArtifactConfig, int64) (artifactStore, error) {
		t.Fatal("canceled request initiated Redis startup")
		return nil, nil
	}
	if _, err := store.Get(ctx, redisArtifactTestEntry().Key); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := new(artifactRedisFake)
	store.dial = func(context.Context, redisArtifactConfig, int64) (artifactStore, error) {
		close(entered)
		<-release
		return redisArtifactTestStore(fake), nil
	}
	getDone := make(chan error, 1)
	go func() { _, err := store.Get(context.Background(), redisArtifactTestEntry().Key); getDone <- err }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	// Once Close marks the wrapper closed, a starter cannot publish its client.
	for {
		store.mu.Lock()
		closed := store.closed
		store.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-getDone; !errors.Is(err, errRedisArtifactClosed) {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil || fake.closes.Load() != 1 {
		t.Fatalf("Close did not join starting client: %v, %d", err, fake.closes.Load())
	}
}
