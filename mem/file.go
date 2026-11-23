// Copyright © 2015 Steve Francia <spf@spf13.com>.
// Copyright 2013 tsuru authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mem

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/afero/internal/common"
)

const FilePathSeparator = string(filepath.Separator)

var _ fs.ReadDirFile = &File{}

// nextInodeID hands out immutable inode identities. A rename changes the
// path of a FileData but never its id, so open handles can keep referring to
// the same inode after it has moved or been unlinked.
var nextInodeID atomic.Uint64

type File struct {
	// atomic requires 64-bit alignment for struct field access
	at           int64
	readDirCount int64
	closed       bool
	readOnly     bool
	dirBuf       []*FileData
	fileData     *FileData

	// Identity and structural version frozen when the handle was opened.
	// inodeID never changes for the underlying inode; openedVersion is the
	// namespace generation of the open and is stamped by the owning
	// filesystem via WithOpenVersion (zero for bare mem-package handles);
	// openedName is the path the handle was created at, matching os.File.
	inodeID       uint64
	openedVersion uint64
	openedName    string
}

// NewFileHandle mints a read/write handle over data and freezes the inode
// identity and opening path on the handle.
func NewFileHandle(data *FileData) *File {
	return newHandle(data, false)
}

func NewReadOnlyFileHandle(data *FileData) *File {
	return newHandle(data, true)
}

func newHandle(data *FileData, readOnly bool) *File {
	data.Lock()
	name := data.name
	id := data.id
	data.Unlock()
	return &File{
		fileData:   data,
		readOnly:   readOnly,
		inodeID:    id,
		openedName: name,
	}
}

// WithOpenVersion returns a copy of the handle carrying the filesystem's
// current structural version. MemMapFs calls this after every successful
// open so each handle records the namespace generation it belongs to; the
// handle offset and read/close state are unaffected.
func (f *File) WithOpenVersion(version uint64) *File {
	f.openedVersion = version
	return f
}

// InodeID reports the immutable inode identity captured at open time.
func (f *File) InodeID() uint64 { return f.inodeID }

// OpenVersion reports the namespace structural version captured at open time.
func (f *File) OpenVersion() uint64 { return f.openedVersion }

// OpenedName reports the path the handle was opened with. Unlike the
// underlying FileData, it does not change when the inode is renamed.
func (f *File) OpenedName() string { return f.openedName }

func (f File) Data() *FileData {
	return f.fileData
}

type FileData struct {
	sync.Mutex
	// id is the immutable inode identity, assigned on creation and preserved
	// across rename and unlink while any handle still references the data.
	id      uint64
	name    string
	data    []byte
	memDir  Dir
	dir     bool
	mode    os.FileMode
	modtime time.Time
	uid     int
	gid     int
}

// InodeID returns the immutable identity of the inode described by d.
func (d *FileData) InodeID() uint64 {
	d.Lock()
	defer d.Unlock()
	return d.id
}

func (d *FileData) Name() string {
	d.Lock()
	defer d.Unlock()
	return d.name
}

func CreateFile(name string) *FileData {
	return &FileData{
		id:      nextInodeID.Add(1),
		name:    name,
		mode:    os.ModeTemporary,
		modtime: time.Now(),
	}
}

func CreateDir(name string) *FileData {
	return &FileData{
		id:      nextInodeID.Add(1),
		name:    name,
		memDir:  &DirMap{},
		dir:     true,
		modtime: time.Now(),
	}
}

func ChangeFileName(f *FileData, newname string) {
	f.Lock()
	f.name = newname
	f.Unlock()
}

func SetMode(f *FileData, mode os.FileMode) {
	f.Lock()
	f.mode = mode
	f.Unlock()
}

func SetModTime(f *FileData, mtime time.Time) {
	f.Lock()
	setModTime(f, mtime)
	f.Unlock()
}

func setModTime(f *FileData, mtime time.Time) {
	f.modtime = mtime
}

func SetUID(f *FileData, uid int) {
	f.Lock()
	f.uid = uid
	f.Unlock()
}

func SetGID(f *FileData, gid int) {
	f.Lock()
	f.gid = gid
	f.Unlock()
}

func GetFileInfo(f *FileData) *FileInfo {
	return &FileInfo{f}
}

func (f *File) Open() error {
	atomic.StoreInt64(&f.at, 0)
	atomic.StoreInt64(&f.readDirCount, 0)
	f.fileData.Lock()
	f.dirBuf = nil
	f.closed = false
	// Re-opening re-freezes the handle against the inode's current path.
	f.openedName = f.fileData.name
	f.inodeID = f.fileData.id
	f.fileData.Unlock()
	return nil
}

func (f *File) Close() error {
	f.fileData.Lock()
	defer f.fileData.Unlock()
	if f.closed {
		return ErrFileClosed
	}
	f.closed = true
	if !f.readOnly {
		setModTime(f.fileData, time.Now())
	}
	return nil
}

// Name returns the path the handle was opened with. Renaming the inode does
// not change an already-open handle's name, matching os.File.
func (f *File) Name() string {
	return f.openedName
}

func (f *File) Stat() (os.FileInfo, error) {
	return &FileInfo{f.fileData}, nil
}

func (f *File) Sync() error {
	return nil
}

func (f *File) Readdir(count int) (res []os.FileInfo, err error) {
	if !f.fileData.dir {
		return nil, &os.PathError{
			Op:   "readdir",
			Path: f.openedName,
			Err:  errors.New("not a dir"),
		}
	}
	var outLength int64

	f.fileData.Lock()
	// The first call snapshots the directory table copy-on-write; every
	// subsequent paginated call on this handle walks the same immutable
	// snapshot, so entries cannot appear twice or vanish mid-walk even if
	// the directory is renamed or its children are removed concurrently.
	if f.dirBuf == nil {
		f.dirBuf = f.fileData.memDir.Files()
	}
	files := f.dirBuf[atomic.LoadInt64(&f.readDirCount):]
	if count > 0 {
		if len(files) < count {
			outLength = int64(len(files))
		} else {
			outLength = int64(count)
		}
		if len(files) == 0 {
			err = io.EOF
		}
	} else {
		outLength = int64(len(files))
	}
	atomic.AddInt64(&f.readDirCount, outLength)
	f.fileData.Unlock()

	res = make([]os.FileInfo, outLength)
	for i := range res {
		res[i] = &FileInfo{files[i]}
	}

	return res, err
}

func (f *File) Readdirnames(n int) (names []string, err error) {
	fi, err := f.Readdir(n)
	names = make([]string, len(fi))
	for i, f := range fi {
		_, names[i] = filepath.Split(f.Name())
	}
	return names, err
}

// Implements fs.ReadDirFile
func (f *File) ReadDir(n int) ([]fs.DirEntry, error) {
	fi, err := f.Readdir(n)
	if err != nil {
		return nil, err
	}
	di := make([]fs.DirEntry, len(fi))
	for i, f := range fi {
		di[i] = common.FileInfoDirEntry{FileInfo: f}
	}
	return di, nil
}

func (f *File) ReadAt(b []byte, off int64) (n int, err error) {
	f.fileData.Lock()
	defer f.fileData.Unlock()
	if f.closed {
		return 0, ErrFileClosed
	}
	if len(b) > 0 && int(off) == len(f.fileData.data) {
		return 0, io.EOF
	}
	if int(off) > len(f.fileData.data) {
		return 0, io.ErrUnexpectedEOF
	}
	if len(f.fileData.data)-int(off) >= len(b) {
		n = len(b)
	} else {
		n = len(f.fileData.data) - int(off)
	}
	copy(b, f.fileData.data[off:off+int64(n)])
	if n < len(b) {
		err = io.EOF
	}
	return
}

func (f *File) Read(b []byte) (n int, err error) {
	n, err = f.ReadAt(b, atomic.LoadInt64(&f.at))
	atomic.AddInt64(&f.at, int64(n))
	if n > 0 && err == io.EOF {
		err = nil
	}
	return
}

func (f *File) Truncate(size int64) error {
	if f.closed {
		return ErrFileClosed
	}
	if f.readOnly {
		return &os.PathError{
			Op:   "truncate",
			Path: f.openedName,
			Err:  errors.New("file handle is read only"),
		}
	}
	if size < 0 {
		return ErrOutOfRange
	}
	f.fileData.Lock()
	defer f.fileData.Unlock()
	if size > int64(len(f.fileData.data)) {
		diff := size - int64(len(f.fileData.data))
		f.fileData.data = append(f.fileData.data, bytes.Repeat([]byte{0o0}, int(diff))...)
	} else {
		f.fileData.data = f.fileData.data[0:size]
	}
	setModTime(f.fileData, time.Now())
	return nil
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, ErrFileClosed
	}
	switch whence {
	case io.SeekStart:
		atomic.StoreInt64(&f.at, offset)
	case io.SeekCurrent:
		atomic.AddInt64(&f.at, offset)
	case io.SeekEnd:
		f.fileData.Lock()
		size := int64(len(f.fileData.data))
		f.fileData.Unlock()
		atomic.StoreInt64(&f.at, size+offset)
	}
	return f.at, nil
}

func (f *File) WriteAt(b []byte, off int64) (n int, err error) {
	if f.closed {
		return 0, ErrFileClosed
	}
	if f.readOnly {
		return 0, &os.PathError{
			Op:   "write",
			Path: f.openedName,
			Err:  errors.New("file handle is read only"),
		}
	}
	n = len(b)
	if n == 0 {
		return 0, nil
	}
	f.fileData.Lock()
	defer f.fileData.Unlock()
	diff := off - int64(len(f.fileData.data))
	var tail []byte
	if n+int(off) < len(f.fileData.data) {
		tail = f.fileData.data[n+int(off):]
	}
	if diff > 0 {
		f.fileData.data = append(
			f.fileData.data,
			append(bytes.Repeat([]byte{0o0}, int(diff)), b...)...)
		f.fileData.data = append(f.fileData.data, tail...)
	} else {
		f.fileData.data = append(f.fileData.data[:off], b...)
		f.fileData.data = append(f.fileData.data, tail...)
	}
	setModTime(f.fileData, time.Now())
	return
}

func (f *File) Write(b []byte) (n int, err error) {
	n, err = f.WriteAt(b, atomic.LoadInt64(&f.at))
	atomic.AddInt64(&f.at, int64(n))
	return
}

func (f *File) WriteString(s string) (ret int, err error) {
	return f.Write([]byte(s))
}

func (f *File) Info() *FileInfo {
	return &FileInfo{f.fileData}
}

type FileInfo struct {
	*FileData
}

// Implements os.FileInfo
func (s *FileInfo) Name() string {
	s.Lock()
	_, name := filepath.Split(s.name)
	s.Unlock()
	return name
}

func (s *FileInfo) Mode() os.FileMode {
	s.Lock()
	defer s.Unlock()
	return s.mode
}

func (s *FileInfo) ModTime() time.Time {
	s.Lock()
	defer s.Unlock()
	return s.modtime
}

func (s *FileInfo) IsDir() bool {
	s.Lock()
	defer s.Unlock()
	return s.dir
}

func (s *FileInfo) Size() int64 {
	if s.IsDir() {
		return int64(42)
	}
	s.Lock()
	defer s.Unlock()
	return int64(len(s.data))
}

func (s *FileInfo) Sys() interface{} {
	s.Lock()
	defer s.Unlock()
	return sysFromFileInfo(s)
}

var (
	ErrFileClosed        = errors.New("File is closed")
	ErrOutOfRange        = errors.New("out of range")
	ErrTooLarge          = errors.New("too large")
	ErrFileNotFound      = os.ErrNotExist
	ErrFileExists        = os.ErrExist
	ErrDestinationExists = os.ErrExist
)
