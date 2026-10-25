package imageproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

func testWebPChunk(kind string, payload []byte) []byte {
	data := make([]byte, 8+len(payload)+len(payload)%2)
	copy(data, kind)
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(payload)))
	copy(data[8:], payload)
	return data
}

func testWebP(chunks ...[]byte) []byte {
	data := []byte("RIFF\x00\x00\x00\x00WEBP")
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	return data
}

func testWebPCanvas(w, h int) []byte {
	payload := make([]byte, 10)
	for i := 0; i < 3; i++ {
		payload[4+i] = byte((w - 1) >> (8 * i))
		payload[7+i] = byte((h - 1) >> (8 * i))
	}
	return testWebPChunk("VP8X", payload)
}

func TestTransformWebPFrameBudget(t *testing.T) {
	// These contain only frame headers, with no compressed pixels. The budget
	// error must therefore come from metadata inspection, before full decode.
	const w, h = 600, 400
	lossy := []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(lossy[6:8], w)
	binary.LittleEndian.PutUint16(lossy[8:10], h)
	lossless := make([]byte, 5)
	lossless[0] = 0x2f
	binary.LittleEndian.PutUint32(lossless[1:5], uint32(w-1)|uint32(h-1)<<14)
	for kind, header := range map[string][]byte{"VP8 ": lossy, "VP8L": lossless} {
		for _, opt := range []Options{{}, {Format: "png"}} {
			data := testWebP(testWebPCanvas(1, 1), testWebPChunk(kind, header))
			out, err := TransformWithLimits(context.Background(), data, opt, 1, 1)
			if !errors.Is(err, ErrImageTooLarge) || out != nil {
				t.Errorf("%q %+v: got %v and %d output bytes, want pixel-limit error", kind, opt, err, len(out))
			}
		}
	}
}

func testValidWebPs(t *testing.T) map[string][]byte {
	t.Helper()
	// cwebp-generated 2x3 solid-red images, in lossy and lossless formats.
	encoded := map[string]string{
		"lossless": "UklGRhwAAABXRUJQVlA4TA8AAAAvAYAAAAcQ/Y/+ByKi/wEA",
		"lossy":    "UklGRkAAAABXRUJQVlA4IDQAAAAwAgCdASoCAAMAAMASJaACdLoB+AH4AARoAAD++iGX/3easNN39a3/9aOfron+tHP/WVgA",
	}
	images := make(map[string][]byte)
	for kind, s := range encoded {
		var err error
		images[kind], err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
	}
	return images
}

func TestTransformWebPCanvasMismatch(t *testing.T) {
	for kind, original := range testValidWebPs(t) {
		data := testWebP(testWebPCanvas(1, 1), original[12:])
		for _, opt := range []Options{{}, {Format: "png"}} {
			out, err := TransformWithLimits(context.Background(), data, opt, 100, 1)
			if !errors.Is(err, ErrInvalidDimensions) || out != nil {
				t.Errorf("%s %+v: got %v and %d output bytes, want canvas mismatch", kind, opt, err, len(out))
			}
		}
	}
}

func TestTransformWebPValidFrames(t *testing.T) {
	for kind, original := range testValidWebPs(t) {
		for _, data := range [][]byte{original, testWebP(testWebPCanvas(2, 3), original[12:])} {
			passthrough, err := TransformWithLimits(context.Background(), data, Options{}, 6, 1)
			if err != nil || !bytes.Equal(passthrough, data) {
				t.Fatalf("%s passthrough failed: %v", kind, err)
			}
			out, err := TransformWithLimits(context.Background(), data, Options{Format: "png"}, 6, 1)
			if err != nil {
				t.Fatalf("%s conversion failed: %v", kind, err)
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(out))
			if err != nil || config.Width != 2 || config.Height != 3 {
				t.Fatalf("%s unexpected output dimensions: %+v, %v", kind, config, err)
			}
		}
	}
}

func TestTransformWebPMalformedChunks(t *testing.T) {
	for _, data := range [][]byte{
		testWebP(testWebPCanvas(1, 1)),
		testWebP(testWebPCanvas(1, 1), testWebPCanvas(1, 1)),
		testWebP([]byte("VP8L\xff\xff\xff\xff")),
		testWebP(testWebPChunk("VP8L", []byte{0x2f})),
	} {
		if _, err := TransformWithLimits(context.Background(), data, Options{}, 100, 1); err == nil {
			t.Fatal("malformed WebP unexpectedly passed validation")
		}
	}
}

type testTIFFEntry struct {
	typ            uint16
	count, value   uint32
	relativeOffset bool
}

func testTIFFEntries() map[uint16]testTIFFEntry {
	return map[uint16]testTIFFEntry{
		256: {typ: 4, count: 1, value: 1},             // ImageWidth
		257: {typ: 4, count: 1, value: 1},             // ImageLength
		258: {typ: 3, count: 1, value: 8},             // BitsPerSample
		259: {typ: 3, count: 1, value: 1},             // Compression
		262: {typ: 3, count: 1, value: 1},             // PhotometricInterpretation
		273: {typ: 4, count: 1, relativeOffset: true}, // StripOffsets
		278: {typ: 4, count: 1, value: 1},             // RowsPerStrip
		279: {typ: 4, count: 1, value: 1},             // StripByteCounts
	}
}

func testTIFF(entries map[uint16]testTIFFEntry, payload []byte) []byte {
	tags := make([]int, 0, len(entries))
	for tag := range entries {
		tags = append(tags, int(tag))
	}
	sort.Ints(tags)
	dataStart := 8 + 2 + len(tags)*12 + 4
	data := make([]byte, dataStart+len(payload))
	copy(data, "II\x2a\x00")
	binary.LittleEndian.PutUint32(data[4:8], 8)
	binary.LittleEndian.PutUint16(data[8:10], uint16(len(tags)))
	for i, tag := range tags {
		entry := entries[uint16(tag)]
		offset := 10 + i*12
		binary.LittleEndian.PutUint16(data[offset:offset+2], uint16(tag))
		binary.LittleEndian.PutUint16(data[offset+2:offset+4], entry.typ)
		binary.LittleEndian.PutUint32(data[offset+4:offset+8], entry.count)
		value := entry.value
		if entry.relativeOffset {
			value += uint32(dataStart)
		}
		binary.LittleEndian.PutUint32(data[offset+8:offset+12], value)
	}
	copy(data[dataStart:], payload)
	return data
}

func TestTransformTIFFPackBitsBudget(t *testing.T) {
	entries := testTIFFEntries()
	entries[259] = testTIFFEntry{typ: 3, count: 1, value: 32773}
	entries[279] = testTIFFEntry{typ: 4, count: 1, value: 2}
	// Two bytes expand to 128 bytes for a 1x1 strip. Older decoders accept
	// arbitrarily much extra strip data; the upgraded codec rejects it early.
	data := testTIFF(entries, []byte{129, 0})
	out, err := TransformWithLimits(context.Background(), data, Options{Format: "png"}, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "PackBits: decompressed data too large") || out != nil {
		t.Fatalf("got %v, %d output bytes; want bounded PackBits failure", err, len(out))
	}
}

func TestTransformTIFFTileBudget(t *testing.T) {
	entries := testTIFFEntries()
	delete(entries, 273)
	delete(entries, 278)
	delete(entries, 279)
	entries[322] = testTIFFEntry{typ: 4, count: 1, value: 1024}
	entries[323] = testTIFFEntry{typ: 4, count: 1, value: 1025}
	entries[324] = testTIFFEntry{typ: 4, count: 1, relativeOffset: true}
	entries[325] = testTIFFEntry{typ: 4, count: 1, value: 0}
	// No tile data is needed: an excessive tile must fail before decoding it.
	out, err := TransformWithLimits(context.Background(), testTIFF(entries, nil), Options{Format: "png"}, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "tile size exceeds image size") || out != nil {
		t.Fatalf("got %v, %d output bytes; want tile-size validation", err, len(out))
	}
}

func TestTransformTIFFMetadataCountBudget(t *testing.T) {
	entries := testTIFFEntries()
	entries[256] = testTIFFEntry{typ: 4, count: 4096, relativeOffset: true}
	entries[273] = testTIFFEntry{typ: 4, count: 1, value: 64, relativeOffset: true}
	// The upgraded codec reads at most 16 values for this fixed-size metadata
	// field. Only that prefix exists, proving it does not fetch all 4096 values.
	payload := make([]byte, 65)
	payload[0] = 1
	out, err := TransformWithLimits(context.Background(), testTIFF(entries, payload), Options{Format: "png"}, 1, 1)
	if err != nil || len(out) == 0 {
		t.Fatalf("bounded metadata parsing failed: %v", err)
	}
}

var registerReaderProbe sync.Once

func TestImageDecoderPreservesRandomAccess(t *testing.T) {
	const magic = "imageproxy-reader-at-test"
	errMissingReaderAt := errors.New("image decoder lost random access")
	registerReaderProbe.Do(func() {
		config := func(r io.Reader) (image.Config, error) {
			if _, ok := r.(io.ReaderAt); !ok {
				return image.Config{}, errMissingReaderAt
			}
			return image.Config{Width: 1, Height: 1}, nil
		}
		image.RegisterFormat("imageproxy-reader-at-test", magic,
			func(r io.Reader) (image.Image, error) {
				if _, err := config(r); err != nil {
					return nil, err
				}
				return image.NewGray(image.Rect(0, 0, 1, 1)), nil
			}, config)
	})
	if _, _, err := image.DecodeConfig(newContextReader(context.Background(), []byte(magic))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.Decode(newContextReader(context.Background(), []byte(magic))); err != nil {
		t.Fatal(err)
	}
}

func TestTransformTIFFOffsetOutsideInput(t *testing.T) {
	data := []byte{'I', 'I', 42, 0, 0xf0, 0xff, 0xff, 0xff}
	// ReadAt fails immediately for a distant IFD. Reader-only TIFF decoding
	// would instead grow an internal buffer while trying to reach this offset.
	if _, err := TransformWithLimits(context.Background(), data, Options{}, 1, 1); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want direct out-of-input read failure", err)
	}
}

func TestContextReaderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newContextReader(ctx, []byte("data"))
	cancel()
	if _, err := r.ReadAt(make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAt: %v", err)
	}
	if _, err := r.ReadByte(); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadByte: %v", err)
	}
	if _, err := r.Peek(1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Peek: %v", err)
	}
}

func TestProxyRejectsUnsafeEncodedImages(t *testing.T) {
	webp := testValidWebPs(t)["lossless"]
	forgedWebP := testWebP(testWebPCanvas(1, 1), webp[12:])
	entries := testTIFFEntries()
	entries[259] = testTIFFEntry{typ: 3, count: 1, value: 32773}
	entries[279] = testTIFFEntry{typ: 4, count: 1, value: 2}
	packBitsTIFF := testTIFF(entries, []byte{129, 0})
	for _, tt := range []struct {
		name, contentType, options string
		data                       []byte
		want                       int
	}{
		{"WebP passthrough", "image/webp", "", forgedWebP, http.StatusRequestEntityTooLarge},
		{"WebP conversion", "image/webp", "png/", forgedWebP, http.StatusRequestEntityTooLarge},
		{"TIFF conversion", "image/tiff", "png/", packBitsTIFF, http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return policyResponse(r, tt.data, tt.contentType), nil
			}), nil)
			p.MaxPixels = 1
			response := servePolicy(p, "/"+tt.options+"http://origin.test/image")
			if response.Code != tt.want || bytes.Equal(response.Body.Bytes(), tt.data) {
				t.Fatalf("got status %d and %d body bytes, want status %d without returning source image",
					response.Code, response.Body.Len(), tt.want)
			}
		})
	}
}
