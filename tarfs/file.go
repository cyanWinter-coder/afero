package tarfs

import (
	"archive/tar"
	"bytes"
	"os"
	"sort"
	"sync"
	"syscall"

	"github.com/spf13/afero"
)

type File struct {
	h      *tar.Header
	data   *bytes.Reader
	closed bool
	fs     *Fs

	// name and info are snapshotted at Open time so the file keeps
	// describing the same archive entry after Close releases the
	// underlying archive reference.
	name string
	info os.FileInfo

	mu sync.Mutex
}

// Close releases the per-Open data reader and header and drops the
// reference to the underlying archive entry. Every File returned by Open
// owns an independent reader, so closing one file never affects another
// file opened on the same entry. Closing an already closed file returns
// [afero.ErrFileClosed].
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return afero.ErrFileClosed
	}

	f.closed = true
	f.h = nil
	f.data = nil
	return nil
}

func (f *File) Read(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, afero.ErrFileClosed
	}

	if f.h.Typeflag == tar.TypeDir {
		return 0, syscall.EISDIR
	}

	return f.data.Read(p)
}

func (f *File) ReadAt(p []byte, off int64) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, afero.ErrFileClosed
	}

	if f.h.Typeflag == tar.TypeDir {
		return 0, syscall.EISDIR
	}

	return f.data.ReadAt(p, off)
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, afero.ErrFileClosed
	}

	if f.h.Typeflag == tar.TypeDir {
		return 0, syscall.EISDIR
	}

	return f.data.Seek(offset, whence)
}

func (f *File) Write(p []byte) (n int, err error) { return 0, syscall.EROFS }

func (f *File) WriteAt(p []byte, off int64) (n int, err error) { return 0, syscall.EROFS }

// Name returns the archive path of this entry. It remains available after
// Close, mirroring os.File.
func (f *File) Name() string {
	return f.name
}

func (f *File) getDirectoryNames() ([]string, error) {
	d, ok := f.fs.files[f.Name()]
	if !ok {
		return nil, &os.PathError{Op: "readdir", Path: f.Name(), Err: syscall.ENOENT}
	}

	var names []string
	for n := range d {
		names = append(names, n)
	}
	sort.Strings(names)

	return names, nil
}

func (f *File) Readdir(count int) ([]os.FileInfo, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, afero.ErrFileClosed
	}
	isDir := f.info.IsDir()
	f.mu.Unlock()

	if !isDir {
		return nil, syscall.ENOTDIR
	}

	names, err := f.getDirectoryNames()
	if err != nil {
		return nil, err
	}

	d := f.fs.files[f.Name()]
	var fi []os.FileInfo
	for _, n := range names {
		if n == "" {
			continue
		}

		e := d[n]
		fi = append(fi, e.h.FileInfo())
		if count > 0 && len(fi) >= count {
			break
		}
	}

	return fi, nil
}

func (f *File) Readdirnames(n int) ([]string, error) {
	fi, err := f.Readdir(n)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, f := range fi {
		names = append(names, f.Name())
	}

	return names, nil
}

// Stat returns the entry metadata snapshotted at Open time, so it always
// reflects the same archive snapshot the file was opened against. It stays
// available after Close.
func (f *File) Stat() (os.FileInfo, error) {
	return f.info, nil
}

func (f *File) Sync() error { return nil }

func (f *File) Truncate(size int64) error { return syscall.EROFS }

func (f *File) WriteString(s string) (ret int, err error) { return 0, syscall.EROFS }
