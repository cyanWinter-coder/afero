// Copyright © 2014 Steve Francia <spf@spf13.com>.
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
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/afero/mem"
)

const chmodBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky // Only a subset of bits are allowed to be changed. Documented under os.Chmod()

type MemMapFs struct {
	mu   sync.RWMutex
	data map[string]*mem.FileData
	// version is the structural generation of the namespace. Every
	// committed Create/Mkdir/Remove/RemoveAll/Rename bumps it exactly once;
	// open handles record the value they were minted with.
	version atomic.Uint64
	init    sync.Once
}

func NewMemMapFs() Fs {
	return &MemMapFs{}
}

func (m *MemMapFs) getData() map[string]*mem.FileData {
	m.init.Do(func() {
		m.data = make(map[string]*mem.FileData)
		// Root should always exist, right?
		// TODO: what about windows?
		root := mem.CreateDir(FilePathSeparator)
		mem.SetMode(root, os.ModeDir|0o755)
		m.data[FilePathSeparator] = root
	})
	return m.data
}

// nextVersion marks one committed structural change and returns the new
// generation. Callers must hold m.mu (write side).
func (m *MemMapFs) nextVersion() uint64 {
	return m.version.Add(1)
}

// currentVersion reports the namespace generation without mutating it.
func (m *MemMapFs) currentVersion() uint64 {
	return m.version.Load()
}

// stampVersion records the current structural generation on a freshly
// minted handle so the handle carries the version frozen at open time.
func (m *MemMapFs) stampVersion(f *mem.File) *mem.File {
	return f.WithOpenVersion(m.currentVersion())
}

func (*MemMapFs) Name() string { return "MemMapFS" }

func (m *MemMapFs) Create(name string) (File, error) {
	name = normalizePath(name)
	m.mu.Lock()
	file := mem.CreateFile(name)
	mem.SetMode(file, 0o666)
	m.getData()[name] = file
	m.attachLocked(file, 0)
	// Stamp inside the critical section so the handle's captured version is
	// exactly the generation produced by this creation.
	version := m.nextVersion()
	handle := mem.NewFileHandle(file).WithOpenVersion(version)
	m.mu.Unlock()
	return handle, nil
}

// detachLocked removes f from its parent directory's entry table. It must
// be called with m.mu held; the parent's own mutex serializes the
// copy-on-write map swap against directory snapshots.
func (m *MemMapFs) detachLocked(f *mem.FileData) {
	parent := m.findParent(f)
	if parent == nil {
		log.Panic("parent of ", f.Name(), " is nil")
	}
	parent.Lock()
	mem.RemoveFromMemDir(parent, f)
	parent.Unlock()
}

// attachLocked inserts f into its parent directory's entry table, creating
// missing ancestor directories first. It must be called with m.mu held.
func (m *MemMapFs) attachLocked(f *mem.FileData, perm os.FileMode) {
	if f == nil {
		return
	}
	parent := m.findParent(f)
	if parent == nil {
		pdir := filepath.Dir(filepath.Clean(f.Name()))
		mkdirPerm := perm
		if mkdirPerm == 0 {
			mkdirPerm = 0o755
		}
		err := m.mkdirLocked(pdir, mkdirPerm)
		if err != nil {
			// log.Println("Mkdir error:", err)
			return
		}
		parent, err = m.openLocked(pdir)
		if err != nil {
			// log.Println("Open after Mkdir error:", err)
			return
		}
	}

	parent.Lock()
	mem.InitializeDir(parent)
	mem.AddToMemDir(parent, f)
	parent.Unlock()
}

func (m *MemMapFs) findParent(f *mem.FileData) *mem.FileData {
	pdir, _ := filepath.Split(f.Name())
	pdir = filepath.Clean(pdir)
	pfile, err := m.openLocked(pdir)
	if err != nil {
		return nil
	}
	return pfile
}

// findDescendantsLocked returns every node below name, ordered shallow
// first so parent directories are always processed before their children.
// The snapshot of keys is taken up front; callers hold m.mu so the map
// cannot change while the slice is built.
func (m *MemMapFs) findDescendantsLocked(name string) []*mem.FileData {
	fData := m.getData()
	descendants := make([]*mem.FileData, 0, len(fData))
	for p, dFile := range fData {
		if strings.HasPrefix(p, name+FilePathSeparator) {
			descendants = append(descendants, dFile)
		}
	}

	sort.Slice(descendants, func(i, j int) bool {
		cur := len(strings.Split(descendants[i].Name(), FilePathSeparator))
		next := len(strings.Split(descendants[j].Name(), FilePathSeparator))
		return cur < next
	})

	return descendants
}

// mkdirLocked creates a directory without touching m.mu; every caller must
// already hold the write lock. Returning ErrFileExists for a non-directory
// path preserves the historical behavior of the old lockfree mkdir path.
func (m *MemMapFs) mkdirLocked(name string, perm os.FileMode) error {
	name = normalizePath(name)
	x, ok := m.getData()[name]
	if ok {
		// Only return ErrFileExists if it's a file, not a directory.
		i := mem.FileInfo{FileData: x}
		if !i.IsDir() {
			return ErrFileExists
		}
	} else {
		item := mem.CreateDir(name)
		mem.SetMode(item, os.ModeDir|perm)
		m.getData()[name] = item
		m.attachLocked(item, perm)
	}
	return nil
}

func (m *MemMapFs) Mkdir(name string, perm os.FileMode) error {
	perm &= chmodBits
	name = normalizePath(name)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Single critical section: existence check and insertion are atomic, so
	// concurrent Mkdir callers cannot both pass the check and clobber one
	// another's directory entry.
	if _, ok := m.getData()[name]; ok {
		return &os.PathError{Op: "mkdir", Path: name, Err: ErrFileExists}
	}
	item := mem.CreateDir(name)
	mem.SetMode(item, os.ModeDir|perm)
	m.getData()[name] = item
	m.attachLocked(item, perm)
	m.nextVersion()

	return m.setFileModeLocked(name, perm|os.ModeDir)
}

func (m *MemMapFs) MkdirAll(path string, perm os.FileMode) error {
	err := m.Mkdir(path, perm)
	if err != nil {
		if err.(*os.PathError).Err == ErrFileExists {
			return nil
		}
		return err
	}
	return nil
}

// Handle some relative paths
func normalizePath(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)

	switch path {
	case ".":
		return FilePathSeparator
	case "..":
		return FilePathSeparator
	}

	// If path isn't rooted make it rooted
	if !strings.HasPrefix(path, FilePathSeparator) {
		path = FilePathSeparator + path
	}
	return path
}

func (m *MemMapFs) Open(name string) (File, error) {
	f, err := m.open(name)
	if f != nil {
		// Check read permission (owner bits).
		if mem.GetFileInfo(f).Mode().Perm()&0o444 == 0 {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		return m.stampVersion(mem.NewReadOnlyFileHandle(f)), err
	}
	return nil, err
}

func (m *MemMapFs) openWrite(name string) (File, error) {
	f, err := m.open(name)
	if f != nil {
		// Check write permission (owner bits).
		if mem.GetFileInfo(f).Mode().Perm()&0o222 == 0 {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		return m.stampVersion(mem.NewFileHandle(f)), err
	}
	return nil, err
}

func (m *MemMapFs) open(name string) (*mem.FileData, error) {
	name = normalizePath(name)

	m.mu.RLock()
	f, ok := m.getData()[name]
	m.mu.RUnlock()
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrFileNotFound}
	}
	return f, nil
}

// openLocked looks up a node while m.mu is already held (either side).
func (m *MemMapFs) openLocked(name string) (*mem.FileData, error) {
	name = normalizePath(name)
	f, ok := m.getData()[name]
	if ok {
		return f, nil
	}
	return nil, ErrFileNotFound
}

func (m *MemMapFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	perm &= chmodBits
	chmod := false
	file, err := m.openWrite(name)
	if err == nil && (flag&os.O_EXCL > 0) {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrFileExists}
	}
	if os.IsNotExist(err) && (flag&os.O_CREATE > 0) {
		file, err = m.Create(name)
		chmod = true
	}
	if err != nil {
		return nil, err
	}
	if flag == os.O_RDONLY {
		file = m.stampVersion(mem.NewReadOnlyFileHandle(file.(*mem.File).Data()))
	}
	if flag&os.O_APPEND > 0 {
		_, err = file.Seek(0, io.SeekEnd)
		if err != nil {
			file.Close()
			return nil, err
		}
	}
	if flag&os.O_TRUNC > 0 && flag&(os.O_RDWR|os.O_WRONLY) > 0 {
		err = file.Truncate(0)
		if err != nil {
			file.Close()
			return nil, err
		}
	}
	if chmod {
		return file, m.setFileMode(name, perm)
	}
	return file, nil
}

func (m *MemMapFs) Remove(name string) error {
	name = normalizePath(name)

	m.mu.Lock()
	defer m.mu.Unlock()

	node, ok := m.getData()[name]
	if !ok {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
	}
	m.detachLocked(node)
	delete(m.getData(), name)
	m.nextVersion()
	return nil
}

func (m *MemMapFs) RemoveAll(path string) error {
	path = normalizePath(path)

	m.mu.Lock()
	defer m.mu.Unlock()

	data := m.getData()

	// Snapshot everything that belongs to the subtree, then mutate: no map
	// entry can be added or removed between the decision and the deletion,
	// and concurrent readers either see the complete old subtree or none of
	// it. Nodes whose parent is removed in the same sweep need no parent
	// table update of their own.
	removing := make(map[string]bool)
	for p := range data {
		if p == path || strings.HasPrefix(p, path+FilePathSeparator) {
			removing[p] = true
		}
	}
	if len(removing) == 0 {
		// os.RemoveAll treats a missing target as success.
		return nil
	}

	for p := range removing {
		node := data[p]
		parentPath := filepath.Clean(filepath.Dir(p))
		if !removing[parentPath] {
			m.detachLocked(node)
		}
		delete(data, p)
	}
	m.nextVersion()
	return nil
}

func (m *MemMapFs) Rename(oldname, newname string) error {
	oldname = normalizePath(oldname)
	newname = normalizePath(newname)

	if oldname == newname {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	data := m.getData()
	root, ok := data[oldname]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldname, Err: ErrFileNotFound}
	}

	// Moving a directory below itself ("/a" -> "/a/b") would create an
	// unreachable cycle; reject it before touching any entry table.
	if strings.HasPrefix(newname, oldname+FilePathSeparator) {
		return &os.PathError{
			Op:   "rename",
			Path: newname,
			Err:  os.ErrInvalid,
		}
	}

	// Validate the destination parent up front, while no mutation has
	// happened. attachLocked would otherwise try to create a missing parent
	// while this call already holds the write lock.
	newParentPath := filepath.Clean(filepath.Dir(newname))
	if _, err := m.openLocked(newParentPath); err != nil {
		return &os.PathError{Op: "rename", Path: newname, Err: err}
	}

	// Phase 1: plan every key move against the immutable current view.
	type move struct {
		node                   *mem.FileData
		oldName, newName       string
		oldParent, newParent   string
		parentMovesWithSubtree bool
		sameParent             bool
	}
	planMoves := func() []move {
		out := []move{{
			node:       root,
			oldName:    oldname,
			newName:    newname,
			oldParent:  filepath.Clean(filepath.Dir(oldname)),
			newParent:  newParentPath,
			sameParent: filepath.Clean(filepath.Dir(oldname)) == newParentPath,
		}}
		for _, desc := range m.findDescendantsLocked(oldname) {
			from := desc.Name()
			to := strings.Replace(from, oldname, newname, 1)
			oldParent := filepath.Clean(filepath.Dir(from))
			newParent := filepath.Clean(filepath.Dir(to))
			out = append(out, move{
				node:                   desc,
				oldName:                from,
				newName:                to,
				oldParent:              oldParent,
				newParent:              newParent,
				parentMovesWithSubtree: strings.HasPrefix(oldParent, oldname+FilePathSeparator),
				sameParent:             oldParent == newParent,
			})
		}
		return out
	}
	moves := planMoves()

	// Phase 2: detach every moved node from its old parent table. Nodes
	// inside the moved subtree keep their table relationships, since the
	// whole parent table moves with them; same-parent renames are handled by
	// an atomic key replace in phase 4 and must not detach here.
	for _, mv := range moves {
		if !mv.parentMovesWithSubtree && !mv.sameParent {
			m.detachLocked(mv.node)
		}
	}

	// Phase 3: repoint the namespace keys and the inode path labels.
	for _, mv := range moves {
		mem.ChangeFileName(mv.node, mv.newName)
		data[mv.newName] = mv.node
		delete(data, mv.oldName)
	}

	// Phase 4: update the parent entry tables. A same-parent rename swaps
	// the key in one copy-on-write operation (atomic, no transient gap);
	// cross-parent moves attach into the new parent's table.
	for _, mv := range moves {
		if mv.parentMovesWithSubtree {
			continue
		}
		parent := data[mv.newParent]
		parent.Lock()
		if mv.sameParent {
			mem.ReplaceInMemDir(parent, mv.oldName, mv.newName, mv.node)
		} else {
			mem.InitializeDir(parent)
			mem.AddToMemDir(parent, mv.node)
		}
		parent.Unlock()
	}

	m.nextVersion()
	return nil
}

func (m *MemMapFs) LstatIfPossible(name string) (os.FileInfo, bool, error) {
	fileInfo, err := m.Stat(name)
	return fileInfo, false, err
}

func (m *MemMapFs) Stat(name string) (os.FileInfo, error) {
	f, err := m.open(name)
	if err != nil {
		return nil, err
	}
	fi := mem.GetFileInfo(f)
	return fi, nil
}

func (m *MemMapFs) Chmod(name string, mode os.FileMode) error {
	mode &= chmodBits
	name = normalizePath(name)

	m.mu.RLock()
	f, ok := m.getData()[name]
	m.mu.RUnlock()
	if !ok {
		return &os.PathError{Op: "chmod", Path: name, Err: ErrFileNotFound}
	}
	prevOtherBits := mem.GetFileInfo(f).Mode() & ^chmodBits

	mode = prevOtherBits | mode
	return m.setFileMode(name, mode)
}

// setFileModeLocked changes mode bits while m.mu is already held.
func (m *MemMapFs) setFileModeLocked(name string, mode os.FileMode) error {
	name = normalizePath(name)
	f, ok := m.getData()[name]
	if !ok {
		return &os.PathError{Op: "chmod", Path: name, Err: ErrFileNotFound}
	}

	mem.SetMode(f, mode)
	return nil
}

func (m *MemMapFs) setFileMode(name string, mode os.FileMode) error {
	name = normalizePath(name)

	m.mu.RLock()
	f, ok := m.getData()[name]
	m.mu.RUnlock()
	if !ok {
		return &os.PathError{Op: "chmod", Path: name, Err: ErrFileNotFound}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	mem.SetMode(f, mode)
	return nil
}

func (m *MemMapFs) Chown(name string, uid, gid int) error {
	name = normalizePath(name)

	m.mu.RLock()
	f, ok := m.getData()[name]
	m.mu.RUnlock()
	if !ok {
		return &os.PathError{Op: "chown", Path: name, Err: ErrFileNotFound}
	}

	mem.SetUID(f, uid)
	mem.SetGID(f, gid)

	return nil
}

func (m *MemMapFs) Chtimes(name string, atime time.Time, mtime time.Time) error {
	name = normalizePath(name)

	m.mu.RLock()
	f, ok := m.getData()[name]
	m.mu.RUnlock()
	if !ok {
		return &os.PathError{Op: "chtimes", Path: name, Err: ErrFileNotFound}
	}

	m.mu.Lock()
	mem.SetModTime(f, mtime)
	m.mu.Unlock()

	return nil
}

func (m *MemMapFs) List() {
	for _, x := range m.data {
		y := mem.FileInfo{FileData: x}
		fmt.Println(x.Name(), y.Size())
	}
}
