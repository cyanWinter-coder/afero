package afero

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	_ Lstater        = (*BasePathFs)(nil)
	_ fs.ReadDirFile = (*BasePathFile)(nil)
	_ io.WriterTo    = (*BasePathFile)(nil)
	_ io.ReaderFrom  = (*BasePathFile)(nil)
)

// The BasePathFs restricts all operations to a given path within an Fs.
// The given file name to the operations on this Fs will be prepended with
// the base path before calling the base Fs.
//
// Every entry point passes its name through the same normalization: leading
// separators are interpreted as the base directory, interior "." and ".."
// elements are resolved, and any name that would climb above the base path is
// rejected as a non-existing file. When the wrapped filesystem is OsFs (or a
// chain of BasePathFs over OsFs), path resolution is delegated to os.Root, so
// symbolic links are resolved inside the base directory and links escaping it
// fail with ErrPathEscape instead of silently reaching outside.
//
// Note that it does not clean the error messages on return, so you may
// reveal the real path on errors.
type BasePathFs struct {
	source Fs
	path   string
}

type BasePathFile struct {
	File
	path string
}

func (f *BasePathFile) Name() string {
	sourcename := f.File.Name()
	name := strings.TrimPrefix(sourcename, filepath.Clean(f.path))
	// Opening the base directory itself yields the bare separator followed
	// by "."; report it as the base-relative root instead.
	if name == string(filepath.Separator)+"." {
		return string(filepath.Separator)
	}
	return name
}

func (f *BasePathFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if rdf, ok := f.File.(fs.ReadDirFile); ok {
		return rdf.ReadDir(n)
	}
	return readDirFile{f.File}.ReadDir(n)
}

func (f *BasePathFile) WriteTo(w io.Writer) (int64, error) {
	if wt, ok := f.File.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}
	return io.Copy(w, f.File)
}

func (f *BasePathFile) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := f.File.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(f.File, r)
}

func NewBasePathFs(source Fs, path string) Fs {
	return &BasePathFs{source: source, path: path}
}

// RealPath normalizes name against the base path without consulting the
// wrapped filesystem. On a name outside the base path it returns the given
// file name and an error, else the given file with the base path prepended.
//
// RealPath is a purely lexical helper: it does not resolve symbolic links.
// Operations on BasePathFs use the same normalization and additionally
// enforce the root boundary against symbolic links.
func (b *BasePathFs) RealPath(name string) (path string, err error) {
	local, err := b.localPath(name)
	if err != nil {
		return name, err
	}
	return filepath.Join(b.path, local), nil
}

// pathResolutionError marks errors produced while normalizing or resolving a
// name, as opposed to errors reported by the wrapped filesystem. Callers wrap
// the former in an *os.PathError carrying the operation name and the original
// user-supplied path, matching the historical BasePathFs error shape.
type pathResolutionError struct{ err error }

func (e *pathResolutionError) Error() string { return e.err.Error() }
func (e *pathResolutionError) Unwrap() error { return e.err }

func resolutionError(err error) error {
	if err == nil {
		return nil
	}
	return &pathResolutionError{err: err}
}

// pathErr unwraps a pathResolutionError so callers can embed the underlying
// cause in their own error type.
func pathErr(err error) error {
	var rerr *pathResolutionError
	if errors.As(err, &rerr) {
		return rerr.err
	}
	return err
}

// wrapResolution wraps only path-normalization failures in an *os.PathError;
// errors coming from the wrapped filesystem are returned unchanged.
func wrapResolution(op, name string, err error) error {
	var rerr *pathResolutionError
	if errors.As(err, &rerr) {
		return &os.PathError{Op: op, Path: name, Err: rerr.err}
	}
	return err
}

// execute normalizes name and invokes fn with a local, root-relative path.
//
// When the wrapped filesystem is OS-backed and its base directory exists, fn
// receives a live *os.Root and all path components - symbolic links included -
// are resolved by the kernel inside that root. If the kernel rejects the path
// because of a symbolic link (os.Root refuses absolute link targets even when
// they stay inside the root), the link chain is resolved inside the root and
// the operation is retried once with the resulting root-relative path. The
// retry runs under the same root, so a concurrent replacement of a path
// component can make it fail but cannot widen the boundary.
//
// Otherwise fn receives a nil root and must use b.source; symbolic links are
// then resolved by resolvePath before the call.
//
// Errors returned by fn are passed through unchanged; normalization and
// confinement failures are marked with pathResolutionError.
func (b *BasePathFs) execute(name string, followFinal bool, fn func(local string, root *os.Root) error) error {
	local, err := b.localPath(name)
	if err != nil {
		return resolutionError(err)
	}

	if root, ok := b.openOSRoot(); ok {
		defer root.Close()

		err := fn(local, root)
		if err == nil || !isEscapeError(err) {
			return err
		}

		// os.Root rejected the path. When a symbolic link in it could still
		// be usable - an absolute target that stays inside the root, which an
		// older BasePathFs wrote - resolve the chain against the root and
		// retry once. The retry uses the same root, so a concurrent swap of a
		// component can make it fail but cannot widen the boundary.
		resolved, resolveErr := resolveInRoot(root, local, followFinal)
		if resolveErr != nil {
			return resolutionError(resolveErr)
		}
		if resolved == local {
			// Nothing to resolve: the rejection is a genuine escape.
			return resolutionError(ErrPathEscape)
		}
		if err := fn(resolved, root); err != nil {
			if isEscapeError(err) {
				return resolutionError(ErrPathEscape)
			}
			return err
		}
		return nil
	}

	resolved, err := b.resolvePath(name, followFinal)
	if err != nil {
		return resolutionError(err)
	}
	return fn(resolved, nil)
}

// sourcePath maps a local, root-relative path to a path understood by the
// wrapped filesystem.
func (b *BasePathFs) sourcePath(local string) string {
	return filepath.Join(b.path, local)
}

func (b *BasePathFs) Chtimes(name string, atime, mtime time.Time) (err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		if root != nil {
			return root.Chtimes(local, atime, mtime)
		}
		return b.source.Chtimes(b.sourcePath(local), atime, mtime)
	})
	return wrapResolution("chtimes", name, err)
}

func (b *BasePathFs) Chmod(name string, mode os.FileMode) (err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		if root != nil {
			return root.Chmod(local, mode)
		}
		return b.source.Chmod(b.sourcePath(local), mode)
	})
	return wrapResolution("chmod", name, err)
}

func (b *BasePathFs) Chown(name string, uid, gid int) (err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		if root != nil {
			return root.Chown(local, uid, gid)
		}
		return b.source.Chown(b.sourcePath(local), uid, gid)
	})
	return wrapResolution("chown", name, err)
}

func (b *BasePathFs) Name() string {
	return "BasePathFs"
}

func (b *BasePathFs) Stat(name string) (fi os.FileInfo, err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		if root != nil {
			var err error
			fi, err = root.Stat(local)
			return err
		}
		var err error
		fi, err = b.source.Stat(b.sourcePath(local))
		return err
	})
	return fi, wrapResolution("stat", name, err)
}

func (b *BasePathFs) Rename(oldname, newname string) (err error) {
	oldLocal, err := b.localPath(oldname)
	if err != nil {
		return &os.PathError{Op: "rename", Path: oldname, Err: pathErr(resolutionError(err))}
	}
	newLocal, err := b.localPath(newname)
	if err != nil {
		return &os.PathError{Op: "rename", Path: newname, Err: pathErr(resolutionError(err))}
	}

	if root, ok := b.openOSRoot(); ok {
		defer root.Close()

		err := root.Rename(oldLocal, newLocal)
		if err == nil || !isEscapeError(err) {
			return err
		}

		// Retry once with the link chains resolved inside the root; see
		// execute. Rename never follows the final element of either name.
		oldResolved, rerr := resolveInRoot(root, oldLocal, false)
		if rerr != nil {
			return &os.PathError{Op: "rename", Path: oldname, Err: pathErr(resolutionError(rerr))}
		}
		newResolved, rerr := resolveInRoot(root, newLocal, false)
		if rerr != nil {
			return &os.PathError{Op: "rename", Path: newname, Err: pathErr(resolutionError(rerr))}
		}
		if oldResolved == oldLocal && newResolved == newLocal {
			return &os.PathError{Op: "rename", Path: oldname, Err: ErrPathEscape}
		}
		if err := root.Rename(oldResolved, newResolved); err != nil {
			if isEscapeError(err) {
				return &os.PathError{Op: "rename", Path: oldname, Err: ErrPathEscape}
			}
			return err
		}
		return nil
	}

	oldResolved, err := b.resolvePath(oldname, false)
	if err != nil {
		return &os.PathError{Op: "rename", Path: oldname, Err: pathErr(resolutionError(err))}
	}
	newResolved, err := b.resolvePath(newname, false)
	if err != nil {
		return &os.PathError{Op: "rename", Path: newname, Err: pathErr(resolutionError(err))}
	}
	return b.source.Rename(b.sourcePath(oldResolved), b.sourcePath(newResolved))
}

func (b *BasePathFs) RemoveAll(name string) (err error) {
	err = b.execute(name, false, func(local string, root *os.Root) error {
		if root != nil && local != "." {
			return root.RemoveAll(local)
		}
		// Removing the root itself is only expressible through the wrapped
		// filesystem; os.Root rejects ".".
		return b.source.RemoveAll(b.sourcePath(local))
	})
	return wrapResolution("remove_all", name, err)
}

func (b *BasePathFs) Remove(name string) (err error) {
	err = b.execute(name, false, func(local string, root *os.Root) error {
		if root != nil && local != "." {
			return root.Remove(local)
		}
		return b.source.Remove(b.sourcePath(local))
	})
	return wrapResolution("remove", name, err)
}

func (b *BasePathFs) OpenFile(name string, flag int, mode os.FileMode) (f File, err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		var sourcef File
		var basePath string
		if root != nil {
			basePath = root.Name()
			file, err := root.OpenFile(local, flag, mode)
			if err != nil {
				return err
			}
			sourcef = file
		} else {
			basePath = b.path
			file, err := b.source.OpenFile(b.sourcePath(local), flag, mode)
			if err != nil {
				return err
			}
			sourcef = file
		}
		f = &BasePathFile{File: sourcef, path: basePath}
		return nil
	})
	return f, wrapResolution("openfile", name, err)
}

func (b *BasePathFs) Open(name string) (f File, err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		var sourcef File
		var basePath string
		if root != nil {
			basePath = root.Name()
			file, err := root.Open(local)
			if err != nil {
				return err
			}
			sourcef = file
		} else {
			basePath = b.path
			file, err := b.source.Open(b.sourcePath(local))
			if err != nil {
				return err
			}
			sourcef = file
		}
		f = &BasePathFile{File: sourcef, path: basePath}
		return nil
	})
	return f, wrapResolution("open", name, err)
}

func (b *BasePathFs) Mkdir(name string, mode os.FileMode) (err error) {
	err = b.execute(name, false, func(local string, root *os.Root) error {
		if root != nil {
			return root.Mkdir(local, mode)
		}
		return b.source.Mkdir(b.sourcePath(local), mode)
	})
	return wrapResolution("mkdir", name, err)
}

func (b *BasePathFs) MkdirAll(name string, mode os.FileMode) (err error) {
	err = b.execute(name, false, func(local string, root *os.Root) error {
		if root != nil {
			return root.MkdirAll(local, mode)
		}
		return b.source.MkdirAll(b.sourcePath(local), mode)
	})
	return wrapResolution("mkdir", name, err)
}

func (b *BasePathFs) Create(name string) (f File, err error) {
	err = b.execute(name, true, func(local string, root *os.Root) error {
		var sourcef File
		var basePath string
		if root != nil {
			basePath = root.Name()
			file, err := root.Create(local)
			if err != nil {
				return err
			}
			sourcef = file
		} else {
			basePath = b.path
			file, err := b.source.Create(b.sourcePath(local))
			if err != nil {
				return err
			}
			sourcef = file
		}
		f = &BasePathFile{File: sourcef, path: basePath}
		return nil
	})
	return f, wrapResolution("create", name, err)
}

func (b *BasePathFs) LstatIfPossible(name string) (os.FileInfo, bool, error) {
	var fi os.FileInfo
	var lstat bool
	err := b.execute(name, false, func(local string, root *os.Root) error {
		if root != nil {
			lstat = true
			var err error
			fi, err = root.Lstat(local)
			return err
		}
		if lstater, ok := b.source.(Lstater); ok {
			var err error
			fi, lstat, err = lstater.LstatIfPossible(b.sourcePath(local))
			return err
		}
		lstat = false
		var err error
		fi, err = b.source.Stat(b.sourcePath(local))
		return err
	})
	if err != nil {
		var rerr *pathResolutionError
		if errors.As(err, &rerr) {
			return nil, false, &os.PathError{Op: "lstat", Path: name, Err: rerr.err}
		}
		return fi, lstat, err
	}
	return fi, lstat, nil
}

func (b *BasePathFs) SymlinkIfPossible(oldname, newname string) error {
	oldLocal, err := b.localPath(oldname)
	if err != nil {
		return &os.LinkError{Op: "symlink", Old: oldname, New: newname, Err: pathErr(resolutionError(err))}
	}

	// The link target is stored the same way the historical implementation
	// stored it: the base path is prepended to the target name. Targets that
	// leave the base path are rejected above, so a link created through
	// BasePathFs can only ever point inside it.
	target := b.sourcePath(oldLocal)

	err = b.execute(newname, false, func(local string, root *os.Root) error {
		if root != nil {
			return root.Symlink(target, local)
		}
		if linker, ok := b.source.(Linker); ok {
			return linker.SymlinkIfPossible(target, b.sourcePath(local))
		}
		return ErrNoSymlink
	})
	if err != nil {
		if err == ErrNoSymlink {
			return &os.LinkError{Op: "symlink", Old: target, New: b.sourcePath(newname), Err: ErrNoSymlink}
		}
		return &os.LinkError{Op: "symlink", Old: target, New: b.sourcePath(newname), Err: pathErr(err)}
	}
	return nil
}

func (b *BasePathFs) ReadlinkIfPossible(name string) (string, error) {
	var target string
	err := b.execute(name, false, func(local string, root *os.Root) error {
		if root != nil {
			var err error
			target, err = root.Readlink(local)
			return err
		}
		if reader, ok := b.source.(LinkReader); ok {
			var err error
			target, err = reader.ReadlinkIfPossible(b.sourcePath(local))
			return err
		}
		return ErrNoReadlink
	})
	if err != nil {
		if err == ErrNoReadlink {
			return "", &os.PathError{Op: "readlink", Path: name, Err: ErrNoReadlink}
		}
		return "", &os.PathError{Op: "readlink", Path: name, Err: pathErr(err)}
	}
	return target, nil
}
