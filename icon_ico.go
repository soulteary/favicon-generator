package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"

	"github.com/willnorris/imageproxy"
)

type icoFrame struct {
	Size int
	PNG  []byte
}

var errInvalidICOFrame = errors.New("invalid ICO frame")

// encodeICO emits PNG-compressed, 32bpp icon images. The directory uses the
// on-disk ICO layout, rather than the similar Windows resource-group layout.
// https://devblogs.microsoft.com/oldnewthing/20101018-00/?p=12513
// https://devblogs.microsoft.com/oldnewthing/20101022-00/?p=12473
func encodeICO(ctx context.Context, frames []icoFrame, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(frames) == 0 || len(frames) > 4 {
		return nil, fmt.Errorf("%w: expected one to four images", errInvalidICOFrame)
	}
	// Every length and offset is a DWORD; also keep the final allocation
	// representable by int on all supported architectures.
	limit := min(maxBytes, int64(math.MaxUint32), int64(math.MaxInt))
	directoryBytes := int64(6 + 16*len(frames))
	if limit < directoryBytes {
		return nil, imageproxy.ErrResponseTooLarge
	}
	seen := make(map[int]bool, len(frames))
	inputBytes := directoryBytes
	for _, frame := range frames {
		switch frame.Size {
		case 16, 32, 48, 256:
		default:
			return nil, fmt.Errorf("%w: unsupported image size", errInvalidICOFrame)
		}
		if seen[frame.Size] || len(frame.PNG) == 0 {
			return nil, fmt.Errorf("%w: duplicate size or empty PNG", errInvalidICOFrame)
		}
		seen[frame.Size] = true
		if int64(len(frame.PNG)) > limit-inputBytes {
			return nil, imageproxy.ErrResponseTooLarge
		}
		inputBytes += int64(len(frame.PNG))
	}

	images := make([][]byte, len(frames))
	totalBytes := directoryBytes
	for i, frame := range frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		config, err := png.DecodeConfig(icoContextReader{ctx, bytes.NewReader(frame.PNG)})
		if err != nil || config.Width != frame.Size || config.Height != frame.Size {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: PNG dimensions or header", errInvalidICOFrame)
		}
		// DecodeConfig alone cannot verify the pixel stream, checksums or IEND.
		reader := bytes.NewReader(frame.PNG)
		decoded, err := png.Decode(icoContextReader{ctx, reader})
		if err != nil || reader.Len() != 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: incomplete or malformed PNG", errInvalidICOFrame)
		}
		data := frame.PNG
		// Microsoft requires PNG icon images to have 8-bit RGBA channels.
		// Go's PNG encoder otherwise omits alpha for fully opaque images.
		if frame.PNG[24] != 8 || frame.PNG[25] != 6 {
			writer := &icoPNGWriter{ctx: ctx, limit: limit - totalBytes}
			if err := png.Encode(writer, icoRGBAImage{decoded}); err != nil {
				return nil, err
			}
			data = writer.buf.Bytes()
		}
		if int64(len(data)) > limit-totalBytes {
			return nil, imageproxy.ErrResponseTooLarge
		}
		images[i] = data
		totalBytes += int64(len(data))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]byte, int(totalBytes))
	binary.LittleEndian.PutUint16(result[2:4], 1) // 1 = icon, 2 = cursor.
	binary.LittleEndian.PutUint16(result[4:6], uint16(len(frames)))
	offset := int(directoryBytes)
	for i, frame := range frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := result[6+16*i : 6+16*(i+1)]
		if frame.Size != 256 {
			entry[0], entry[1] = byte(frame.Size), byte(frame.Size)
		}
		binary.LittleEndian.PutUint16(entry[4:6], 1)
		binary.LittleEndian.PutUint16(entry[6:8], 32)
		binary.LittleEndian.PutUint32(entry[8:12], uint32(len(images[i])))
		binary.LittleEndian.PutUint32(entry[12:16], uint32(offset))
		copy(result[offset:], images[i])
		offset += len(images[i])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type icoContextReader struct {
	ctx context.Context
	r   *bytes.Reader
}

func (r icoContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Preserve alpha in the PNG header even when every pixel is opaque. The color
// model also converts 16-bit and grayscale source PNGs to the required 32bpp.
type icoRGBAImage struct{ image.Image }

func (icoRGBAImage) ColorModel() color.Model { return color.NRGBAModel }
func (icoRGBAImage) Opaque() bool            { return false }

type icoPNGWriter struct {
	ctx   context.Context
	buf   bytes.Buffer
	limit int64
}

func (w *icoPNGWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-int64(w.buf.Len()) {
		return 0, imageproxy.ErrResponseTooLarge
	}
	return w.buf.Write(p)
}
