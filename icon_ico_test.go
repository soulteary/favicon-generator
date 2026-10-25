package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

// These test-only structs independently describe the published on-disk fields.
type decodedICOHeader struct{ Reserved, Type, Count uint16 }
type decodedICOEntry struct {
	Width, Height, Colors, Reserved byte
	Planes, BitCount                uint16
	Length, Offset                  uint32
}

func icoTestPNG(t *testing.T, width, height int, opaque bool) ([]byte, *image.NRGBA) {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			alpha := byte(255)
			if !opaque {
				alpha = []byte{0, 128, 255}[(x+y)%3]
			}
			im.SetNRGBA(x, y, color.NRGBA{R: byte(x + 20), G: byte(y + 40), B: 200, A: alpha})
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		t.Fatal(err)
	}
	return data.Bytes(), im
}

func readTestICO(t *testing.T, data []byte) ([]decodedICOEntry, []image.Image) {
	t.Helper()
	r := bytes.NewReader(data)
	var header decodedICOHeader
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		t.Fatal(err)
	}
	if header.Reserved != 0 || header.Type != 1 || header.Count == 0 {
		t.Fatalf("invalid icon header: %+v", header)
	}
	entries := make([]decodedICOEntry, header.Count)
	if err := binary.Read(r, binary.LittleEndian, entries); err != nil {
		t.Fatal(err)
	}
	images := make([]image.Image, len(entries))
	end := uint64(6 + binary.Size(entries))
	for i, entry := range entries {
		if entry.Colors != 0 || entry.Reserved != 0 || entry.Planes != 1 || entry.BitCount != 32 || entry.Length == 0 {
			t.Fatalf("invalid entry %d: %+v", i, entry)
		}
		if uint64(entry.Offset) != end || uint64(entry.Length) > uint64(len(data))-end {
			t.Fatalf("entry %d has an overlapping, missing or out-of-range payload", i)
		}
		end += uint64(entry.Length)
		payload := data[entry.Offset:end]
		if !bytes.HasPrefix(payload, []byte("\x89PNG\r\n\x1a\n")) || len(payload) < 26 || payload[24] != 8 || payload[25] != 6 {
			t.Fatalf("entry %d does not contain a complete 32bpp RGBA PNG", i)
		}
		var err error
		images[i], err = png.Decode(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("entry %d PNG: %v", i, err)
		}
		width, height := int(entry.Width), int(entry.Height)
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		if images[i].Bounds().Dx() != width || images[i].Bounds().Dy() != height {
			t.Fatalf("entry %d dimensions disagree with PNG", i)
		}
	}
	if end != uint64(len(data)) {
		t.Fatal("unreferenced bytes after icon images")
	}
	return entries, images
}

func TestEncodeICOMultipleSizesAndTransparency(t *testing.T) {
	var frames []icoFrame
	var originals []*image.NRGBA
	for _, size := range []int{16, 32, 48, 256} {
		data, original := icoTestPNG(t, size, size, false)
		frames = append(frames, icoFrame{Size: size, PNG: data})
		originals = append(originals, original)
	}
	encoded, err := encodeICO(context.Background(), frames, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	entries, images := readTestICO(t, encoded)
	if len(images) != 4 || entries[3].Width != 0 || entries[3].Height != 0 {
		t.Fatal("missing image or incorrect 256px dimension marker")
	}
	for i, actual := range images {
		for y := range frames[i].Size {
			for x := range frames[i].Size {
				got := color.NRGBAModel.Convert(actual.At(x, y)).(color.NRGBA)
				if want := originals[i].NRGBAAt(x, y); got != want {
					t.Fatalf("frame %d pixel (%d,%d)=%v, want %v", i, x, y, got, want)
				}
			}
		}
	}
}

func TestEncodeICONormalizesOpaquePNGTo32BPP(t *testing.T) {
	data, source := icoTestPNG(t, 16, 16, true)
	if data[25] != 2 {
		t.Fatal("test fixture should be an opaque, RGB PNG")
	}
	encoded, err := encodeICO(context.Background(), []icoFrame{{16, data}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, images := readTestICO(t, encoded)
	if got := color.NRGBAModel.Convert(images[0].At(5, 5)); got != source.NRGBAAt(5, 5) {
		t.Fatalf("opaque pixel changed: %v", got)
	}
}

func TestEncodeICORejectsInvalidFrames(t *testing.T) {
	valid, _ := icoTestPNG(t, 16, 16, false)
	rectangular, _ := icoTestPNG(t, 16, 32, false)
	corrupt := append([]byte(nil), valid...)
	corrupt[len(corrupt)-1] ^= 1 // IEND checksum: DecodeConfig alone would accept it.
	huge := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(huge[16:20], 1<<20)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	cases := []struct {
		name   string
		frames []icoFrame
	}{
		{"empty", nil},
		{"unsupported size", []icoFrame{{24, valid}}},
		{"duplicate size", []icoFrame{{16, valid}, {16, valid}}},
		{"empty PNG", []icoFrame{{Size: 16}}},
		{"not PNG", []icoFrame{{16, []byte("not a PNG")}}},
		{"size mismatch", []icoFrame{{32, valid}}},
		{"rectangular", []icoFrame{{16, rectangular}}},
		{"truncated", []icoFrame{{16, valid[:len(valid)-1]}}},
		{"bad checksum", []icoFrame{{16, corrupt}}},
		{"oversized header", []icoFrame{{16, huge}}},
		{"trailing bytes", []icoFrame{{16, append(append([]byte(nil), valid...), 0)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := encodeICO(context.Background(), tc.frames, 1<<20)
			if !errors.Is(err, errInvalidICOFrame) || result != nil {
				t.Fatalf("result bytes=%d, err=%v", len(result), err)
			}
		})
	}
}

func TestEncodeICOOutputBudget(t *testing.T) {
	data, _ := icoTestPNG(t, 16, 16, false)
	frames := []icoFrame{{16, data}}
	exact := int64(22 + len(data))
	for _, limit := range []int64{-1, 0, 5, 21, exact - 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			result, err := encodeICO(context.Background(), frames, limit)
			if !errors.Is(err, imageproxy.ErrResponseTooLarge) || result != nil {
				t.Fatalf("result bytes=%d, err=%v", len(result), err)
			}
		})
	}
	for _, limit := range []int64{exact, math.MaxInt64} {
		result, err := encodeICO(context.Background(), frames, limit)
		if err != nil || int64(len(result)) != exact {
			t.Fatalf("limit=%d, bytes=%d, err=%v", limit, len(result), err)
		}
	}
	opaque, _ := icoTestPNG(t, 16, 16, true)
	_, err := encodeICO(context.Background(), []icoFrame{{16, opaque}}, int64(22+len(opaque)))
	if !errors.Is(err, imageproxy.ErrResponseTooLarge) {
		t.Fatalf("RGBA normalization escaped byte limit: %v", err)
	}
}

func TestEncodeICOCancellation(t *testing.T) {
	data, _ := icoTestPNG(t, 16, 16, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := encodeICO(ctx, []icoFrame{{16, data}}, 1<<20); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("result bytes=%d, err=%v", len(result), err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if result, err := encodeICO(ctx, []icoFrame{{16, data}}, 1<<20); !errors.Is(err, context.DeadlineExceeded) || result != nil {
		t.Fatalf("result bytes=%d, err=%v", len(result), err)
	}
}
