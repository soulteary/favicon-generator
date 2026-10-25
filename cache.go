package main

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/lrucache"
	"github.com/die-net/lrucache/twotier"
	"github.com/soulteary/cli-kit/validator"
	"github.com/willnorris/imageproxy"
)

const cacheMegabyte int64 = 1_000_000

// CacheConfig is a validated memory tier. Fields stay private so NewCache only
// receives sizes and expiry values checked while parsing startup configuration.
type CacheConfig struct {
	maxBytes      int64
	maxAgeSeconds int64
}

type cacheConfigValue struct{ target *[]CacheConfig }

func (v cacheConfigValue) String() string { return "" }

func (v cacheConfigValue) Set(value string) error {
	var tiers []CacheConfig
	for _, entry := range strings.Fields(value) {
		tier, err := parseCacheConfig(entry)
		if err != nil {
			return err
		}
		tiers = append(tiers, tier)
	}
	*v.target = append(*v.target, tiers...)
	return nil
}

func parseCacheConfig(value string) (CacheConfig, error) {
	if value == "memory" {
		value = "memory:100"
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "memory" {
		return CacheConfig{}, errors.New("cache must use memory:<MB>[:age]")
	}
	megabytes, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || validator.ValidateInRangeInt64(megabytes, 1, math.MaxInt64/cacheMegabyte) != nil {
		return CacheConfig{}, errors.New("cache capacity must be a positive integer number of MB without overflow")
	}
	var ageSeconds int64
	if len(parts) == 3 {
		age, err := time.ParseDuration(parts[2])
		if err != nil || validator.ValidateNonNegativeInt64(int64(age)) != nil {
			return CacheConfig{}, errors.New("cache age must be a nonnegative duration without overflow")
		}
		// LRU expiry uses seconds. Round a positive subsecond duration up rather
		// than turning it into zero, which would silently disable expiration.
		ageSeconds = int64(age / time.Second)
		if age%time.Second != 0 {
			ageSeconds++
		}
	}
	return CacheConfig{maxBytes: megabytes * cacheMegabyte, maxAgeSeconds: ageSeconds}, nil
}

// NewCache allocates the already validated tiers. With no -cache option, the
// service does not cache anything; memory allocation starts only on this call.
func (c Config) NewCache() imageproxy.Cache {
	var result imageproxy.Cache
	for _, tier := range c.Cache {
		next := lrucache.New(tier.maxBytes, tier.maxAgeSeconds)
		if result == nil {
			result = next
		} else {
			result = twotier.New(result, next)
		}
	}
	if result == nil {
		return imageproxy.NopCache
	}
	return result
}
