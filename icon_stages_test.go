package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

type iconStageEvent struct {
	stage, result string
	duration      time.Duration
}

type iconStageRecorder struct {
	mu      sync.Mutex
	events  []iconStageEvent
	onStage func(iconStageEvent)
}

func (r *iconStageRecorder) ObserveStage(stage, result string, duration time.Duration) {
	event := iconStageEvent{stage, result, duration}
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	if r.onStage != nil {
		r.onStage(event)
	}
}

func (*iconStageRecorder) ObserveCache(string, string) {}

func (r *iconStageRecorder) snapshot() []iconStageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]iconStageEvent(nil), r.events...)
}

func iconStageCount(events []iconStageEvent, stage, result string) int {
	var count int
	for _, event := range events {
		if event.stage == stage && (result == "" || event.result == result) {
			count++
		}
	}
	return count
}

func iconStageFixture(t *testing.T, size int, opaque bool) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			alpha := byte(128)
			if opaque {
				alpha = 255
			}
			im.SetNRGBA(x, y, color.NRGBA{R: byte(17 + x), G: byte(43 + y), B: 123, A: alpha})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, im); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestIconStageGenerationSuccess(t *testing.T) {
	data := iconStageFixture(t, 8, false)
	limits := imageproxy.SourceLimits{MaxResponseBytes: 4 << 20, MaxPixels: 16_000_000, MaxFrames: 100}
	for _, filename := range []string{"favicon-16x16.png", "favicon.ico", "icons.zip"} {
		t.Run(filename, func(t *testing.T) {
			recorder := new(iconStageRecorder)
			ctx := imageproxy.WithObserver(context.Background(), recorder)
			output, err := generateIcon(ctx, data, limits, filename, "App", "App", nil)
			if err != nil || len(output) == 0 {
				t.Fatalf("generation: %d bytes, %v", len(output), err)
			}
			events := recorder.snapshot()
			for _, stage := range []string{"decode", "transform", "encode"} {
				if iconStageCount(events, stage, "success") == 0 {
					t.Fatalf("missing successful %s operation: %+v", stage, events)
				}
			}
			wantPack := 0
			if filename == "icons.zip" {
				wantPack = 1
			}
			if iconStageCount(events, "pack", "") != wantPack {
				t.Fatalf("pack operations = %d, want %d", iconStageCount(events, "pack", ""), wantPack)
			}
			for _, event := range events {
				if event.result != "success" || event.duration < 0 || event.stage == "fetch" || event.stage == "write" {
					t.Fatalf("generation recorded an unrelated or failed stage: %+v", event)
				}
			}
			// Observation must leave deterministic generated artifacts unchanged.
			plain, err := generateIcon(context.Background(), data, limits, filename, "App", "App", nil)
			if err != nil || !bytes.Equal(output, plain) {
				t.Fatalf("observer changed the artifact: %v", err)
			}
		})
	}
}

func TestIconStageManifestEncoding(t *testing.T) {
	for _, limit := range []int64{4096, 1} {
		recorder := new(iconStageRecorder)
		ctx := imageproxy.WithObserver(context.Background(), recorder)
		_, err := generateIcon(ctx, nil, imageproxy.SourceLimits{MaxResponseBytes: limit}, "site.webmanifest", "App", "App", nil)
		wantResult := "success"
		if limit == 1 {
			wantResult = "limit"
			if !errors.Is(err, imageproxy.ErrResponseTooLarge) {
				t.Fatalf("manifest limit: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		events := recorder.snapshot()
		if len(events) != 1 || events[0].stage != "encode" || events[0].result != wantResult {
			t.Fatalf("manifest stages = %+v, want encode/%s", events, wantResult)
		}
	}
}

func TestIconStageDecodeFailuresDoNotBecomeEncodeFailures(t *testing.T) {
	data := iconStageFixture(t, 16, false)
	data[len(data)-1] ^= 1 // Corrupt IEND after a valid PNG configuration.
	recorder := new(iconStageRecorder)
	ctx := imageproxy.WithObserver(context.Background(), recorder)
	_, err := encodeICO(ctx, []icoFrame{{Size: 16, PNG: data}}, 4096)
	if !errors.Is(err, errInvalidICOFrame) {
		t.Fatalf("malformed PNG: %v", err)
	}
	events := recorder.snapshot()
	if iconStageCount(events, "decode", "error") != 1 || iconStageCount(events, "encode", "error") != 0 || iconStageCount(events, "pack", "") != 0 {
		t.Fatalf("PNG validation was attributed to a container encoding/packing stage: %+v", events)
	}
	if iconStageCount(events, "encode", "success") != 1 {
		t.Fatalf("only ICO layout preflight should precede a corrupt PNG: %+v", events)
	}

	recorder = new(iconStageRecorder)
	ctx = imageproxy.WithObserver(context.Background(), recorder)
	if _, err := decodeIconPNG(ctx, []byte("not a PNG")); err == nil {
		t.Fatal("normalized PNG decode unexpectedly succeeded")
	}
	events = recorder.snapshot()
	if len(events) != 1 || events[0].stage != "decode" || events[0].result != "error" {
		t.Fatalf("normalized PNG decode stages: %+v", events)
	}
}

func TestIconStageICOTranscodingAndLimit(t *testing.T) {
	data := iconStageFixture(t, 16, true)
	if data[25] != 2 {
		t.Fatal("fixture must be RGB to exercise RGBA transcoding")
	}
	for _, limit := range []int64{4096, int64(22 + len(data))} {
		recorder := new(iconStageRecorder)
		ctx := imageproxy.WithObserver(context.Background(), recorder)
		output, err := encodeICO(ctx, []icoFrame{{Size: 16, PNG: data}}, limit)
		events := recorder.snapshot()
		if iconStageCount(events, "decode", "success") != 1 || iconStageCount(events, "pack", "") != 0 {
			t.Fatalf("ICO decode/pack stages: %+v", events)
		}
		if limit == 4096 {
			if err != nil {
				t.Fatal(err)
			}
			offset := binary.LittleEndian.Uint32(output[18:22])
			if output[offset+24] != 8 || output[offset+25] != 6 || iconStageCount(events, "encode", "success") < 2 {
				t.Fatalf("ICO RGBA frame encoding was not observed: %+v", events)
			}
		} else if !errors.Is(err, imageproxy.ErrResponseTooLarge) || iconStageCount(events, "encode", "limit") != 1 {
			t.Fatalf("ICO transcode limit = %v, stages %+v", err, events)
		}
	}
}

func TestIconStageAggregateLimits(t *testing.T) {
	recorder := new(iconStageRecorder)
	ctx := imageproxy.WithObserver(context.Background(), recorder)
	_, err := generateIcon(ctx, nil, imageproxy.SourceLimits{MaxPixels: 1}, "icons.zip", "App", "App", nil)
	if !errors.Is(err, imageproxy.ErrImageTooLarge) {
		t.Fatal(err)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].stage != "transform" || events[0].result != "limit" {
		t.Fatalf("aggregate pixel budget stages: %+v", events)
	}
	for _, limit := range []int64{2, 3} {
		recorder = new(iconStageRecorder)
		ctx = imageproxy.WithObserver(context.Background(), recorder)
		_, err := encodeIconBundle(ctx, map[string][]byte{"favicon.ico": []byte("ico")}, limit)
		if !errors.Is(err, imageproxy.ErrResponseTooLarge) {
			t.Fatal(err)
		}
		events = recorder.snapshot()
		if len(events) != 1 || events[0].stage != "pack" || events[0].result != "limit" {
			t.Fatalf("source or ZIP byte budget stages: %+v", events)
		}
	}
}

func TestIconStagePackErrorAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, result string
	}{
		{"invalid path", "error"}, {"expired context", "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := context.Background()
			files := map[string][]byte{"../favicon.ico": []byte("ico")}
			wantError := errInvalidIconBundleName
			if tc.result == "timeout" {
				var cancel context.CancelFunc
				base, cancel = context.WithDeadline(base, time.Now().Add(-time.Second))
				defer cancel()
				files = nil
				wantError = context.DeadlineExceeded
			}
			recorder := new(iconStageRecorder)
			_, err := encodeIconBundle(imageproxy.WithObserver(base, recorder), files, 4096)
			if !errors.Is(err, wantError) {
				t.Fatalf("pack error = %v, want %v", err, wantError)
			}
			if events := recorder.snapshot(); len(events) != 1 || events[0].stage != "pack" || events[0].result != tc.result {
				t.Fatalf("pack error stages: %+v", events)
			}
		})
	}
}

func TestIconStageCancellationAtOperationBoundaries(t *testing.T) {
	data := iconStageFixture(t, 16, false)
	for _, tc := range []struct {
		name, stage string
		call        func(context.Context) error
	}{
		{"normalized decode", "decode", func(ctx context.Context) error { _, err := decodeIconPNG(ctx, data); return err }},
		{"scale", "transform", func(ctx context.Context) error {
			_, err := scaleIcon(ctx, image.NewNRGBA(image.Rect(0, 0, 1, 1)), 16)
			return err
		}},
		{"PNG encode", "encode", func(ctx context.Context) error {
			_, err := encodeIconPNG(ctx, image.NewNRGBA(image.Rect(0, 0, 1, 1)), 4096)
			return err
		}},
		{"ICO", "encode", func(ctx context.Context) error { _, err := encodeICO(ctx, []icoFrame{{16, data}}, 4096); return err }},
		{"manifest", "encode", func(ctx context.Context) error { _, err := makeIconManifest(ctx, "App", "App", nil, 4096); return err }},
		{"ZIP", "pack", func(ctx context.Context) error {
			_, err := encodeIconBundle(ctx, map[string][]byte{"favicon.ico": data}, 4096)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := new(iconStageRecorder)
			base, cancel := context.WithCancel(context.Background())
			cancel()
			ctx := imageproxy.WithObserver(base, recorder)
			if err := tc.call(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled operation: %v", err)
			}
			events := recorder.snapshot()
			if len(events) != 1 || events[0].stage != tc.stage || events[0].result != "canceled" {
				t.Fatalf("canceled operation stages: %+v", events)
			}
		})
	}

	// Cancellation after successful PNG validation must stop the next encoding
	// operation, rather than report a successful ICO container or a pack stage.
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &iconStageRecorder{onStage: func(event iconStageEvent) {
		if event.stage == "decode" && event.result == "success" {
			cancel()
		}
	}}
	ctx := imageproxy.WithObserver(base, recorder)
	if _, err := encodeICO(ctx, []icoFrame{{16, data}}, 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("ICO continued after canceled PNG validation: %v", err)
	}
	if events := recorder.snapshot(); iconStageCount(events, "encode", "canceled") != 1 || iconStageCount(events, "pack", "") != 0 {
		t.Fatalf("late cancellation stages: %+v", events)
	}
}
