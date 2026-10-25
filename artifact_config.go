package main

import (
	"flag"
	"time"

	"github.com/soulteary/cli-kit/validator"
)

const (
	defaultArtifactCacheBytes      int64 = 64 << 20
	defaultArtifactCacheEntryBytes int64 = 20 << 20
	defaultArtifactCacheTTL              = 5 * time.Minute
)

func registerArtifactFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.ArtifactCache, "artifactCache", "off", "finished icon cache: off, memory, redis or tiered")
	fs.Int64Var(&c.ArtifactCacheBytes, "artifactCacheBytes", defaultArtifactCacheBytes, "maximum finished cache payload and header bytes")
	fs.Int64Var(&c.ArtifactCacheEntryBytes, "artifactCacheEntryBytes", defaultArtifactCacheEntryBytes, "maximum individual finished artifact bytes, capped by maxResponseBytes")
	fs.DurationVar(&c.ArtifactCacheTTL, "artifactCacheTTL", defaultArtifactCacheTTL, "finished cache lifetime, positive and at most 24h")
	registerRedisArtifactFlags(fs, c)
}

func validateArtifactConfig(c *Config) error {
	if validator.ValidateEnumCaseSensitive(c.ArtifactCache, []string{"off", "memory", "redis", "tiered"}) != nil {
		return invalidConfig("artifactCache", "must be off, memory, redis or tiered")
	}
	if validator.ValidatePositiveInt64(c.ArtifactCacheBytes) != nil {
		return invalidConfig("artifactCacheBytes", "must be positive")
	}
	if validator.ValidatePositiveInt64(c.ArtifactCacheEntryBytes) != nil {
		return invalidConfig("artifactCacheEntryBytes", "must be positive")
	}
	if validator.ValidateInRangeInt64(int64(c.ArtifactCacheTTL), 1, int64(maxArtifactTTL)) != nil {
		return invalidConfig("artifactCacheTTL", "must be positive and at most 24h")
	}
	if (c.ArtifactCache == "memory" || c.ArtifactCache == "tiered") && artifactWireLimit(c.artifactEntryLimit()) > c.ArtifactCacheBytes-artifactResourceOverhead {
		return invalidConfig("artifactCacheBytes", "must fit one effective artifact entry, envelope and headers")
	}
	return validateRedisArtifactConfig(c)
}

func (c Config) artifactEntryLimit() int64 { return min(c.ArtifactCacheEntryBytes, c.MaxResponseBytes) }

func (c Config) newArtifactService(observe func(string, string)) (*artifactService, error) {
	var store artifactStore
	if c.ArtifactCache == "memory" || c.ArtifactCache == "tiered" {
		var err error
		store, err = newMemoryArtifactStore(c.ArtifactCacheBytes, c.artifactEntryLimit(), c.ArtifactCacheTTL)
		if err != nil {
			return nil, err
		}
	}
	if c.ArtifactCache == "redis" || c.ArtifactCache == "tiered" {
		remote, err := c.newRedisArtifactBackend()
		if err != nil {
			if store != nil {
				_ = store.Close()
			}
			return nil, err
		}
		if store == nil {
			store = remote
		} else {
			store = &tieredArtifactStore{local: store, remote: remote, observe: observe}
		}
	}
	return newArtifactService(store, c.ArtifactCache, c.ArtifactCacheTTL, c.artifactEntryLimit(), observe), nil
}
