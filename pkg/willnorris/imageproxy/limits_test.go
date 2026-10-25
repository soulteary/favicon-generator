package imageproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math"
	"testing"
)

func limitTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTransformLimitsCheckHeaderBeforeDecode(t *testing.T) {
	data := limitTestPNG(t, 1, 1)
	// Keep a valid IHDR but leave image data inconsistent with the new size.
	// A pixel-limit error proves the check precedes allocation and full decode.
	binary.BigEndian.PutUint32(data[16:20], 50_000)
	binary.BigEndian.PutUint32(data[20:24], 50_000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	for _, opt := range []Options{{}, {Format: "png"}} {
		_, err := TransformWithLimits(context.Background(), data, opt, 100, 10)
		if !errors.Is(err, ErrImageTooLarge) {
			t.Fatalf("got %v, want pixel-limit error before decoding", err)
		}
	}
}

func TestTransformLimitsRejectExtremeAspectRatio(t *testing.T) {
	data := limitTestPNG(t, MaxImageDimension+1, 1)
	_, err := TransformWithLimits(context.Background(), data, Options{Width: 1}, DefaultMaxPixels, 10)
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("got %v, want side-length limit before resampling", err)
	}
}

func TestTransformLimitsRejectUnsafeDimensions(t *testing.T) {
	data := limitTestPNG(t, 2, 64)
	for _, opt := range []Options{
		{Width: math.NaN()},
		{Height: math.Inf(1)},
		{CropX: math.Inf(-1)},
		{CropHeight: math.MaxFloat64},
	} {
		_, err := TransformWithLimits(context.Background(), data, opt, 1000, 10)
		if !errors.Is(err, ErrInvalidDimensions) {
			t.Errorf("%+v: got %v, want invalid dimensions", opt, err)
		}
	}
	for _, opt := range []Options{
		{Width: 100, Height: 100, ScaleUp: true},
		{Width: 16, Height: 16, ScaleUp: true}, // Thumbnail's intermediate is 16x512.
		{Width: 100, ScaleUp: true},            // Aspect-preserving height is 3200.
	} {
		_, err := TransformWithLimits(context.Background(), data, opt, 1000, 10)
		if !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%+v: got %v, want pixel-limit error", opt, err)
		}
	}
}

func TestTransformLimitsCheckSeparableResizeIntermediate(t *testing.T) {
	// The final 100x1 image is small, but resizing changes width first and
	// creates a temporary 100x100 image from this 1x100 source.
	data := limitTestPNG(t, 1, 100)
	_, err := TransformWithLimits(context.Background(), data,
		Options{Width: 100, Height: 1, ScaleUp: true, Fit: true}, 1000, 10)
	// Fit keeps the narrow aspect ratio, so it remains safe.
	if err != nil {
		t.Fatalf("safe fit failed: %v", err)
	}
	if err := checkResizePixels(1, 100, 100, 1, 1000); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("got %v, want intermediate pixel-limit error", err)
	}
}

func limitTestGIF(t *testing.T, w, h, frames int) []byte {
	t.Helper()
	im := &gif.GIF{Config: image.Config{Width: w, Height: h}}
	for i := 0; i < frames; i++ {
		im.Image = append(im.Image, image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White}))
		im.Delay = append(im.Delay, 0)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTransformLimitsGIFBudgetsBeforeDecode(t *testing.T) {
	data := limitTestGIF(t, 2, 2, 3)
	// Make the LZW code size invalid. Metadata scanning must still find the
	// frame and aggregate-pixel limits before the decoder reaches this data.
	for i := 13; i+10 < len(data); i++ {
		if data[i] == 0x2c {
			codeOffset := i + 10
			if packed := data[i+9]; packed&0x80 != 0 {
				codeOffset += 3 * (1 << ((packed & 7) + 1))
			}
			data[codeOffset] = 0
			break
		}
	}
	if _, err := gif.DecodeAll(bytes.NewReader(data)); err == nil {
		t.Fatal("test GIF unexpectedly decodes; LZW must be invalid")
	}
	for _, tt := range []struct {
		name      string
		pixels    int64
		frames    int
		wantError error
	}{
		{"frames", 100, 2, ErrTooManyFrames},
		{"total canvas pixels", 8, 10, ErrImageTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TransformWithLimits(context.Background(), data, Options{Width: 1}, tt.pixels, tt.frames)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("got %v, want %v", err, tt.wantError)
			}
		})
	}
}

func TestTransformLimitsGIFOutputBudget(t *testing.T) {
	data := limitTestGIF(t, 2, 2, 3)
	_, err := TransformWithLimits(context.Background(), data,
		Options{Width: 10, Height: 10, ScaleUp: true}, 200, 10)
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("got %v, want aggregate output pixel-limit error", err)
	}
}

func TestTransformLimitsPreserveAnimation(t *testing.T) {
	data := limitTestGIF(t, 4, 4, 3)
	out, err := TransformWithLimits(context.Background(), data, Options{Width: 2, Height: 2}, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	im, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Image) != 3 || im.Config.Width != 2 || im.Config.Height != 2 {
		t.Fatalf("unexpected animation: %d frames, %dx%d", len(im.Image), im.Config.Width, im.Config.Height)
	}
}

func TestTransformLimitsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := TransformWithLimits(ctx, limitTestPNG(t, 2, 2), Options{Width: 1}, 100, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestTransformLimitsInputByteBudget(t *testing.T) {
	data := limitTestPNG(t, 2, 2)
	for _, opt := range []Options{{}, {Width: 1}} {
		_, err := transformWithLimits(context.Background(), data, opt, 100, 10, int64(len(data)-1))
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("%+v: got %v, want input byte-limit error", opt, err)
		}
	}
	// A byte-limit error must precede the image decoder even for invalid data.
	_, err := transformWithLimits(context.Background(), []byte("invalid image"), Options{}, 100, 10, 1)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("got %v, want byte-limit error before decoding", err)
	}
}

func TestTransformLimitsEncodingByteBudget(t *testing.T) {
	data := limitTestPNG(t, 10, 10)
	for _, format := range []string{"bmp", "jpeg", "png", "tiff"} {
		t.Run(format, func(t *testing.T) {
			// The tiny PNG input fits, but each encoded 100x100 output is
			// larger. The encoder must abort instead of returning partial data.
			out, err := transformWithLimits(context.Background(), data,
				Options{Width: 100, Height: 100, ScaleUp: true, Format: format}, 10_000, 10, int64(len(data)))
			if !errors.Is(err, ErrResponseTooLarge) || out != nil {
				t.Fatalf("got %v, %d output bytes; want byte-limit error", err, len(out))
			}
		})
	}
}

func TestBoundedWriterStopsBeforeGrowingPastLimit(t *testing.T) {
	var buf bytes.Buffer
	w := boundedWriter{w: &buf, remaining: 4}
	if n, err := w.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("exact-budget write: %d, %v", n, err)
	}
	if n, err := w.Write([]byte("5")); n != 0 || !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("over-budget write: %d, %v", n, err)
	}
	if buf.String() != "1234" {
		t.Fatalf("buffer grew past limit: %q", buf.String())
	}
}
