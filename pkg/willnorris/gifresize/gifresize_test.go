package gifresize

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

func animatedGIF(t *testing.T) []byte {
	t.Helper()
	im := &gif.GIF{Config: image.Config{Width: 2, Height: 2}}
	for i := 0; i < 3; i++ {
		im.Image = append(im.Image, image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White}))
		im.Delay = append(im.Delay, 0)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcessWithContextStopsBetweenFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := 0
	var out bytes.Buffer
	err := ProcessWithContext(ctx, &out, bytes.NewReader(animatedGIF(t)), func(img image.Image) (image.Image, error) {
		frames++
		cancel()
		return img, nil
	})
	if !errors.Is(err, context.Canceled) || frames != 1 || out.Len() != 0 {
		t.Fatalf("got %v, %d frames, %d output bytes", err, frames, out.Len())
	}
}

func TestProcessWithContextStopsOnTransformError(t *testing.T) {
	wantErr := errors.New("transform failed")
	frames := 0
	var out bytes.Buffer
	err := ProcessWithContext(context.Background(), &out, bytes.NewReader(animatedGIF(t)), func(img image.Image) (image.Image, error) {
		frames++
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) || frames != 1 || out.Len() != 0 {
		t.Fatalf("got %v, %d frames, %d output bytes", err, frames, out.Len())
	}
}
