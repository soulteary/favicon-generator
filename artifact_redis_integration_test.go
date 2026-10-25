package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	redisclient "github.com/soulteary/redis-kit/client"
)

func redisArtifactIntegration(t *testing.T) (*redisArtifactStore, redis.UniversalClient, redisArtifactConfig) {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		if os.Getenv("REDIS_TEST_REQUIRED") == "1" {
			t.Fatal("REDIS_TEST_ADDR is required for the Redis integration job")
		}
		t.Skip("set REDIS_TEST_ADDR to run against a disposable Redis instance")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	cfg := defaultRedisArtifactConfig()
	cfg.Prefix = "favicon:test:" + hex.EncodeToString(nonce[:]) + ":"
	cfg.OperationTimeout = 500 * time.Millisecond
	cfg.Client = cfg.Client.WithAddr(addr).WithUsername(os.Getenv("REDIS_TEST_USERNAME")).WithPassword(os.Getenv("REDIS_TEST_PASSWORD"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	admin, err := redisclient.NewUniversalClientContext(ctx, cfg.Client.WithContextTimeoutEnabled(true).WithMaxRetries(-1))
	if err != nil {
		t.Fatal("cannot connect to configured test Redis")
	}
	store, err := newRedisArtifactStore(ctx, cfg, 4096)
	if err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		var cursor uint64
		for range 20 {
			keys, next, err := admin.Scan(cleanup, cursor, cfg.Prefix+"*", 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				_ = admin.Del(cleanup, keys...).Err()
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		_ = admin.Close()
	})
	return store, admin, cfg
}

func TestRedisArtifactIntegrationRoundTrip(t *testing.T) {
	store, admin, cfg := redisArtifactIntegration(t)
	entry := redisArtifactTestEntry()
	ctx := context.Background()
	if err := store.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, entry.Key)
	if err != nil || !bytes.Equal(got.Body, entry.Body) || got.Key != entry.Key || got.Digest != entry.Digest || !got.ExpiresAt.Equal(entry.ExpiresAt) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	got.Body[0] ^= 1
	fresh, err := store.Get(ctx, entry.Key)
	if err != nil || !bytes.Equal(fresh.Body, entry.Body) {
		t.Fatalf("returned bytes alias Redis or another result: %v", err)
	}
	ttl, err := admin.PTTL(ctx, cfg.Prefix+entry.Key).Result()
	if err != nil || ttl <= 0 || ttl > time.Until(entry.ExpiresAt)+time.Millisecond {
		t.Fatalf("absolute expiry: ttl=%v err=%v", ttl, err)
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := store.Put(ctx, entry); err != nil {
				t.Error(err)
				return
			}
			if value, err := store.Get(ctx, entry.Key); err != nil || !bytes.Equal(value.Body, entry.Body) {
				t.Errorf("parallel snapshot: %v", err)
			}
		}()
	}
	workers.Wait()
	if err := store.Delete(ctx, entry.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, entry.Key); !errors.Is(err, errArtifactMiss) {
		t.Fatalf("delete did not remove entry: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRedisArtifactIntegrationBoundedAndCorruptReads(t *testing.T) {
	store, admin, cfg := redisArtifactIntegration(t)
	ctx := context.Background()
	entry := redisArtifactTestEntry()
	key := cfg.Prefix + entry.Key
	if err := admin.Set(ctx, key, bytes.Repeat([]byte{'x'}, 1<<20), time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	// This actual Redis Lua result carries only a refusal code, not the 1 MiB
	// value. The size gate runs before GET in the same atomic script.
	reply, err := admin.Eval(ctx, redisArtifactGetScript, []string{key}, artifactWireLimit(4096)).Slice()
	if err != nil || len(reply) != 1 || reply[0] != int64(2) {
		t.Fatalf("oversized payload escaped server-side gate: %v, %v", reply, err)
	}
	if _, err := store.Get(ctx, entry.Key); !errors.Is(err, errArtifactTooLarge) {
		t.Fatal(err)
	}
	if err := admin.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	if err := admin.HSet(ctx, key, "field", "value").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, entry.Key); !errors.Is(err, errInvalidArtifact) {
		t.Fatalf("wrong Redis type: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func([]byte)
		want   error
	}{
		{"body digest", func(w []byte) { w[len(w)-1] ^= 1 }, errInvalidArtifact},
		{"key binding", func(w []byte) { w[16] ^= 1 }, errInvalidArtifact},
		{"length overflow", func(w []byte) { binary.BigEndian.PutUint64(w[81:89], ^uint64(0)) }, errInvalidArtifact},
		{"expired envelope", func(w []byte) { binary.BigEndian.PutUint64(w[8:16], uint64(time.Now().Add(-time.Second).UnixNano())) }, errArtifactMiss},
		{"future envelope", func(w []byte) {
			binary.BigEndian.PutUint64(w[8:16], uint64(time.Now().Add(2*maxArtifactTTL).UnixNano()))
		}, errInvalidArtifact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := encodeArtifact(ctx, entry, 4096)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(wire)
			if err := admin.Set(ctx, key, wire, time.Minute).Err(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(ctx, entry.Key); !errors.Is(err, tc.want) {
				t.Fatalf("corrupt entry: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRedisArtifactIntegrationACLAndCommandDeadlines(t *testing.T) {
	store, admin, cfg := redisArtifactIntegration(t)
	ctx := context.Background()
	username := "favicon-" + strings.TrimSuffix(strings.TrimPrefix(cfg.Prefix, "favicon:test:"), ":")
	password := "integration-only-password"
	if err := admin.ACLSetUser(ctx, username, "on", ">"+password, "~"+cfg.Prefix+"*", "+@all").Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.ACLDelUser(context.Background(), username).Err() })
	cfg.Client = cfg.Client.WithUsername(username).WithPassword(password)
	authenticated, err := newRedisArtifactStore(ctx, cfg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticated.Close()
	entry := redisArtifactTestEntry()
	if err := authenticated.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticated.Get(ctx, entry.Key); err != nil {
		t.Fatal(err)
	}
	if single, ok := authenticated.client.(*redis.Client); !ok || !single.Options().ContextTimeoutEnabled || single.Options().MaxRetries != 0 || single.Options().PoolSize != 8 {
		t.Fatal("Redis client deadline/retry/pool policy was not applied")
	}
	cfg.Client = cfg.Client.WithPassword("incorrect-secret")
	if bad, err := newRedisArtifactStore(ctx, cfg, 4096); bad != nil || !errors.Is(err, errRedisArtifactUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("failed ACL authentication was not sanitized: %v", err)
	}
	store.timeout = 25 * time.Millisecond
	if err := admin.Do(ctx, "CLIENT", "PAUSE", "200", "ALL").Err(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := store.Get(ctx, entry.Key); !errors.Is(err, errRedisArtifactUnavailable) || time.Since(start) > time.Second {
		t.Fatalf("network stall did not become bounded backend failure: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := store.Get(ctx, entry.Key); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("backend did not recover after Redis resumed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The client TLS connection carries real Redis commands through a local TLS
// terminator. All accepted connections and copy workers are joined on cleanup.
func redisArtifactTLSProxy(t *testing.T, upstream string) (string, *tls.Config) {
	t.Helper()
	certificateSource := httptest.NewTLSServer(http.NotFoundHandler())
	serverTLS := certificateSource.TLS.Clone()
	roots := x509.NewCertPool()
	roots.AddCert(certificateSource.Certificate())
	certificateSource.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	stopping := false
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			incoming, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if stopping {
				mu.Unlock()
				_ = incoming.Close()
				return
			}
			connections[incoming] = true
			workers.Add(1)
			mu.Unlock()
			go func() {
				defer workers.Done()
				defer incoming.Close()
				remote, err := net.DialTimeout("tcp", upstream, time.Second)
				if err != nil {
					return
				}
				defer remote.Close()
				mu.Lock()
				if stopping {
					mu.Unlock()
					return
				}
				connections[remote] = true
				mu.Unlock()
				defer func() { mu.Lock(); delete(connections, incoming); delete(connections, remote); mu.Unlock() }()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(remote, incoming); _ = remote.Close(); close(done) }()
				_, _ = io.Copy(incoming, remote)
				_ = incoming.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		stopping = true
		for conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
		_ = listener.Close()
		workers.Wait()
	})
	return listener.Addr().String(), &tls.Config{RootCAs: roots}
}

func TestRedisArtifactIntegrationVerifiedTLS(t *testing.T) {
	_, _, cfg := redisArtifactIntegration(t)
	addr, clientTLS := redisArtifactTLSProxy(t, cfg.Client.Addr)
	cfg.Client = cfg.Client.WithAddr(addr).WithTLSConfig(clientTLS)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store, err := newRedisArtifactStore(ctx, cfg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry := redisArtifactTestEntry()
	if err := store.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, entry.Key); err != nil {
		t.Fatal(err)
	}
	if single := store.client.(*redis.Client); single.Options().TLSConfig.MinVersion < tls.VersionTLS12 || clientTLS.MinVersion != 0 {
		t.Fatal("TLS minimum was not applied to an isolated config clone")
	}
	cfg.Client = cfg.Client.WithTLSConfig(&tls.Config{RootCAs: x509.NewCertPool()})
	if bad, err := newRedisArtifactStore(ctx, cfg, 4096); bad != nil || !errors.Is(err, errRedisArtifactUnavailable) {
		t.Fatalf("untrusted certificate was accepted: %v", err)
	}
}
