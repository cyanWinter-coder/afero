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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// RecoverableRenamer is an optional interface implemented by filesystems
// that can rename (replace) a file with crash-safe, recoverable semantics.
//
// A successful RecoverableRename has the same externally visible effect as
// Rename: the file oldname becomes visible as newname and any previous file
// at newname is replaced. The difference is what happens when the operation
// is interrupted by an I/O error or a process crash:
//
//   - On the same volume the rename stays a single atomic Rename call, so
//     observers either see the old state or the complete new state.
//   - Across volumes, where an atomic rename is impossible, the new content
//     is first written to a hidden temporary file next to the destination and
//     verified, and only then switched into place. A journal stored on the
//     filesystem allows a retry of the same request to finish or undo the
//     interrupted operation instead of leaving a half-renamed file.
//
// After a successful call, retrying the same (oldname, newname) pair is a
// no-op. After a failed call, retrying either completes the replacement or
// restores the original source and destination and then returns an error.
type RecoverableRenamer interface {
	RecoverableRename(oldname, newname string) error
}

// RecoverableRename renames oldname to newname with recoverable replacement
// semantics.
//
// If fs implements RecoverableRenamer its implementation is used directly.
// Otherwise a generic, journal-based implementation built on the Fs
// primitives is used, which works with any conforming filesystem.
//
// Only regular files are supported across volumes; renaming a directory
// across volumes returns an error without modifying either path. On the same
// volume directories are handled by Rename.
//
// Files already opened by the caller keep working and keep seeing their
// original bytes for the lifetime of their handles. New opens observe either
// the complete old file or the complete new file, never a partial one.
func RecoverableRename(fs Fs, oldname, newname string) error {
	if rr, ok := fs.(RecoverableRenamer); ok {
		return rr.RecoverableRename(oldname, newname)
	}
	return genericRecoverableRename(fs, oldname, newname)
}

// RecoverableRename is the Afero wrapper for the package-level
// RecoverableRename helper.
func (a Afero) RecoverableRename(oldname, newname string) error {
	return RecoverableRename(a.Fs, oldname, newname)
}

// Rename journal phases. The phase recorded inside the journal is the point
// the journal itself is durable, so on recovery every step strictly before
// the recorded phase is known to have completed.
const (
	// phasePrepared: source has been copied to the temporary destination and
	// any pre-existing destination has been moved aside as backup.
	phasePrepared = "prepared"
	// phaseCommitted: the temporary destination has been switched to the
	// visible name; the source still has to be removed.
	phaseCommitted = "committed"
	// phaseDone: the replacement is fully visible and all scratch files are
	// about to be removed.
	phaseDone = "done"
)

// renameJournal is persisted next to the destination as the durable record
// that makes an interrupted cross-volume rename recoverable.
type renameJournal struct {
	OldName    string `json:"oldName"`
	NewName    string `json:"newName"`
	TempName   string `json:"tempName"`
	BackupName string `json:"backupName,omitempty"`
	Checksum   string `json:"checksum"`
	Mode       uint32 `json:"mode"`
	Phase      string `json:"phase"`
}

// genericRecoverableRename is the journal-based fallback used when an Fs does
// not provide its own RecoverableRename. OsFs routes through this as well:
// Rename is attempted first so same-volume moves stay a single kernel-level
// atomic operation, and only a cross-device error escalates to the
// copy-and-switch protocol.
func genericRecoverableRename(fs Fs, oldname, newname string) error {
	if oldname == newname {
		if _, err := fs.Stat(oldname); err != nil {
			return err
		}
		return nil
	}

	journalPath := renameJournalPath(oldname, newname)

	// Finish or undo an operation interrupted in a previous invocation. With
	// deterministic, content-addressed scratch names a retried request finds
	// the same journal, so recovery doubles as idempotency. A rolled-back
	// "prepared" transaction leaves the original state intact and the request
	// is re-attempted below; a rolled-forward or cleaned-up transaction has
	// already satisfied the request.
	if exists, err := Exists(fs, journalPath); err != nil {
		return err
	} else if exists {
		completed, err := reconcileRename(fs, journalPath)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
	}

	// Fast path: one atomic rename covers same-volume moves, overwrites and
	// directories. Cross-device renames are the only case that needs the
	// journal protocol.
	err := fs.Rename(oldname, newname)
	if err == nil || !isCrossDeviceError(err) {
		return err
	}

	info, err := fs.Stat(oldname)
	if err != nil {
		return err
	}
	if info.IsDir() {
		// A recursive copy would have to journal a whole tree; keep the
		// failure atomic and let the caller recurse explicitly if needed.
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: errCrossDeviceDirectory}
	}

	txn, err := newRenameTxn(fs, oldname, newname, info.Mode())
	if err != nil {
		return err
	}
	journalPath = txn.journalPath()

	// prepare runs every step up to, but not including, the visibility
	// switch. Any failure here must leave oldname and newname exactly as they
	// were before the call (rollback restores a moved-aside destination).
	if err := txn.prepare(fs, oldname, newname); err != nil {
		return errors.Join(err, txn.rollbackPrepared())
	}

	// Commit point. From here on the destination already carries the complete
	// new content and failures are rolled forward (source removal and
	// cleanup), never backwards: a restored old destination would be a second
	// observable mutation after one already became visible.
	if err := fs.Rename(txn.j.TempName, newname); err != nil {
		return errors.Join(err, txn.rollbackPrepared())
	}

	if err := txn.writeJournal(phaseCommitted); err != nil {
		// The switch happened but its journal may not be durable. As long as
		// the bytes are the verified ones the replacement still stands; the
		// next invocation's reconcileRename finishes from the "prepared"
		// journal.
		if ok, vErr := txn.destinationMatches(); vErr != nil || !ok {
			return errors.Join(err, vErr)
		}
	}

	// Roll forward: the destination is authoritative now.
	if err := fs.Remove(oldname); err != nil && !os.IsNotExist(err) {
		return err
	}

	if mode := os.FileMode(txn.j.Mode); mode != info.Mode() {
		if err := fs.Chmod(newname, mode); err != nil {
			return err
		}
	}
	if !info.ModTime().IsZero() {
		_ = fs.Chtimes(newname, info.ModTime(), info.ModTime())
	}

	if err := txn.writeJournal(phaseDone); err != nil {
		return err
	}
	return txn.cleanupLeftovers()
}

// prepare performs every step that happens while newname still shows its old
// contents: scratch cleanup, directory creation, the verified copy into the
// hidden temporary file, the durable "prepared" journal and finally moving
// any existing destination aside as the backup.
func (t *renameTxn) prepare(fs Fs, oldname, newname string) error {
	// A previous attempt may have left scratch files behind without creating
	// a journal. Remove them while the paths are still hidden.
	if err := t.cleanupLeftovers(); err != nil {
		return err
	}

	if err := fs.MkdirAll(filepath.Dir(newname), 0o777); err != nil {
		return err
	}

	sum, err := t.copyAndVerify()
	if err != nil {
		return err
	}
	t.j.Checksum = sum

	// Persist the journal BEFORE moving the existing destination aside. This
	// establishes the invariant that a backup file can only exist while a
	// journal exists, so a crash between the two steps leaves the next
	// invocation able to find and restore the backup instead of treating it
	// as orphaned scratch and deleting it. At this point both visible names
	// are still exactly as the caller left them.
	if err := t.writeJournal(phasePrepared); err != nil {
		return err
	}

	// Preserve the replaced destination so it can be put back on rollback.
	if exists, statErr := Exists(fs, newname); statErr != nil {
		return statErr
	} else if exists {
		if err := fs.Rename(newname, t.j.BackupName); err != nil {
			return err
		}
	}
	return nil
}

// renameTxn holds the working state of one cross-volume replacement.
type renameTxn struct {
	fs Fs
	j  renameJournal
}

func newRenameTxn(fs Fs, oldname, newname string, mode os.FileMode) (*renameTxn, error) {
	id := renameID(oldname, newname)
	dir := filepath.Dir(newname)
	j := renameJournal{
		OldName:    oldname,
		NewName:    newname,
		TempName:   filepath.Join(dir, ".afero-rr-"+id+".tmp"),
		BackupName: filepath.Join(dir, ".afero-rr-"+id+".bak"),
		Mode:       uint32(mode.Perm()),
	}
	return &renameTxn{fs: fs, j: j}, nil
}

// renameID derives a stable identifier for an (oldname, newname) pair so a
// retried request addresses exactly the scratch files its first attempt used.
func renameID(oldname, newname string) string {
	sum := sha256.Sum256([]byte(oldname + "\x00" + newname))
	return hex.EncodeToString(sum[:16])
}

func renameJournalPath(oldname, newname string) string {
	return filepath.Join(filepath.Dir(newname), ".afero-rr-"+renameID(oldname, newname)+".journal")
}

func (t *renameTxn) journalPath() string {
	return renameJournalPath(t.j.OldName, t.j.NewName)
}

// copyAndVerify streams oldname into the hidden temporary destination,
// syncing and check-summing the result before the destination name is ever
// touched.
func (t *renameTxn) copyAndVerify() (string, error) {
	src, err := t.fs.Open(t.j.OldName)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := t.fs.OpenFile(t.j.TempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(t.j.Mode))
	if err != nil {
		return "", err
	}

	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(dst, hash), src)
	syncErr := dst.Sync()
	closeErr := dst.Close()
	if copyErr != nil {
		_ = t.fs.Remove(t.j.TempName)
		return "", copyErr
	}
	if syncErr != nil {
		_ = t.fs.Remove(t.j.TempName)
		return "", syncErr
	}
	if closeErr != nil {
		_ = t.fs.Remove(t.j.TempName)
		return "", closeErr
	}

	sum := hex.EncodeToString(hash.Sum(nil))
	if ok, err := t.fileChecksumMatches(t.j.TempName, sum); err != nil || !ok {
		_ = t.fs.Remove(t.j.TempName)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("recoverable rename: verification of %q failed", t.j.TempName)
	}
	return sum, nil
}

// fileChecksumMatches re-opens and re-hashes a file, guarding against a write
// that reported success while the bytes did not land completely.
func (t *renameTxn) fileChecksumMatches(name, want string) (bool, error) {
	f, err := t.fs.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == want, nil
}

func (t *renameTxn) destinationMatches() (bool, error) {
	return t.fileChecksumMatches(t.j.NewName, t.j.Checksum)
}

// writeJournal atomically persists the journal at the given phase by writing a
// fresh file and renaming it over any previous journal. Readers therefore
// always see a complete journal or none at all.
func (t *renameTxn) writeJournal(phase string) error {
	t.j.Phase = phase
	payload, err := json.Marshal(&t.j)
	if err != nil {
		return err
	}

	stage := t.journalPath() + ".stage"
	f, err := t.fs.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = t.fs.Remove(stage)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = t.fs.Remove(stage)
		return err
	}
	if err := f.Close(); err != nil {
		_ = t.fs.Remove(stage)
		return err
	}
	return t.fs.Rename(stage, t.journalPath())
}

// rollbackPrepared undoes a transaction that has not durably committed. The
// source file is never removed before the commit point, so it only has to be
// left in place; the work is restoring the displaced destination and removing
// every scratch file.
//
// The sequence tolerates the visibility switch having partially happened
// (Rename returned an error after the kernel moved the name): if newname holds
// the new content it is removed before the backup is moved back.
func (t *renameTxn) rollbackPrepared() error {
	var errs []error

	// 1. Drop the new content if it has already become visible, so the backup
	// can reclaim its original name.
	if ok, err := t.destinationMatches(); err == nil && ok {
		if err := t.fs.Remove(t.j.NewName); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}

	// 2. Put the displaced destination back. When there was no previous
	// destination, newname simply stays absent.
	if exists, err := Exists(t.fs, t.j.BackupName); err != nil {
		errs = append(errs, err)
	} else if exists {
		if err := t.fs.Rename(t.j.BackupName, t.j.NewName); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}

	// 3. Remove the hidden temporary copy and any staged journal.
	if err := t.fs.Remove(t.j.TempName); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	if err := t.fs.Remove(t.journalPath() + ".stage"); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}

	// 4. Remove the journal last: its presence is what lets another retry find
	// this transaction.
	if err := t.fs.Remove(t.journalPath()); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// cleanupLeftovers removes every scratch path belonging to this transaction.
func (t *renameTxn) cleanupLeftovers() error {
	var errs []error
	for _, p := range []string{
		t.j.TempName,
		t.j.BackupName,
		t.journalPath(),
		t.journalPath() + ".stage",
	} {
		if err := t.fs.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileRename resumes an operation whose journal survived an interruption.
// It returns completed=true when the request has been satisfied (rolled
// forward or fully cleaned up), and completed=false when the interruption
// happened before the visibility switch and the original state was restored,
// in which case the caller re-attempts the whole request.
func reconcileRename(fs Fs, journalPath string) (bool, error) {
	f, err := fs.Open(journalPath)
	if err != nil {
		return false, err
	}
	payload, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return false, err
	}

	var j renameJournal
	if err := json.Unmarshal(bytes.TrimSpace(payload), &j); err != nil {
		return false, fmt.Errorf("recoverable rename: unreadable journal %q: %w", journalPath, err)
	}

	txn := &renameTxn{fs: fs, j: j}

	switch j.Phase {
	case phaseDone:
		return true, txn.cleanupLeftovers()
	case phaseCommitted:
		// The switch is durable; either finish removing the source or, if the
		// destination is somehow wrong, restore the backup.
		ok, err := txn.destinationMatches()
		if err != nil {
			return false, err
		}
		if !ok {
			return false, txn.restoreBackup()
		}
		if err := fs.Remove(j.OldName); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		return true, txn.cleanupLeftovers()

	case phasePrepared:
		// The visible names were never switched; verify whether the switch in
		// fact happened without a durable "committed" journal, then roll
		// forward or back accordingly.
		ok, err := txn.destinationMatches()
		switch {
		case err == nil && ok:
			if err := fs.Remove(j.OldName); err != nil && !os.IsNotExist(err) {
				return false, err
			}
			return true, txn.cleanupLeftovers()
		case err != nil && !os.IsNotExist(err):
			return false, err
		default:
			// Restore the original state and let the caller run the request
			// again from scratch against the restored files.
			return false, txn.rollbackPrepared()
		}

	default:
		return false, fmt.Errorf("recoverable rename: journal %q has unknown phase %q", journalPath, j.Phase)
	}
}

// restoreBackup puts the displaced destination back in place and removes the
// scratch files.
func (t *renameTxn) restoreBackup() error {
	var errs []error
	if exists, err := Exists(t.fs, t.j.BackupName); err != nil {
		return err
	} else if exists {
		if err := t.fs.Remove(t.j.NewName); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		if err := t.fs.Rename(t.j.BackupName, t.j.NewName); err != nil {
			errs = append(errs, err)
		}
	}
	if err := t.cleanupLeftovers(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// errCrossDeviceDirectory is returned for cross-volume directory renames,
// which the journal protocol does not attempt.
var errCrossDeviceDirectory = errors.New("cross-device rename of directories is not supported; rename directories within the same volume")

// isCrossDeviceError reports whether err is the platform's "rename across
// mounted filesystems" error. Unix surfaces EXDEV; Windows surfaces
// ERROR_NOT_SAME_DEVICE (17).
func isCrossDeviceError(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	if runtime.GOOS == "windows" {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == 17 {
			return true
		}
	}
	return false
}
