package imageproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
)

// checkWebP checks the actual VP8/VP8L frame before DecodeConfig can return an
// extended WebP's VP8X canvas dimensions. Checking only that canvas permits a
// forged small canvas to hide a larger frame, including on the passthrough path.
// It reads chunk and frame headers only; compressed pixels are never decoded.
func checkWebP(ctx context.Context, data []byte, maxPixels int64) error {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil
	}
	end := int64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if end < 12 {
		return fmt.Errorf("%w: invalid WebP container size", ErrInvalidDimensions)
	}
	if end > int64(len(data)) {
		return io.ErrUnexpectedEOF
	}
	var canvasW, canvasH int
	seenCanvas := false
	for offset := int64(12); offset < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if end-offset < 8 {
			return io.ErrUnexpectedEOF
		}
		kind := string(data[offset : offset+4])
		size := int64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size > end-offset {
			return io.ErrUnexpectedEOF
		}
		chunk := data[offset : offset+size]
		offset += size + size%2
		if offset > end {
			return io.ErrUnexpectedEOF
		}
		switch kind {
		case "VP8X":
			if seenCanvas || len(chunk) != 10 {
				return fmt.Errorf("%w: invalid WebP canvas header", ErrInvalidDimensions)
			}
			seenCanvas = true
			canvasW = 1 + int(chunk[4]) + int(chunk[5])<<8 + int(chunk[6])<<16
			canvasH = 1 + int(chunk[7]) + int(chunk[8])<<8 + int(chunk[9])<<16
			if err := checkPixels(canvasW, canvasH, maxPixels); err != nil {
				return err
			}
		case "VP8 ", "VP8L":
			var w, h int
			if kind == "VP8 " {
				decoder := vp8.NewDecoder()
				decoder.Init(bytes.NewReader(chunk), len(chunk))
				header, err := decoder.DecodeFrameHeader()
				if err != nil {
					return err
				}
				w, h = header.Width, header.Height
			} else {
				config, err := vp8l.DecodeConfig(bytes.NewReader(chunk))
				if err != nil {
					return err
				}
				w, h = config.Width, config.Height
			}
			if err := checkPixels(w, h, maxPixels); err != nil {
				return err
			}
			if seenCanvas && (w != canvasW || h != canvasH) {
				return fmt.Errorf("%w: WebP canvas and frame dimensions differ", ErrInvalidDimensions)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: WebP has no supported image frame", ErrInvalidDimensions)
}
