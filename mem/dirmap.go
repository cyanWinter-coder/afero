// Copyright © 2015 Steve Francia <spf@spf13.com>.
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

import "sort"

// DirMap is the mutable directory-entry table stored inside a directory
// FileData. It is updated copy-on-write: Add and Replace build a fresh map
// and swap it in while the owning FileData mutex is held, so a reader that
// snapshotted the previous map keeps walking a complete, immutable view and
// never observes a partially applied rename or removal.
type DirMap map[string]*FileData

func (m DirMap) Len() int { return len(m) }

// Add inserts f under its current name. The caller must hold the owning
// FileData mutex.
func (m *DirMap) Add(f *FileData) {
	next := make(DirMap, len(*m)+1)
	for name, entry := range *m {
		next[name] = entry
	}
	f.Lock()
	name := f.name
	f.Unlock()
	next[name] = f
	*m = next
}

// Remove deletes the entry named by f's current name. The caller must hold
// the owning FileData mutex. Removing a name that is no longer present is a
// no-op, which keeps rename/remove idempotent under the filesystem lock.
func (m *DirMap) Remove(f *FileData) {
	f.Lock()
	name := f.name
	f.Unlock()
	if _, ok := (*m)[name]; !ok {
		return
	}
	next := make(DirMap, len(*m)-1)
	for k, v := range *m {
		if k != name {
			next[k] = v
		}
	}
	*m = next
}

// Replace moves the entry keyed by oldName to newName in a single
// copy-on-write swap. If oldName is absent nothing changes. The caller must
// hold the owning FileData mutex.
func (m *DirMap) Replace(oldName, newName string, f *FileData) {
	if _, ok := (*m)[oldName]; !ok {
		return
	}
	next := make(DirMap, len(*m))
	for k, v := range *m {
		if k != oldName {
			next[k] = v
		}
	}
	next[newName] = f
	*m = next
}

// Files returns a sorted snapshot of the current entries. The map itself is
// read under the owning FileData mutex; the returned slice is immutable and
// safe to use after the lock is released.
func (m *DirMap) Files() (files []*FileData) {
	snapshot := m.snapshot()
	files = make([]*FileData, 0, len(snapshot))
	for _, f := range snapshot {
		files = append(files, f)
	}
	sort.Sort(filesSorter(files))
	return files
}

// Names returns a snapshot of the current entry names. The caller must not
// mutate the returned slice.
func (m *DirMap) Names() (names []string) {
	snapshot := m.snapshot()
	names = make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	return names
}

// snapshot copies the entry table under the owning FileData mutex. Every
// map read goes through here so concurrent Add/Remove/Replace swaps can
// never race an iteration.
func (m *DirMap) snapshot() DirMap {
	// DirMap instances are only ever touched through the owning FileData
	// mutex (see dir.go), which the caller is expected to hold.
	current := *m
	snapshot := make(DirMap, len(current))
	for k, v := range current {
		snapshot[k] = v
	}
	return snapshot
}

// implement sort.Interface for []*FileData
type filesSorter []*FileData

func (s filesSorter) Len() int      { return len(s) }
func (s filesSorter) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s filesSorter) Less(i, j int) bool {
	// FileData.Name takes the per-file lock; sorting happens on a snapshot
	// taken by Files, so a concurrent ChangeFileName cannot race here.
	return s[i].Name() < s[j].Name()
}
