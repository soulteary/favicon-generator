package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	redisclient "github.com/soulteary/redis-kit/client"
)

var (
	errRedisArtifactConfig      = errors.New("invalid Redis artifact cache configuration")
	errRedisArtifactUnavailable = errors.New("Redis artifact cache unavailable")
	errRedisArtifactClosed      = errors.New("Redis artifact cache closed")
)

// A server-side size check and GET must share one atomic execution. Separate
// STRLEN/GET commands allow a concurrent writer to replace a checked small value
// with an oversized one before go-redis allocates its response buffer.
const redisArtifactGetScript = `
local kind = redis.call('TYPE', KEYS[1]).ok
if kind == 'none' then return {0} end
if kind ~= 'string' then return {3} end
if redis.call('STRLEN', KEYS[1]) > tonumber(ARGV[1]) then return {2} end
local value = redis.call('GET', KEYS[1])
if not value then return {0} end
return {1, value}
`

type redisArtifactConfig struct {
	Client           redisclient.Config
	Prefix           string
	OperationTimeout time.Duration
}

func defaultRedisArtifactConfig() redisArtifactConfig {
	return redisArtifactConfig{
		Client: redisclient.DefaultConfig().WithPoolSize(8).WithMinIdleConns(0),
		Prefix: "favicon:artifacts:v1:", OperationTimeout: 250 * time.Millisecond,
	}
}

// Validation happens before any connection attempt. Secrets remain in the
// client config and are never included in returned errors or cache keys.
func (c redisArtifactConfig) validate(maxBody int64) error {
	if maxBody <= 0 || maxBody > int64(math.MaxInt-artifactHeaderBytes) || len(c.Prefix) == 0 || len(c.Prefix) > 64 || c.OperationTimeout <= 0 || c.OperationTimeout > 5*time.Second || c.Client.DB < 0 || c.Client.PoolSize <= 0 || c.Client.PoolSize > 128 || c.Client.MinIdleConns < 0 || c.Client.MinIdleConns > c.Client.PoolSize || c.Client.DialTimeout <= 0 || c.Client.DialTimeout > 30*time.Second {
		return errRedisArtifactConfig
	}
	for _, ch := range c.Prefix {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune(":_-", ch)) {
			return errRedisArtifactConfig
		}
	}
	addrs := c.Client.Addrs
	if len(addrs) == 0 {
		addrs = []string{c.Client.Addr}
	}
	if len(addrs) > 16 || (len(addrs) > 1 && c.Client.MasterName == "" && c.Client.DB != 0) {
		return errRedisArtifactConfig
	}
	for _, addr := range addrs {
		host, port, err := net.SplitHostPort(addr)
		number, parseErr := strconv.Atoi(port)
		if err != nil || parseErr != nil || host == "" || strings.ContainsAny(host, " \t\r\n/@") || number < 1 || number > 65535 {
			return errRedisArtifactConfig
		}
	}
	if c.Client.TLSConfig != nil && c.Client.TLSConfig.InsecureSkipVerify {
		return errRedisArtifactConfig
	}
	return nil
}

type redisArtifactCommands interface {
	Eval(context.Context, string, []string, ...interface{}) *redis.Cmd
	SetArgs(context.Context, string, interface{}, redis.SetArgs) *redis.StatusCmd
	Del(context.Context, ...string) *redis.IntCmd
	Close() error
}

type redisArtifactStore struct {
	client    redisArtifactCommands
	prefix    string
	maxBody   int64
	timeout   time.Duration
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

var _ artifactStore = (*redisArtifactStore)(nil)

func newRedisArtifactStore(ctx context.Context, cfg redisArtifactConfig, maxBody int64) (*redisArtifactStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cfg.validate(maxBody); err != nil {
		return nil, err
	}
	clientCfg := cfg.Client.WithContextTimeoutEnabled(true).WithMaxRetries(-1).
		WithReadTimeout(cfg.OperationTimeout).WithWriteTimeout(cfg.OperationTimeout).
		WithPoolTimeout(cfg.OperationTimeout)
	if clientCfg.TLSConfig != nil {
		tlsConfig := clientCfg.TLSConfig.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
		clientCfg = clientCfg.WithTLSConfig(tlsConfig)
	}
	client, err := redisclient.NewUniversalClientContext(ctx, clientCfg)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errRedisArtifactUnavailable
	}
	return &redisArtifactStore{client: client, prefix: cfg.Prefix, maxBody: maxBody, timeout: cfg.OperationTimeout}, nil
}

func (s *redisArtifactStore) operation(ctx context.Context, key string) (context.Context, context.CancelFunc, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, "", err
	}
	if s.closed.Load() {
		return nil, nil, "", errRedisArtifactClosed
	}
	if len(key) != 64 || strings.ToLower(key) != key {
		return nil, nil, "", errInvalidArtifact
	}
	if _, err := hex.DecodeString(key); err != nil {
		return nil, nil, "", errInvalidArtifact
	}
	bounded, cancel := context.WithTimeout(ctx, s.timeout)
	return bounded, cancel, s.prefix + key, nil
}

func redisArtifactFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return errRedisArtifactUnavailable
}

func (s *redisArtifactStore) Get(ctx context.Context, key string) (artifactEntry, error) {
	operation, cancel, redisKey, err := s.operation(ctx, key)
	if err != nil {
		return artifactEntry{}, err
	}
	defer cancel()
	value, err := s.client.Eval(operation, redisArtifactGetScript, []string{redisKey}, artifactWireLimit(s.maxBody)).Result()
	if err != nil {
		return artifactEntry{}, redisArtifactFailure(ctx)
	}
	if operation.Err() != nil {
		return artifactEntry{}, redisArtifactFailure(ctx)
	}
	parts, ok := value.([]interface{})
	if !ok || len(parts) == 0 {
		return artifactEntry{}, errInvalidArtifact
	}
	code, ok := parts[0].(int64)
	if !ok {
		return artifactEntry{}, errInvalidArtifact
	}
	switch code {
	case 0:
		return artifactEntry{}, errArtifactMiss
	case 2:
		return artifactEntry{}, errArtifactTooLarge
	case 3:
		return artifactEntry{}, errInvalidArtifact
	case 1:
		if len(parts) != 2 {
			return artifactEntry{}, errInvalidArtifact
		}
	default:
		return artifactEntry{}, errInvalidArtifact
	}
	wire, ok := parts[1].(string)
	if !ok {
		return artifactEntry{}, errInvalidArtifact
	}
	if int64(len(wire)) > artifactWireLimit(s.maxBody) {
		return artifactEntry{}, errArtifactTooLarge
	}
	entry, err := decodeArtifact(operation, []byte(wire), key, s.maxBody)
	if err != nil {
		if operation.Err() != nil {
			return artifactEntry{}, redisArtifactFailure(ctx)
		}
		return artifactEntry{}, err
	}
	now := time.Now()
	if !entry.ExpiresAt.After(now) {
		return artifactEntry{}, errArtifactMiss
	}
	if entry.ExpiresAt.After(now.Add(maxArtifactTTL)) {
		return artifactEntry{}, errInvalidArtifact
	}
	if operation.Err() != nil {
		return artifactEntry{}, redisArtifactFailure(ctx)
	}
	return entry, nil
}

func (s *redisArtifactStore) Put(ctx context.Context, entry artifactEntry) error {
	operation, cancel, redisKey, err := s.operation(ctx, entry.Key)
	if err != nil {
		return err
	}
	defer cancel()
	wire, err := encodeArtifact(operation, entry, s.maxBody)
	if err != nil {
		if operation.Err() != nil {
			return redisArtifactFailure(ctx)
		}
		return err
	}
	// Absolute, downward-rounded expiry cannot be extended by queueing or a
	// slow connection. The envelope independently enforces the exact deadline.
	expiry := entry.ExpiresAt.Truncate(time.Millisecond)
	if !expiry.After(time.Now()) {
		return errInvalidArtifact
	}
	if err := s.client.SetArgs(operation, redisKey, wire, redis.SetArgs{ExpireAt: expiry}).Err(); err != nil {
		return redisArtifactFailure(ctx)
	}
	if operation.Err() != nil {
		return redisArtifactFailure(ctx)
	}
	return nil
}

func (s *redisArtifactStore) Delete(ctx context.Context, key string) error {
	operation, cancel, redisKey, err := s.operation(ctx, key)
	if err != nil {
		return err
	}
	defer cancel()
	if err := s.client.Del(operation, redisKey).Err(); err != nil {
		return redisArtifactFailure(ctx)
	}
	if operation.Err() != nil {
		return redisArtifactFailure(ctx)
	}
	return nil
}

func (s *redisArtifactStore) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if err := s.client.Close(); err != nil {
			s.closeErr = errRedisArtifactUnavailable
		}
	})
	return s.closeErr
}
