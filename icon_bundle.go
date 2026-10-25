package main

import (
	"bytes"
	"context"
	"errors"
	"math"

	vfs "github.com/soulteary/vfs-kit"
	"github.com/willnorris/imageproxy"
)

var errInvalidIconBundleName = errors.New("invalid icon bundle filename")

// encodeIconBundle owns its in-memory tree for this request. Both the staged
// source bytes and the final ZIP must fit maxBytes; no archive is read or
// extracted, and callers cannot select paths outside the fixed product files.
func encodeIconBundle(ctx context.Context, files map[string][]byte, maxBytes int64) (_ []byte, err error) {
	finish := imageproxy.StartStage(ctx, "pack")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, imageproxy.ErrResponseTooLarge
	}
	// The result is one Go byte slice, including on a 32-bit target.
	maxBytes = min(maxBytes, int64(math.MaxInt))
	fs, err := stageIconBundle(ctx, files, maxBytes)
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
