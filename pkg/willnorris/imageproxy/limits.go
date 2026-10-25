package imageproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
)

const (
	// DefaultMaxPixels bounds individual images, resize intermediates, and the
	// combined full-canvas pixels of an animation.
	DefaultMaxPixels int64 = 16_000_000
	DefaultMaxFrames       = 100
	// MaxImageDimension also bounds resampler weight tables, which grow with
	// individual side lengths even when an image has few total pixels.
	MaxImageDimension = 32_768
)

var (
	ErrImageTooLarge     = errors.New("image exceeds pixel limit")
	ErrTooManyFrames     = errors.New("image exceeds animation frame limit")
	ErrInvalidDimensions = errors.New("invalid image dimensions")
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

// boundedWriter rejects a write before the encoder's output buffer can grow
// past its budget. Bytes from a failed encoding are never returned to callers.
type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrResponseTooLarge
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

func validateOptions(opt Options) error {
	// Leave headroom for crop coordinate additions and pixel strides. Check
	// before converting floats to int, for which overflowing values are unsafe.
	max := float64(int(^uint(0)>>1) / 4)
	for _, n := range []float64{opt.Width, opt.Height, opt.CropX, opt.CropY, opt.CropWidth, opt.CropHeight} {
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) >= max {
			return ErrInvalidDimensions
		}
	}
	return nil
}

func checkPixels(w, h int, maxPixels int64) error {
	if w <= 0 || h <= 0 {
		return ErrInvalidDimensions
	}
	if w > MaxImageDimension || h > MaxImageDimension {
		return fmt.Errorf("%w: side exceeds %d", ErrImageTooLarge, MaxImageDimension)
	}
	// Besides the configured limit, prevent overflow in decoder and imaging
	// buffer lengths on both 32-bit and 64-bit systems.
	allocationLimit := int64(int(^uint(0)>>1) / 16)
	if maxPixels > allocationLimit {
		maxPixels = allocationLimit
	}
	if int64(w) > maxPixels/int64(h) {
		return fmt.Errorf("%w: %dx%d (limit %d)", ErrImageTooLarge, w, h, maxPixels)
	}
	return nil
}

func checkTransformPixels(crop image.Rectangle, w, h int, resize, fit bool, maxPixels int64) error {
	srcW, srcH := crop.Dx(), crop.Dy()
	if err := checkPixels(srcW, srcH, maxPixels); err != nil {
		return err
	}
	if !resize {
		return nil
	}
	if w == 0 && h == 0 {
		return nil
	}
	if w == 0 || h == 0 {
		return checkResizePixels(srcW, srcH, w, h, maxPixels)
	}
	if err := checkPixels(w, h, maxPixels); err != nil {
		return err
	}
	srcAspect, dstAspect := float64(srcW)/float64(srcH), float64(w)/float64(h)
	if fit {
		if srcW <= w && srcH <= h {
			return nil
		}
		if srcAspect > dstAspect {
			h = int(float64(w) / srcAspect)
		} else {
			w = int(float64(h) * srcAspect)
		}
		return checkResizePixels(srcW, srcH, w, h, maxPixels)
	}
	// Thumbnail crops before resizing only when both source sides are >=100.
	// With narrow sources, it first expands one side to cover the target. The
	// temporary image can be much larger than either input or output.
	if srcW >= 100 && srcH >= 100 {
		if srcAspect < dstAspect {
			srcH = int(math.Max(1, float64(srcW)*float64(h)/float64(w)) + 0.5)
		} else {
			srcW = int(math.Max(1, float64(srcH)*float64(w)/float64(h)) + 0.5)
		}
	} else if srcAspect < dstAspect {
		h = 0
	} else {
		w = 0
	}
	return checkResizePixels(srcW, srcH, w, h, maxPixels)
}

func checkResizePixels(srcW, srcH, w, h int, maxPixels int64) error {
	// Match imaging.Resize's aspect-preserving rounding without converting an
	// overflowing floating-point value to int.
	if w == 0 {
		n := math.Max(1, math.Floor(float64(h)*float64(srcW)/float64(srcH)+0.5))
		if n > float64(maxPixels) || n >= float64(int(^uint(0)>>1)/4) {
			return ErrImageTooLarge
		}
		w = int(n)
	}
	if h == 0 {
		n := math.Max(1, math.Floor(float64(w)*float64(srcH)/float64(srcW)+0.5))
		if n > float64(maxPixels) || n >= float64(int(^uint(0)>>1)/4) {
			return ErrImageTooLarge
		}
		h = int(n)
	}
	if err := checkPixels(w, h, maxPixels); err != nil {
		return err
	}
	// The separable resizer changes width first, retaining the source height.
	if srcW != w && srcH != h {
		return checkPixels(w, srcH, maxPixels)
	}
	return nil
}

// checkGIF walks block headers and skips compressed sub-blocks without decoding
// pixels. gif.DecodeAll retains every frame, so checking after it returns would
// allow small compressed inputs to exhaust memory before a limit is enforced.
func checkGIF(ctx context.Context, data []byte, maxPixels int64, maxFrames int) (int, error) {
	if len(data) < 13 {
		return 0, io.ErrUnexpectedEOF
	}
	w := int(binary.LittleEndian.Uint16(data[6:8]))
	h := int(binary.LittleEndian.Uint16(data[8:10]))
	if err := checkPixels(w, h, maxPixels); err != nil {
		return 0, err
	}
	offset := 13
	if data[10]&0x80 != 0 {
		offset += 3 * (1 << ((data[10] & 7) + 1))
	}
	frames := 0
	for offset < len(data) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		block := data[offset]
		offset++
		switch block {
		case 0x3b: // trailer
			if frames == 0 {
				return 0, errors.New("gif: missing image data")
			}
			return frames, nil
		case 0x21: // extension, followed by label and sub-blocks
			offset++
		case 0x2c: // image descriptor
			if len(data)-offset < 9 {
				return 0, io.ErrUnexpectedEOF
			}
			frames++
			if frames > maxFrames {
				return 0, ErrTooManyFrames
			}
			if int64(w)*int64(h) > maxPixels/int64(frames) {
				return 0, fmt.Errorf("%w: animation canvas pixels", ErrImageTooLarge)
			}
			x := int(binary.LittleEndian.Uint16(data[offset : offset+2]))
			y := int(binary.LittleEndian.Uint16(data[offset+2 : offset+4]))
			fw := int(binary.LittleEndian.Uint16(data[offset+4 : offset+6]))
			fh := int(binary.LittleEndian.Uint16(data[offset+6 : offset+8]))
			if err := checkPixels(fw, fh, maxPixels); err != nil {
				return 0, err
			}
			if x+fw > w || y+fh > h {
				return 0, ErrInvalidDimensions
			}
			packed := data[offset+8]
			offset += 9
			if packed&0x80 != 0 {
				offset += 3 * (1 << ((packed & 7) + 1))
			}
			offset++ // LZW minimum code size
		default:
			return 0, fmt.Errorf("gif: unknown block type 0x%x", block)
		}
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if offset >= len(data) {
				return 0, io.ErrUnexpectedEOF
			}
			n := int(data[offset])
			offset++
			if n == 0 {
				break
			}
			if n > len(data)-offset {
				return 0, io.ErrUnexpectedEOF
			}
			offset += n
		}
	}
	return 0, io.ErrUnexpectedEOF
}
