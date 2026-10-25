package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"time"

	vfs "github.com/soulteary/vfs-kit"
	"github.com/willnorris/imageproxy"
)

func assertIconBundleInfo(t *testing.T, info fs.FileInfo, name string, size int64, directory bool) {
	t.Helper()
	mode := fs.FileMode(0644)
	if directory {
		mode = fs.ModeDir | 0755
	}
	stamp := time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)
	if info.Name() != name || info.Size() != size || info.Mode() != mode || info.IsDir() != directory || info.ModTime() != stamp || info.Sys() != nil {
		t.Fatalf("metadata for %q: name=%q size=%d mode=%v dir=%v time=%v sys=%v", name, info.Name(), info.Size(), info.Mode(), info.IsDir(), info.ModTime(), info.Sys())
	}
}

func TestIconBundleMemfsMetadataAndArchive(t *testing.T) {
	files := map[string][]byte{"site.webmanifest": []byte("manifest"), "favicon.ico": []byte("icon")}
	tree, err := stageIconBundle(context.Background(), files, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", ".", "/"} {
		for _, stat := range []func(string) (fs.FileInfo, error){tree.Stat, tree.Lstat} {
			info, err := stat(root)
			if err != nil {
				t.Fatal(err)
			}
			assertIconBundleInfo(t, info, ".", 0, true)
		}
		entries, err := tree.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].Name() != "favicon.ico" || entries[1].Name() != "site.webmanifest" {
			t.Fatalf("directory order: %+v", entries)
		}
		for _, info := range entries {
			assertIconBundleInfo(t, info, info.Name(), int64(len(files[info.Name()])), false)
		}
	}
	for name, data := range files {
		for _, path := range []string{name, "/" + name} {
			for _, stat := range []func(string) (fs.FileInfo, error){tree.Stat, tree.Lstat} {
				info, err := stat(path)
				if err != nil {
					t.Fatal(err)
				}
				assertIconBundleInfo(t, info, name, int64(len(data)), false)
			}
			file, err := tree.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := file.(fs.File).Stat()
			if err != nil {
				t.Fatal(err)
			}
			assertIconBundleInfo(t, info, name, int64(len(data)), false)
			got, readErr := io.ReadAll(file)
			if err := errors.Join(readErr, file.Close()); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("open %q: %q, %v", path, got, err)
			}
		}
	}
	data, err := encodeIconBundle(context.Background(), files, 4096)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range archive.File {
		_, offset := member.Modified.Zone()
		if member.Mode() != 0644 || !member.Modified.Equal(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)) || offset != 0 || member.UncompressedSize64 != uint64(len(files[member.Name])) {
			t.Fatalf("archive metadata: %+v", member.FileHeader)
		}
	}
}

func TestIconBundleMemfsCopiesInputsAndKeepsOpenSnapshots(t *testing.T) {
	input := []byte("original")
	files := map[string][]byte{"favicon.ico": input}
	tree, err := stageIconBundle(context.Background(), files, 4096)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	files["favicon.ico"] = []byte("map replacement")
	first, err := tree.Open("favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := tree.tree.WriteFile("favicon.ico", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(first)
	if err != nil || string(got) != "original" {
		t.Fatalf("input/open snapshot changed: %q, %v", got, err)
	}
	info, err := first.(fs.File).Stat()
	if err != nil {
		t.Fatal(err)
	}
	assertIconBundleInfo(t, info, "favicon.ico", 8, false)
	if _, err := first.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	peek := make([]byte, 3)
	if _, err := first.(io.ReaderAt).ReadAt(peek, 1); err != nil || string(peek) != "rig" {
		t.Fatalf("snapshot ReadAt: %q, %v", peek, err)
	}
	if _, err := first.Read(peek); err != nil || string(peek) != "ori" {
		t.Fatalf("ReadAt changed offset: %q, %v", peek, err)
	}
	second, err := tree.Open("favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(second)
	if err := errors.Join(readErr, second.Close()); err != nil || string(got) != "new" {
		t.Fatalf("fresh open: %q, %v", got, err)
	}
	if _, err := second.Read(peek); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed handle remained readable: %v", err)
	}
}

func TestIconBundleMemfsBridgeRejectsPathsAndWrites(t *testing.T) {
	tree, err := stageIconBundle(context.Background(), map[string][]byte{"favicon.ico": []byte("ico")}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"..", "../favicon.ico", "//favicon.ico", "/../favicon.ico", "favicon.ico/", "./favicon.ico", `dir\favicon.ico`, "favicon.ico\x00", "other.png"} {
		for _, operation := range []func(string) error{
			func(p string) error { _, err := tree.Open(p); return err },
			func(p string) error { _, err := tree.Stat(p); return err },
			func(p string) error { _, err := tree.Lstat(p); return err },
			func(p string) error { _, err := tree.ReadDir(p); return err },
		} {
			if err := operation(name); !errors.Is(err, errInvalidIconBundleName) {
				t.Fatalf("accepted path %q: %v", name, err)
			}
		}
	}
	for _, flag := range []int{os.O_RDONLY, os.O_RDWR, os.O_WRONLY | os.O_CREATE, os.O_APPEND, os.O_TRUNC} {
		if _, err := tree.OpenFile("favicon.ico", flag, 0644); !errors.Is(err, vfs.ErrReadOnlyFileSystem) {
			t.Fatalf("OpenFile(%d): %v", flag, err)
		}
	}
	if err := tree.Remove("favicon.ico"); !errors.Is(err, vfs.ErrReadOnlyFileSystem) {
		t.Fatal(err)
	}
	if err := tree.Mkdir("new", 0755); !errors.Is(err, vfs.ErrReadOnlyFileSystem) {
		t.Fatal(err)
	}
	if _, err := tree.Open("favicon-32x32.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing product file: %v", err)
	}
	// Even an accidental internal extension cannot expand archive membership.
	if err := tree.tree.WriteFile("other.png", nil, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.ReadDir("/"); !errors.Is(err, errInvalidIconBundleName) {
		t.Fatalf("unexpected staged member was exposed: %v", err)
	}
}

func TestIconBundleMemfsCancellationAfterStaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tree, err := stageIconBundle(ctx, map[string][]byte{"favicon.ico": []byte("ico")}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	file, err := tree.Open("favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for _, operation := range []func() error{
		func() error { _, err := tree.Open("favicon.ico"); return err },
		func() error { _, err := tree.Stat("favicon.ico"); return err },
		func() error { _, err := tree.Lstat("favicon.ico"); return err },
		func() error { _, err := tree.ReadDir("/"); return err },
		func() error { _, err := file.Read(make([]byte, 1)); return err },
		func() error { _, err := file.Seek(0, io.SeekStart); return err },
		func() error { _, err := file.(io.ReaderAt).ReadAt(make([]byte, 1), 0); return err },
		func() error { _, err := file.(fs.File).Stat(); return err },
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatalf("operation ignored cancellation: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("cancellation prevented handle close: %v", err)
	}
}

func TestIconBundleMemfsPreflightAndEmptyArchive(t *testing.T) {
	files := map[string][]byte{"favicon.ico": []byte("123"), "site.webmanifest": []byte("456")}
	for _, limit := range []int64{-1, 0, 5} {
		if tree, err := stageIconBundle(context.Background(), files, limit); tree != nil || !errors.Is(err, imageproxy.ErrResponseTooLarge) {
			t.Fatalf("preflight limit %d: %v, %v", limit, tree, err)
		}
	}
	tree, err := stageIconBundle(context.Background(), files, 6)
	if err != nil {
		t.Fatalf("exact aggregate source budget: %v", err)
	}
	if entries, err := tree.ReadDir("/"); err != nil || len(entries) != 2 {
		t.Fatalf("exact-budget directory: %v, %v", entries, err)
	}
	data, err := encodeIconBundle(context.Background(), nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	names, contents, err := readIconBundle(data)
	if err != nil || len(names) != 0 || !reflect.DeepEqual(contents, map[string][]byte{}) {
		t.Fatalf("empty archive: %v, %v, %v", names, contents, err)
	}
}

func TestIconBundleMemfsMatchesPreviousArchive(t *testing.T) {
	files := map[string][]byte{"favicon.ico": []byte("ico"), "site.webmanifest": []byte("manifest")}
	entries := make(map[string]*vfs.File, len(files))
	for name, data := range files {
		entries[name] = &vfs.File{Data: data, Mode: 0644, ModTime: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	previous, err := vfs.Map(entries)
	if err != nil {
		t.Fatal(err)
	}
	var reference bytes.Buffer
	if err := vfs.WriteZip(&reference, previous); err != nil {
		t.Fatal(err)
	}
	got, err := encodeIconBundle(context.Background(), files, 4096)
	if err != nil || !bytes.Equal(got, reference.Bytes()) {
		t.Fatalf("memfs changed archive bytes: %v", err)
	}
}
