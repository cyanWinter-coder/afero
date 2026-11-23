// package tarfs implements a read-only in-memory representation of a tar archive
package tarfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/afero"
	"github.com/spf13/afero/internal/archivefs"
)

type Fs struct {
	files map[string]map[string]*fsEntry
}

type fsEntry struct {
	h    *tar.Header
	data []byte
}

func splitpath(name string) (dir, file string) {
	name = filepath.ToSlash(name)
	if len(name) == 0 || name[0] != '/' {
		name = "/" + name
	}
	name = filepath.Clean(name)
	dir, file = filepath.Split(name)
	dir = filepath.Clean(dir)
	return
}

// New builds a read-only file system from a tar stream.
//
// Deprecated: New returns nil and drops the error when the stream is
// corrupt, and silently overwrites entries on duplicate paths. Use
// [NewStrict], which returns an index-bound, locatable error and never a
// partial file system. New is retained for compatibility and delegates to
// NewStrict.
func New(t *tar.Reader) *Fs {
	fs, err := NewStrict(t)
	if err != nil {
		return nil
	}
	return fs
}

// NewStrict consumes t and returns the resulting read-only file system.
//
// Indexing is an all-or-nothing boundary: an entry with an absolute name,
// a name escaping the archive root, an oversized or empty name, a duplicate
// cleaned path, or a truncated/unreadable entry body makes the whole archive
// unusable. The returned error wraps an [archivefs.EntryError] naming the
// offending entry (use errors.As to locate it), and the returned Fs is nil
// so a partial index can never be mistaken for a complete file system.
// Independent per-entry validation errors are joined with errors.Join; the
// first stream-level error aborts the scan.
//
// The returned index is immutable after construction, so concurrent Stat,
// Open and Close calls observe the same archive snapshot.
func NewStrict(t *tar.Reader) (*Fs, error) {
	fs := &Fs{files: make(map[string]map[string]*fsEntry)}
	seen := make(map[string]bool)
	var errs []error

	addError := func(entry string, err error) {
		errs = append(errs, archivefs.EntryErrorf(entry, err))
	}

	for {
		hdr, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A stream-level failure (invalid header, truncated block)
			// aborts the scan; keep any per-entry errors already found.
			return nil, errors.Join(append(errs, err)...)
		}

		entryName := "<unknown>"
		if hdr != nil {
			entryName = hdr.Name
		}

		if verr := archivefs.ValidateName(entryName); verr != nil {
			addError(entryName, verr)
			continue
		}

		cleaned := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+filepath.ToSlash(hdr.Name))), "/")
		if seen[cleaned] {
			addError(entryName, &archivefs.DuplicateEntryError{Entry: entryName, Path: cleaned})
			continue
		}
		seen[cleaned] = true

		var buf bytes.Buffer
		size, err := buf.ReadFrom(t)
		if err != nil {
			addError(entryName, err)
			return nil, errors.Join(errs...)
		}
		if size != hdr.Size {
			addError(entryName, io.ErrUnexpectedEOF)
			return nil, errors.Join(errs...)
		}

		d, f := splitpath(hdr.Name)
		if _, ok := fs.files[d]; !ok {
			fs.files[d] = make(map[string]*fsEntry)
		}
		// First entry wins; duplicates were reported above and skipped.
		if _, ok := fs.files[d][f]; !ok {
			fs.files[d][f] = &fsEntry{h: hdr, data: buf.Bytes()}
		}
	}

	if fs.files[afero.FilePathSeparator] == nil {
		fs.files[afero.FilePathSeparator] = make(map[string]*fsEntry)
	}
	// Add a pseudoroot.
	fs.files[afero.FilePathSeparator][""] = &fsEntry{
		h: &tar.Header{
			Name:     afero.FilePathSeparator,
			Typeflag: tar.TypeDir,
			Size:     0,
		},
		data: nil,
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return fs, nil
}

func (fs *Fs) Open(name string) (afero.File, error) {
	d, f := splitpath(name)
	if _, ok := fs.files[d]; !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: syscall.ENOENT}
	}

	file, ok := fs.files[d][f]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: syscall.ENOENT}
	}

	nf := File{
		h:    file.h,
		data: bytes.NewReader(file.data),
		fs:   fs,
		name: filepath.Join(splitpath(file.h.Name)),
		info: file.h.FileInfo(),
	}

	return &nf, nil
}

func (fs *Fs) Name() string { return "tarfs" }

func (fs *Fs) Create(name string) (afero.File, error) { return nil, syscall.EROFS }

func (fs *Fs) Mkdir(name string, perm os.FileMode) error { return syscall.EROFS }

func (fs *Fs) MkdirAll(path string, perm os.FileMode) error { return syscall.EROFS }

func (fs *Fs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	if flag != os.O_RDONLY {
		return nil, &os.PathError{Op: "open", Path: name, Err: syscall.EPERM}
	}

	return fs.Open(name)
}

func (fs *Fs) Remove(name string) error { return syscall.EROFS }

func (fs *Fs) RemoveAll(path string) error { return syscall.EROFS }

func (fs *Fs) Rename(oldname string, newname string) error { return syscall.EROFS }

func (fs *Fs) Stat(name string) (os.FileInfo, error) {
	d, f := splitpath(name)
	if _, ok := fs.files[d]; !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}

	file, ok := fs.files[d][f]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}

	return file.h.FileInfo(), nil
}

func (fs *Fs) Chmod(name string, mode os.FileMode) error { return syscall.EROFS }

func (fs *Fs) Chown(name string, uid, gid int) error { return syscall.EROFS }

func (fs *Fs) Chtimes(name string, atime time.Time, mtime time.Time) error { return syscall.EROFS }
