package afero

import (
	"errors"
	"os"
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"
)

// trackedFs records every mutating call and wraps every opened file so
// tests can verify that a wrapper denied an operation without consulting
// its source, and that intermediate handles get released.
type trackedFs struct {
	Fs

	mu    sync.Mutex
	ops   []string
	files []*trackedFile
}

type trackedFile struct {
	File
	writes int
	closed bool
}

func (f *trackedFile) Write(p []byte) (int, error) {
	f.writes++
	return f.File.Write(p)
}

func (f *trackedFile) WriteAt(p []byte, off int64) (int, error) {
	f.writes++
	return f.File.WriteAt(p, off)
}

func (f *trackedFile) WriteString(s string) (int, error) {
	f.writes++
	return f.File.WriteString(s)
}

func (f *trackedFile) Truncate(size int64) error {
	f.writes++
	return f.File.Truncate(size)
}

func (f *trackedFile) Close() error {
	f.closed = true
	return f.File.Close()
}

func (t *trackedFs) note(op string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ops = append(t.ops, op)
}

func (t *trackedFs) calls(op string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, o := range t.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (t *trackedFs) wrap(f File, err error) (File, error) {
	if err != nil {
		return nil, err
	}
	tf := &trackedFile{File: f}
	t.mu.Lock()
	t.files = append(t.files, tf)
	t.mu.Unlock()
	return tf, nil
}

func (t *trackedFs) Create(name string) (File, error) {
	t.note("Create")
	return t.wrap(t.Fs.Create(name))
}

func (t *trackedFs) Mkdir(name string, perm os.FileMode) error {
	t.note("Mkdir")
	return t.Fs.Mkdir(name, perm)
}

func (t *trackedFs) MkdirAll(name string, perm os.FileMode) error {
	t.note("MkdirAll")
	return t.Fs.MkdirAll(name, perm)
}

func (t *trackedFs) Open(name string) (File, error) {
	t.note("Open")
	return t.wrap(t.Fs.Open(name))
}

func (t *trackedFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	t.note("OpenFile")
	return t.wrap(t.Fs.OpenFile(name, flag, perm))
}

func (t *trackedFs) Remove(name string) error {
	t.note("Remove")
	return t.Fs.Remove(name)
}

func (t *trackedFs) RemoveAll(name string) error {
	t.note("RemoveAll")
	return t.Fs.RemoveAll(name)
}

func (t *trackedFs) Rename(oldname, newname string) error {
	t.note("Rename")
	return t.Fs.Rename(oldname, newname)
}

func (t *trackedFs) Chmod(name string, mode os.FileMode) error {
	t.note("Chmod")
	return t.Fs.Chmod(name, mode)
}

func (t *trackedFs) Chown(name string, uid, gid int) error {
	t.note("Chown")
	return t.Fs.Chown(name, uid, gid)
}

func (t *trackedFs) Chtimes(name string, atime, mtime time.Time) error {
	t.note("Chtimes")
	return t.Fs.Chtimes(name, atime, mtime)
}

// Requirement 1: every wrapper declares its capabilities, and composing
// wrappers intersects them instead of silently elevating them.
func TestCapabilitiesComposition(t *testing.T) {
	mem := NewMemMapFs()
	if got := CapabilitiesOf(mem); got != CapabilityAll {
		t.Errorf("CapabilitiesOf(MemMapFs) = %v, want CapabilityAll", got)
	}

	ro := NewReadOnlyFs(mem)
	if got := CapabilitiesOf(ro); got != CapabilityRead {
		t.Errorf("CapabilitiesOf(ReadOnlyFs) = %v, want CapabilityRead", got)
	}

	re := NewRegexpFs(mem, regexp.MustCompile(`\.txt$`))
	if got := CapabilitiesOf(re); got != CapabilityAll {
		t.Errorf("CapabilitiesOf(RegexpFs) = %v, want CapabilityAll", got)
	}

	// A name filter over a read-only source must not regain write
	// capabilities, regardless of wrapping order.
	for name, fs := range map[string]Fs{
		"regexp over readonly":   NewRegexpFs(ro, regexp.MustCompile(`\.txt$`)),
		"readonly over regexp":   NewReadOnlyFs(re),
		"basepath over readonly": NewBasePathFs(ro, "/"),
		"copyonwrite ro base":    NewCopyOnWriteFs(ro, NewMemMapFs()),
		"cacheonread ro base":    NewCacheOnReadFs(ro, NewMemMapFs(), 0),
	} {
		if got := CapabilitiesOf(fs); got != CapabilityRead {
			t.Errorf("CapabilitiesOf(%s) = %v, want CapabilityRead", name, got)
		}
		if Supports(fs, OpCreate) || Supports(fs, OpOpenWrite) || Supports(fs, OpRemove) {
			t.Errorf("Supports(%s, write op) = true, want false", name)
		}
		if !Supports(fs, OpOpen) || !Supports(fs, OpStat) {
			t.Errorf("Supports(%s, read op) = false, want true", name)
		}
	}
}

// Requirement 1: wrappers declare the error callers observe for operations
// the wrapper itself denies.
func TestDeniedErrorMapping(t *testing.T) {
	ro, ok := NewReadOnlyFs(NewMemMapFs()).(DeniedErrorMapper)
	if !ok {
		t.Fatal("ReadOnlyFs does not implement DeniedErrorMapper")
	}
	if err := ro.DeniedError(OpCreate); err != ErrReadOnly || !errors.Is(err, syscall.EPERM) {
		t.Errorf("ReadOnlyFs.DeniedError = %v, want ErrReadOnly", err)
	}

	re, ok := NewRegexpFs(NewMemMapFs(), nil).(DeniedErrorMapper)
	if !ok {
		t.Fatal("RegexpFs does not implement DeniedErrorMapper")
	}
	if err := re.DeniedError(OpOpen); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("RegexpFs.DeniedError = %v, want ENOENT", err)
	}
}

// Requirement 2: the read-only wrapper answers every write entry point
// with the unified error and never invokes the source.
func TestReadOnlyFsUnifiedErrorSourceNotCalled(t *testing.T) {
	tracked := &trackedFs{Fs: NewMemMapFs()}
	if err := WriteFile(tracked, "/file.txt", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := NewReadOnlyFs(tracked)

	mutatingOps := []string{
		"Create", "Mkdir", "MkdirAll", "OpenFile", "Remove",
		"RemoveAll", "Rename", "Chmod", "Chown", "Chtimes",
	}
	before := map[string]int{}
	for _, op := range mutatingOps {
		before[op] = tracked.calls(op)
	}

	now := time.Now()
	writeEntries := map[string]func() error{
		"Create":    func() error { _, err := ro.Create("/new"); return err },
		"Mkdir":     func() error { return ro.Mkdir("/newdir", 0o755) },
		"MkdirAll":  func() error { return ro.MkdirAll("/newdir/sub", 0o755) },
		"Remove":    func() error { return ro.Remove("/file.txt") },
		"RemoveAll": func() error { return ro.RemoveAll("/file.txt") },
		"Rename":    func() error { return ro.Rename("/file.txt", "/renamed") },
		"Chmod":     func() error { return ro.Chmod("/file.txt", 0o644) },
		"Chown":     func() error { return ro.Chown("/file.txt", 1, 1) },
		"Chtimes":   func() error { return ro.Chtimes("/file.txt", now, now) },
		"OpenFile O_WRONLY": func() error {
			_, err := ro.OpenFile("/file.txt", os.O_WRONLY, 0)
			return err
		},
		"OpenFile O_RDWR": func() error {
			_, err := ro.OpenFile("/file.txt", os.O_RDWR, 0)
			return err
		},
		"OpenFile O_APPEND": func() error {
			_, err := ro.OpenFile("/file.txt", os.O_WRONLY|os.O_APPEND, 0)
			return err
		},
		"OpenFile O_CREATE": func() error {
			_, err := ro.OpenFile("/new", os.O_WRONLY|os.O_CREATE, 0o600)
			return err
		},
		"OpenFile O_TRUNC": func() error {
			_, err := ro.OpenFile("/file.txt", os.O_WRONLY|os.O_TRUNC, 0)
			return err
		},
	}

	for name, call := range writeEntries {
		err := call()
		if err != ErrReadOnly {
			t.Errorf("%s: error = %v, want ErrReadOnly", name, err)
		}
		if !errors.Is(err, syscall.EPERM) {
			t.Errorf("%s: errors.Is(%v, syscall.EPERM) = false", name, err)
		}
	}

	for _, op := range mutatingOps {
		if got := tracked.calls(op); got != before[op] {
			t.Errorf("source %s called %d times by denied operations, want %d",
				op, got, before[op])
		}
	}
}

// Requirement 2: write methods on a file handle obtained through the
// read-only wrapper fail with the unified error without touching the
// underlying file.
func TestReadOnlyFsFileHandleWritesDenied(t *testing.T) {
	tracked := &trackedFs{Fs: NewMemMapFs()}
	if err := WriteFile(tracked, "/file.txt", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := NewReadOnlyFs(tracked)
	setupFiles := len(tracked.files)

	openers := map[string]func() (File, error){
		"Open": func() (File, error) { return ro.Open("/file.txt") },
		"OpenFile O_RDONLY": func() (File, error) {
			return ro.OpenFile("/file.txt", os.O_RDONLY, 0)
		},
	}
	for name, openFile := range openers {
		f, err := openFile()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("x")); err != ErrReadOnly {
			t.Errorf("%s: Write = %v, want ErrReadOnly", name, err)
		}
		if _, err := f.WriteAt([]byte("x"), 0); err != ErrReadOnly {
			t.Errorf("%s: WriteAt = %v, want ErrReadOnly", name, err)
		}
		if _, err := f.WriteString("x"); err != ErrReadOnly {
			t.Errorf("%s: WriteString = %v, want ErrReadOnly", name, err)
		}
		if err := f.Truncate(0); err != ErrReadOnly {
			t.Errorf("%s: Truncate = %v, want ErrReadOnly", name, err)
		}
		// Reads still work.
		buf := make([]byte, 4)
		if n, err := f.Read(buf); n != 4 || err != nil || string(buf) != "data" {
			t.Errorf("%s: Read = %q, %v; want %q, nil", name, buf, err, "data")
		}
		f.Close()
	}

	for _, tf := range tracked.files[setupFiles:] {
		if tf.writes != 0 {
			t.Errorf("underlying file received %d write calls, want 0", tf.writes)
		}
	}

	data, err := ReadFile(tracked, "/file.txt")
	if err != nil || string(data) != "data" {
		t.Errorf("file contents = %q, %v; want %q, nil", data, err, "data")
	}
}

// Requirement 3: path strings and os.FileInfo values go through the same
// normalization, so a name is filtered identically whether it comes from a
// path argument or a directory listing.
func TestRegexpFsConsistentNormalization(t *testing.T) {
	source := NewMemMapFs()
	for _, name := range []string{"/d/keep.txt", "/d/drop.md"} {
		if err := WriteFile(source, name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fs := NewRegexpFs(source, regexp.MustCompile(`\.txt$`))

	// Path-based checks, including paths that need cleaning first.
	for _, path := range []string{"/d/keep.txt", "/d/./keep.txt", "/d/sub/../keep.txt"} {
		if _, err := fs.Stat(path); err != nil {
			t.Errorf("Stat(%q) = %v, want nil", path, err)
		}
	}
	for _, path := range []string{"/d/drop.md", "/d/./drop.md", "/d/sub/../drop.md"} {
		if _, err := fs.Stat(path); !errors.Is(err, syscall.ENOENT) {
			t.Errorf("Stat(%q) = %v, want ENOENT", path, err)
		}
	}

	// The directory listing must agree with the path-based checks.
	f, err := fs.Open("/d")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fis, err := f.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(fis) != 1 || fis[0].Name() != "keep.txt" {
		names := []string{}
		for _, fi := range fis {
			names = append(names, fi.Name())
		}
		t.Errorf("Readdir = %v, want [keep.txt]", names)
	}
}

// Requirement 3 (regression): a nil regexp disables the filter everywhere,
// including directory listings.
func TestRegexpFsNilRegexpReaddir(t *testing.T) {
	source := NewMemMapFs()
	if err := WriteFile(source, "/d/anything.md", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := NewRegexpFs(source, nil)

	f, err := fs.Open("/d")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fis, err := f.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(fis) != 1 || fis[0].Name() != "anything.md" {
		t.Errorf("Readdir = %v, want [anything.md]", fis)
	}
}

// Renaming a directory passes the name filter and must actually rename it
// instead of silently reporting success.
func TestRegexpFsRenameDirectory(t *testing.T) {
	source := NewMemMapFs()
	if err := WriteFile(source, "/old/file.txt", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := NewRegexpFs(source, regexp.MustCompile(`\.txt$`))

	if err := fs.Rename("/old", "/new"); err != nil {
		t.Fatalf("Rename = %v, want nil", err)
	}
	if ok, _ := DirExists(source, "/new"); !ok {
		t.Error("directory was not renamed in the source")
	}
	if ok, _ := DirExists(source, "/old"); ok {
		t.Error("old directory still present in the source")
	}
}

// Requirement 4: errors stay identifiable through the wrapper chain.
func TestWrapperChainErrorIdentifiable(t *testing.T) {
	source := NewMemMapFs()
	if err := WriteFile(source, "/file.txt", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Filter denial stays a "not exist" error through a read-only layer.
	chain := NewReadOnlyFs(NewRegexpFs(source, regexp.MustCompile(`\.yaml$`)))
	if _, err := chain.Stat("/file.txt"); !os.IsNotExist(err) {
		t.Errorf("Stat through chain = %v, want not-exist", err)
	}

	// Read-only denial stays EPERM through a name filter.
	chain = NewRegexpFs(NewReadOnlyFs(source), regexp.MustCompile(`\.txt$`))
	if err := chain.Remove("/file.txt"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Remove through chain = %v, want ErrReadOnly", err)
	}

	// A missing file on Open keeps the source's not-exist error.
	if _, err := NewReadOnlyFs(source).Open("/missing"); !os.IsNotExist(err) {
		t.Errorf("Open missing = %v, want not-exist", err)
	}
}

// Requirement 4: when a union Open fails on one side, the handle already
// opened on the other side is released and the original error stays
// identifiable.
func TestCopyOnWriteFsOpenReleasesHandleOnError(t *testing.T) {
	openErr := errors.New("open failed")

	for _, tt := range []struct {
		name string
		// failBase reports which side's Open fails; the other side is
		// tracked so the test can check its handle was closed.
		failBase bool
	}{
		{name: "layer fails", failBase: false},
		{name: "base fails", failBase: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, layer := NewMemMapFs(), NewMemMapFs()
			for _, fs := range []Fs{base, layer} {
				if err := fs.MkdirAll("/dir", 0o755); err != nil {
					t.Fatal(err)
				}
			}

			tracked := &trackedFs{Fs: NewMemMapFs()}
			if err := tracked.MkdirAll("/dir", 0o755); err != nil {
				t.Fatal(err)
			}
			failing := &openErrorFs{Fs: NewMemMapFs(), err: openErr}
			if err := failing.MkdirAll("/dir", 0o755); err != nil {
				t.Fatal(err)
			}

			var ufs Fs
			if tt.failBase {
				ufs = NewCopyOnWriteFs(failing, tracked)
			} else {
				ufs = NewCopyOnWriteFs(tracked, failing)
			}

			f, err := ufs.Open("/dir")
			if f != nil {
				f.Close()
			}
			if !errors.Is(err, openErr) {
				t.Fatalf("Open error = %v, want wrapped %v", err, openErr)
			}
			if len(tracked.files) != 1 {
				t.Fatalf("tracked side opened %d files, want 1", len(tracked.files))
			}
			if !tracked.files[0].closed {
				t.Error("intermediate handle was not released on failed Open")
			}
		})
	}
}

type openErrorFs struct {
	Fs
	err error
}

func (f *openErrorFs) Open(string) (File, error) {
	return nil, f.err
}
