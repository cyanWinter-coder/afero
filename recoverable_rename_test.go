// Copyright © 2026 Afero Authors.
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

package afero

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// failingFs wraps an Fs and turns selected operations into deterministic
// failures so crash windows can be exercised without a real kernel crash.
type failingFs struct {
	Fs

	// failRename, when it returns true, makes the Rename of the given
	// (old, new) pair fail with failErr. failErr is read per call, so the
	// callback can switch between EXDEV probing and later injected failures.
	failRename func(oldname, newname string) bool
	failErr    error

	// failCopy, when set, makes the first write into a hidden temporary
	// destination fail, simulating a broken copy.
	failCopy func(tempName string) bool

	renameCount int
}

func (f *failingFs) Rename(oldname, newname string) error {
	f.renameCount++
	if f.failRename != nil && f.failRename(oldname, newname) {
		return f.failErr
	}
	return f.Fs.Rename(oldname, newname)
}

func (f *failingFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	file, err := f.Fs.OpenFile(name, flag, perm)
	if err != nil || f.failCopy == nil || !f.failCopy(name) {
		return file, err
	}
	return &failingFile{File: file, fail: true}, nil
}

type failingFile struct {
	File
	fail bool
}

func (f *failingFile) Write(p []byte) (int, error) {
	if f.fail {
		f.fail = false
		return 0, errors.New("simulated write failure")
	}
	return f.File.Write(p)
}

func (f *failingFile) WriteAt(p []byte, off int64) (int, error) {
	if f.fail {
		f.fail = false
		return 0, errors.New("simulated write failure")
	}
	return f.File.WriteAt(p, off)
}

func mustReadFile(t *testing.T, fs Fs, name string) string {
	t.Helper()
	data, err := ReadFile(fs, name)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", name, err)
	}
	return string(data)
}

func writeContent(t *testing.T, fs Fs, name, content string) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Dir(name), 0o777); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := WriteFile(fs, name, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}

// TestRecoverableRenameSameVolume runs against both MemMapFs and OsFs where
// the fast path is the native atomic rename.
func TestRecoverableRenameSameVolume(t *testing.T) {
	for _, fs := range Fss {
		t.Run(fs.Name(), func(t *testing.T) {
			dir := testDir(fs)
			from := filepath.Join(dir, "same-from")
			to := filepath.Join(dir, "same-to")
			writeContent(t, fs, from, "payload")

			if err := RecoverableRename(fs, from, to); err != nil {
				t.Fatalf("RecoverableRename: %v", err)
			}
			if exists, _ := Exists(fs, from); exists {
				t.Error("source still exists after rename")
			}
			if got := mustReadFile(t, fs, to); got != "payload" {
				t.Errorf("destination content = %q, want payload", got)
			}

			// Retrying the completed request must be idempotent.
			err := RecoverableRename(fs, from, to)
			if err == nil {
				t.Fatal("retry of completed rename should report the missing source")
			}
			if !os.IsNotExist(err) {
				t.Fatalf("retry error = %v, want a not-exist error", err)
			}
			if got := mustReadFile(t, fs, to); got != "payload" {
				t.Errorf("destination changed on retry: %q", got)
			}
		})
	}
}

// TestRecoverableRenameOverwrite verifies that a same-volume replacement
// atomically displaces the previous destination.
func TestRecoverableRenameOverwrite(t *testing.T) {
	for _, fs := range Fss {
		t.Run(fs.Name(), func(t *testing.T) {
			dir := testDir(fs)
			from := filepath.Join(dir, "ow-from")
			to := filepath.Join(dir, "ow-to")
			writeContent(t, fs, from, "new")
			writeContent(t, fs, to, "old")

			if err := RecoverableRename(fs, from, to); err != nil {
				t.Fatalf("RecoverableRename: %v", err)
			}
			if got := mustReadFile(t, fs, to); got != "new" {
				t.Errorf("destination = %q, want new", got)
			}
			if exists, _ := Exists(fs, from); exists {
				t.Error("source still exists")
			}

			// No scratch artifacts may remain, including hidden entries that do
			// not show up through plain Stat on the visible names.
			entries, err := ReadDir(fs, dir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".afero-rr-") {
					t.Errorf("scratch artifact left behind: %q", e.Name())
				}
			}
		})
	}
}

// TestRecoverableRenameSameName is a no-op for an existing file.
func TestRecoverableRenameSameName(t *testing.T) {
	for _, fs := range Fss {
		t.Run(fs.Name(), func(t *testing.T) {
			dir := testDir(fs)
			name := filepath.Join(dir, "self")
			writeContent(t, fs, name, "x")
			if err := RecoverableRename(fs, name, name); err != nil {
				t.Fatalf("self rename: %v", err)
			}
			if got := mustReadFile(t, fs, name); got != "x" {
				t.Errorf("content changed: %q", got)
			}
		})
	}
}

// TestRecoverableRenameOpenHandlesStayReadable pins requirement 2: a handle
// opened before the replacement keeps serving the old bytes, while new opens
// see only the complete new file.
func TestRecoverableRenameOpenHandlesStayReadable(t *testing.T) {
	for _, fs := range Fss {
		t.Run(fs.Name(), func(t *testing.T) {
			dir := testDir(fs)
			from := filepath.Join(dir, "handle-from")
			to := filepath.Join(dir, "handle-to")
			writeContent(t, fs, from, "version-1")

			oldHandle, err := fs.Open(from)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer oldHandle.Close()

			writeContent(t, fs, to, "stale")
			if err := RecoverableRename(fs, from, to); err != nil {
				t.Fatalf("RecoverableRename: %v", err)
			}

			// The pre-replace handle keeps reading the bytes it was opened on.
			got, err := io.ReadAll(oldHandle)
			if err != nil {
				t.Fatalf("reading old handle: %v", err)
			}
			if string(got) != "version-1" {
				t.Errorf("old handle = %q, want version-1", string(got))
			}

			// A fresh open sees only the complete replacement.
			if got := mustReadFile(t, fs, to); got != "version-1" {
				t.Errorf("new open = %q, want version-1", got)
			}
		})
	}
}

// TestRecoverableRenameCrossVolume drives the journaled copy-and-switch
// protocol on an in-memory filesystem by forcing the initial rename to report
// EXDEV, then re-runs the same request after every distinct interruption.
func TestRecoverableRenameCrossVolume(t *testing.T) {
	fs := NewMemMapFs()
	dir := "/cross"
	from := dir + "/src/file.txt"
	to := dir + "/dst/file.txt"
	writeContent(t, fs, from, "cross-volume-payload")
	writeContent(t, fs, to, "previous-destination")

	injected := &failingFs{
		Fs:      fs,
		failErr: syscall.EXDEV,
		failRename: func(oldname, newname string) bool {
			// Only the original cross-volume request crosses devices; internal
			// scratch renames stay inside the destination directory.
			return oldname == from && newname == to
		},
	}

	if err := RecoverableRename(injected, from, to); err != nil {
		t.Fatalf("cross-volume RecoverableRename: %v", err)
	}
	if exists, _ := Exists(fs, from); exists {
		t.Error("source still exists")
	}
	if got := mustReadFile(t, fs, to); got != "cross-volume-payload" {
		t.Errorf("destination = %q, want cross-volume-payload", got)
	}

	assertNoScratch(t, fs, filepath.Dir(to))

	// A retried request finds no source and reports not-exist without touching
	// the destination.
	err := RecoverableRename(injected, from, to)
	if !os.IsNotExist(err) {
		t.Fatalf("retry = %v, want not-exist", err)
	}
	if got := mustReadFile(t, fs, to); got != "cross-volume-payload" {
		t.Errorf("destination changed after retry: %q", got)
	}
}

// TestRecoverableRenameCrossVolumeCopyFailure aborts before the visibility
// switch and must restore both names exactly.
func TestRecoverableRenameCrossVolumeCopyFailure(t *testing.T) {
	fs := NewMemMapFs()
	dir := "/copyfail"
	from := dir + "/src/file.txt"
	to := dir + "/dst/file.txt"
	writeContent(t, fs, from, "source-bytes")
	writeContent(t, fs, to, "destination-bytes")

	copyFailed := false
	injected := &failingFs{
		Fs:      fs,
		failErr: syscall.EXDEV,
		failRename: func(oldname, newname string) bool {
			return oldname == from && newname == to
		},
		failCopy: func(tempName string) bool {
			// Fail only the very first copy attempt so the retried request
			// runs the whole protocol cleanly.
			if strings.Contains(tempName, ".tmp") && !copyFailed {
				copyFailed = true
				return true
			}
			return false
		},
	}

	err := RecoverableRename(injected, from, to)
	if err == nil || !strings.Contains(err.Error(), "simulated write failure") {
		t.Fatalf("expected simulated write failure, got %v", err)
	}
	if got := mustReadFile(t, fs, from); got != "source-bytes" {
		t.Errorf("source = %q, want source-bytes", got)
	}
	if got := mustReadFile(t, fs, to); got != "destination-bytes" {
		t.Errorf("destination = %q, want destination-bytes", got)
	}
	assertNoScratch(t, fs, filepath.Dir(to))

	// Retrying the identical request now succeeds and is idempotent afterwards.
	if err := RecoverableRename(injected, from, to); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if got := mustReadFile(t, fs, to); got != "source-bytes" {
		t.Errorf("destination after retry = %q, want source-bytes", got)
	}
	if exists, _ := Exists(fs, from); exists {
		t.Error("source still exists after successful retry")
	}
	assertNoScratch(t, fs, filepath.Dir(to))
}

// TestRecoverableRenameSwitchFailure restores the original state when the
// visibility switch itself fails after the "prepared" journal was durable.
func TestRecoverableRenameSwitchFailure(t *testing.T) {
	fs := NewMemMapFs()
	dir := "/switchfail"
	from := dir + "/src/file.txt"
	to := dir + "/dst/file.txt"
	writeContent(t, fs, from, "source-bytes")
	writeContent(t, fs, to, "destination-bytes")

	switched := false
	var injected *failingFs
	injected = &failingFs{
		Fs: fs,
		failRename: func(oldname, newname string) bool {
			switch {
			// The original cross-volume request must surface EXDEV so the
			// generic protocol takes the copy-and-switch path.
			case oldname == from && newname == to:
				injected.failErr = syscall.EXDEV
				return true
			// Fail the temp -> visible switch exactly once.
			case strings.Contains(oldname, ".tmp") && newname == to && !switched:
				switched = true
				injected.failErr = errors.New("simulated switch failure")
				return true
			default:
				return false
			}
		},
	}

	err := RecoverableRename(injected, from, to)
	if err == nil || !strings.Contains(err.Error(), "simulated switch failure") {
		t.Fatalf("expected switch failure, got %v", err)
	}
	if got := mustReadFile(t, fs, from); got != "source-bytes" {
		t.Errorf("source = %q, want source-bytes", got)
	}
	if got := mustReadFile(t, fs, to); got != "destination-bytes" {
		t.Errorf("destination = %q, want destination-bytes", got)
	}
	assertNoScratch(t, fs, filepath.Dir(to))

	// The retried request must complete the replacement end-to-end.
	if err := RecoverableRename(injected, from, to); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := mustReadFile(t, fs, to); got != "source-bytes" {
		t.Errorf("destination after retry = %q", got)
	}
	assertNoScratch(t, fs, filepath.Dir(to))
}

// TestRecoverableRenameJournalRecovery reconstructs each durable journal
// phase by hand and checks that a fresh invocation reconciles it.
func TestRecoverableRenameJournalRecovery(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		// setup arranges the on-disk state expected for that phase.
		setup func(t *testing.T, fs Fs, j *renameJournal)
		// contentAt is the file whose bytes the journal checksum must match.
		contentAt func(j renameJournal) string
		// reattempt is true when recovery should roll back and run the whole
		// request again rather than just rolling forward.
		reattempt bool
		// assert checks the state after reconciliation.
		assert func(t *testing.T, fs Fs, j renameJournal)
	}{
		{
			name:  "prepared-rolls-back-and-reattempts",
			phase: phasePrepared,
			setup: func(t *testing.T, fs Fs, j *renameJournal) {
				writeContent(t, fs, j.OldName, "src")
				writeContent(t, fs, j.BackupName, "old-dst")
				writeContent(t, fs, j.TempName, "src")
			},
			contentAt: func(j renameJournal) string { return j.TempName },
			// The rollback restores the old destination; the retried request
			// then completes the replacement.
			reattempt: true,
			assert: func(t *testing.T, fs Fs, j renameJournal) {
				if exists, _ := Exists(fs, j.OldName); exists {
					t.Error("source should have been removed by the retried request")
				}
				if got := mustReadFile(t, fs, j.NewName); got != "src" {
					t.Errorf("destination = %q, want src", got)
				}
			},
		},
		{
			name:  "prepared-before-destination-moved-aside",
			phase: phasePrepared,
			// Crash window: the journal and verified temp copy are durable but
			// the existing destination has not yet been renamed to the backup.
			// Reconciliation must leave that destination untouched and then the
			// retried request completes the replacement.
			setup: func(t *testing.T, fs Fs, j *renameJournal) {
				writeContent(t, fs, j.OldName, "src")
				writeContent(t, fs, j.NewName, "old-dst")
				writeContent(t, fs, j.TempName, "src")
			},
			contentAt: func(j renameJournal) string { return j.TempName },
			reattempt: true,
			assert: func(t *testing.T, fs Fs, j renameJournal) {
				if exists, _ := Exists(fs, j.OldName); exists {
					t.Error("source should have been removed by the retried request")
				}
				if got := mustReadFile(t, fs, j.NewName); got != "src" {
					t.Errorf("destination = %q, want src", got)
				}
			},
		},
		{
			name:  "committed-rolls-forward",
			phase: phaseCommitted,
			setup: func(t *testing.T, fs Fs, j *renameJournal) {
				writeContent(t, fs, j.OldName, "src")
				writeContent(t, fs, j.NewName, "src")
				writeContent(t, fs, j.BackupName, "old-dst")
			},
			contentAt: func(j renameJournal) string { return j.NewName },
			assert: func(t *testing.T, fs Fs, j renameJournal) {
				if exists, _ := Exists(fs, j.OldName); exists {
					t.Error("source should have been removed")
				}
				if got := mustReadFile(t, fs, j.NewName); got != "src" {
					t.Errorf("destination = %q, want src", got)
				}
			},
		},
		{
			name:  "done-just-cleans",
			phase: phaseDone,
			setup: func(t *testing.T, fs Fs, j *renameJournal) {
				writeContent(t, fs, j.NewName, "src")
				writeContent(t, fs, j.BackupName, "leftover")
				writeContent(t, fs, j.TempName, "leftover")
			},
			contentAt: func(j renameJournal) string { return j.NewName },
			assert: func(t *testing.T, fs Fs, j renameJournal) {
				if got := mustReadFile(t, fs, j.NewName); got != "src" {
					t.Errorf("destination = %q", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := NewMemMapFs()
			dir := "/journal-" + tc.name
			oldName := dir + "/src.txt"
			newName := dir + "/dst.txt"
			// Use the production constructor so the scratch paths and the
			// journal name are exactly those a retried request will address.
			txn, err := newRenameTxn(fs, oldName, newName, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			j := &txn.j
			j.Phase = tc.phase
			tc.setup(t, fs, j)
			j.Checksum = sha256Hex(t, mustReadFile(t, fs, tc.contentAt(*j)))
			persistJournal(t, fs, *j)

			// The prepared case has to run the full cross-volume protocol on
			// the retried request, so force the visible rename to report EXDEV.
			// The other phases complete purely through reconciliation.
			var target Fs = fs
			if tc.reattempt {
				target = &failingFs{
					Fs:      fs,
					failErr: syscall.EXDEV,
					failRename: func(o, n string) bool {
						return o == j.OldName && n == j.NewName
					},
				}
			}

			if err := RecoverableRename(target, j.OldName, j.NewName); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			tc.assert(t, fs, *j)
			assertNoScratch(t, fs, dir)
		})
	}
}

// TestRecoverableRenameCrossVolumeDirectory rejects directory moves across
// volumes without mutating either path.
func TestRecoverableRenameCrossVolumeDirectory(t *testing.T) {
	fs := NewMemMapFs()
	from := "/dirsrc/child"
	to := "/dirdst/child"
	if err := fs.MkdirAll(from, 0o755); err != nil {
		t.Fatal(err)
	}
	injected := &failingFs{
		Fs:      fs,
		failErr: syscall.EXDEV,
		failRename: func(oldname, newname string) bool {
			return oldname == from && newname == to
		},
	}
	err := RecoverableRename(injected, from, to)
	if err == nil {
		t.Fatal("expected cross-device directory error")
	}
	if !errors.Is(err, errCrossDeviceDirectory) {
		t.Fatalf("err = %v, want errCrossDeviceDirectory", err)
	}
	if exists, _ := Exists(fs, from); !exists {
		t.Error("source directory was removed")
	}
	if exists, _ := Exists(fs, to); exists {
		t.Error("destination directory was created")
	}
}

// TestRecoverableRenameBasePathOwnership makes sure every scratch path stays
// inside the base path and outside paths are rejected like every other
// BasePathFs operation.
func TestRecoverableRenameBasePathOwnership(t *testing.T) {
	memFs := NewMemMapFs()
	base := "/sandbox"
	bp := NewBasePathFs(memFs, base)

	writeContent(t, bp, "src/file.txt", "jailed")
	writeContent(t, bp, "dst/file.txt", "old")

	if err := RecoverableRename(bp, "src/file.txt", "dst/file.txt"); err != nil {
		t.Fatalf("RecoverableRename: %v", err)
	}
	if got := mustReadFile(t, bp, "dst/file.txt"); got != "jailed" {
		t.Errorf("destination = %q", got)
	}

	// Nothing escaped the sandbox root.
	for p := range memFs.(*MemMapFs).getData() {
		if p != base && !strings.HasPrefix(p, base+"/") && p != "/" {
			t.Errorf("path %q escaped base %q", p, base)
		}
		if strings.Contains(p, ".afero-rr-") {
			t.Errorf("scratch artifact left behind: %q", p)
		}
	}

	// An outside destination is refused.
	if err := RecoverableRename(bp, "dst/file.txt", "../escape.txt"); err == nil {
		t.Fatal("expected escape attempt to fail")
	}
}

// TestRecoverableRenameReadOnly rejects the operation without side effects.
func TestRecoverableRenameReadOnly(t *testing.T) {
	fs := NewMemMapFs()
	writeContent(t, fs, "/ro.txt", "x")
	ro := NewReadOnlyFs(fs)
	if err := RecoverableRename(ro, "/ro.txt", "/ro2.txt"); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err = %v, want EPERM", err)
	}
	if got := mustReadFile(t, fs, "/ro.txt"); got != "x" {
		t.Errorf("source was modified: %q", got)
	}
}

// TestRecoverableRenameOnRealOsFsCrossDevice exercises the full protocol on
// two genuinely different devices (/tmp and /dev/shm on Linux CI hosts). It
// skips itself when those are not available as separate mounts.
func TestRecoverableRenameOnRealOsFsCrossDevice(t *testing.T) {
	srcRoot, err := os.MkdirTemp("/tmp", "afero-rr-src-")
	if err != nil {
		t.Skipf("cannot create /tmp dir: %v", err)
	}
	dstRoot, err := os.MkdirTemp("/dev/shm", "afero-rr-dst-")
	if err != nil {
		os.RemoveAll(srcRoot)
		t.Skipf("cannot create /dev/shm dir: %v", err)
	}
	defer os.RemoveAll(srcRoot)
	defer os.RemoveAll(dstRoot)

	srcStat, err := os.Stat(srcRoot)
	if err != nil {
		t.Skip(err)
	}
	dstStat, err := os.Stat(dstRoot)
	if err != nil {
		t.Skip(err)
	}
	if os.SameFile(srcStat, dstStat) {
		t.Skip("/tmp and /dev/shm are the same device here")
	}

	fs := NewOsFs()
	from := filepath.Join(srcRoot, "moved.txt")
	to := filepath.Join(dstRoot, "nested", "moved.txt")
	payload := strings.Repeat("abc123", 4096)
	if err := WriteFile(fs, from, []byte(payload), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(fs, filepath.Join(dstRoot, "nested-stub"), []byte("marker"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Pre-existing destination to exercise backup/restore paths.
	if err := fs.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(fs, to, []byte("old-content"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := RecoverableRename(fs, from, to); err != nil {
		t.Fatalf("cross-device RecoverableRename: %v", err)
	}
	if exists, _ := Exists(fs, from); exists {
		t.Error("source still exists")
	}
	if got := mustReadFile(t, fs, to); got != payload {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// Mode must be carried across the copy.
	info, err := fs.Stat(to)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 0640", info.Mode().Perm())
	}

	entries, err := os.ReadDir(filepath.Dir(to))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".afero-rr-") {
			t.Errorf("scratch artifact left behind: %q", e.Name())
		}
	}
}

// TestRecoverableRenameInterfaceConformance documents which backends expose
// the capability directly. MemMapFs deliberately relies on the generic
// journaled helper because its in-memory rename is already atomic.
func TestRecoverableRenameInterfaceConformance(t *testing.T) {
	var _ RecoverableRenamer = OsFs{}
	var _ RecoverableRenamer = &BasePathFs{}
	var _ RecoverableRenamer = &ReadOnlyFs{}

	if _, ok := any(&MemMapFs{}).(RecoverableRenamer); ok {
		t.Error("MemMapFs must not implement RecoverableRenamer so it keeps using the generic protocol")
	}

	// The package-level helper must still work for an Fs without the method.
	plain := &plainOnlyFs{Fs: NewMemMapFs()}
	writeContent(t, plain, "/p/a.txt", "hello")
	if err := RecoverableRename(plain, "/p/a.txt", "/p/b.txt"); err != nil {
		t.Fatalf("generic fallback: %v", err)
	}
	if got := mustReadFile(t, plain, "/p/b.txt"); got != "hello" {
		t.Errorf("got %q", got)
	}
}

// plainOnlyFs deliberately does not implement RecoverableRenamer.
type plainOnlyFs struct {
	Fs
}

func assertNoScratch(t *testing.T, fs Fs, dir string) {
	t.Helper()
	entries, err := ReadDir(fs, dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".afero-rr-") {
			t.Errorf("scratch artifact left in %q: %q", dir, e.Name())
		}
	}
}

func sha256Hex(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func persistJournal(t *testing.T, fs Fs, j renameJournal) {
	t.Helper()
	txn := &renameTxn{fs: fs, j: j}
	if err := txn.writeJournal(j.Phase); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
}

// TestRecoverableRenameConcurrentVisibility hammers the destination with
// concurrent reads while a cross-volume replacement runs. Every read must
// observe either the complete old content or the complete new content, never
// a partial or torn write.
func TestRecoverableRenameConcurrentVisibility(t *testing.T) {
	fs := NewMemMapFs()
	dir := "/concurrent"
	from := dir + "/src/file.txt"
	to := dir + "/dst/file.txt"
	oldContent := "OLD-" + strings.Repeat("o", 2048)
	newContent := "NEW-" + strings.Repeat("n", 2048)
	writeContent(t, fs, from, newContent)
	writeContent(t, fs, to, oldContent)

	injected := &failingFs{
		Fs:      fs,
		failErr: syscall.EXDEV,
		failRename: func(o, n string) bool {
			return o == from && n == to
		},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := ReadFile(fs, to)
				if err != nil {
					// A transient not-exist during the switch is acceptable;
					// torn content is not.
					if !os.IsNotExist(err) {
						t.Errorf("unexpected read error: %v", err)
						return
					}
					continue
				}
				s := string(data)
				if s != oldContent && s != newContent {
					t.Errorf("torn read: got %d bytes that match neither old nor new content", len(data))
					return
				}
			}
		}()
	}

	if err := RecoverableRename(injected, from, to); err != nil {
		t.Fatalf("RecoverableRename: %v", err)
	}
	close(stop)
	wg.Wait()

	if got := mustReadFile(t, fs, to); got != newContent {
		t.Errorf("final destination = %d bytes, want new content", len(got))
	}
}

// TestRecoverableRenameAferoWrapper exercises the Afero convenience struct so
// the method is reachable through the wrapper as well as the package helper.
func TestRecoverableRenameAferoWrapper(t *testing.T) {
	fs := Afero{Fs: NewMemMapFs()}
	writeContent(t, fs, "/wrap/a.txt", "wrapped")
	if err := fs.RecoverableRename("/wrap/a.txt", "/wrap/b.txt"); err != nil {
		t.Fatalf("Afero.RecoverableRename: %v", err)
	}
	if got := mustReadFile(t, fs, "/wrap/b.txt"); got != "wrapped" {
		t.Errorf("got %q", got)
	}
}

// TestRecoverableRenameBasePathCrossVolume makes sure a forced EXDEV on the
// underlying filesystem still completes through the BasePath wrapper, with
// every scratch path confined inside the base directory.
func TestRecoverableRenameBasePathCrossVolume(t *testing.T) {
	root := NewMemMapFs()
	base := "/bp"
	bp := NewBasePathFs(root, base)

	writeContent(t, bp, "src/f.txt", "cross")
	writeContent(t, bp, "dst/f.txt", "old")

	// Inject EXDEV underneath the BasePath wrapper by wrapping the underlying
	// memory fs before constructing the wrapper.
	injectedRoot := &failingFs{
		Fs:      root,
		failErr: syscall.EXDEV,
		failRename: func(o, n string) bool {
			return o == base+"/src/f.txt" && n == base+"/dst/f.txt"
		},
	}
	bp = NewBasePathFs(injectedRoot, base)

	if err := RecoverableRename(bp, "src/f.txt", "dst/f.txt"); err != nil {
		t.Fatalf("RecoverableRename through BasePath: %v", err)
	}
	if got := mustReadFile(t, bp, "dst/f.txt"); got != "cross" {
		t.Errorf("destination = %q, want cross", got)
	}
	if exists, _ := Exists(bp, "src/f.txt"); exists {
		t.Error("source still exists")
	}
	// All scratch paths must live under the base directory.
	for p := range root.(*MemMapFs).getData() {
		if strings.Contains(p, ".afero-rr-") {
			t.Errorf("scratch artifact left behind: %q", p)
		}
		if !strings.HasPrefix(p, base) && p != "/" {
			t.Errorf("path %q escaped base %q", p, base)
		}
	}
}
