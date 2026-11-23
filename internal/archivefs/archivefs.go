// Copyright © 2025 The Afero Authors.
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

// Package archivefs contains the indexing and validation rules shared by
// the read-only archive file systems (zipfs and tarfs).
package archivefs

import (
	"errors"
	"path"
	"strconv"
	"strings"
)

// MaxEntryName is the maximum length, in bytes, of an archive entry name
// accepted while building an index. It mirrors the common PATH_MAX bound
// and keeps a hostile archive from exhausting memory with oversized names.
const MaxEntryName = 4096

var (
	// ErrEmptyName is returned when an archive entry has an empty name.
	ErrEmptyName = errors.New("archive entry name is empty")
	// ErrNameTooLong is returned when an archive entry name exceeds
	// [MaxEntryName] bytes.
	ErrNameTooLong = errors.New("archive entry name is too long")
	// ErrAbsolutePath is returned when an archive entry name is absolute,
	// i.e. rooted at the file system root or a Windows volume/UNC share.
	ErrAbsolutePath = errors.New("archive entry name must not be absolute")
	// ErrRootEscape is returned when an archive entry name, after cleaning,
	// escapes the archive root (for example "../evil" or "a/../../evil").
	ErrRootEscape = errors.New("archive entry name escapes the root")
)

// EntryError associates an indexing error with the offending archive entry,
// so that callers can locate the entry that made an archive unusable. It
// mirrors the surface of os.PathError (Error plus the optional Timeout and
// Temporary methods) without tying the index to the os package.
type EntryError struct {
	// Op is the operation that failed, typically "index".
	Op string
	// Entry is the name of the archive entry being indexed, as stored in
	// the archive and before any cleaning.
	Entry string
	// Err is the underlying error.
	Err error
}

func (e *EntryError) Error() string {
	return e.Op + " " + strconv.Quote(e.Entry) + ": " + e.Err.Error()
}

func (e *EntryError) Unwrap() error { return e.Err }

// Timeout reports whether the underlying error is a timeout.
func (e *EntryError) Timeout() bool {
	var ne interface{ Timeout() bool }
	return errors.As(e.Err, &ne) && ne.Timeout()
}

// Temporary reports whether the underlying error is temporary.
func (e *EntryError) Temporary() bool {
	var ne interface{ Temporary() bool }
	return errors.As(e.Err, &ne) && ne.Temporary()
}

var (
	_ error                       = (*EntryError)(nil)
	_ interface{ Timeout() bool } = (*EntryError)(nil)
	_ interface {
		Temporary() bool
	} = (*EntryError)(nil)
)

// EntryErrorf wraps err with the entry name it applies to.
func EntryErrorf(entry string, err error) error {
	if err == nil {
		return nil
	}
	return &EntryError{Op: "index", Entry: entry, Err: err}
}

// ValidateName rejects entry names that cannot be indexed safely. The check
// is independent of the host operating system: names are validated with
// slash-separated semantics so that a backslash in an entry name cannot
// become an absolute or escaping path on Windows.
//
// It rejects empty names, names longer than [MaxEntryName], absolute names
// (including Windows drive letters and UNC paths) and names whose cleaned
// form escapes the archive root.
func ValidateName(name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if len(name) > MaxEntryName {
		return ErrNameTooLong
	}

	// Normalize Windows separators before validating so the result is
	// identical on every platform.
	slashed := strings.ReplaceAll(name, "\\", "/")

	if slashed[0] == '/' || isVolumeName(slashed) {
		return ErrAbsolutePath
	}

	cleaned := path.Clean(slashed)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ErrRootEscape
	}
	return nil
}

// isVolumeName reports whether name starts with a Windows volume name
// ("C:") or a UNC share prefix ("//server/share" or "?/...").
func isVolumeName(name string) bool {
	if len(name) >= 2 &&
		((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) &&
		name[1] == ':' {
		return true
	}
	// After backslash normalization UNC paths start with "//".
	return strings.HasPrefix(name, "//") || strings.HasPrefix(name, "?/")
}

// DuplicateEntryError reports an entry whose cleaned path collides with an
// entry seen earlier in the same archive. The first entry always wins and
// remains visible through the file system; the conflict is surfaced instead
// of silently overwriting the earlier entry.
type DuplicateEntryError struct {
	// Entry is the duplicate name as stored in the archive.
	Entry string
	// Path is the cleaned, root-relative path it collides on.
	Path string
}

func (e *DuplicateEntryError) Error() string {
	return "archive entry " + strconv.Quote(e.Entry) +
		" duplicates already indexed path " + strconv.Quote(e.Path)
}
