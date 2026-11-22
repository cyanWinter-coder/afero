package afero

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// failingBaseFs fails every mutating call (the writes a read-only base layer
// would reject), while reads/stat keep working.
type failingBaseFs struct {
	Fs
}

func (f *failingBaseFs) Create(name string) (File, error) { return nil, os.ErrPermission }
func (f *failingBaseFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, os.ErrPermission
	}
	return f.Fs.OpenFile(name, flag, perm)
}
func (f *failingBaseFs) Rename(o, n string) error                  { return os.ErrPermission }
func (f *failingBaseFs) Remove(name string) error                  { return os.ErrPermission }
func (f *failingBaseFs) RemoveAll(name string) error               { return os.ErrPermission }
func (f *failingBaseFs) Mkdir(name string, perm os.FileMode) error { return os.ErrPermission }
func (f *failingBaseFs) MkdirAll(name string, perm os.FileMode) error {
	if fi, err := f.Fs.Stat(name); err == nil && fi.IsDir() {
		return nil
	}
	return os.ErrPermission
}

var _ Fs = (*failingBaseFs)(nil)

// failingWritesFs opens files fine but every write fails, modelling a base
// that rejects the data write itself (quota, read-only mount, ...).
type failingWritesFs struct {
	*MemMapFs
	failWrites bool
}

type failingWriteFile struct {
	File
}

func (f failingWriteFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (f failingWriteFile) WriteAt([]byte, int64) (int, error) { return 0, os.ErrPermission }
func (f failingWriteFile) WriteString(string) (int, error)    { return 0, os.ErrPermission }

func (f *failingWritesFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	fh, err := f.MemMapFs.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	if f.failWrites && flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND) != 0 {
		return failingWriteFile{File: fh}, nil
	}
	return fh, nil
}

// orderRecordingFs records the order in which layer/base writes land, so a
// test can assert that the base commit happens before the cache update.
type orderRecordingFs struct {
	*MemMapFs
	order *[]string
	label string
}

type orderRecordingFile struct {
	File
	order *[]string
	label string
}

func (f orderRecordingFile) Write(p []byte) (int, error) {
	*f.order = append(*f.order, f.label)
	return f.File.Write(p)
}

func (f orderRecordingFile) WriteAt(p []byte, off int64) (int, error) {
	*f.order = append(*f.order, f.label)
	return f.File.WriteAt(p, off)
}

func (o *orderRecordingFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	fh, err := o.MemMapFs.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND) != 0 {
		return orderRecordingFile{File: fh, order: o.order, label: o.label}, nil
	}
	return fh, nil
}

// 1. Every read is bound to a source version: after the base changes, a new
// reader must see the new content even when the modification timestamp does
// not move (cacheTime == 0 never re-checks today).
func TestCacheOnReadGenerationFreshness(t *testing.T) {
	base, layer := NewMemMapFs(), NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/v.txt", []byte("v1"), 0o666); err != nil {
		t.Fatal(err)
	}

	first, err := fs.Open("/v.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(first)
	first.Close()
	if err != nil || string(got) != "v1" {
		t.Fatalf("first read = %q, %v; want v1", got, err)
	}

	// Replace the source file in place: same path, new FileData generation.
	if err := WriteFile(base, "/v.txt", []byte("v2-next-generation"), 0o666); err != nil {
		t.Fatal(err)
	}

	second, err := fs.Open("/v.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	got, err = io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2-next-generation" {
		t.Fatalf("second read = %q; want refreshed base content", got)
	}

	if fi, err := layer.Stat("/v.txt"); err != nil {
		t.Fatalf("cache index not refreshed: %v", err)
	} else if fi.Size() != int64(len("v2-next-generation")) {
		t.Fatalf("cached file size = %d; want refreshed copy", fi.Size())
	}
}

// 3. A handle already open keeps seeing the version it captured; only new
// handles observe the committed replacement.
func TestCacheOnReadHandleVersionPinning(t *testing.T) {
	base, layer := NewMemMapFs(), NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/p.txt", []byte("pinned-v1"), 0o666); err != nil {
		t.Fatal(err)
	}

	old, err := fs.Open("/p.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	if err := WriteFile(base, "/p.txt", []byte("pinned-v2"), 0o666); err != nil {
		t.Fatal(err)
	}

	// The captured handle still resolves to the data it was opened against.
	got, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pinned-v1" {
		t.Fatalf("open handle saw %q; want its captured version pinned-v1", got)
	}

	// A freshly opened handle sees the committed version.
	fresh, err := fs.Open("/p.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	got, err = io.ReadAll(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pinned-v2" {
		t.Fatalf("new handle saw %q; want committed version pinned-v2", got)
	}
}

// 2. Writes commit through the base first: a rejected base write must leave
// the cached view (and the layer) exactly where it was.
func TestCacheOnReadWriteFailureKeepsOldView(t *testing.T) {
	base := &failingBaseFs{Fs: NewMemMapFs()}
	layer := NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base.Fs, "/w.txt", []byte("original"), 0o666); err != nil {
		t.Fatal(err)
	}

	// Prime the cache.
	if data, err := ReadFile(fs, "/w.txt"); err != nil || string(data) != "original" {
		t.Fatalf("cache prime = %q, %v", data, err)
	}

	fh, err := fs.OpenFile("/w.txt", os.O_RDWR|os.O_TRUNC, 0o666)
	if err == nil {
		fh.Close()
		t.Fatal("write against a rejecting base layer must fail")
	}

	// The cached view is untouched.
	if data, err := ReadFile(fs, "/w.txt"); err != nil || string(data) != "original" {
		t.Fatalf("view after failed write = %q, %v; want original", data, err)
	}
	if data, err := ReadFile(layer, "/w.txt"); err != nil || string(data) != "original" {
		t.Fatalf("layer after failed write = %q, %v; want original", data, err)
	}

	// Create() must obey the same commit ordering.
	if _, err := fs.Create("/w.txt"); err == nil {
		t.Fatal("Create against a rejecting base layer must fail")
	}
	if data, err := ReadFile(layer, "/w.txt"); err != nil || string(data) != "original" {
		t.Fatalf("layer after failed Create = %q, %v; want original", data, err)
	}
}

// 2d. When the data write itself fails in the base layer, the cache layer
// must not have accepted the same write ahead of it.
func TestCacheOnReadDataWriteFailureKeepsOldView(t *testing.T) {
	base := &failingWritesFs{MemMapFs: NewMemMapFs().(*MemMapFs)}
	layer := NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/d.txt", []byte("original-data"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(fs, "/d.txt"); err != nil {
		t.Fatal(err)
	}
	base.failWrites = true

	fh, err := fs.OpenFile("/d.txt", os.O_RDWR, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("forked")); err == nil {
		t.Fatal("base write failure must propagate")
	}
	fh.Close()

	if data, err := ReadFile(layer, "/d.txt"); err != nil || string(data) != "original-data" {
		t.Fatalf("layer forked after failed base write: %q, %v", data, err)
	}
}

// 2b. Once the base commit succeeds, the layer is updated and subsequent
// reads return the committed data.
func TestCacheOnReadWriteSuccessCommitsBoth(t *testing.T) {
	base, layer := NewMemMapFs(), NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/c.txt", []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(fs, "/c.txt"); err != nil {
		t.Fatal(err)
	}

	fh, err := fs.OpenFile("/c.txt", os.O_RDWR|os.O_TRUNC, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("committed")); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	for name, x := range map[string]Fs{"base": base, "layer": layer, "union": fs} {
		data, err := ReadFile(x, "/c.txt")
		if err != nil || !bytes.Equal(data, []byte("committed")) {
			t.Errorf("%s = %q, %v; want committed", name, data, err)
		}
	}
}

// 2c. The base layer must receive the write before the cache layer is
// touched: otherwise a base failure leaves the two views forked.
func TestCacheOnReadWriteCommitsBaseBeforeLayer(t *testing.T) {
	var order []string
	base := &orderRecordingFs{MemMapFs: NewMemMapFs().(*MemMapFs), order: &order, label: "base"}
	layer := &orderRecordingFs{MemMapFs: NewMemMapFs().(*MemMapFs), order: &order, label: "layer"}
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/o.txt", []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(fs, "/o.txt"); err != nil {
		t.Fatal(err)
	}
	order = nil

	fh, err := fs.OpenFile("/o.txt", os.O_RDWR|os.O_TRUNC, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	if len(order) < 2 || order[0] != "base" || order[1] != "layer" {
		t.Fatalf("write order = %v; want base before layer", order)
	}
}

// 4. Rename/Remove/Stat keep the cache index in lockstep with the base.
func TestCacheOnReadIndexTracksRenameAndRemove(t *testing.T) {
	base, layer := NewMemMapFs(), NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/r.txt", []byte("rename-me"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(fs, "/r.txt"); err != nil {
		t.Fatal(err)
	}

	if err := fs.Rename("/r.txt", "/r2.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := layer.Stat("/r.txt"); !os.IsNotExist(err) {
		t.Errorf("old cache entry still present after rename: %v", err)
	}
	if data, err := ReadFile(fs, "/r2.txt"); err != nil || string(data) != "rename-me" {
		t.Errorf("renamed read = %q, %v", data, err)
	}
	if _, err := base.Stat("/r.txt"); !os.IsNotExist(err) {
		t.Errorf("base still has old name: %v", err)
	}

	if err := fs.Remove("/r2.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := layer.Stat("/r2.txt"); !os.IsNotExist(err) {
		t.Errorf("cache entry still present after remove: %v", err)
	}
	if _, err := fs.Stat("/r2.txt"); !os.IsNotExist(err) {
		t.Errorf("Stat still resolves removed file: %v", err)
	}
}

// 4b. A failed rename must not move the cache index either.
func TestCacheOnReadRenameFailureKeepsIndex(t *testing.T) {
	base := &failingBaseFs{Fs: NewMemMapFs()}
	layer := NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base.Fs, "/k.txt", []byte("keep"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(fs, "/k.txt"); err != nil {
		t.Fatal(err)
	}

	if err := fs.Rename("/k.txt", "/k2.txt"); err == nil {
		t.Fatal("Rename against a rejecting base layer must fail")
	}
	if data, err := ReadFile(layer, "/k.txt"); err != nil || string(data) != "keep" {
		t.Fatalf("cache moved despite failed rename: %q, %v", data, err)
	}
	if _, err := layer.Stat("/k2.txt"); !os.IsNotExist(err) {
		t.Fatal("layer gained target entry despite failed rename")
	}
}

// 4c. A read-only base layer must never hand out a writable handle.
func TestCopyOnWriteReadOnlyBaseNoWritableHandle(t *testing.T) {
	base := NewMemMapFs()
	if err := WriteFile(base, "/ro.txt", []byte("read-only"), 0o666); err != nil {
		t.Fatal(err)
	}
	ufs := NewCopyOnWriteFs(NewReadOnlyFs(base), NewMemMapFs())

	fh, err := ufs.OpenFile("/ro.txt", os.O_RDWR, 0o666)
	if err != nil {
		t.Fatalf("copy-up open should succeed in the layer: %v", err)
	}
	if _, err := fh.Write([]byte("x")); err != nil {
		t.Errorf("writable handle expected, got write error: %v", err)
	}
	fh.Close()

	// The read-only base must be untouched; the mutation lives in the layer.
	if data, _ := ReadFile(base, "/ro.txt"); string(data) != "read-only" {
		t.Errorf("read-only base leaked a write: %q", data)
	}
}

// 4d. In the copy-on-write overlay, Rename and Remove keep the layer index
// in sync and never mutate the read-only base.
func TestCopyOnWriteIndexTracksRenameAndRemove(t *testing.T) {
	base := NewMemMapFs()
	ro := NewReadOnlyFs(base)
	layer := NewMemMapFs()
	ufs := NewCopyOnWriteFs(ro, layer)

	if err := WriteFile(base, "/a.txt", []byte("aaa"), 0o666); err != nil {
		t.Fatal(err)
	}

	// Force a copy-up so the entry exists in the overlay.
	fh, err := ufs.OpenFile("/a.txt", os.O_RDWR, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("bbb"); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	if err := ufs.Rename("/a.txt", "/c.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := layer.Stat("/a.txt"); !os.IsNotExist(err) {
		t.Errorf("overlay still indexes the old name after rename: %v", err)
	}
	if data, err := ReadFile(ufs, "/c.txt"); err != nil || string(data) != "bbb" {
		t.Errorf("renamed read = %q, %v; want bbb", data, err)
	}
	if _, err := base.Stat("/c.txt"); err == nil {
		t.Error("read-only base must not gain the renamed entry")
	}

	if err := ufs.Remove("/c.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := layer.Stat("/c.txt"); !os.IsNotExist(err) {
		t.Errorf("overlay still indexes the removed entry: %v", err)
	}
	if _, err := ufs.Stat("/c.txt"); !os.IsNotExist(err) {
		t.Errorf("Stat still resolves the removed entry: %v", err)
	}
	// The read-only base still holds its original copy.
	if data, err := ReadFile(base, "/a.txt"); err != nil || string(data) != "aaa" {
		t.Errorf("base changed: %q, %v", data, err)
	}
}

// 4e. Removing or renaming an entry that exists only in the read-only base
// must be rejected without touching either layer.
func TestCopyOnWriteBaseOnlyMutationRejected(t *testing.T) {
	base := NewMemMapFs()
	ro := NewReadOnlyFs(base)
	layer := NewMemMapFs()
	ufs := NewCopyOnWriteFs(ro, layer)

	if err := WriteFile(base, "/only-base.txt", []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Rename("/only-base.txt", "/moved.txt"); !os.IsPermission(err) {
		t.Errorf("Rename base-only = %v; want permission error", err)
	}
	if err := ufs.Remove("/only-base.txt"); !os.IsPermission(err) {
		t.Errorf("Remove base-only = %v; want permission error", err)
	}
	if _, err := layer.Stat("/only-base.txt"); err == nil {
		t.Error("rejected mutation must not create an overlay entry")
	}
	if _, err := layer.Stat("/moved.txt"); err == nil {
		t.Error("rejected rename must not create the overlay target")
	}
}

// 5. Creating a brand new file through the union must commit it to both
// layers, including when neither layer has seen the path before.
func TestCacheOnReadCreateOnCacheMiss(t *testing.T) {
	base, layer := NewMemMapFs(), NewMemMapFs()
	fs := NewCacheOnReadFs(base, layer, 0)

	fh, err := fs.OpenFile("/deep/nested/new.txt", os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		t.Fatalf("OpenFile O_CREATE on miss: %v", err)
	}
	if _, err := fh.WriteString("created"); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	for name, x := range map[string]Fs{"base": base, "layer": layer} {
		data, err := ReadFile(x, "/deep/nested/new.txt")
		if err != nil || string(data) != "created" {
			t.Errorf("%s = %q, %v; want created", name, data, err)
		}
	}
}

// 6. Generation consistency must also hold for real OS-backed layers, where
// the only available signal is the file metadata fingerprint.
func TestCacheOnReadOsFsGenerationRefresh(t *testing.T) {
	defer CleanupTempDirs(t)
	base := NewTempOsBaseFs(t)
	layer := NewTempOsBaseFs(t)
	fs := NewCacheOnReadFs(base, layer, 0)

	if err := WriteFile(base, "/os.txt", []byte("one"), 0o666); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadFile(fs, "/os.txt"); err != nil || string(data) != "one" {
		t.Fatalf("prime = %q, %v", data, err)
	}

	if err := WriteFile(base, "/os.txt", []byte("two"), 0o666); err != nil {
		t.Fatal(err)
	}

	// Force distinguishable metadata on file systems with coarse timestamps.
	future := time.Now().Add(time.Hour)
	if err := base.Chtimes("/os.txt", future, future); err != nil {
		t.Fatal(err)
	}

	if data, err := ReadFile(fs, "/os.txt"); err != nil || string(data) != "two" {
		t.Fatalf("read after base update = %q, %v; want two", data, err)
	}
}
