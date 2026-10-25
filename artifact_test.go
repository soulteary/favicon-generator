package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

func artifactTestLimits() imageproxy.SourceLimits {
	return imageproxy.SourceLimits{MaxResponseBytes: imageproxy.DefaultMaxResponseBytes, MaxPixels: imageproxy.DefaultMaxPixels, MaxFrames: imageproxy.DefaultMaxFrames}
}

func artifactTestMetadata(t *testing.T, data []byte) imageproxy.SourceMetadata {
	t.Helper()
	p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Type": {"image/png"}, "Date": {time.Now().UTC().Format(http.TimeFormat)}, "Cache-Control": {"public, max-age=3600"}}}, nil
	}), nil)
	u, _ := url.Parse("https://origin.test/logo")
	var metadata imageproxy.SourceMetadata
	if err := p.WithSourceMetadata(context.Background(), u, "", func(_ context.Context, _ []byte, _ imageproxy.SourceLimits, m imageproxy.SourceMetadata) error {
		metadata = m
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func artifactTestEntry(t *testing.T, filename string) artifactEntry {
	t.Helper()
	body, err := generateIcon(context.Background(), iconSourcePNG(t), artifactTestLimits(), filename, "Example", "Ex", nil)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("artifact-key"))
	return newArtifactEntry(hex.EncodeToString(key[:]), filename, body, time.Now().Add(time.Minute))
}

func TestArtifactIdentityAndRetention(t *testing.T) {
	metadata := artifactTestMetadata(t, iconSourcePNG(t))
	u, _ := url.Parse("https://origin.test/logo?token=SOURCE_SECRET")
	limits := artifactTestLimits()
	base, ok := artifactKey(u, metadata, "icons.zip", "name", "short", limits)
	if !ok || len(base) != 64 || strings.Contains(base, "SOURCE_SECRET") {
		t.Fatal("key not opaque")
	}
	for _, test := range []struct {
		u                     *url.URL
		filename, name, short string
		limits                imageproxy.SourceLimits
	}{
		{u, "favicon.ico", "name", "short", limits}, {u, "icons.zip", "different", "short", limits}, {u, "icons.zip", "name", "different", limits},
		{u, "icons.zip", "name", "short", imageproxy.SourceLimits{MaxResponseBytes: 1, MaxPixels: limits.MaxPixels, MaxFrames: limits.MaxFrames}},
		{&url.URL{Scheme: "https", Host: "origin.test", Path: "/other"}, "icons.zip", "name", "short", limits},
	} {
		key, ok := artifactKey(test.u, metadata, test.filename, test.name, test.short, test.limits)
		if !ok || key == base {
			t.Fatal("distinct output identities alias")
		}
	}
	for _, filename := range []string{"site.webmanifest", "../icons.zip", "unknown.png"} {
		if _, ok := artifactKey(u, metadata, filename, "", "", limits); ok {
			t.Fatal("unsafe product eligible")
		}
	}
	if _, ok := artifactKey(u, imageproxy.SourceMetadata{}, "icons.zip", "", "", limits); ok {
		t.Fatal("source policy bypass")
	}
	a, _ := artifactKey(u, metadata, "favicon-32x32.png", "a", "a", limits)
	b, _ := artifactKey(u, metadata, "favicon-32x32.png", "b", "b", limits)
	if a != b {
		t.Fatal("irrelevant PNG metadata fragments keys")
	}
	now := time.Now()
	until := now.Add(time.Minute)
	if !artifactExpiry(now, until, time.Hour).Equal(until) || !artifactExpiry(now, until, time.Second).Equal(now.Add(time.Second)) {
		t.Fatal("expiry not bounded by source and application")
	}
	for _, ttl := range []time.Duration{0, -1, maxArtifactTTL + 1} {
		if !artifactExpiry(now, until, ttl).IsZero() {
			t.Fatal("invalid TTL accepted")
		}
	}
}

func TestArtifactEnvelopeBoundsAndCorruption(t *testing.T) {
	ctx := context.Background()
	entry := artifactTestEntry(t, "favicon-32x32.png")
	wire, err := encodeArtifact(ctx, entry, int64(len(entry.Body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != len(entry.Body)+artifactHeaderBytes {
		t.Fatal("envelope expanded body")
	}
	decoded, err := decodeArtifact(ctx, wire, entry.Key, int64(len(entry.Body)))
	if err != nil || !bytes.Equal(decoded.Body, entry.Body) || !decoded.ExpiresAt.Equal(entry.ExpiresAt) {
		t.Fatal("roundtrip failed", err)
	}
	if _, err := decodeArtifact(ctx, wire, entry.Key, int64(len(entry.Body)-1)); !errors.Is(err, errArtifactTooLarge) {
		t.Fatal("wire cap not enforced", err)
	}
	if _, err := encodeArtifact(ctx, entry, int64(len(entry.Body)-1)); !errors.Is(err, errArtifactTooLarge) {
		t.Fatal("store cap not enforced", err)
	}
	for _, mutate := range []func([]byte){func(b []byte) { b[0] ^= 1 }, func(b []byte) { b[16] ^= 1 }, func(b []byte) { b[48] ^= 1 }, func(b []byte) { b[80] = 255 }, func(b []byte) { binary.BigEndian.PutUint64(b[81:89], ^uint64(0)) }, func(b []byte) { b[len(b)-1] ^= 1 }} {
		broken := bytes.Clone(wire)
		mutate(broken)
		if _, err := decodeArtifact(ctx, broken, entry.Key, int64(len(entry.Body))); err == nil {
			t.Fatal("corruption accepted")
		}
	}
	for n := 0; n < artifactHeaderBytes; n++ {
		if _, err := decodeArtifact(ctx, wire[:n], entry.Key, int64(len(entry.Body))); err == nil {
			t.Fatal("truncated header accepted")
		}
	}
	before := bytes.Clone(wire)
	decoded.Body[0] ^= 1
	if !bytes.Equal(before, wire) {
		t.Fatal("returned body aliases storage")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := decodeArtifact(ctx, wire, entry.Key, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := encodeArtifact(ctx, entry, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestArtifactHitRechecksCurrentProductBudgets(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, filename := range []string{"favicon-32x32.png", "favicon.ico", "icons.zip"} {
		t.Run(filename, func(t *testing.T) {
			entry := artifactTestEntry(t, filename)
			limits := artifactTestLimits()
			if err := validateArtifact(ctx, entry, entry.Key, filename, limits, now); err != nil {
				t.Fatal(err)
			}
			limits.MaxPixels = artifactPixels(filename) - 1
			if err := validateArtifact(ctx, entry, entry.Key, filename, limits, now); !errors.Is(err, imageproxy.ErrImageTooLarge) {
				t.Fatal("cached pixels bypassed budget", err)
			}
			limits = artifactTestLimits()
			limits.MaxResponseBytes = int64(len(entry.Body) - 1)
			if err := validateArtifact(ctx, entry, entry.Key, filename, limits, now); !errors.Is(err, imageproxy.ErrResponseTooLarge) {
				t.Fatal("cached bytes bypassed budget", err)
			}
			limits = artifactTestLimits()
			if err := validateArtifact(ctx, entry, entry.Key, filename, limits, entry.ExpiresAt); err == nil {
				t.Fatal("expired item accepted")
			}
			entry.Body = bytes.Clone(entry.Body)
			entry.Body[0] ^= 1
			entry.Digest = sha256.Sum256(entry.Body)
			if err := validateArtifact(ctx, entry, entry.Key, filename, limits, now); err == nil {
				t.Fatal("wrong product structure accepted")
			}
		})
	}
}

func FuzzDecodeArtifact(f *testing.F) {
	f.Add([]byte("FAVART01"))
	f.Add(make([]byte, artifactHeaderBytes))
	key := strings.Repeat("0", 64)
	f.Fuzz(func(t *testing.T, data []byte) {
		entry, err := decodeArtifact(context.Background(), data, key, 1<<20)
		if err == nil && (len(entry.Body) > 1<<20 || entry.Key != key) {
			t.Fatal("unbounded or misbound envelope")
		}
	})
}

func TestArtifactZIPDirectoryParsingBound(t *testing.T) {
	entry := artifactTestEntry(t, "icons.zip")
	// A matching body checksum and an advertised count of eight must not let
	// archive/zip allocate 65544 File objects before the final count check.
	header := make([]byte, 46+len("favicon.ico"))
	copy(header, []byte{'P', 'K', 1, 2})
	binary.LittleEndian.PutUint16(header[28:30], uint16(len("favicon.ico")))
	copy(header[46:], "favicon.ico")
	for _, count := range []int{9, 65536 + 8} {
		body := make([]byte, 4, 4+count*len(header)+22)
		copy(body, []byte{'P', 'K', 3, 4})
		for range count {
			body = append(body, header...)
		}
		tail := make([]byte, 22)
		copy(tail, []byte{'P', 'K', 5, 6})
		binary.LittleEndian.PutUint16(tail[8:10], 8)
		binary.LittleEndian.PutUint16(tail[10:12], 8)
		binary.LittleEndian.PutUint32(tail[12:16], uint32(count*len(header)))
		binary.LittleEndian.PutUint32(tail[16:20], 4)
		body = append(body, tail...)
		malformed := newArtifactEntry(entry.Key, entry.Filename, body, entry.ExpiresAt)
		var validationErr error
		allocations := testing.AllocsPerRun(1, func() {
			validationErr = validateArtifact(context.Background(), malformed, malformed.Key, malformed.Filename, artifactTestLimits(), time.Now())
		})
		if !errors.Is(validationErr, errInvalidArtifact) || allocations > 32 {
			t.Fatalf("actual directory count %d: error=%v allocations=%g; parsing must reject before per-entry allocation", count, validationErr, allocations)
		}
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[len(b)-22+12] ^= 1 },                                     // understated directory size
		func(b []byte) { b[len(b)-22+16] ^= 1 },                                     // shifted base offset
		func(b []byte) { binary.LittleEndian.PutUint32(b[len(b)-22+12:], 1<<32-1) }, // ZIP64 size sentinel
		func(b []byte) { binary.LittleEndian.PutUint32(b[len(b)-22+16:], 1<<32-1) }, // ZIP64 offset sentinel
		func(b []byte) {
			offset := binary.LittleEndian.Uint32(b[len(b)-22+16:])
			binary.LittleEndian.PutUint16(b[offset+28:offset+30], 65535)
		}, // variable-length header exceeds the directory
	} {
		body := bytes.Clone(entry.Body)
		mutate(body)
		if err := validateArtifactZIPDirectory(body); !errors.Is(err, errInvalidArtifact) {
			t.Fatal("malformed directory accepted", err)
		}
	}
}
