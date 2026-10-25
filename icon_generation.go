package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/url"
	"sort"
	"strconv"

	"github.com/willnorris/imageproxy"
	"golang.org/x/image/draw"
)

var iconPNGSizes = map[string]int{
	"favicon-16x16.png":          16,
	"favicon-32x32.png":          32,
	"favicon-48x48.png":          48,
	"apple-touch-icon.png":       180,
	"android-chrome-192x192.png": 192,
	"android-chrome-512x512.png": 512,
}

var icoSizes = []int{16, 32, 48, 256}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

type iconManifest struct {
	Name      string         `json:"name"`
	ShortName string         `json:"short_name"`
	Icons     []manifestIcon `json:"icons"`
	Display   string         `json:"display"`
}

// A bundle uses relative filenames so it can be deployed in any directory.
// A standalone manifest retains the source and signature in its PNG links.
// Deliberately omit start_url/scope: this service cannot know the app's URL.
func makeIconManifest(ctx context.Context, name, shortName string, source url.Values, maxBytes int64) (_ []byte, err error) {
	finish := imageproxy.StartStage(ctx, "encode")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, imageproxy.ErrResponseTooLarge
	}
	m := iconManifest{Name: name, ShortName: shortName, Display: "standalone"}
	for _, size := range []int{192, 512} {
		filename := "android-chrome-" + strconv.Itoa(size) + "x" + strconv.Itoa(size) + ".png"
		src := filename
		if source != nil {
			src = "/icons/" + filename + "?" + source.Encode()
		}
		m.Icons = append(m.Icons, manifestIcon{src, strconv.Itoa(size) + "x" + strconv.Itoa(size), "image/png", "any"})
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err == nil && int64(len(data)) > maxBytes {
		err = imageproxy.ErrResponseTooLarge
	}
	if err == nil {
		err = ctx.Err()
	}
	return data, err
}

func generateIcon(ctx context.Context, data []byte, limits imageproxy.SourceLimits, filename, name, shortName string, source url.Values) ([]byte, error) {
	if filename == "site.webmanifest" {
		return makeIconManifest(ctx, name, shortName, source, limits.MaxResponseBytes)
	}
	plan := imageproxy.StartStage(ctx, "transform")
	if err := ctx.Err(); err != nil {
		plan(err)
		return nil, err
	}
	sizes := map[int]bool{}
	if size, ok := iconPNGSizes[filename]; ok {
		sizes[size] = true
	} else {
		for _, size := range icoSizes {
			sizes[size] = true
		}
		if filename == "icons.zip" {
			for _, size := range iconPNGSizes {
				sizes[size] = true
			}
		}
	}
	// Count all unique output canvases before allocating any of them. The
	// original image and its animation are separately checked by WithSource.
	var pixels int64
	ordered := make([]int, 0, len(sizes))
	for size := range sizes {
		pixels += int64(size * size)
		ordered = append(ordered, size)
	}
	if pixels > limits.MaxPixels {
		plan(imageproxy.ErrImageTooLarge)
		return nil, imageproxy.ErrImageTooLarge
	}
	sort.Ints(ordered)
	maxSize := ordered[len(ordered)-1]
	err := ctx.Err()
	plan(err)
	if err != nil {
		return nil, err
	}
	// Decode the original only once, including EXIF orientation and safe GIF
	// flattening. All subsequent operations use at most a 512px square.
	normalized, err := imageproxy.TransformWithBudget(ctx, data, imageproxy.Options{
		Width: float64(maxSize), Height: float64(maxSize), Fit: true, Format: "png", ScaleUp: true,
	}, limits)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeIconPNG(ctx, normalized)
	if err != nil {
		return nil, err
	}
	base, err := scaleIcon(ctx, decoded, maxSize)
	if err != nil {
		return nil, err
	}
	encoded := make(map[int][]byte, len(sizes))
	remaining := limits.MaxResponseBytes
	for _, size := range ordered {
		var img image.Image = base
		if size != maxSize {
			img, err = scaleIcon(ctx, base, size)
			if err != nil {
				return nil, err
			}
		}
		encoded[size], err = encodeIconPNG(ctx, img, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(encoded[size]))
	}
	if size, ok := iconPNGSizes[filename]; ok {
		return encoded[size], ctx.Err()
	}
	frames := make([]icoFrame, 0, len(icoSizes))
	for _, size := range icoSizes {
		frames = append(frames, icoFrame{Size: size, PNG: encoded[size]})
	}
	icon, err := encodeICO(ctx, frames, remaining)
	if err != nil {
		return nil, err
	}
	if filename == "favicon.ico" {
		return icon, nil
	}
	remaining -= int64(len(icon))
	manifest, err := makeIconManifest(ctx, name, shortName, nil, remaining)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"favicon.ico": icon, "site.webmanifest": manifest}
	for filename, size := range iconPNGSizes {
		files[filename] = encoded[size]
	}
	return encodeIconBundle(ctx, files, limits.MaxResponseBytes)
}

func decodeIconPNG(ctx context.Context, data []byte) (_ image.Image, err error) {
	finish := imageproxy.StartStage(ctx, "decode")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoded, err := png.Decode(&iconReader{ctx: ctx, Reader: bytes.NewReader(data)})
	if err != nil {
		return nil, err
	}
	return decoded, ctx.Err()
}

func scaleIcon(ctx context.Context, source image.Image, size int) (_ *image.NRGBA, err error) {
	finish := imageproxy.StartStage(ctx, "transform")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resized := image.NewNRGBA(image.Rect(0, 0, size, size))
	w, h := source.Bounds().Dx(), source.Bounds().Dy()
	if w >= h {
		h, w = max(1, h*size/w), size
	} else {
		w, h = max(1, w*size/h), size
	}
	left, top := (size-w)/2, (size-h)/2
	draw.CatmullRom.Scale(resized, image.Rect(left, top, left+w, top+h), source, source.Bounds(), draw.Src, nil)
	return resized, ctx.Err()
}

func encodeIconPNG(ctx context.Context, img image.Image, maxBytes int64) (_ []byte, err error) {
	finish := imageproxy.StartStage(ctx, "encode")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, imageproxy.ErrResponseTooLarge
	}
	var buf bytes.Buffer
	if err := png.Encode(&iconWriter{ctx: ctx, w: &buf, remaining: maxBytes}, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), ctx.Err()
}

// These wrappers keep encoders/decoders inside the request deadline. Writers
// reject growth before bytes.Buffer can allocate beyond the remaining budget.
type iconWriter struct {
	ctx       context.Context
	w         io.Writer
	remaining int64
}

func (w *iconWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, imageproxy.ErrResponseTooLarge
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

type iconReader struct {
	ctx context.Context
	io.Reader
}

func (r *iconReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
