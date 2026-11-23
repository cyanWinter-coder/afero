package tarfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/spf13/afero"
	"github.com/spf13/afero/internal/archivefs"
)

// buildTar writes the given entries into an in-memory tar archive. All data
// is untrusted test input and never touches the host file system.
func buildTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, content := range entries {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(content)),
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatalf("header %q: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes()
}

func TestNewStrictRejectsUnsafeNames(t *testing.T) {
	bad := []struct {
		name string
		want error
	}{
		{"/absolute/path", archivefs.ErrAbsolutePath},
		{"C:/evil", archivefs.ErrAbsolutePath},
		{"../escape", archivefs.ErrRootEscape},
		{"a/../../escape", archivefs.ErrRootEscape},
		{strings.Repeat("a", archivefs.MaxEntryName+1), archivefs.ErrNameTooLong},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildTar(t, map[string]string{tc.name: "x"})
			fs, err := NewStrict(tar.NewReader(bytes.NewReader(raw)))
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
	raw := buildTar(t, map[string]string{
		"a/b":  "first",
		"a//b": "second",
	})
	fs, err := NewStrict(tar.NewReader(bytes.NewReader(raw)))
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

func TestLegacyNewKeepsBehavior(t *testing.T) {
	// Corrupt stream: legacy New returns nil instead of panicking.
	if fs := New(tar.NewReader(strings.NewReader("not a tar stream"))); fs != nil {
		t.Errorf("legacy New on corrupt stream must return nil, got %#v", fs)
	}
	// Empty stream: a valid, empty file system.
	if fs := New(tar.NewReader(strings.NewReader(""))); fs == nil {
		t.Error("legacy New on empty stream must not return nil")
	}
}

func TestTruncatedEntryBodyIsLocated(t *testing.T) {
	raw := buildTar(t, map[string]string{"dir/short": "payload"})

	// Keep the 512-byte header but cut the body (and everything after).
	truncated := raw[:512+3]
	fs, err := NewStrict(tar.NewReader(bytes.NewReader(truncated)))
	if err == nil {
		t.Fatal("expected error on truncated entry body")
	}
	if fs != nil {
		t.Fatal("partial index must not be returned")
	}
	var ee *archivefs.EntryError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v does not wrap archivefs.EntryError", err)
	}
	if ee.Entry != "dir/short" {
		t.Errorf("EntryError.Entry = %q, want %q", ee.Entry, "dir/short")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error %v does not wrap io.ErrUnexpectedEOF", err)
	}

	// A block whose header checksum cannot be validated is structurally
	// corrupt: indexing must fail rather than return an empty,
	// complete-looking file system. (An all-zero block is the legitimate
	// end-of-archive marker and correctly yields an empty file system.)
	junk := bytes.Repeat([]byte("x"), 700)
	if _, err := NewStrict(tar.NewReader(bytes.NewReader(junk))); err == nil {
		t.Error("garbage stream must fail indexing")
	}
}

func TestIndependentReadersAndCloseTar(t *testing.T) {
	raw := buildTar(t, map[string]string{"f": "hello tar"})
	fs := New(tar.NewReader(bytes.NewReader(raw)))

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

func TestTarCloseKeepsSnapshotMetadata(t *testing.T) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{
		Name:     "dir/",
		Typeflag: tar.TypeDir,
		Mode:     0o700,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteHeader(&tar.Header{
		Name: "dir/f",
		Mode: 0o600,
		Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "data"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fs := New(tar.NewReader(bytes.NewReader(buf.Bytes())))

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
	after, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat after close: %v", err)
	}
	if after.Name() != info.Name() || after.Size() != info.Size() {
		t.Errorf("Stat after close diverged: %v vs %v", after, info)
	}

	d, err := fs.Open("dir")
	if err != nil {
		t.Fatalf("explicit directory entry should open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Readdir(-1); !errors.Is(err, afero.ErrFileClosed) {
		t.Errorf("Readdir after close: got %v, want ErrFileClosed", err)
	}
}

func TestConcurrentStatOpenAndCloseTar(t *testing.T) {
	raw := buildTar(t, map[string]string{
		"a":   strings.Repeat("a", 1024),
		"b/c": strings.Repeat("b", 1024),
		"d/e": strings.Repeat("c", 1024),
	})
	fs := New(tar.NewReader(bytes.NewReader(raw)))

	const goroutines = 16
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
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
					buf := make([]byte, 16)
					if _, rerr := io.ReadFull(f, buf); rerr != nil &&
						!errors.Is(rerr, io.EOF) &&
						!errors.Is(rerr, io.ErrUnexpectedEOF) &&
						!errors.Is(rerr, syscall.EISDIR) {
						t.Errorf("Read %q: %v", p, rerr)
						return
					}
					if err := f.Close(); err != nil {
						t.Errorf("Close %q: %v", p, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

func TestNoPartialIndexOnValidationFailure(t *testing.T) {
	// Two valid entries followed by one escaping entry: none of them may
	// be observable because the whole index is rejected.
	raw := buildTar(t, map[string]string{
		"good":      "data",
		"also/good": "data",
		"../evil":   "data",
	})
	fs, err := NewStrict(tar.NewReader(bytes.NewReader(raw)))
	if err == nil {
		t.Fatal("expected indexing error")
	}
	if fs != nil {
		t.Fatal("caller must not receive a partial index")
	}
}
