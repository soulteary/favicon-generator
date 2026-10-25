package imageproxy

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestTransformSmartCropPreservesOffCenterSubject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject image.Rectangle
	}{
		{"left", image.Rect(120, 160, 200, 240)},
		{"right", image.Rect(600, 160, 680, 240)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The textured opaque subject sits outside the square selected by
			// ordinary centered cropping. The rest of the image is transparent.
			src := image.NewRGBA(image.Rect(0, 0, 800, 400))
			for y := tc.subject.Min.Y; y < tc.subject.Max.Y; y++ {
				for x := tc.subject.Min.X; x < tc.subject.Max.X; x++ {
					c := color.RGBA{220, 160, 120, 255}
					if (x/8+y/8)%2 == 0 {
						c = color.RGBA{255, 220, 180, 255}
					}
					src.SetRGBA(x, y, c)
				}
			}
			var input bytes.Buffer
			if err := png.Encode(&input, src); err != nil {
				t.Fatal(err)
			}
			for _, smartCrop := range []bool{false, true} {
				out, err := Transform(input.Bytes(), Options{Width: 250, Height: 250, Format: "png", SmartCrop: smartCrop})
				if err != nil {
					t.Fatalf("SmartCrop=%t: %v", smartCrop, err)
				}
				result, err := png.Decode(bytes.NewReader(out))
				if err != nil {
					t.Fatal(err)
				}
				if got := result.Bounds().Size(); got != image.Pt(250, 250) {
					t.Fatalf("SmartCrop=%t: output size %v, want 250x250", smartCrop, got)
				}
				opaquePixels := 0
				for y := 0; y < 250; y++ {
					for x := 0; x < 250; x++ {
						if _, _, _, alpha := result.At(x, y).RGBA(); alpha >= 0xe000 {
							opaquePixels++
						}
					}
				}
				// An 80x80 subject fitted from a crop no larger than 400x400
				// occupies at least 50x50 output pixels. Allow edge resampling
				// without depending on the analyzer's exact crop coordinates.
				if smartCrop && opaquePixels < 2000 {
					t.Fatalf("content-aware crop lost the %s subject: %d opaque pixels, want at least 2000", tc.name, opaquePixels)
				}
				if !smartCrop && opaquePixels != 0 {
					t.Fatalf("fixture no longer distinguishes centered cropping: %d opaque pixels", opaquePixels)
				}
			}
		})
	}
}
