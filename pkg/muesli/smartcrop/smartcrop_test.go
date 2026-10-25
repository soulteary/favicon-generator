/*
 * Copyright (c) 2014-2017 Christian Muehlhaeuser
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 *
 *	Authors:
 *		Christian Muehlhaeuser <muesli@gmail.com>
 *		Michael Wendland <michael@michiwend.com>
 *		Bjørn Erik Pedersen <bjorn.erik.pedersen@gmail.com>
 */

package smartcrop

import (
	"image"
	"image/color"
	"testing"

	"github.com/muesli/smartcrop/nfnt"
)

// A textured subject against a flat background gives the analyzer a reproducible
// region of interest without depending on external example images.
func cropFixture(subject image.Rectangle) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for y := subject.Min.Y; y < subject.Max.Y; y++ {
		for x := subject.Min.X; x < subject.Max.X; x++ {
			c := color.RGBA{220, 160, 120, 255}
			if (x/8+y/8)%2 == 0 {
				c = color.RGBA{255, 220, 180, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func smartCrop(img image.Image, width, height int) (image.Rectangle, error) {
	analyzer := NewAnalyzer(nfnt.NewDefaultResizer())
	return analyzer.FindBestCrop(img, width, height)
}

func TestCrop(t *testing.T) {
	for _, tc := range []struct {
		subject image.Rectangle
		want    image.Rectangle
	}{
		{image.Rect(120, 160, 200, 240), image.Rect(0, 40, 360, 400)},
		{image.Rect(600, 160, 680, 240), image.Rect(368, 40, 728, 400)},
	} {
		subject := tc.subject
		img := cropFixture(subject)
		crop, err := smartCrop(img, 250, 250)
		if err != nil {
			t.Fatal(err)
		}
		if crop != tc.want {
			t.Fatalf("subject %v: crop = %v, want %v", subject, crop, tc.want)
		}
		if crop.Empty() || !crop.In(img.Bounds()) || crop.Dx() != crop.Dy() {
			t.Fatalf("invalid square crop: %v", crop)
		}
		if !subject.In(crop) {
			t.Fatalf("crop %v lost the subject %v", crop, subject)
		}
		if got := img.SubImage(crop).Bounds(); got != crop {
			t.Fatalf("subimage bounds = %v, want %v", got, crop)
		}
	}
}

func BenchmarkCrop(b *testing.B) {
	img := cropFixture(image.Rect(600, 160, 680, 240))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := smartCrop(img, 250, 250); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEdge(b *testing.B) {
	img := cropFixture(image.Rect(600, 160, 680, 240))
	out := image.NewRGBA(img.Bounds())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		edgeDetect(img, out)
	}
}
