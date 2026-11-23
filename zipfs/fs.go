package zipfs

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/afero"
	"github.com/spf13/afero/internal/archivefs"
)

type Fs struct {
	r     *zip.Reader
	files map[string]map[string]*zip.File
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

// New builds a read-only file system from an already parsed zip archive.
//
// Deprecated: New silently ignores entries whose names are unsafe and keeps
// only the first entry on duplicate paths. Use [NewStrict] to get a
// locatable, index-bound error instead; it never returns a partial index.
// New is retained for compatibility with existing callers and delegates to
// NewStrict, returning an empty, still usable read-only file system when the
// archive cannot be indexed.
func New(r *zip.Reader) afero.Fs {
	fs, err := NewStrict(r)
	if err != nil {
		return &Fs{r: r, files: make(map[string]map[string]*zip.File)}
	}
	return fs
}

// NewStrict indexes r and returns the resulting read-only file system.
//
// Indexing is an all-or-nothing boundary: an entry with an absolute name,
// a name escaping the archive root, an oversized or empty name, a duplicate
// cleaned path, or a directory entry whose metadata cannot be read makes the
// whole archive unusable. The returned error wraps an
// [archivefs.EntryError] naming the offending entry (use errors.As to
// locate it), and the returned Fs is nil so a partial index can never be
// mistaken for a complete file system. Multiple independent indexing errors
// are joined with errors.Join.
//
// The returned index is immutable after construction, so concurrent Stat,
// Open and Close calls observe the same archive snapshot.
func NewStrict(r *zip.Reader) (*Fs, error) {
	fs := &Fs{r: r, files: make(map[string]map[string]*zip.File)}
	seen := make(map[string]bool)
	var errs []error

	addError := func(entry string, err error) {
		errs = append(errs, archivefs.EntryErrorf(entry, err))
	}

	for _, file := range r.File {
		if err := archivefs.ValidateName(file.Name); err != nil {
			addError(file.Name, err)
			continue
		}

		cleaned := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+filepath.ToSlash(file.Name))), "/")
		if seen[cleaned] {
			addError(file.Name, &archivefs.DuplicateEntryError{Entry: file.Name, Path: cleaned})
			continue
		}
		seen[cleaned] = true

		d, f := splitpath(file.Name)
		if _, ok := fs.files[d]; !ok {
			fs.files[d] = make(map[string]*zip.File)
		}
		// Only the first entry wins: never overwrite an entry that is
		// already indexed (in practice duplicates were reported above).
		if _, ok := fs.files[d][f]; !ok {
			fs.files[d][f] = file
		}
		if file.FileInfo().IsDir() {
			dirname := filepath.Join(d, f)
			if _, ok := fs.files[dirname]; !ok {
				fs.files[dirname] = make(map[string]*zip.File)
			}
		}

		dv := d
		for {
			d, f = splitpath(dv)
			if _, ok := fs.files[d]; !ok {
				fs.files[d] = make(map[string]*zip.File)
			}
			if f == "" {
				break
			}
			if _, ok := fs.files[d][f]; !ok {
				fs.files[d][f] = nil
			}
			dv = d
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return fs, nil
}

func (fs *Fs) Create(name string) (afero.File, error) { return nil, syscall.EPERM }

func (fs *Fs) Mkdir(name string, perm os.FileMode) error { return syscall.EPERM }

func (fs *Fs) MkdirAll(path string, perm os.FileMode) error { return syscall.EPERM }

func (fs *Fs) Open(name string) (afero.File, error) {
	d, f := splitpath(name)
	if f == "" {
		return &File{
			fs:    fs,
			name:  string(filepath.Separator),
			info:  &pseudoRoot{},
			isdir: true,
		}, nil
	}
	if _, ok := fs.files[d]; !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}
	file, ok := fs.files[d][f]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}
	if file == nil {
		pdPath := filepath.Join(d, f)
		return &File{
			fs:        fs,
			pseudodir: &pseudoDir{path: pdPath},
			name:      pdPath,
			info:      pseudoDir{path: pdPath},
			isdir:     true,
		}, nil
	}
	info := file.FileInfo()
	return &File{
		fs:      fs,
		zipfile: file,
		name:    filepath.Join(splitpath(file.Name)),
		info:    info,
		size:    int64(file.UncompressedSize64),
		isdir:   info.IsDir(),
	}, nil
}

func (fs *Fs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	if flag != os.O_RDONLY {
		return nil, syscall.EPERM
	}
	return fs.Open(name)
}

func (fs *Fs) Remove(name string) error { return syscall.EPERM }

func (fs *Fs) RemoveAll(path string) error { return syscall.EPERM }

func (fs *Fs) Rename(oldname, newname string) error { return syscall.EPERM }

type pseudoRoot struct{}

func (p *pseudoRoot) Name() string       { return string(filepath.Separator) }
func (p *pseudoRoot) Size() int64        { return 0 }
func (p *pseudoRoot) Mode() os.FileMode  { return os.ModeDir | os.ModePerm }
func (p *pseudoRoot) ModTime() time.Time { return time.Now() }
func (p *pseudoRoot) IsDir() bool        { return true }
func (p *pseudoRoot) Sys() interface{}   { return nil }

type pseudoDir struct {
	path string
}

func (fi pseudoDir) Name() string       { return filepath.Base(fi.path) }
func (fi pseudoDir) Size() int64        { return 0 }
func (fi pseudoDir) IsDir() bool        { return true }
func (fi pseudoDir) ModTime() time.Time { return time.Now() }
func (fi pseudoDir) Mode() os.FileMode  { return os.ModeDir | os.ModePerm }
func (fi pseudoDir) Sys() interface{}   { return nil }

func (fs *Fs) Stat(name string) (os.FileInfo, error) {
	d, f := splitpath(name)
	if f == "" {
		return &pseudoRoot{}, nil
	}
	if _, ok := fs.files[d]; !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}
	file, ok := fs.files[d][f]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}
	if file == nil {
		return pseudoDir{path: filepath.Join(d, f)}, nil
	}
	return file.FileInfo(), nil
}

func (fs *Fs) Name() string { return "zipfs" }

func (fs *Fs) Chmod(name string, mode os.FileMode) error { return syscall.EPERM }

func (fs *Fs) Chown(name string, uid, gid int) error { return syscall.EPERM }

func (fs *Fs) Chtimes(name string, atime time.Time, mtime time.Time) error { return syscall.EPERM }
