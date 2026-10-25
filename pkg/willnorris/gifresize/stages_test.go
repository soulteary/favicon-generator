package gifresize

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"reflect"
	"testing"
)

type failingStageWriter struct{ err error }

func (w failingStageWriter) Write([]byte) (int, error) { return 0, w.err }

func TestProcessWithStagesAttributesActualFailures(t *testing.T) {
	want := errors.New("operation failed")
	for _, tt := range []struct {
		name      string
		data      []byte
		writer    io.Writer
		transform func(image.Image) (image.Image, error)
		stages    []string
		wantErr   error
	}{
		{"success", animatedGIF(t), io.Discard, func(img image.Image) (image.Image, error) { return img, nil }, []string{"decode", "transform", "encode"}, nil},
		{"decode", []byte("not GIF"), io.Discard, func(img image.Image) (image.Image, error) { return img, nil }, []string{"decode"}, nil},
		{"transform", animatedGIF(t), io.Discard, func(image.Image) (image.Image, error) { return nil, want }, []string{"decode", "transform"}, want},
		{"encode", animatedGIF(t), failingStageWriter{want}, func(img image.Image) (image.Image, error) { return img, nil }, []string{"decode", "transform", "encode"}, want},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stages []string
			var results []error
			err := ProcessWithStages(context.Background(), tt.writer, bytes.NewReader(tt.data), tt.transform, func(stage string) func(error) {
				stages = append(stages, stage)
				return func(err error) { results = append(results, err) }
			})
			if !reflect.DeepEqual(stages, tt.stages) || len(results) != len(stages) {
				t.Fatalf("stages=%v results=%v", stages, results)
			}
			if tt.name == "success" {
				if err != nil {
					t.Fatal(err)
				}
				for _, result := range results {
					if result != nil {
						t.Fatal(result)
					}
				}
			} else if err == nil || results[len(results)-1] == nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("failure not attributed to its last actual stage: %v %v", err, results)
			}
		})
	}
}

func TestProcessWithStagesCancellationDoesNotStartEncoding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stages []string
	err := ProcessWithStages(ctx, io.Discard, bytes.NewReader(animatedGIF(t)), func(img image.Image) (image.Image, error) {
		cancel()
		return img, nil
	}, func(stage string) func(error) {
		stages = append(stages, stage)
		return func(error) {}
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(stages, []string{"decode", "transform"}) {
		t.Fatalf("canceled frame loop was encoded: %v %v", err, stages)
	}
}
