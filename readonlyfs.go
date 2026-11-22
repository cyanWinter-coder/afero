package afero

import (
	"os"
	"syscall"
	"time"
)

var (
	_ Lstater            = (*ReadOnlyFs)(nil)
	_ CapabilityReporter = (*ReadOnlyFs)(nil)
	_ DeniedErrorMapper  = (*ReadOnlyFs)(nil)
)

// ErrReadOnly is the single error ReadOnlyFs returns for every write
// operation. It is declared as syscall.EPERM so that both
// errors.Is(err, ErrReadOnly) and the historical err == syscall.EPERM
// comparisons hold.
var ErrReadOnly = syscall.EPERM

type ReadOnlyFs struct {
	source Fs
}

func NewReadOnlyFs(source Fs) Fs {
	return &ReadOnlyFs{source: source}
}

// Capabilities reports the read-only subset of the source's capabilities.
// Wrapping a source can never add operations back.
func (r *ReadOnlyFs) Capabilities() Operation {
	return CapabilityRead & CapabilitiesOf(r.source)
}

// DeniedError declares the error mapping of this wrapper: every operation
// it denies is reported as ErrReadOnly.
func (r *ReadOnlyFs) DeniedError(Operation) error {
	return ErrReadOnly
}

// readOnlyFile wraps a File obtained through a ReadOnlyFs and rejects every
// mutating call with ErrReadOnly without invoking the underlying file.
type readOnlyFile struct {
	File
}

func (f readOnlyFile) Write([]byte) (int, error) {
	return 0, ErrReadOnly
}

func (f readOnlyFile) WriteAt([]byte, int64) (int, error) {
	return 0, ErrReadOnly
}

func (f readOnlyFile) WriteString(string) (int, error) {
	return 0, ErrReadOnly
}

func (f readOnlyFile) Truncate(int64) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) ReadDir(name string) ([]os.FileInfo, error) {
	return ReadDir(r.source, name)
}

func (r *ReadOnlyFs) Chtimes(n string, a, m time.Time) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) Chmod(n string, m os.FileMode) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) Chown(n string, uid, gid int) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) Name() string {
	return "ReadOnlyFilter"
}

func (r *ReadOnlyFs) Stat(name string) (os.FileInfo, error) {
	return r.source.Stat(name)
}

func (r *ReadOnlyFs) LstatIfPossible(name string) (os.FileInfo, bool, error) {
	if lsf, ok := r.source.(Lstater); ok {
		return lsf.LstatIfPossible(name)
	}
	fi, err := r.Stat(name)
	return fi, false, err
}

func (r *ReadOnlyFs) SymlinkIfPossible(oldname, newname string) error {
	return &os.LinkError{Op: "symlink", Old: oldname, New: newname, Err: ErrNoSymlink}
}

func (r *ReadOnlyFs) ReadlinkIfPossible(name string) (string, error) {
	if srdr, ok := r.source.(LinkReader); ok {
		return srdr.ReadlinkIfPossible(name)
	}

	return "", &os.PathError{Op: "readlink", Path: name, Err: ErrNoReadlink}
}

func (r *ReadOnlyFs) Rename(o, n string) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) RemoveAll(p string) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) Remove(n string) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	if flag&(os.O_WRONLY|syscall.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, ErrReadOnly
	}
	f, err := r.source.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return readOnlyFile{f}, nil
}

func (r *ReadOnlyFs) Open(n string) (File, error) {
	f, err := r.source.Open(n)
	if err != nil {
		return nil, err
	}
	return readOnlyFile{f}, nil
}

func (r *ReadOnlyFs) Mkdir(n string, p os.FileMode) error {
	return ErrReadOnly
}

func (r *ReadOnlyFs) MkdirAll(n string, p os.FileMode) error {
	fi, err := r.source.Stat(n)
	if err == nil && fi.IsDir() {
		return nil
	}
	return ErrReadOnly
}

func (r *ReadOnlyFs) Create(n string) (File, error) {
	return nil, ErrReadOnly
}
