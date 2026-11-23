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

package mem

// Dir is the directory-entry table abstraction used by a directory
// FileData. The concrete implementation, DirMap, updates itself
// copy-on-write so that readers either observe the table before a mutation
// or after it, never a half-applied state.
type Dir interface {
	Len() int
	Names() []string
	Files() []*FileData
	Add(*FileData)
	Remove(*FileData)
}

// Compile-time guarantee that the copy-on-write map keeps satisfying Dir.
var _ Dir = (*DirMap)(nil)

func RemoveFromMemDir(dir *FileData, f *FileData) {
	dir.memDir.Remove(f)
}

func AddToMemDir(dir *FileData, f *FileData) {
	dir.memDir.Add(f)
}

// ReplaceInMemDir renames a single directory entry atomically. Only
// DirMap-backed directories support the in-place key swap; other Dir
// implementations fall back to remove + add.
func ReplaceInMemDir(dir *FileData, oldName, newName string, f *FileData) {
	if m, ok := dir.memDir.(*DirMap); ok {
		m.Replace(oldName, newName, f)
		return
	}
	dir.memDir.Remove(f)
	dir.memDir.Add(f)
}

func InitializeDir(d *FileData) {
	if d.memDir == nil {
		d.dir = true
		d.memDir = &DirMap{}
	}
}

// Children returns an immutable snapshot of the entries currently held by
// directory d. It is primarily a test/invariant helper; regular traversal
// goes through a File handle's Readdir, which snapshots once per open.
func Children(d *FileData) []*FileData {
	d.Lock()
	defer d.Unlock()
	if d.memDir == nil {
		return nil
	}
	return d.memDir.Files()
}
