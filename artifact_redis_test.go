package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type artifactRedisFake struct {
	calls  atomic.Int32
	closes atomic.Int32
	eval   func(context.Context, string, []string, ...interface{}) *redis.Cmd
	set    func(context.Context, string, interface{}, redis.SetArgs) *redis.StatusCmd
	del    func(context.Context, ...string) *redis.IntCmd
}

func (f *artifactRedisFake) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	f.calls.Add(1)
	if f.eval != nil {
		return f.eval(ctx, script, keys, args...)
	}
	return redis.NewCmdResult([]interface{}{int64(0)}, nil)
}
func (f *artifactRedisFake) SetArgs(ctx context.Context, key string, val interface{}, args redis.SetArgs) *redis.StatusCmd {
	f.calls.Add(1)
	if f.set != nil {
		return f.set(ctx, key, val, args)
	}
	return redis.NewStatusResult("OK", nil)
}
func (f *artifactRedisFake) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	f.calls.Add(1)
	if f.del != nil {
		return f.del(ctx, keys...)
	}
	return redis.NewIntResult(1, nil)
}
func (f *artifactRedisFake) Close() error { f.closes.Add(1); return nil }

func redisArtifactTestKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func redisArtifactTestEntry() artifactEntry {
	return newArtifactEntry(redisArtifactTestKey("redis fixture"), "favicon.ico", []byte("binary icon fixture"), time.Now().Add(time.Minute))
}
func redisArtifactTestStore(client redisArtifactCommands) *redisArtifactStore {
	return &redisArtifactStore{client: client, prefix: "test:artifact:", maxBody: 4096, timeout: 50 * time.Millisecond}
}

func TestRedisArtifactConfigRejectsInvalidBeforeDial(t *testing.T) {
	for _, change := range []func(*redisArtifactConfig){
		func(c *redisArtifactConfig) { c.Prefix = "" },
		func(c *redisArtifactConfig) { c.Prefix = strings.Repeat("x", 65) },
		func(c *redisArtifactConfig) { c.Prefix = "https://secret.example/" },
		func(c *redisArtifactConfig) { c.OperationTimeout = 0 },
		func(c *redisArtifactConfig) { c.OperationTimeout = 6 * time.Second },
		func(c *redisArtifactConfig) { c.Client.Addr = "bad" },
		func(c *redisArtifactConfig) { c.Client.Addr = "localhost:0" },
		func(c *redisArtifactConfig) { c.Client.Addr = "user@localhost:6379" },
		func(c *redisArtifactConfig) { c.Client.DB = -1 },
		func(c *redisArtifactConfig) { c.Client.PoolSize = 0 },
		func(c *redisArtifactConfig) { c.Client.MinIdleConns = 100 },
		func(c *redisArtifactConfig) { c.Client.DialTimeout = 0 },
		func(c *redisArtifactConfig) {
			c.Client.Addrs = []string{"localhost:6379", "localhost:6380"}
			c.Client.DB = 1
		},
		func(c *redisArtifactConfig) { c.Client.TLSConfig = &tls.Config{InsecureSkipVerify: true} },
	} {
		cfg := defaultRedisArtifactConfig()
		change(&cfg)
		var calls atomic.Int32
		cfg.Client.Dialer = func(context.Context, string, string) (net.Conn, error) {
			calls.Add(1)
			return nil, errors.New("unexpected dial")
		}
		if store, err := newRedisArtifactStore(context.Background(), cfg, 4096); store != nil || !errors.Is(err, errRedisArtifactConfig) || calls.Load() != 0 {
			t.Fatalf("invalid config reached network: %v, %v, calls=%d", store, err, calls.Load())
		}
	}
}

func TestRedisArtifactReplyValidation(t *testing.T) {
	entry := redisArtifactTestEntry()
	wire, err := encodeArtifact(context.Background(), entry, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		reply interface{}
		want  error
	}{
		{"missing", []interface{}{int64(0)}, errArtifactMiss},
		{"too large", []interface{}{int64(2)}, errArtifactTooLarge},
		{"wrong type", []interface{}{int64(3)}, errInvalidArtifact},
		{"not array", "payload", errInvalidArtifact},
		{"empty", []interface{}{}, errInvalidArtifact},
		{"wrong code type", []interface{}{1}, errInvalidArtifact},
		{"unknown code", []interface{}{int64(9)}, errInvalidArtifact},
		{"missing payload", []interface{}{int64(1)}, errInvalidArtifact},
		{"binary type", []interface{}{int64(1), []byte("wrong type")}, errInvalidArtifact},
		{"oversized payload", []interface{}{int64(1), strings.Repeat("x", 4096+artifactHeaderBytes+1)}, errArtifactTooLarge},
		{"corrupt envelope", []interface{}{int64(1), "invalid"}, errInvalidArtifact},
		{"valid", []interface{}{int64(1), string(wire)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &artifactRedisFake{eval: func(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
				if script != redisArtifactGetScript || len(keys) != 1 || keys[0] != "test:artifact:"+entry.Key || len(args) != 1 || args[0] != artifactWireLimit(4096) {
					t.Fatalf("unbounded or miskeyed read: %q, %v, %v", script, keys, args)
				}
				return redis.NewCmdResult(tc.reply, nil)
			}}
			got, err := redisArtifactTestStore(fake).Get(context.Background(), entry.Key)
			if !errors.Is(err, tc.want) || (tc.want == nil && (string(got.Body) != string(entry.Body) || got.Digest != entry.Digest)) {
				t.Fatalf("Get = %+v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestRedisArtifactCancelledAndInvalidNeverTouchBackend(t *testing.T) {
	fake := new(artifactRedisFake)
	store := redisArtifactTestStore(fake)
	entry := redisArtifactTestEntry()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operation := range []func(context.Context, string) error{
		func(ctx context.Context, key string) error { _, err := store.Get(ctx, key); return err },
		func(ctx context.Context, key string) error {
			value := entry
			value.Key = key
			return store.Put(ctx, value)
		},
		store.Delete,
	} {
		if err := operation(ctx, entry.Key); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation became cache miss: %v", err)
		}
		for _, key := range []string{"", "https://secret.example?signature=secret", strings.Repeat("z", 64), strings.ToUpper(entry.Key)} {
			if err := operation(context.Background(), key); !errors.Is(err, errInvalidArtifact) {
				t.Fatalf("invalid key admitted: %v", err)
			}
		}
	}
	bad := entry
	bad.Body = make([]byte, 4097)
	if err := store.Put(context.Background(), bad); !errors.Is(err, errArtifactTooLarge) {
		t.Fatal(err)
	}
	bad = entry
	bad.ExpiresAt = time.Now().Add(-time.Second)
	if err := store.Put(context.Background(), bad); !errors.Is(err, errInvalidArtifact) {
		t.Fatal(err)
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("refused operation touched Redis %d times", fake.calls.Load())
	}
}

func TestRedisArtifactPutUsesAbsoluteExpiryAndBinary(t *testing.T) {
	entry := redisArtifactTestEntry()
	fake := &artifactRedisFake{set: func(ctx context.Context, key string, value interface{}, args redis.SetArgs) *redis.StatusCmd {
		wire, ok := value.([]byte)
		if !ok || len(wire) != artifactHeaderBytes+len(entry.Body) || key != "test:artifact:"+entry.Key || args.TTL != 0 || args.KeepTTL || !args.ExpireAt.Equal(entry.ExpiresAt.Truncate(time.Millisecond)) {
			t.Fatalf("SET lost wire/expiry bounds: %v, %+v", value, args)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 50*time.Millisecond {
			t.Fatal("Redis operation has no bounded deadline")
		}
		return redis.NewStatusResult("OK", nil)
	}}
	if err := redisArtifactTestStore(fake).Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

func TestRedisArtifactBackendFailureAndDeadlineClassification(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		ctx := context.Background()
		if callerDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 5*time.Millisecond)
			defer cancel()
		}
		fake := &artifactRedisFake{eval: func(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
			<-ctx.Done()
			return redis.NewCmdResult(nil, errors.New("sensitive backend details"))
		}}
		store := redisArtifactTestStore(fake)
		_, err := store.Get(ctx, redisArtifactTestEntry().Key)
		want := errRedisArtifactUnavailable
		if callerDeadline {
			want = context.DeadlineExceeded
		}
		if !errors.Is(err, want) || errors.Is(err, errArtifactMiss) || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("backend/request deadline = %v, want %v", err, want)
		}
	}
}

func TestRedisArtifactCloseIsIdempotent(t *testing.T) {
	fake := new(artifactRedisFake)
	store := redisArtifactTestStore(fake)
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() { defer workers.Done(); _ = store.Close() }()
	}
	workers.Wait()
	if fake.closes.Load() != 1 {
		t.Fatalf("client closed %d times", fake.closes.Load())
	}
	if _, err := store.Get(context.Background(), redisArtifactTestEntry().Key); !errors.Is(err, errRedisArtifactClosed) || fake.calls.Load() != 0 {
		t.Fatalf("closed cache touched backend: %v", err)
	}
}
