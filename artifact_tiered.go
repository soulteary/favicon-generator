package main

import (
	"context"
	"errors"
	"sync"
)

type tieredArtifactStore struct {
	local, remote artifactStore
	observe       func(string, string)
	closeOnce     sync.Once
	closeErr      error
}

var _ artifactStore = (*tieredArtifactStore)(nil)

func (s *tieredArtifactStore) remoteError(err error) {
	if err != nil && !errors.Is(err, errArtifactMiss) && s.observe != nil {
		s.observe("redis", "error")
	}
}

func (s *tieredArtifactStore) Get(ctx context.Context, key string) (artifactEntry, error) {
	if err := ctx.Err(); err != nil {
		return artifactEntry{}, err
	}
	entry, localErr := s.local.Get(ctx, key)
	if ctx.Err() != nil {
		return artifactEntry{}, ctx.Err()
	}
	if localErr == nil {
		return entry, nil
	}
	entry, err := s.remote.Get(ctx, key)
	if ctx.Err() != nil {
		return artifactEntry{}, ctx.Err()
	}
	if err != nil {
		s.remoteError(err)
		// A degraded remote miss must still let the service populate local
		// storage. Returning an availability error would bypass all storage.
		return artifactEntry{}, errArtifactMiss
	}
	_ = s.local.Put(ctx, entry)
	if err := ctx.Err(); err != nil {
		return artifactEntry{}, err
	}
	return entry, nil
}

func (s *tieredArtifactStore) Put(ctx context.Context, entry artifactEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	localErr := s.local.Put(ctx, entry)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	remoteErr := s.remote.Put(ctx, entry)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.remoteError(remoteErr)
	return errors.Join(localErr, remoteErr)
}

func (s *tieredArtifactStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	localErr := s.local.Delete(ctx, key)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	remoteErr := s.remote.Delete(ctx, key)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.remoteError(remoteErr)
	return errors.Join(localErr, remoteErr)
}

func (s *tieredArtifactStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = errors.Join(s.remote.Close(), s.local.Close()) })
	return s.closeErr
}
