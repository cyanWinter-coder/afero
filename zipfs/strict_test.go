package zipfs

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/spf13/afero"
	"github.com/spf13/afero/internal/archivefs"
)

// buildZip writes the given entries into an in-memory zip archive and
// returns a reader over it. All data is untrusted test input and never
// touches the host file system.
func buildZip(t *testing.T, entries map[string]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range entries {
		hdr := &zip.FileHeader{Name: name, Method: zip.Store}
		f, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		if _, err := io.WriteString(f, content); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	return r
}

func TestNewStrictRejectsUnsafeNames(t *testing.T) {
	bad := []struct {
		name string
		want error
	}{
		{"/absolute/path", archivefs.ErrAbsolutePath},
		{"C:/evil", archivefs.ErrAbsolutePath},
		{`\\server\share`, archivefs.ErrAbsolutePath},
		{"../escape", archivefs.ErrRootEscape},
		{"a/../../escape", archivefs.ErrRootEscape},
		{strings.Repeat("a", archivefs.MaxEntryName+1), archivefs.ErrNameTooLong},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			r := buildZip(t, map[string]string{tc.name: "x"})
			fs, err := NewStrict(r)
			if err == nil {
				t.Fatalf("NewStrict(%q): want error, got nil", tc.name)
			}
			if fs != nil {
				t.Fatalf("NewStrict must return a nil Fs on indexing failure, got %#v", fs)
			}
			var ee *archivefs.EntryError
			if !errors.As(err, &ee) {
				t.Fatalf("error %v does not wrap archivefs.EntryError", err)
			}
			if ee.Entry != tc.name {
				t.Errorf("EntryError.Entry = %q, want %q", ee.Entry, tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error %v does not wrap %v", err, tc.want)
			}
		})
	}
}

func TestNewStrictRejectsDuplicatePaths(t *testing.T) {
	// "a//b" and "./a/b" both clean to "a/b".
	r := buildZip(t, map[string]string{
		"a/b":  "first",
		"a//b": "second",
	})
	fs, err := NewStrict(r)
	if err == nil {
		t.Fatal("expected duplicate path error")
	}
	if fs != nil {
		t.Fatal("partial index must not be returned")
	}
	var dup *archivefs.DuplicateEntryError
	if !errors.As(err, &dup) {
		t.Fatalf("error %v does not wrap DuplicateEntryError", err)
	}
	if dup.Path != "a/b" || dup.Entry != "a//b" {
		t.Errorf("got duplicate report entry=%q path=%q", dup.Entry, dup.Path)
	}
}

func TestNewKeepsLegacyBehavior(t *testing.T) {
	// New must keep its original signature and never fail its caller;
	// an un-indexable archive becomes an empty read-only file system.
	r := buildZip(t, map[string]string{"../escape": "x"})
	fs := New(r)
	if fs == nil {
		t.Fatal("legacy New must not return nil")
	}
	if _, err := fs.Stat("/"); err != nil {
		t.Errorf("root of empty fs should stat: %v", err)
	}
	if _, err := fs.Stat("../escape"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unsafe entry must not be indexed, stat err = %v", err)
	}
	if _, err := fs.Create("x"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("archive fs stays read-only, Create err = %v", err)
	}
}

func TestIndependentReadersAndClose(t *testing.T) {
	r := buildZip(t, map[string]string{"f": "hello world"})
	fs := New(r)

	a, err := fs.Open("f")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.Open("f")
	if err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 5)
	if _, err := io.ReadFull(a, got); err != nil {
		t.Fatalf("read a: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("a = %q", got)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	if err := a.Close(); !errors.Is(err, afero.ErrFileClosed) {
		t.Errorf("double close: got %v, want ErrFileClosed", err)
	}
	if _, err := a.Read(got); !errors.Is(err, afero.ErrFileClosed) {
		t.Errorf("read after close: got %v", err)
	}

	// b owns an independent reader and must be unaffected.
	got = make([]byte, 5)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("read b after closing a: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("b = %q", got)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
}

func TestCloseReleasesReferenceButKeepsSnapshot(t *testing.T) {
	r := buildZip(t, map[string]string{"dir/f": "data"})
	fs := New(r)

	f, err := fs.Open("dir/f")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if f.Name() != name {
		t.Errorf("Name after close = %q, want %q", f.Name(), name)
	}
	statAfter, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat after close: %v", err)
	}
	if statAfter.Name() != info.Name() || statAfter.Size() != info.Size() {
		t.Errorf("Stat after close diverged: %v vs %v", statAfter, info)
	}

	d, err := fs.Open("dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Readdir(-1); !errors.Is(err, afero.ErrFileClosed) {
		t.Errorf("Readdir after close: got %v, want ErrFileClosed", err)
	}
}

func TestConcurrentStatOpenAndCloseSeeSameSnapshot(t *testing.T) {
	r := buildZip(t, map[string]string{
		"a":   strings.Repeat("a", 4096),
		"b/c": strings.Repeat("b", 4096),
		"d/e": strings.Repeat("c", 4096),
	})
	fs := New(r)

	const goroutines = 16
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				for _, p := range []string{"/", "a", "b", "b/c", "d", "d/e", "missing"} {
					if _, err := fs.Stat(p); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Errorf("Stat %q: %v", p, err)
						return
					}
					f, err := fs.Open(p)
					if err != nil {
						if !errors.Is(err, os.ErrNotExist) {
							t.Errorf("Open %q: %v", p, err)
							return
						}
						continue
					}
					if !strings.HasSuffix(p, "/") {
						buf := make([]byte, 16)
						_, _ = io.ReadFull(f, buf)
					}
					if _, err := f.Readdir(-1); err != nil &&
						!errors.Is(err, syscall.ENOTDIR) &&
						!errors.Is(err, afero.ErrFileClosed) {
						t.Errorf("Readdir %q: %v", p, err)
						return
					}
					if err := f.Close(); err != nil {
						t.Errorf("Close %q: %v", p, err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestCorruptEntryErrorIsLocated(t *testing.T) {
	// Build a zip with Stored data, then corrupt the payload bytes of one
	// entry. The index must still load, but reading the damaged entry has
	// to fail with an error naming that entry; sibling entries stay usable.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	mark := strings.Repeat("Z", 64)
	writeStored(t, w, "corrupt", mark)
	writeStored(t, w, "healthy", "fine")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	idx := bytes.Index(raw, []byte(mark))
	if idx < 0 {
		t.Fatal("stored payload not found in archive")
	}
	raw[idx] = 'Q'

	r, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	fs, err := NewStrict(r)
	if err != nil {
		t.Fatalf("indexing a structurally valid zip must succeed: %v", err)
	}
	if fs == nil {
		t.Fatal("a structurally valid zip with damaged data must index")
	}

	f, err := fs.Open("corrupt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(f)
	if err == nil {
		t.Fatal("expected checksum error on corrupt entry")
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not an os.PathError locating the entry", err)
	}
	if pe.Path != filepath.ToSlash(f.Name()) {
		t.Errorf("PathError.Path = %q, want %q", pe.Path, f.Name())
	}

	healthy, err := fs.Open("healthy")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(healthy)
	if err != nil || string(data) != "fine" {
		t.Fatalf("sibling entry broken: %q, %v", data, err)
	}
}

func writeStored(t *testing.T, w *zip.Writer, name, content string) {
	t.Helper()
	hdr := &zip.FileHeader{Name: name, Method: zip.Store}
	f, err := w.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, content); err != nil {
		t.Fatal(err)
	}
}
