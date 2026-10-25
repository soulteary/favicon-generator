package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"time"

	vfs "github.com/soulteary/vfs-kit"
	"github.com/willnorris/imageproxy"
)

var errInvalidIconBundleName = errors.New("invalid icon bundle filename")

// encodeIconBundle owns its in-memory tree for this request. Both the staged
// source bytes and the final ZIP must fit maxBytes; no archive is read or
// extracted, and callers cannot select paths outside the fixed product files.
func encodeIconBundle(ctx context.Context, files map[string][]byte, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, imageproxy.ErrResponseTooLarge
	}
	// The result is one Go byte slice, including on a 32-bit target.
	maxBytes = min(maxBytes, int64(math.MaxInt))
	names := make([]string, 0, 8)
	remaining := maxBytes
	for name, data := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !validIconBundleName(name) {
			return nil, errInvalidIconBundleName
		}
		// Subtract instead of summing lengths, avoiding aggregate overflow.
		if int64(len(data)) > remaining {
			return nil, imageproxy.ErrResponseTooLarge
		}
		remaining -= int64(len(data))
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make(map[string]*vfs.File, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries[name] = &vfs.File{
			Data: bytes.Clone(files[name]), Mode: 0644,
			// A fixed representable ZIP date makes equal bundles reproducible.
			ModTime: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fs, err := vfs.Map(entries)
	if err != nil {
		return nil, err
	}
	output := &iconBundleWriter{ctx: ctx, remaining: maxBytes}
	if err := vfs.WriteZip(output, fs); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return output.buffer.Bytes(), nil
}

func validIconBundleName(name string) bool {
	switch name {
	case "favicon.ico", "favicon-16x16.png", "favicon-32x32.png", "favicon-48x48.png",
		"apple-touch-icon.png", "android-chrome-192x192.png", "android-chrome-512x512.png", "site.webmanifest":
		return true
	default:
		return false
	}
}

type iconBundleWriter struct {
	ctx       context.Context
	remaining int64
	buffer    bytes.Buffer
}

func (w *iconBundleWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, imageproxy.ErrResponseTooLarge
	}
	n, err := w.buffer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
