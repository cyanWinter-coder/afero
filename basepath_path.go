package afero

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrPathEscape is returned when a path - either directly or through a
// symbolic link - resolves outside the root directory of a BasePathFs.
//
// It is wrapped in an *os.PathError or *os.LinkError by the operation that
// detected the escape. It is deliberately distinct from os.ErrNotExist so a
// confinement violation can be told apart from a missing file; lexical
// escapes using ".." are still reported as os.ErrNotExist, preserving the
// historical contract that names outside the base path do not exist.
var ErrPathEscape = errors.New("path escapes base path")

// ErrSymlinkLoop is returned when resolving the symbolic links in a path
// exceeds the maximum number of indirections.
var ErrSymlinkLoop = errors.New("too many levels of symbolic links")

// maxSymlinkResolutions mirrors the limit used by the operating system for a
// single path resolution.
const maxSymlinkResolutions = 255

// PathFs is an alias for BasePathFs. It exists so that callers which refer
// to the root-confined path filesystem by this name keep compiling; all
// behaviour is defined by BasePathFs.
type PathFs = BasePathFs

// NewPathFs is an alias for NewBasePathFs.
func NewPathFs(source Fs, path string) Fs {
	return NewBasePathFs(source, path)
}

// localPath normalizes name to a clean path relative to the base directory.
//
// It is the single lexical entry point used by every BasePathFs operation:
//
//   - a leading separator is interpreted as the base directory, matching the
//     historical behaviour where "/foo" means "<base>/foo";
//   - empty names, ".", and "/" all denote the base directory itself;
//   - interior ".." elements are collapsed, but any name that would climb
//     above the base directory is rejected with os.ErrNotExist;
//   - names containing a NUL byte, drive-qualified names, and (on Windows)
//     absolute OS paths are rejected.
//
// The returned path never starts with a separator and always satisfies
// filepath.IsLocal, or is exactly ".".
func (b *BasePathFs) localPath(name string) (string, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return "", fs.ErrInvalid
	}

	// On Windows a common mistake is to provide an absolute OS path. We
	// could strip the base part, but that would not be portable.
	if runtime.GOOS == "windows" {
		if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
			return "", os.ErrNotExist
		}
	}

	clean := filepath.Clean(name)
	if clean == "." {
		return ".", nil
	}

	// Interpret a rooted path as relative to the base directory. This
	// preserves the long-standing BasePathFs contract that "/foo" is
	// "<base>/foo"; it is not an escape. os.IsPathSeparator also covers the
	// Windows case where a leading backslash is not considered absolute.
	if os.IsPathSeparator(clean[0]) {
		clean = strings.TrimLeftFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
		if clean == "" {
			return ".", nil
		}
	}

	if filepath.VolumeName(clean) != "" || !filepath.IsLocal(clean) {
		return "", os.ErrNotExist
	}
	return clean, nil
}

// resolveInRoot resolves the symbolic links in local inside root, never
// letting a target climb above it. It is used when an operation on an
// os.Root failed but a link inside the root may still be usable, notably for
// links created with an absolute target by an older BasePathFs.
//
// Paths handed to root are relative to the root directory; absolute link
// targets are accepted only when they land inside root.Name(). When
// followFinal is false the last element is left unresolved, matching Lstat,
// Rename, Remove, and Mkdir.
func resolveInRoot(root *os.Root, local string, followFinal bool) (string, error) {
	if local == "." {
		return ".", nil
	}

	rootDir, err := filepath.Abs(root.Name())
	if err != nil {
		return "", ErrPathEscape
	}

	resolved := make([]string, 0, strings.Count(local, string(filepath.Separator))+1)
	pending := strings.Split(local, string(filepath.Separator))
	hops := 0

	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		if comp == "" || comp == "." {
			continue
		}

		cur := filepath.Join(append(append([]string{}, resolved...), comp)...)
		isFinal := len(pending) == 0

		fi, statErr := root.Lstat(cur)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				// Nothing exists here yet, so no symbolic link can hide in
				// the remaining components.
				resolved = append(resolved, comp)
				resolved = append(resolved, pending...)
				break
			}
			return "", statErr
		}

		if fi.Mode()&os.ModeSymlink == 0 || (isFinal && !followFinal) {
			resolved = append(resolved, comp)
			continue
		}

		target, linkErr := root.Readlink(cur)
		if linkErr != nil {
			return "", linkErr
		}
		hops++
		if hops > maxSymlinkResolutions {
			return "", ErrSymlinkLoop
		}

		next, targetErr := resolveTarget(rootDir, resolved, target)
		if targetErr != nil {
			return "", targetErr
		}
		pending = append(next, pending...)
	}

	out := filepath.Join(resolved...)
	if out == "" {
		return ".", nil
	}
	if !filepath.IsLocal(out) {
		return "", ErrPathEscape
	}
	return out, nil
}

// physicalBase resolves the directory that contains f's root, following a
// BasePathFs chain down to OsFs. The empty string with ok=true means the
// process working directory (a bare OsFs). It reports false when the chain is
// not based on OsFs, in which case an absolute symbolic-link target cannot be
// related to the boundary and is rejected (fail closed).
func physicalBase(f Fs) (string, bool) {
	switch t := f.(type) {
	case *OsFs, OsFs:
		return "", true
	case *BasePathFs:
		base, ok := physicalBase(t.source)
		if !ok {
			return "", false
		}
		if base == "" {
			return filepath.Clean(t.path), true
		}
		return filepath.Join(base, t.path), true
	}
	return "", false
}

// sourceRootDir returns the directory that should be treated as the boundary
// for absolute symbolic-link targets, expressed in the wrapped filesystem's
// own path space. It reports false when the boundary cannot be determined.
//
// Absolute targets are only accepted when they land inside this directory, so
// an unknown boundary means absolute targets are rejected (fail closed).
// Relative targets do not need the boundary and are unaffected.
func (b *BasePathFs) sourceRootDir() (string, bool) {
	if base, ok := physicalBase(b); ok {
		if base == "" {
			base = "."
		}
		abs, err := filepath.Abs(base)
		if err != nil {
			return "", false
		}
		return abs, true
	}
	// Not OS-backed: a non-OS filesystem has no meaningful physical root, so
	// only an absolute base path can serve as a boundary.
	if filepath.IsAbs(b.path) || filepath.VolumeName(b.path) != "" {
		return filepath.Clean(b.path), true
	}
	return "", false
}

// resolvePath is the source-filesystem counterpart of resolveInRoot. It
// resolves symbolic links for wrappers that do not expose an os.Root, using
// the same root-boundary rules. When the wrapped filesystem does not expose
// both Lstat and Readlink, the normalized path is returned unchanged (there
// are no symbolic links to resolve).
func (b *BasePathFs) resolvePath(name string, followFinal bool) (string, error) {
	local, err := b.localPath(name)
	if err != nil {
		return "", err
	}
	if local == "." {
		return ".", nil
	}

	lstater, ok := b.source.(Lstater)
	if !ok {
		return local, nil
	}
	reader, ok := b.source.(LinkReader)
	if !ok {
		return local, nil
	}

	rootDir, _ := b.sourceRootDir()

	resolved := make([]string, 0, strings.Count(local, string(filepath.Separator))+1)
	pending := strings.Split(local, string(filepath.Separator))
	hops := 0

	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		if comp == "" || comp == "." {
			continue
		}

		cur := filepath.Join(append(append([]string{}, resolved...), comp)...)
		isFinal := len(pending) == 0

		fi, _, statErr := lstater.LstatIfPossible(b.sourcePath(cur))
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				resolved = append(resolved, comp)
				resolved = append(resolved, pending...)
				break
			}
			return "", statErr
		}

		if fi.Mode()&os.ModeSymlink == 0 || (isFinal && !followFinal) {
			resolved = append(resolved, comp)
			continue
		}

		target, linkErr := reader.ReadlinkIfPossible(b.sourcePath(cur))
		if linkErr != nil {
			return "", linkErr
		}
		hops++
		if hops > maxSymlinkResolutions {
			return "", ErrSymlinkLoop
		}

		next, targetErr := resolveTarget(rootDir, resolved, target)
		if targetErr != nil {
			return "", targetErr
		}
		pending = append(next, pending...)
	}

	out := filepath.Join(resolved...)
	if out == "" {
		return ".", nil
	}
	if !filepath.IsLocal(out) {
		return "", ErrPathEscape
	}
	return out, nil
}

// resolveTarget converts the target of a symbolic link, located in the
// directory dir (relative to the root), into path components that are
// themselves relative to the root.
//
// Absolute targets are accepted only when they land inside rootDir; this
// keeps links created by an older BasePathFs (which wrote base-prefixed
// targets) working. Relative targets are joined with the link's directory and
// may not climb above the root. When rootDir is unknown, absolute targets are
// rejected.
func resolveTarget(rootDir string, dir []string, target string) ([]string, error) {
	target = filepath.Clean(target)
	if target == "" || target == "." {
		return nil, nil
	}

	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		if rootDir == "" {
			return nil, ErrPathEscape
		}
		absTarget := target
		if !filepath.IsAbs(absTarget) {
			var err error
			if absTarget, err = filepath.Abs(absTarget); err != nil {
				return nil, ErrPathEscape
			}
		}
		rel, err := filepath.Rel(rootDir, absTarget)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, ErrPathEscape
		}
		if rel == "." {
			return nil, nil
		}
		if !filepath.IsLocal(rel) {
			return nil, ErrPathEscape
		}
		return strings.Split(rel, string(filepath.Separator)), nil
	}

	joined := filepath.Join(append(append([]string{}, dir...), target)...)
	if joined == "" || joined == "." {
		return nil, nil
	}
	if filepath.IsAbs(joined) || !filepath.IsLocal(joined) {
		return nil, ErrPathEscape
	}
	return strings.Split(joined, string(filepath.Separator)), nil
}

// isEscapeError reports whether err is the "path escapes" error produced by
// os.Root. The standard library does not export a sentinel for it, so this
// matches the documented message text.
func isEscapeError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

// osBackedBase reports the OS directory that backs f, when f is directly
// based on OsFs. Nested BasePathFs chains are deliberately not collapsed into
// a single OS root: each layer must keep enforcing its own boundary, so a
// nested wrapper delegates to its parent instead (see execute).
func osBackedBase(f Fs) (string, bool) {
	switch t := f.(type) {
	case *OsFs, OsFs:
		return ".", true
	case *BasePathFs:
		switch t.source.(type) {
		case *OsFs, OsFs:
			return filepath.Clean(t.path), true
		}
	}
	return "", false
}

// openOSRoot opens an os.Root for the directory backing b. It returns false
// when the wrapped filesystem is not based on OsFs or the base directory does
// not exist yet (for example before MkdirAll has created it).
//
// os.Root resolves every path - including symbolic links - relative to the
// opened directory and refuses to escape it, which makes the path validation
// resilient to concurrent replacement of path components.
func (b *BasePathFs) openOSRoot() (*os.Root, bool) {
	base, ok := osBackedBase(b)
	if !ok {
		return nil, false
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, false
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, false
	}
	return root, true
}
