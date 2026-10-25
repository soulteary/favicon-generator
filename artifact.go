package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"math"
	"net/url"
	"time"

	"github.com/willnorris/imageproxy"
)

var (
	errArtifactMiss     = errors.New("artifact not found")
	errInvalidArtifact  = errors.New("invalid cached artifact")
	errArtifactTooLarge = errors.New("cached artifact exceeds storage limit")
)

const (
	artifactSchema      = "FAVART01"
	artifactHeaderBytes = 8 + 8 + 32 + 32 + 1 + 8
	maxArtifactTTL      = 24 * time.Hour
	artifactRenderer    = "icons-v1"
)

// Backends return owned immutable snapshots. Operations are bounded by the
// caller's context; Close is called only after admitted handlers have drained.
type artifactStore interface {
	Get(context.Context, string) (artifactEntry, error)
	Put(context.Context, artifactEntry) error
	Delete(context.Context, string) error
	Close() error
}

type artifactEntry struct {
	Key       string
	Filename  string
	Body      []byte
	Digest    [sha256.Size]byte
	ExpiresAt time.Time
}

var artifactProducts = []string{
	"favicon.ico", "favicon-16x16.png", "favicon-32x32.png", "favicon-48x48.png",
	"apple-touch-icon.png", "android-chrome-192x192.png", "android-chrome-512x512.png", "icons.zip",
}

func artifactProduct(filename string) (byte, bool) {
	for i, name := range artifactProducts {
		if filename == name {
			return byte(i), true
		}
	}
	return 0, false
}

// Keys bind the original source identity and actual bytes, not just a URL that
// might later serve a different image. Standalone signed manifests are excluded.
func artifactKey(source *url.URL, metadata imageproxy.SourceMetadata, filename, name, shortName string, limits imageproxy.SourceLimits) (string, bool) {
	if source == nil || !metadata.SharedCacheAllowed() {
		return "", false
	}
	if _, ok := artifactProduct(filename); !ok {
		return "", false
	}
	if filename != "icons.zip" {
		name, shortName = "", ""
	}
	identity := struct {
		Renderer                  string
		Source, Content, Variant  [sha256.Size]byte
		Filename, Name, ShortName string
		Limits                    imageproxy.SourceLimits
	}{artifactRenderer, sha256.Sum256([]byte(source.String())), metadata.ContentDigest(), metadata.VariantDigest(), filename, name, shortName, limits}
	data, _ := json.Marshal(identity)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), true
}

func artifactExpiry(now, sourceUntil time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 || ttl > maxArtifactTTL || !sourceUntil.After(now) {
		return time.Time{}
	}
	return minTime(now.Add(ttl), sourceUntil)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func newArtifactEntry(key, filename string, body []byte, expiry time.Time) artifactEntry {
	return artifactEntry{Key: key, Filename: filename, Body: body, Digest: sha256.Sum256(body), ExpiresAt: expiry}
}

// Both local and remote stores use one versioned binary envelope, avoiding JSON
// base64 expansion. The wire limit can be enforced before Redis returns a value.
func artifactWireLimit(maxBody int64) int64 {
	if maxBody <= 0 {
		return 0
	}
	return min(maxBody, int64(math.MaxInt-artifactHeaderBytes)) + artifactHeaderBytes
}

func encodeArtifact(ctx context.Context, entry artifactEntry, maxBody int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(entry.Body) == 0 || int64(len(entry.Body)) > maxBody || len(entry.Body) > math.MaxInt-artifactHeaderBytes {
		return nil, errArtifactTooLarge
	}
	key, err := hex.DecodeString(entry.Key)
	id, valid := artifactProduct(entry.Filename)
	now := time.Now()
	if err != nil || len(key) != sha256.Size || !valid || !entry.ExpiresAt.After(now) || entry.ExpiresAt.After(now.Add(maxArtifactTTL)) || sha256.Sum256(entry.Body) != entry.Digest {
		return nil, errInvalidArtifact
	}
	output := make([]byte, artifactHeaderBytes+len(entry.Body))
	copy(output[:8], artifactSchema)
	binary.BigEndian.PutUint64(output[8:16], uint64(entry.ExpiresAt.UnixNano()))
	copy(output[16:48], key)
	copy(output[48:80], entry.Digest[:])
	output[80] = id
	binary.BigEndian.PutUint64(output[81:89], uint64(len(entry.Body)))
	copy(output[artifactHeaderBytes:], entry.Body)
	return output, ctx.Err()
}

func decodeArtifact(ctx context.Context, data []byte, key string, maxBody int64) (artifactEntry, error) {
	var entry artifactEntry
	if err := ctx.Err(); err != nil {
		return entry, err
	}
	if int64(len(data)) > artifactWireLimit(maxBody) {
		return entry, errArtifactTooLarge
	}
	if len(data) < artifactHeaderBytes || string(data[:8]) != artifactSchema || int(data[80]) >= len(artifactProducts) {
		return entry, errInvalidArtifact
	}
	length := binary.BigEndian.Uint64(data[81:89])
	if length == 0 || length > uint64(maxBody) || length != uint64(len(data)-artifactHeaderBytes) {
		return entry, errInvalidArtifact
	}
	if hex.EncodeToString(data[16:48]) != key {
		return entry, errInvalidArtifact
	}
	entry = artifactEntry{Key: key, Filename: artifactProducts[data[80]], ExpiresAt: time.Unix(0, int64(binary.BigEndian.Uint64(data[8:16])))}
	copy(entry.Digest[:], data[48:80])
	if sha256.Sum256(data[artifactHeaderBytes:]) != entry.Digest {
		return artifactEntry{}, errInvalidArtifact
	}
	entry.Body = bytes.Clone(data[artifactHeaderBytes:])
	return entry, ctx.Err()
}

func artifactPixels(filename string) int64 {
	if size, ok := iconPNGSizes[filename]; ok {
		return int64(size * size)
	}
	sizes := map[int]bool{}
	for _, size := range icoSizes {
		sizes[size] = true
	}
	if filename == "icons.zip" {
		for _, size := range iconPNGSizes {
			sizes[size] = true
		}
	}
	var total int64
	for size := range sizes {
		total += int64(size * size)
	}
	return total
}

func validateArtifact(ctx context.Context, entry artifactEntry, key, filename string, limits imageproxy.SourceLimits, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry.Key != key || entry.Filename != filename || !entry.ExpiresAt.After(now) || entry.ExpiresAt.After(now.Add(maxArtifactTTL)) || len(entry.Body) == 0 || sha256.Sum256(entry.Body) != entry.Digest {
		return errInvalidArtifact
	}
	if _, valid := artifactProduct(filename); !valid {
		return errInvalidArtifact
	}
	if int64(len(entry.Body)) > limits.MaxResponseBytes {
		return imageproxy.ErrResponseTooLarge
	}
	if artifactPixels(filename) > limits.MaxPixels {
		return imageproxy.ErrImageTooLarge
	}
	if size, ok := iconPNGSizes[filename]; ok {
		config, err := png.DecodeConfig(bytes.NewReader(entry.Body))
		if err != nil || config.Width != size || config.Height != size {
			return errInvalidArtifact
		}
	} else if filename == "favicon.ico" {
		if err := validateArtifactICO(entry.Body); err != nil {
			return err
		}
	} else {
		archive, err := zip.NewReader(bytes.NewReader(entry.Body), int64(len(entry.Body)))
		if err != nil || len(archive.File) != 8 {
			return errInvalidArtifact
		}
		seen := map[string]bool{}
		remaining := uint64(limits.MaxResponseBytes)
		for _, file := range archive.File {
			if !validIconBundleName(file.Name) || seen[file.Name] || file.UncompressedSize64 == 0 || file.UncompressedSize64 > remaining || file.Flags&1 != 0 {
				return errInvalidArtifact
			}
			remaining -= file.UncompressedSize64
			seen[file.Name] = true
		}
	}
	return ctx.Err()
}

func validateArtifactICO(data []byte) error {
	if len(data) < 6+16*len(icoSizes) || binary.LittleEndian.Uint16(data[:2]) != 0 || binary.LittleEndian.Uint16(data[2:4]) != 1 || int(binary.LittleEndian.Uint16(data[4:6])) != len(icoSizes) {
		return errInvalidArtifact
	}
	end := uint64(6 + 16*len(icoSizes))
	for i, size := range icoSizes {
		field := data[6+16*i : 6+16*(i+1)]
		w, h := int(field[0]), int(field[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		offset, length := uint64(binary.LittleEndian.Uint32(field[12:16])), uint64(binary.LittleEndian.Uint32(field[8:12]))
		if w != size || h != size || offset != end || length == 0 || offset > uint64(len(data)) || length > uint64(len(data))-offset {
			return errInvalidArtifact
		}
		config, err := png.DecodeConfig(bytes.NewReader(data[int(offset):int(offset+length)]))
		if err != nil || config.Width != size || config.Height != size {
			return errInvalidArtifact
		}
		end = offset + length
	}
	if end != uint64(len(data)) {
		return errInvalidArtifact
	}
	return nil
}
