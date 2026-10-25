package main

import (
	"context"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	memfs "github.com/soulteary/memfs-kit"
	vfs "github.com/soulteary/vfs-kit"
	"github.com/willnorris/imageproxy"
)

// stageIconBundle validates every name and the aggregate input size before
// memfs copies any input. The filesystem belongs only to this request; no hook
// or shared tree can supply additional content while the archive is written.
func stageIconBundle(ctx context.Context, files map[string][]byte, maxBytes int64) (*iconBundleFS, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, imageproxy.ErrResponseTooLarge
	}
	remaining := min(maxBytes, int64(math.MaxInt))
	names := make([]string, 0, 8)
	for name, data := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !validIconBundleName(name) {
			return nil, errInvalidIconBundleName
		}
		if int64(len(data)) > remaining {
			return nil, imageproxy.ErrResponseTooLarge
		}
		remaining -= int64(len(data))
		names = append(names, name)
	}
	sort.Strings(names)
	tree := memfs.New()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// WriteFile makes the single owned copy. Cloning here would retain a
		// second transient copy without improving snapshot isolation.
		if err := tree.WriteFile(name, files[name], 0644); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &iconBundleFS{ctx: ctx, tree: tree}, nil
}

// iconBundleFS bridges memfs's io/fs API to the existing ZIP writer. Only the
// root and the fixed product basenames can be read, and all mutations fail.
type iconBundleFS struct {
	ctx  context.Context
	tree *memfs.FS
}

var _ vfs.VFS = (*iconBundleFS)(nil)

func iconBundlePath(op, name string) (string, error) {
	if name == "" || name == "." || name == "/" {
		return ".", nil
	}
	rel := strings.TrimPrefix(name, "/")
	if !validIconBundleName(rel) {
		return "", &fs.PathError{Op: op, Path: name, Err: errInvalidIconBundleName}
	}
	return rel, nil
}

func (b *iconBundleFS) Open(name string) (vfs.RFile, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	rel, err := iconBundlePath("open", name)
	if err != nil {
		return nil, err
	}
	file, err := b.tree.Open(rel)
	if err != nil {
		return nil, err
	}
	regular, ok := file.(*memfs.File)
	if !ok {
		_ = file.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return &iconBundleFile{File: regular, ctx: b.ctx}, nil
}

func (*iconBundleFS) OpenFile(string, int, os.FileMode) (vfs.WFile, error) {
	return nil, vfs.ErrReadOnlyFileSystem
}

func (b *iconBundleFS) Stat(name string) (os.FileInfo, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	rel, err := iconBundlePath("stat", name)
	if err != nil {
		return nil, err
	}
	info, err := b.tree.Stat(rel)
	if err != nil {
		return nil, err
	}
	return iconBundleInfo{info}, nil
}

func (b *iconBundleFS) Lstat(name string) (os.FileInfo, error) { return b.Stat(name) }

func (b *iconBundleFS) ReadDir(name string) ([]os.FileInfo, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	rel, err := iconBundlePath("readdir", name)
	if err != nil {
		return nil, err
	}
	entries, err := b.tree.ReadDir(rel)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		if !validIconBundleName(entry.Name()) || entry.IsDir() {
			return nil, errInvalidIconBundleName
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, iconBundleInfo{info})
	}
	return infos, nil
}

func (*iconBundleFS) Mkdir(string, os.FileMode) error { return vfs.ErrReadOnlyFileSystem }
func (*iconBundleFS) Remove(string) error             { return vfs.ErrReadOnlyFileSystem }
func (*iconBundleFS) String() string                  { return "read-only icon bundle" }

// Metadata is fixed on every route: directory listings, path stats, and open
// handle stats. memfs's current wall-clock timestamps never reach the ZIP.
type iconBundleInfo struct{ fs.FileInfo }

func (i iconBundleInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0755
	}
	return 0644
}
func (iconBundleInfo) ModTime() time.Time {
	return time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)
}
func (iconBundleInfo) Sys() any { return nil }

type iconBundleFile struct {
	*memfs.File
	ctx context.Context
}

func (f *iconBundleFile) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}

func (f *iconBundleFile) ReadAt(p []byte, offset int64) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.ReadAt(p, offset)
}

func (f *iconBundleFile) Seek(offset int64, whence int) (int64, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.Seek(offset, whence)
}

func (f *iconBundleFile) Stat() (fs.FileInfo, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return iconBundleInfo{info}, nil
}
