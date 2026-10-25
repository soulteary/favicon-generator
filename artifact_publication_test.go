package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

func TestArtifactPublicationExpiryBypassesCache(t *testing.T) {
	source, data, limits, metadata := artifactTestSource(t, "public, max-age=3600")
	body, err := generateIcon(context.Background(), data, limits, "favicon-32x32.png", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"nanosecond-ttl", "source-horizon", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			base := time.Now().Add(time.Minute)
			ttl := time.Nanosecond
			clocks := []time.Time{base, base, base.Add(2 * time.Nanosecond)}
			if scenario == "source-horizon" {
				ttl = time.Minute
				until := metadata.FreshUntil()
				clocks = []time.Time{until.Add(-10 * time.Nanosecond), until.Add(-5 * time.Nanosecond), until}
			}
			store := &artifactTestStore{}
			var result string
			service := newArtifactService(store, "memory", ttl, 1<<20, func(_, observed string) { result = observed })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clockReads := 0
			service.now = func() time.Time {
				if clockReads >= len(clocks) {
					t.Fatal("unexpected publication clock read")
				}
				now := clocks[clockReads]
				clockReads++
				if scenario == "canceled" && clockReads == 3 {
					cancel()
				}
				return now
			}
			generated := 0
			output, err := service.GetOrGenerate(ctx, source, metadata, "favicon-32x32.png", "", "", limits, func(context.Context) ([]byte, error) { generated++; return body, nil })
			if scenario == "canceled" {
				if !errors.Is(err, context.Canceled) || output != nil {
					t.Fatalf("canceled publication = %d bytes, %v", len(output), err)
				}
			} else if err != nil || !bytes.Equal(output, body) || result != "bypass" {
				t.Fatalf("expired publication = %d bytes, %v, %s", len(output), err, result)
			}
			if generated != 1 || clockReads != 3 || store.puts != 0 {
				t.Fatalf("generated=%d clocks=%d puts=%d", generated, clockReads, store.puts)
			}
		})
	}
}

func TestArtifactPublicationStillRejectsInvalidProductsAndBudgets(t *testing.T) {
	source, data, limits, metadata := artifactTestSource(t, "public, max-age=3600")
	body, err := generateIcon(context.Background(), data, limits, "favicon-32x32.png", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"invalid-product", "pixels", "bytes"} {
		t.Run(scenario, func(t *testing.T) {
			current := limits
			product := body
			want := errInvalidArtifact
			switch scenario {
			case "invalid-product":
				product = []byte("invalid PNG")
			case "pixels":
				current.MaxPixels = 1
				want = imageproxy.ErrImageTooLarge
			case "bytes":
				current.MaxResponseBytes = int64(len(body) - 1)
				want = imageproxy.ErrResponseTooLarge
			}
			store := &artifactTestStore{}
			service := newArtifactService(store, "memory", time.Minute, 1<<20, nil)
			validationNow := time.Now()
			service.now = func() time.Time { return validationNow }
			output, err := service.GetOrGenerate(context.Background(), source, metadata, "favicon-32x32.png", "", "", current, func(context.Context) ([]byte, error) { return product, nil })
			if !errors.Is(err, want) || output != nil || store.puts != 0 {
				t.Fatalf("product validation = %d bytes, %v, puts=%d", len(output), err, store.puts)
			}
		})
	}
}
