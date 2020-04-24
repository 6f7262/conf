package kipp

import (
	"net/http"
	"os"
	"time"

	"github.com/uhthomas/kipp/database"
	"github.com/uhthomas/kipp/fs"
)

// fileSystemFunc implements http.FileSystem.
type fileSystemFunc func(string) (http.File, error)

func (f fileSystemFunc) Open(name string) (http.File, error) { return f(name) }

type file struct {
	entry database.Entry
	file  fs.ReadSeekCloser
}

func (f *file) Close() error { return f.file.Close() }

func (f *file) Read(b []byte) (n int, err error) { return f.file.Read(b) }

func (f *file) Seek(offset int64, whence int) (int64, error) { return f.file.Seek(offset, whence) }

func (f *file) Readdir(int) ([]os.FileInfo, error) { return nil, nil }

func (f *file) Stat() (os.FileInfo, error) { return &fileInfo{entry: f.entry}, nil }

type fileInfo struct{ entry database.Entry }

func (fi *fileInfo) Name() string { return fi.entry.Name }

func (fi *fileInfo) Size() int64 { return fi.entry.Size }

func (fi *fileInfo) Mode() os.FileMode { return 0600 }

func (fi *fileInfo) IsDir() bool { return false }

func (fi *fileInfo) Sys() interface{} { return nil }

func (fi *fileInfo) ModTime() time.Time { return fi.entry.Timestamp }
