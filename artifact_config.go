package main

import (
	"flag"
	"time"
)

const (
	defaultArtifactCacheBytes      int64 = 64 << 20
	defaultArtifactCacheEntryBytes int64 = 20 << 20
	defaultArtifactCacheTTL              = 5 * time.Minute
)

func registerArtifactFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.ArtifactCache, "artifactCache", "off", "finished icon cache: off or memory")
	fs.Int64Var(&c.ArtifactCacheBytes, "artifactCacheBytes", defaultArtifactCacheBytes, "maximum finished cache payload and header bytes")
	fs.Int64Var(&c.ArtifactCacheEntryBytes, "artifactCacheEntryBytes", defaultArtifactCacheEntryBytes, "maximum individual finished artifact bytes, capped by maxResponseBytes")
	fs.DurationVar(&c.ArtifactCacheTTL, "artifactCacheTTL", defaultArtifactCacheTTL, "finished cache lifetime, positive and at most 24h")
}

func validateArtifactConfig(c *Config) error {
	if c.ArtifactCache != "off" && c.ArtifactCache != "memory" {
		return invalidConfig("artifactCache", "must be off or memory")
	}
	if c.ArtifactCacheBytes <= 0 {
		return invalidConfig("artifactCacheBytes", "must be positive")
	}
	if c.ArtifactCacheEntryBytes <= 0 {
		return invalidConfig("artifactCacheEntryBytes", "must be positive")
	}
	if c.ArtifactCacheTTL <= 0 || c.ArtifactCacheTTL > maxArtifactTTL {
		return invalidConfig("artifactCacheTTL", "must be positive and at most 24h")
	}
	if c.ArtifactCache == "memory" && artifactWireLimit(c.artifactEntryLimit()) > c.ArtifactCacheBytes-artifactResourceOverhead {
		return invalidConfig("artifactCacheBytes", "must fit one effective artifact entry, envelope and headers")
	}
	return nil
}

func (c Config) artifactEntryLimit() int64 { return min(c.ArtifactCacheEntryBytes, c.MaxResponseBytes) }

func (c Config) newArtifactService(observe func(string, string)) (*artifactService, error) {
	var store artifactStore
	if c.ArtifactCache == "memory" {
		var err error
		store, err = newMemoryArtifactStore(c.ArtifactCacheBytes, c.artifactEntryLimit(), c.ArtifactCacheTTL)
		if err != nil {
			return nil, err
		}
	}
	return newArtifactService(store, c.ArtifactCache, c.ArtifactCacheTTL, c.artifactEntryLimit(), observe), nil
}
