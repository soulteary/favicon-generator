package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type artifactTieredSpy struct {
	artifactStore
	gets atomic.Int32
}

func (s *artifactTieredSpy) Get(ctx context.Context, key string) (artifactEntry, error) {
	s.gets.Add(1)
	return s.artifactStore.Get(ctx, key)
}

func TestArtifactTieredRemoteReadThroughAndClose(t *testing.T) {
	entry := redisArtifactTestEntry()
	local := &artifactTestStore{}
	remote := &artifactTieredSpy{artifactStore: &artifactTestStore{entries: map[string]artifactEntry{entry.Key: entry}}}
	store := &tieredArtifactStore{local: local, remote: remote}
	for range 2 {
		got, err := store.Get(context.Background(), entry.Key)
		if err != nil || !bytes.Equal(got.Body, entry.Body) {
			t.Fatalf("read through: %v", err)
		}
	}
	if remote.gets.Load() != 1 || local.puts != 1 {
		t.Fatalf("local hit still reached remote: gets=%d puts=%d", remote.gets.Load(), local.puts)
	}
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() { defer workers.Done(); _ = store.Close() }()
	}
	workers.Wait()
	if local.closes != 1 || remote.artifactStore.(*artifactTestStore).closes != 1 {
		t.Fatal("tiered store did not close both clients once")
	}
	if _, err := store.Get(context.Background(), entry.Key); !errors.Is(err, errArtifactStoreClosed) || remote.gets.Load() != 1 {
		t.Fatalf("closed tiered store touched backend: %v", err)
	}
}

func TestArtifactTieredRedisOutageStillWarmsLocal(t *testing.T) {
	source, _, limits, metadata := artifactTestSource(t, "public,max-age=3600")
	local, err := newMemoryArtifactStore(1<<20, 1<<16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	remote := &artifactTieredSpy{artifactStore: &artifactTestStore{getErr: errRedisArtifactUnavailable, putErr: errRedisArtifactUnavailable}}
	var remoteErrors, hits atomic.Int32
	observe := func(backend, result string) {
		if backend == "redis" && result == "error" {
			remoteErrors.Add(1)
		}
		if backend == "tiered" && result == "hit" {
			hits.Add(1)
		}
	}
	store := &tieredArtifactStore{local: local, remote: remote, observe: observe}
	service := newArtifactService(store, "tiered", time.Minute, 1<<16, observe)
	defer service.Close()
	body := iconStageFixture(t, 32, false)
	var generations int
	generate := func(context.Context) ([]byte, error) { generations++; return bytes.Clone(body), nil }
	for range 2 {
		got, err := service.GetOrGenerate(context.Background(), source, metadata, "favicon-32x32.png", "", "", limits, generate)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("outage failed open incorrectly: %v", err)
		}
	}
	if generations != 1 || hits.Load() != 1 || remoteErrors.Load() == 0 {
		t.Fatalf("local cache did not warm during outage: generations=%d hits=%d remote errors=%d", generations, hits.Load(), remoteErrors.Load())
	}
}

func TestArtifactTieredCancelledDoesNotReachEitherBackend(t *testing.T) {
	local := &artifactTieredSpy{artifactStore: &artifactTestStore{}}
	remote := &artifactTieredSpy{artifactStore: &artifactTestStore{}}
	store := &tieredArtifactStore{local: local, remote: remote}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := redisArtifactTestEntry().Key
	if _, err := store.Get(ctx, key); !errors.Is(err, context.Canceled) || local.gets.Load() != 0 || remote.gets.Load() != 0 {
		t.Fatalf("canceled tiered lookup: %v", err)
	}
}
