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
func makeIconManifest(name, shortName string, source url.Values) ([]byte, error) {
	m := iconManifest{Name: name, ShortName: shortName, Display: "standalone"}
	for _, size := range []int{192, 512} {
		filename := "android-chrome-" + strconv.Itoa(size) + "x" + strconv.Itoa(size) + ".png"
		src := filename
		if source != nil {
			src = "/icons/" + filename + "?" + source.Encode()
		}
		m.Icons = append(m.Icons, manifestIcon{src, strconv.Itoa(size) + "x" + strconv.Itoa(size), "image/png", "any"})
	}
	return json.MarshalIndent(m, "", "  ")
}

func generateIcon(ctx context.Context, data []byte, limits imageproxy.SourceLimits, filename, name, shortName string, source url.Values) ([]byte, error) {
	if filename == "site.webmanifest" {
		manifest, err := makeIconManifest(name, shortName, source)
		if err == nil && int64(len(manifest)) > limits.MaxResponseBytes {
			err = imageproxy.ErrResponseTooLarge
		}
		return manifest, err
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
		return nil, imageproxy.ErrImageTooLarge
	}
	sort.Ints(ordered)
	maxSize := ordered[len(ordered)-1]
	// Decode the original only once, including EXIF orientation and safe GIF
	// flattening. All subsequent operations use at most a 512px square.
	normalized, err := imageproxy.TransformWithBudget(ctx, data, imageproxy.Options{
		Width: float64(maxSize), Height: float64(maxSize), Fit: true, Format: "png", ScaleUp: true,
	}, limits)
	if err != nil {
		return nil, err
	}
	decoded, err := png.Decode(&iconReader{ctx: ctx, Reader: bytes.NewReader(normalized)})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base := image.NewNRGBA(image.Rect(0, 0, maxSize, maxSize))
	w, h := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	if w >= h {
		h, w = max(1, h*maxSize/w), maxSize
	} else {
		w, h = max(1, w*maxSize/h), maxSize
	}
	left, top := (maxSize-w)/2, (maxSize-h)/2
	draw.CatmullRom.Scale(base, image.Rect(left, top, left+w, top+h), decoded, decoded.Bounds(), draw.Src, nil)
	encoded := make(map[int][]byte, len(sizes))
	remaining := limits.MaxResponseBytes
	for _, size := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var img image.Image = base
		if size != maxSize {
			resized := image.NewNRGBA(image.Rect(0, 0, size, size))
			draw.CatmullRom.Scale(resized, resized.Bounds(), base, base.Bounds(), draw.Src, nil)
			img = resized
		}
		var buf bytes.Buffer
		if err := png.Encode(&iconWriter{ctx: ctx, w: &buf, remaining: remaining}, img); err != nil {
			return nil, err
		}
		encoded[size] = buf.Bytes()
		remaining -= int64(buf.Len())
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
	manifest, err := makeIconManifest(name, shortName, nil)
	if err != nil {
		return nil, err
	}
	if int64(len(manifest)) > remaining {
		return nil, imageproxy.ErrResponseTooLarge
	}
	files := map[string][]byte{"favicon.ico": icon, "site.webmanifest": manifest}
	for filename, size := range iconPNGSizes {
		files[filename] = encoded[size]
	}
	return encodeIconBundle(ctx, files, limits.MaxResponseBytes)
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
