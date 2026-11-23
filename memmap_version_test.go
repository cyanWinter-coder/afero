package afero

// Versioned handle, atomic directory mutation and stable snapshot tests for
// MemMapFs. Run with the race detector:
//
//	go test -race -run 'TestMemMapFsVersioned|TestMemMapFsAtomic|TestMemMapFsStable' .
//
// The chain under test:
//
//  1. Every open freezes inode identity, structural version and opening path.
//  2. Rename/Remove/Mkdir commit under one write lock; readers see a complete
//     old or new namespace, never a partial one.
//  3. After removal, new Open fails not-exist while existing handles complete
//     their in-flight work and close cleanly.
//  4. Concurrent directory walks use an immutable snapshot: no duplicates, no
//     vanished or half-renamed entries.
//  5. The public Fs/File/MemMapFs surface is unchanged.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/spf13/afero/mem"
)

// 1. Identity and version are frozen on the handle at open time.
func TestMemMapFsVersionedHandleFreezesIdentity(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()

	f, err := fs.Create("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	h := f.(*mem.File)
	createdVersion := h.OpenVersion()
	if createdVersion == 0 {
		t.Fatal("Create did not stamp a structural version")
	}
	if h.OpenedName() != "/a.txt" {
		t.Fatalf("opened name = %q, want /a.txt", h.OpenedName())
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Read-only open captures the same inode and its current generation.
	ro, err := fs.Open("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	roH := ro.(*mem.File)
	if roH.InodeID() != h.InodeID() {
		t.Fatal("Open returned a different inode than Create")
	}
	if roH.OpenedName() != "/a.txt" {
		t.Fatalf("read-only opened name = %q, want /a.txt", roH.OpenedName())
	}

	// Rename bumps the generation and moves the inode, but the open handle
	// keeps its captured name and inode.
	beforeRename := roH.OpenVersion()
	if err := fs.Rename("/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := ro.Name(); got != "/a.txt" {
		t.Fatalf("handle Name changed after rename: got %q, want /a.txt", got)
	}
	if roH.InodeID() != h.InodeID() {
		t.Fatal("rename changed the inode identity of an open handle")
	}
	if roH.OpenVersion() != beforeRename {
		t.Fatal("rename mutated the version captured by an existing handle")
	}

	// A fresh open at the new path is a new generation handle; old path is gone.
	nf, err := fs.Open("/b.txt")
	if err != nil {
		t.Fatalf("Open at renamed path: %v", err)
	}
	defer nf.Close()
	if _, err := fs.Open("/a.txt"); !os.IsNotExist(err) {
		t.Fatalf("Open old path after rename: expected not-exist, got %v", err)
	}

	// Old handle still reads the bytes of its captured inode.
	buf := make([]byte, 5)
	if _, err := ro.ReadAt(buf, 0); err != nil {
		t.Fatalf("old handle read after rename: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("old handle data = %q, want hello", buf)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
}

// 1b. Every structural mutation bumps the generation; pure reads do not.
func TestMemMapFsVersionedVersionMonotonic(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()

	version := func() uint64 {
		f, err := fs.OpenFile("/v.txt", os.O_RDWR|os.O_CREATE, 0o666)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return f.(*mem.File).OpenVersion()
	}

	v0 := version()
	for i := 0; i < 3; i++ {
		if _, err := fs.Open("/v.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Stat("/v.txt"); err != nil {
			t.Fatal(err)
		}
	}
	if got := version(); got != v0 {
		t.Fatalf("reads changed the version: %d -> %d", v0, got)
	}

	mustBump := func(label string, mutate func() error) {
		before := version()
		if err := mutate(); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		after := version()
		if after <= before {
			t.Fatalf("%s did not bump the version: %d -> %d", label, before, after)
		}
	}

	mustBump("Mkdir", func() error { return fs.Mkdir("/d", 0o755) })
	mustBump("Rename", func() error { return fs.Rename("/v.txt", "/w.txt") })
	mustBump("Remove", func() error { return fs.Remove("/w.txt") })
	mustBump("Create", func() error {
		_, err := fs.Create("/z.txt")
		return err
	})
}

// 2 & 4. Rename and directory traversal never expose a partial snapshot and
// never return duplicate or removed entries.
func TestMemMapFsAtomicRenameAndStableReadDir(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()
	const n = 50
	for i := 0; i < n; i++ {
		if err := WriteFile(fs, fmt.Sprintf("/d/%d", i), []byte("x"), 0o777); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; ; j++ {
			select {
			case <-stop:
				return
			default:
			}
			from := filepath.Join("/d", fmt.Sprintf("%d", j%n))
			tmp := filepath.Join("/d", fmt.Sprintf("tmp-%d", j))
			if err := fs.Rename(from, tmp); err != nil {
				t.Errorf("rename out: %v", err)
				return
			}
			if err := fs.Rename(tmp, from); err != nil {
				t.Errorf("rename back: %v", err)
				return
			}
		}
	}()

	for k := 0; k < 500; k++ {
		entries, err := ReadDir(fs, "/d")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range entries {
			if seen[e.Name()] {
				t.Fatalf("duplicate entry %q in directory snapshot", e.Name())
			}
			seen[e.Name()] = true
		}
		if len(entries) != n {
			t.Fatalf("partial directory snapshot: got %d entries, want %d", len(entries), n)
		}
	}
	close(stop)
	wg.Wait()

	// After the storm the directory must contain exactly the original set.
	entries, err := ReadDir(fs, "/d")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("final listing has %d entries, want %d", len(entries), n)
	}
	for i := 0; i < n; i++ {
		if _, err := fs.Stat(fmt.Sprintf("/d/%d", i)); err != nil {
			t.Fatalf("original entry /d/%d lost: %v", i, err)
		}
	}
}

// 2b. Mkdir/Remove racing a reader: every listing is a coherent snapshot and
// every map entry reachable through its parent directory table.
func TestMemMapFsAtomicMkdirRemoveReadDir(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()
	if err := fs.Mkdir("/root", 0o755); err != nil {
		t.Fatal(err)
	}
	mfs := fs.(*MemMapFs)

	const workers = 8
	const iterations = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				name := fmt.Sprintf("/root/f-%d-%d", w, i)
				if err := WriteFile(fs, name, []byte("x"), 0o666); err != nil {
					t.Error(err)
					return
				}
				if err := fs.Remove(name); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}

	// Unlocked hammering: only needs to never error, race or panic.
	for k := 0; k < 500; k++ {
		if _, err := ReadDir(fs, "/root"); err != nil {
			t.Fatal(err)
		}
	}

	// Coherence check with writers frozen: while the read lock is held no
	// Remove/Create can commit, so every entry in the directory snapshot
	// must resolve through the namespace and appear exactly once. The
	// namespace is inspected directly because taking the lock and then
	// calling another locking API on the same goroutine would deadlock
	// while a writer waits for this read section to drain.
	for k := 0; k < 500; k++ {
		mfs.mu.RLock()
		dirNode, ok := mfs.getData()["/root"]
		if !ok {
			mfs.mu.RUnlock()
			t.Fatal("/root vanished")
		}
		snapshot := mem.Children(dirNode)
		seen := map[string]bool{}
		for _, child := range snapshot {
			name := child.Name()
			if seen[name] {
				mfs.mu.RUnlock()
				t.Fatalf("duplicate entry %q", name)
			}
			seen[name] = true
			// FileData.Name() is the full namespace path already.
			if _, ok := mfs.getData()[name]; !ok {
				mfs.mu.RUnlock()
				t.Fatalf("snapshot entry %q not in namespace", name)
			}
		}
		mfs.mu.RUnlock()
	}
	wg.Wait()
}

// 3. After Remove, new opens fail not-exist while pre-existing handles keep
// operating on their captured inode and close cleanly.
func TestMemMapFsVersionedRemoveHandleSemantics(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()
	f, err := fs.OpenFile("/x.txt", os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fs.Open("/x.txt"); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		}()
	}
	if err := fs.Remove("/x.txt"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if _, err := fs.Open("/x.txt"); !os.IsNotExist(err) {
		t.Fatalf("Open after remove: expected not-exist, got %v", err)
	}
	if _, err := fs.Stat("/x.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat after remove: expected not-exist, got %v", err)
	}

	// The old handle keeps read, write, seek and close semantics on its
	// captured inode (POSIX unlink behavior).
	buf := make([]byte, 7)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("old handle read after remove: %v", err)
	}
	if string(buf) != "payload" {
		t.Fatalf("old handle data = %q, want payload", buf)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("old handle seek: %v", err)
	}
	if _, err := f.Write([]byte("!")); err != nil {
		t.Fatalf("old handle write after remove: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("old handle close: %v", err)
	}
	if err := f.Close(); err == nil {
		t.Fatal("second Close should still error")
	}

	// Re-creating the path mints a brand new inode; nothing resurrects the
	// removed generation.
	g, err := fs.Create("/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.(*mem.File).InodeID() == f.(*mem.File).InodeID() {
		t.Fatal("re-created path reused the removed inode identity")
	}
}

// 4b. A paginated Readdir on one open directory handle walks one immutable
// snapshot across calls: entries removed during the walk do not shift the
// window, and nothing is reported twice.
func TestMemMapFsStablePagedReaddir(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()
	const n = 20
	for i := 0; i < n; i++ {
		if err := WriteFile(fs, fmt.Sprintf("/p/%d", i), []byte("x"), 0o777); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := fs.Open("/p")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	first, err := dir.Readdir(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 5 {
		t.Fatalf("first page = %d entries, want 5", len(first))
	}

	// Mutate the live directory while the handle is mid-walk.
	for i := 0; i < n; i += 2 {
		if err := fs.Remove(fmt.Sprintf("/p/%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteFile(fs, "/p/new.txt", []byte("x"), 0o777); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, fi := range first {
		seen[fi.Name()] = true
	}
	for {
		page, err := dir.Readdir(5)
		for _, fi := range page {
			if seen[fi.Name()] {
				t.Fatalf("entry %q reported twice across pages", fi.Name())
			}
			seen[fi.Name()] = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != n {
		t.Fatalf("paged walk covered %d entries, want the full snapshot of %d", len(seen), n)
	}
}

// 2c. Cross-parent rename moves a whole subtree atomically and keeps every
// parent directory table consistent with the namespace map.
func TestMemMapFsAtomicRenameSubtree(t *testing.T) {
	t.Parallel()

	fs := NewMemMapFs()
	if err := fs.MkdirAll("/src/dir", 0o777); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(fs, "/src/file.txt", []byte("data"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(fs, "/src/dir/nested.txt", []byte("nested"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("/other", 0o755); err != nil {
		t.Fatal(err)
	}

	if err := fs.Rename("/src", "/other/dst"); err != nil {
		t.Fatalf("rename subtree: %v", err)
	}

	for _, p := range []string{"/src", "/src/file.txt", "/src/dir/nested.txt"} {
		if _, err := fs.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%q should be gone, got %v", p, err)
		}
	}
	for _, p := range []string{"/other/dst", "/other/dst/file.txt", "/other/dst/dir/nested.txt"} {
		if _, err := fs.Stat(p); err != nil {
			t.Fatalf("%q should exist: %v", p, err)
		}
	}

	// Parent tables and namespace map must agree exactly.
	mfs := fs.(*MemMapFs)
	mfs.mu.RLock()
	defer mfs.mu.RUnlock()
	for path, node := range mfs.getData() {
		if path == FilePathSeparator {
			continue
		}
		parentPath := filepath.Clean(filepath.Dir(path))
		parent, ok := mfs.getData()[parentPath]
		if !ok {
			t.Fatalf("node %q has no parent %q in namespace", path, parentPath)
		}
		found := false
		for _, child := range mem.Children(parent) {
			if child == node {
				found = true
			}
		}
		if !found {
			t.Fatalf("node %q missing from parent %q entry table", path, parentPath)
		}
	}
}
