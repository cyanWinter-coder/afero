package afero

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// skipUnlessOSSymlinks skips tests that need real symbolic links. MemMapFs has
// no link support, and Windows needs privileges to create links.
func skipUnlessOSSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link tests require an OS that supports unprivileged symlinks")
	}
}

// newConfinementFixture builds an OS-backed BasePathFs root plus an unrelated
// directory outside it holding a secret.
func newConfinementFixture(t *testing.T) (fsys Fs, root, outside string) {
	t.Helper()
	root = t.TempDir()
	outside = t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "in.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewBasePathFs(NewOsFs(), root), root, outside
}

func TestBasePathRejectsEscapes(t *testing.T) {
	fsys, _, _ := newConfinementFixture(t)
	bp := fsys.(*BasePathFs)

	for _, name := range []string{
		"..",
		"../",
		"../secret",
		"../../etc/passwd",
		"sub/../../secret",
		"sub/../../outside/secret",
	} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			if _, err := bp.RealPath(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("RealPath(%q) error = %v, want os.ErrNotExist", name, err)
			}
			if _, err := bp.Open(name); !os.IsNotExist(err) {
				t.Fatalf("Open(%q) error = %v, want not-exist", name, err)
			}
			if err := bp.MkdirAll(name, 0o755); !os.IsNotExist(err) {
				t.Fatalf("MkdirAll(%q) error = %v, want not-exist", name, err)
			}
			if err := bp.Remove(name); !os.IsNotExist(err) {
				t.Fatalf("Remove(%q) error = %v, want not-exist", name, err)
			}
			if err := bp.Rename(name, "target"); !os.IsNotExist(err) {
				t.Fatalf("Rename(%q, ...) error = %v, want not-exist", name, err)
			}
		})
	}
}

func TestBasePathNormalizesEveryEntryPoint(t *testing.T) {
	fsys, root, _ := newConfinementFixture(t)
	bp := fsys.(*BasePathFs)

	// Absolute-looking names are interpreted relative to the base path, and
	// interior "." / ".." elements are collapsed. Every entry point must
	// agree on the resulting location.
	if err := bp.MkdirAll("/a//b/../b/c", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", "c")); err != nil {
		t.Fatalf("MkdirAll created the wrong directory: %v", err)
	}

	f, err := bp.Create("./a/./b/c/file.txt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.WriteString("payload"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if fi, err := bp.Stat("/a/b/c/file.txt"); err != nil || fi.IsDir() {
		t.Fatalf("Stat: %v", err)
	}
	if got, err := ReadFile(bp, "a//b//c//file.txt"); err != nil || string(got) != "payload" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if err := bp.Rename("a/b/c/file.txt", "/a/b/c/moved.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", "c", "moved.txt")); err != nil {
		t.Fatalf("Rename moved the wrong file: %v", err)
	}
	if err := bp.Remove("/a/b/c/moved.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", "c", "moved.txt")); !os.IsNotExist(err) {
		t.Fatalf("Remove removed the wrong file: %v", err)
	}

	// The base directory itself is addressable as "", ".", and "/".
	for _, rootName := range []string{"", ".", string(os.PathSeparator)} {
		if _, err := bp.Stat(rootName); err != nil {
			t.Fatalf("Stat(%q): %v", rootName, err)
		}
	}
}

func TestBasePathRejectsInvalidSeparators(t *testing.T) {
	bp := NewBasePathFs(NewMemMapFs(), "/base").(*BasePathFs)

	if _, err := bp.RealPath("bad\x00name"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("RealPath(NUL) error = %v, want fs.ErrInvalid", err)
	}
	if _, err := bp.Open("bad\x00name"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("Open(NUL) error = %v, want fs.ErrInvalid", err)
	}

	if runtime.GOOS == "windows" {
		for _, name := range []string{`C:\Windows\System32`, `\\server\share\file`, `C:relative`} {
			if _, err := bp.RealPath(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("RealPath(%q) error = %v, want os.ErrNotExist", name, err)
			}
		}
	} else {
		// Backslash is an ordinary filename byte on Unix, not a separator.
		if _, err := bp.RealPath(`a\b`); err != nil {
			t.Fatalf("RealPath(%q) on Unix = %v, want nil", `a\b`, err)
		}
	}
}

func TestBasePathSymlinkConfinement(t *testing.T) {
	skipUnlessOSSymlinks(t)
	fsys, root, outside := newConfinementFixture(t)
	bp := fsys.(*BasePathFs)

	// A link to a directory outside the root, a link directly to a file
	// outside, and a link that stays inside.
	if err := os.Symlink(outside, filepath.Join(root, "dir_escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "file_escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/in.txt", filepath.Join(root, "inside_link")); err != nil {
		t.Fatal(err)
	}

	mustEscape := func(op string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected the escape to be rejected", op)
		}
		if !errors.Is(err, ErrPathEscape) {
			t.Fatalf("%s: error = %v, want ErrPathEscape", op, err)
		}
	}

	_, err := bp.Open("dir_escape/secret")
	mustEscape("Open(dir link)", err)

	_, err = bp.Open("file_escape")
	mustEscape("Open(file link)", err)

	_, err = bp.Stat("file_escape")
	mustEscape("Stat(file link)", err)

	mustEscape("Chmod(file link)", bp.Chmod("file_escape", 0o600))

	_, err = bp.Stat("nope")
	if !os.IsNotExist(err) {
		t.Fatalf("Stat(missing) = %v, want not-exist", err)
	}

	f, err := bp.Create("dir_escape/created")
	if f != nil {
		f.Close()
	}
	mustEscape("Create(dir link)", err)

	mustEscape("Remove(dir link)", bp.Remove("dir_escape/secret"))
	mustEscape("RemoveAll(dir link)", bp.RemoveAll("dir_escape/sub"))
	mustEscape("Rename through link", bp.Rename("sub/in.txt", "dir_escape/stolen"))
	mustEscape("MkdirAll through link", bp.MkdirAll("dir_escape/deep/dir", 0o755))

	// Removing the link entry itself, like the standard library, does not
	// follow the link and is allowed.
	if err := bp.RemoveAll("dir_escape"); err != nil {
		t.Fatalf("RemoveAll(link itself): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "dir_escape")); !os.IsNotExist(err) {
		t.Fatalf("RemoveAll(link itself) left the link behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatalf("RemoveAll(link itself) deleted the link target: %v", err)
	}

	// Lstat and Readlink do not follow the final element, so they keep
	// working on the link itself.
	if fi, isLstat, err := bp.LstatIfPossible("file_escape"); err != nil || !isLstat || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("LstatIfPossible(file link) = %v, %v, %v", fi, isLstat, err)
	}

	// A link whose target is inside the root still resolves.
	if got, err := ReadFile(bp, "inside_link"); err != nil || string(got) != "inside" {
		t.Fatalf("ReadFile(inside link) = %q, %v", got, err)
	}

	// Nothing outside was created, removed, or modified.
	if got, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(got) != "outside" {
		t.Fatalf("outside secret changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "created")); !os.IsNotExist(err) {
		t.Fatalf("outside directory gained a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "stolen")); !os.IsNotExist(err) {
		t.Fatalf("outside directory gained a renamed file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "deep")); !os.IsNotExist(err) {
		t.Fatalf("outside directory gained a directory: %v", err)
	}
}

func TestBasePathSymlinkEscapeIsNotExistCompatible(t *testing.T) {
	skipUnlessOSSymlinks(t)
	fsys, root, outside := newConfinementFixture(t)
	_ = outside

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// BasePathFs documents out-of-base names as non-existing. The escape
	// error is deliberately distinct so callers can tell it apart from a
	// genuinely missing file, but wrapping must keep the standard error
	// protocol intact.
	_, err := fsys.Open("link/secret")
	if err == nil {
		t.Fatal("expected an error")
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %T is not an *os.PathError", err)
	}
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("error = %v, want ErrPathEscape", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("escape must not masquerade as not-exist: %v", err)
	}
}

func TestBasePathSymlinkCreationStaysInside(t *testing.T) {
	skipUnlessOSSymlinks(t)
	fsys, root, _ := newConfinementFixture(t)
	bp := fsys.(*BasePathFs)

	if err := bp.SymlinkIfPossible("sub/in.txt", "link"); err != nil {
		t.Fatalf("SymlinkIfPossible: %v", err)
	}
	if got, err := ReadFile(bp, "link"); err != nil || string(got) != "inside" {
		t.Fatalf("ReadFile(link) = %q, %v", got, err)
	}

	// A link whose target is outside the base path cannot be created.
	if err := bp.SymlinkIfPossible("../outside", "bad_link"); err == nil {
		t.Fatal("expected a symlink escaping the base path to be rejected")
	}
	if _, err := os.Lstat(filepath.Join(root, "bad_link")); !os.IsNotExist(err) {
		t.Fatalf("escape link was created: %v", err)
	}

	target, err := bp.ReadlinkIfPossible("link")
	if err != nil {
		t.Fatalf("ReadlinkIfPossible: %v", err)
	}
	if want := filepath.Join(root, "sub", "in.txt"); target != want {
		t.Fatalf("ReadlinkIfPossible = %q, want %q", target, want)
	}
}

func TestBasePathLegacyAbsoluteLinkTarget(t *testing.T) {
	skipUnlessOSSymlinks(t)
	fsys, root, outside := newConfinementFixture(t)
	bp := fsys.(*BasePathFs)

	// Older BasePathFs versions wrote absolute, base-prefixed targets. Links
	// inside the root must keep working; links outside must stay rejected.
	if err := os.Symlink(filepath.Join(root, "sub", "in.txt"), filepath.Join(root, "legacy_inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "legacy_outside")); err != nil {
		t.Fatal(err)
	}

	if got, err := ReadFile(bp, "legacy_inside"); err != nil || string(got) != "inside" {
		t.Fatalf("ReadFile(legacy inside link) = %q, %v", got, err)
	}
	if _, err := bp.Open("legacy_outside"); err == nil || !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Open(legacy outside link) = %v, want ErrPathEscape", err)
	}
}

func TestBasePathNestedConfinement(t *testing.T) {
	skipUnlessOSSymlinks(t)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "outer", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outer", "inner", "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outer", "inner", "escape")); err != nil {
		t.Fatal(err)
	}

	level1 := NewBasePathFs(NewOsFs(), root)
	level2 := NewBasePathFs(level1, "outer")

	if got, err := ReadFile(level2, "inner/ok.txt"); err != nil || string(got) != "ok" {
		t.Fatalf("ReadFile(nested inside) = %q, %v", got, err)
	}
	if _, err := level2.Open("inner/escape/secret"); err == nil || !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Open(nested escape) = %v, want ErrPathEscape", err)
	}
	if _, err := level2.Open("../../secret"); err == nil {
		t.Fatal("nested ../.. escape was allowed")
	}
}

func TestBasePathUnsupportedLinkFsStaysConfined(t *testing.T) {
	// MemMapFs has no symlink support, so the confinement layer must not
	// turn link-less filesystems into an error source.
	bp := NewBasePathFs(NewMemMapFs(), "/root").(*BasePathFs)
	if err := bp.MkdirAll("a/b", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := WriteFile(bp, "a/b/f", []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got, err := ReadFile(bp, "a/b/f"); err != nil || string(got) != "x" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := bp.Open("../f"); !os.IsNotExist(err) {
		t.Fatalf("Open(../f) = %v, want not-exist", err)
	}
}

// TestBasePathConcurrentPathSwap verifies that a concurrent replacement of a
// validated path component cannot redirect an operation to a target outside
// the root. The check is inherently racy, so the test asserts the invariant
// (nothing outside the root is touched) rather than a particular error.
func TestBasePathConcurrentPathSwap(t *testing.T) {
	skipUnlessOSSymlinks(t)

	root := t.TempDir()
	outside := t.TempDir()
	const fileName = "marker"
	if err := os.WriteFile(filepath.Join(outside, fileName), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	bp := NewBasePathFs(NewOsFs(), root)

	linkPath := filepath.Join(root, "swap")
	dirPath := filepath.Join(root, "dir")
	// Start with an inside directory so reads can legitimately succeed.
	if err := os.MkdirAll(filepath.Join(dirPath, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, fileName), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", linkPath); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Flip the component between an inside directory and an
			// outside-pointing link.
			os.Remove(linkPath)
			if err := os.Symlink(outside, linkPath); err != nil {
				return
			}
			os.Remove(linkPath)
			if err := os.Symlink("dir", linkPath); err != nil {
				return
			}
		}
	}()

	var bad int
	var mu sync.Mutex
	for i := 0; i < 200; i++ {
		func() {
			f, err := bp.Open("swap/" + fileName)
			if err != nil {
				return
			}
			defer f.Close()
			data, err := io.ReadAll(f)
			if err != nil {
				return
			}
			if string(data) != "inside" {
				mu.Lock()
				bad++
				mu.Unlock()
			}
		}()
	}
	close(stop)
	wg.Wait()

	if bad > 0 {
		t.Fatalf("read %d payloads from outside the root", bad)
	}
}

func TestPathFsAlias(t *testing.T) {
	// PathFs is a compatibility alias for BasePathFs.
	var _ *PathFs = (*BasePathFs)(nil)
	var _ *BasePathFs = (*PathFs)(nil)

	fsys := NewPathFs(NewMemMapFs(), "/root")
	if err := fsys.MkdirAll("a", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := fsys.Open("../escape"); !os.IsNotExist(err) {
		t.Fatalf("Open(../escape) = %v, want not-exist", err)
	}
}

func TestBasePathRealPathCompatibility(t *testing.T) {
	// The historical lexical contract of RealPath is preserved.
	cases := []struct {
		base, input, want string
	}{
		{"/var/data", "file.txt", "/var/data/file.txt"},
		{"/var/data", "/file.txt", "/var/data/file.txt"},
		{"/var/data", "subdir//file.txt", "/var/data/subdir/file.txt"},
		{"/var/data", "subdir/../file.txt", "/var/data/file.txt"},
		{"/var/data", "..foo", "/var/data/..foo"},
		{".", "file.txt", "file.txt"},
	}
	for _, tc := range cases {
		bp := NewBasePathFs(NewMemMapFs(), tc.base).(*BasePathFs)
		if got, err := bp.RealPath(tc.input); err != nil || got != tc.want {
			t.Fatalf("RealPath(%q, %q) = %q, %v; want %q", tc.base, tc.input, got, err, tc.want)
		}
	}
}

func TestBasePathRelativeBase(t *testing.T) {
	skipUnlessOSSymlinks(t)

	work := t.TempDir()
	t.Chdir(work)

	if err := os.MkdirAll(filepath.Join("relbase", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("relbase", "sub", "f"), []byte("relative"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("secret", []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret", filepath.Join("relbase", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "secret"), filepath.Join("relbase", "absescape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/f", filepath.Join("relbase", "inlink")); err != nil {
		t.Fatal(err)
	}

	bp := NewBasePathFs(NewOsFs(), "relbase")

	if got, err := ReadFile(bp, "sub/f"); err != nil || string(got) != "relative" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if got, err := ReadFile(bp, "inlink"); err != nil || string(got) != "relative" {
		t.Fatalf("ReadFile(inlink) = %q, %v", got, err)
	}
	if _, err := bp.Open("escape"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Open(relative escape) = %v, want ErrPathEscape", err)
	}
	if _, err := bp.Open("absescape"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Open(absolute escape) = %v, want ErrPathEscape", err)
	}

	if err := WriteFile(bp, "sub/new", []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bp.Rename("sub/new", "sub/renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := bp.Remove("sub/renamed"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestBasePathRemoveRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		fsys Fs
	}{
		{name: "MemMapFs", fsys: NewMemMapFs()},
		{name: "OsFs", fsys: NewOsFs()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			if _, ok := tc.fsys.(*OsFs); !ok {
				base = "/root"
			}
			bp := NewBasePathFs(tc.fsys, base)
			if err := bp.MkdirAll("a/b", 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := WriteFile(bp, "a/b/f", []byte("x"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			// Clearing the whole tree through the root spelling is still
			// supported; it must not touch anything outside the base.
			if err := bp.RemoveAll("."); err != nil {
				t.Fatalf("RemoveAll(.): %v", err)
			}
			if _, err := bp.Stat("a/b/f"); !os.IsNotExist(err) {
				t.Fatalf("Stat after RemoveAll(.) = %v, want not-exist", err)
			}
		})
	}
}

func TestBasePathFileNamesDoNotLeakBase(t *testing.T) {
	root := t.TempDir()
	bp := NewBasePathFs(NewOsFs(), root)

	if err := bp.MkdirAll("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := bp.Create("sub/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := f.Name(); got != "/sub/file.txt" {
		t.Fatalf("Create name = %q, want %q", got, "/sub/file.txt")
	}

	dir, err := bp.Open("sub")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if got := dir.Name(); got != "/sub" {
		t.Fatalf("Open(dir) name = %q, want %q", got, "/sub")
	}

	rootDir, err := bp.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer rootDir.Close()
	if strings.Contains(rootDir.Name(), root) {
		t.Fatalf("Open(root) name leaks the base: %q", rootDir.Name())
	}
	if got := rootDir.Name(); got != "" && got != "." && got != string(os.PathSeparator) {
		t.Fatalf("Open(root) name = %q, want a base-relative value", got)
	}
}
