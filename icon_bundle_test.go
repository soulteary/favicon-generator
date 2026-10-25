package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

func readIconBundle(data []byte) ([]string, map[string][]byte, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(archive.File))
	files := make(map[string][]byte, len(archive.File))
	for _, member := range archive.File {
		file, err := member.Open()
		if err != nil {
			return nil, nil, err
		}
		content, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, nil, err
		}
		names = append(names, member.Name)
		files[member.Name] = content
	}
	return names, files, nil
}

func TestEncodeIconBundleContentsAndOrder(t *testing.T) {
	files := map[string][]byte{
		"site.webmanifest":           []byte(`{"icons":[]}`),
		"favicon-32x32.png":          {0, 1, 2, 255},
		"apple-touch-icon.png":       []byte("apple"),
		"android-chrome-512x512.png": []byte("android large"),
		"favicon.ico":                []byte("ico"),
		"favicon-16x16.png":          []byte("small"),
		"favicon-48x48.png":          []byte("large"),
		"android-chrome-192x192.png": []byte("android small"),
	}
	data, err := encodeIconBundle(context.Background(), files, 4096)
	if err != nil {
		t.Fatal(err)
	}
	names, got, err := readIconBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"android-chrome-192x192.png", "android-chrome-512x512.png", "apple-touch-icon.png", "favicon-16x16.png", "favicon-32x32.png", "favicon-48x48.png", "favicon.ico", "site.webmanifest"}
	if !reflect.DeepEqual(names, wantNames) || !reflect.DeepEqual(got, files) {
		t.Fatalf("bundle names = %v, contents = %v", names, got)
	}
	again, err := encodeIconBundle(context.Background(), files, 4096)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("equal input did not produce a stable ZIP: %v", err)
	}
}

func TestEncodeIconBundleBudgets(t *testing.T) {
	files := map[string][]byte{"favicon.ico": []byte("ico")}
	data, err := encodeIconBundle(context.Background(), files, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{-1, 0, 2, 3, int64(len(data) - 1)} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			got, err := encodeIconBundle(context.Background(), files, limit)
			if !errors.Is(err, imageproxy.ErrResponseTooLarge) || got != nil {
				t.Fatalf("limit %d: data length = %d, error = %v", limit, len(got), err)
			}
		})
	}
	if got, err := encodeIconBundle(context.Background(), files, int64(len(data))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exact output limit failed: %v", err)
	}
	if got, err := encodeIconBundle(context.Background(), map[string][]byte{
		"favicon.ico": []byte("123"), "site.webmanifest": []byte("456"),
	}, 5); !errors.Is(err, imageproxy.ErrResponseTooLarge) || got != nil {
		t.Fatalf("aggregate source limit: %d bytes, %v", len(got), err)
	}
}

func TestEncodeIconBundleRejectsUnsafeAndUnknownNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../favicon.ico", "/favicon.ico", `dir\favicon.ico`, "dir/favicon.ico", "a..b.png", "favicon.ico/", "favicon.ico\x00", "FAVICON.ICO", "other.png", "C:foobar.png"} {
		t.Run(name, func(t *testing.T) {
			got, err := encodeIconBundle(context.Background(), map[string][]byte{name: nil}, 1024)
			if !errors.Is(err, errInvalidIconBundleName) || got != nil {
				t.Fatalf("name %q: output = %d bytes, error = %v", name, len(got), err)
			}
		})
	}
}

func TestEncodeIconBundleCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if data, err := encodeIconBundle(ctx, map[string][]byte{"favicon.ico": []byte("ico")}, 1024); !errors.Is(err, context.Canceled) || data != nil {
		t.Fatalf("canceled bundle: %d bytes, %v", len(data), err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if data, err := encodeIconBundle(expired, nil, 1024); !errors.Is(err, context.DeadlineExceeded) || data != nil {
		t.Fatalf("expired bundle: %d bytes, %v", len(data), err)
	}
	// The final archive flush, not only entry creation, also checks cancellation.
	ctx, cancel = context.WithCancel(context.Background())
	writer := &iconBundleWriter{ctx: ctx, remaining: 8}
	if n, err := writer.Write([]byte("ok")); n != 2 || err != nil {
		t.Fatalf("initial write: %d, %v", n, err)
	}
	cancel()
	if n, err := writer.Write([]byte("later")); n != 0 || !errors.Is(err, context.Canceled) || writer.buffer.String() != "ok" {
		t.Fatalf("write after cancellation: %d, %v, %q", n, err, writer.buffer.String())
	}
}

func TestEncodeIconBundleParallelIsolation(t *testing.T) {
	var workers sync.WaitGroup
	results := make(chan error, 24)
	for i := range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			content := []byte(fmt.Sprintf("request-%d", i))
			data, err := encodeIconBundle(context.Background(), map[string][]byte{"favicon.ico": content}, 1024)
			if err == nil {
				content[0] = 'X'
				var names []string
				var files map[string][]byte
				names, files, err = readIconBundle(data)
				if err == nil && (!reflect.DeepEqual(names, []string{"favicon.ico"}) || string(files["favicon.ico"]) != fmt.Sprintf("request-%d", i)) {
					err = errors.New("bundle contents crossed request boundaries")
				}
			}
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestIconBundleWriterRejectsOversizedWrites(t *testing.T) {
	writer := &iconBundleWriter{ctx: context.Background(), remaining: 2}
	if n, err := writer.Write([]byte("too large")); n != 0 || !errors.Is(err, imageproxy.ErrResponseTooLarge) || writer.buffer.Len() != 0 {
		t.Fatalf("oversized write: %d, %v, buffered %d", n, err, writer.buffer.Len())
	}
}
