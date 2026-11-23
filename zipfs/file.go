package zipfs

import (
	"archive/zip"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/spf13/afero"
)

type File struct {
	fs        *Fs
	zipfile   *zip.File
	pseudodir *pseudoDir
	reader    io.ReadCloser
	offset    int64
	size      int64
	isdir     bool

	// name and info are snapshotted at Open time so the file keeps
	// describing the same archive entry after Close releases the
	// underlying zip reference.
	name string
	info os.FileInfo

	mu     sync.Mutex
	closed bool
	done   bool // the whole entry has been buffered and verified
	buf    []byte
}

func (f *File) fillBuffer(offset int64) (err error) {
	if offset > f.size {
		offset = f.size
		err = io.EOF
	}
	if len(f.buf) >= int(offset) {
		return
	}
	if f.done {
		// All available data was buffered; anything past it is EOF.
		return io.EOF
	}
	if f.reader == nil {
		reader, openErr := f.zipfile.Open()
		if openErr != nil {
			return &os.PathError{Op: "open", Path: f.name, Err: openErr}
		}
		f.reader = reader
	}
	buf := make([]byte, int(offset)-len(f.buf))
	if n, readErr := io.ReadFull(f.reader, buf); n > 0 {
		f.buf = append(f.buf, buf[:n]...)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			// ReadFull can report data and an error together (for
			// example a checksum failure on the final block): keep the
			// data but do not swallow the failure, and locate it at the
			// entry being read.
			return &os.PathError{Op: "read", Path: f.name, Err: readErr}
		}
	} else if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		// Keep EOF/ErrUnexpectedEOF unwrapped so callers still see the
		// standard end-of-file sentinels; locate every other failure
		// (e.g. checksum errors on corrupt entries) at this entry.
		err = &os.PathError{Op: "read", Path: f.name, Err: readErr}
	}

	// io.ReadFull turns the short final read into ErrUnexpectedEOF and
	// can therefore mask a checksum failure reported on that read. Once
	// the whole entry is buffered, drain one more byte and close the
	// underlying reader so deferred integrity checks run deterministically
	// while the entry is still identifiable.
	if len(f.buf) >= int(f.size) {
		if verifyErr := f.verifyAndClose(); verifyErr != nil {
			return verifyErr
		}
	}
	return
}

// verifyAndClose runs the end-of-entry integrity checks of the underlying
// zip reader and releases it. The entry contents are already fully buffered,
// so the reader is never needed again afterwards.
func (f *File) verifyAndClose() error {
	f.done = true
	if f.reader == nil {
		return nil
	}
	var probe [1]byte
	if _, err := f.reader.Read(probe[:]); err != nil && err != io.EOF {
		_ = f.reader.Close()
		f.reader = nil
		return &os.PathError{Op: "read", Path: f.name, Err: err}
	}
	if err := f.reader.Close(); err != nil {
		f.reader = nil
		return &os.PathError{Op: "close", Path: f.name, Err: err}
	}
	f.reader = nil
	return nil
}

// Close releases the per-Open zip reader and drops the reference to the
// underlying archive entry. Every File returned by Open owns an independent
// reader, so closing one file never affects another file opened on the same
// entry. Closing an already closed file returns [afero.ErrFileClosed].
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return afero.ErrFileClosed
	}
	f.closed = true

	var err error
	if f.reader != nil {
		// Release the per-Open reader, running any deferred integrity
		// check first. The failure is located at the entry so operators
		// can find the corrupt member; the handle is closed either way.
		err = f.verifyAndClose()
	}
	f.zipfile = nil
	f.buf = nil
	return err
}

func (f *File) Read(p []byte) (n int, err error) {
	if f.isdir {
		return 0, syscall.EISDIR
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, afero.ErrFileClosed
	}
	err = f.fillBuffer(f.offset + int64(len(p)))
	n = copy(p, f.buf[f.offset:])
	f.offset += int64(n)
	return
}

func (f *File) ReadAt(p []byte, off int64) (n int, err error) {
	if f.isdir {
		return 0, syscall.EISDIR
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, afero.ErrFileClosed
	}
	err = f.fillBuffer(off + int64(len(p)))
	n = copy(p, f.buf[int(off):])
	return
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	if f.isdir {
		return 0, syscall.EISDIR
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, afero.ErrFileClosed
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.offset
	case io.SeekEnd:
		offset += f.size
	default:
		return 0, syscall.EINVAL
	}
	if offset < 0 || offset > f.size {
		return 0, afero.ErrOutOfRange
	}
	f.offset = offset
	return offset, nil
}

func (f *File) Write(p []byte) (n int, err error) { return 0, syscall.EPERM }

func (f *File) WriteAt(p []byte, off int64) (n int, err error) { return 0, syscall.EPERM }

// Name returns the archive path of this entry. It remains available after
// Close, mirroring os.File.
func (f *File) Name() string {
	return f.name
}

func (f *File) getDirEntries() (map[string]*zip.File, error) {
	if !f.isdir {
		return nil, syscall.ENOTDIR
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, afero.ErrFileClosed
	}
	f.mu.Unlock()
	name := f.Name()
	entries, ok := f.fs.files[name]
	if !ok {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: syscall.ENOENT}
	}
	return entries, nil
}

func (f *File) Readdir(count int) (fi []os.FileInfo, err error) {
	zipfiles, err := f.getDirEntries()
	if err != nil {
		return nil, err
	}
	for d, zipfile := range zipfiles {
		if zipfile == nil {
			fi = append(fi, pseudoDir{path: d})
		} else {
			fi = append(fi, zipfile.FileInfo())
		}
		if count > 0 && len(fi) >= count {
			break
		}
	}
	return
}

func (f *File) Readdirnames(count int) (names []string, err error) {
	zipfiles, err := f.getDirEntries()
	if err != nil {
		return nil, err
	}
	for filename := range zipfiles {
		names = append(names, filename)
		if count > 0 && len(names) >= count {
			break
		}
	}
	return
}

// Stat returns the entry metadata snapshotted at Open time, so it always
// reflects the same archive snapshot the file was opened against.
func (f *File) Stat() (os.FileInfo, error) {
	if f.info != nil {
		return f.info, nil
	}
	if f.pseudodir != nil {
		return f.pseudodir, nil
	}
	return &pseudoRoot{}, nil
}

func (f *File) Sync() error { return nil }

func (f *File) Truncate(size int64) error { return syscall.EPERM }

func (f *File) WriteString(s string) (ret int, err error) { return 0, syscall.EPERM }
