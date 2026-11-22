package afero

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// If the cache duration is 0, cache time will be unlimited, i.e. once
// a file is in the layer, the base will never be read again for this file.
//
// For cache times greater than 0, the modification time of a file is
// checked. Note that a lot of file system implementations only allow a
// resolution of a second for timestamps... or as the godoc for os.Chtimes()
// states: "The underlying filesystem may truncate or round the values to a
// less precise time unit."
//
// This caching union will forward all write calls also to the base file
// system first. To prevent writing to the base Fs, wrap it in a read-only
// filter - Note: this will also make the overlay read-only, for writing files
// in the overlay, use the overlay Fs directly, not via the union Fs.
type CacheOnReadFs struct {
	base      Fs
	layer     Fs
	cacheTime time.Duration

	// generations records, per cached path, the version of the base entry
	// the cached copy was made from. A cached path is only reusable while
	// the base entry still matches that version, so a replaced or rewritten
	// source file always forces a fresh copy instead of handing out the
	// stale handle. Pointer identity catches entry replacements; the
	// modification time/size fingerprint catches in-place truncations that
	// reuse the same entry (some in-memory file systems do so).
	genMu       sync.Mutex
	generations map[string]sourceGeneration
}

// sourceGeneration describes the base entry version a cache copy is bound to.
type sourceGeneration struct {
	identity interface{} // underlying entry pointer, nil when unavailable
	modTime  time.Time
	size     int64
}

func NewCacheOnReadFs(base Fs, layer Fs, cacheTime time.Duration) Fs {
	return &CacheOnReadFs{base: base, layer: layer, cacheTime: cacheTime, generations: map[string]sourceGeneration{}}
}

type cacheState int

const (
	// not present in the overlay, unknown if it exists in the base:
	cacheMiss cacheState = iota
	// present in the overlay and in base, base file is newer:
	cacheStale
	// present in the overlay - with cache time == 0 it may exist in the base,
	// with cacheTime > 0 it exists in the base and is same age or newer in the
	// overlay
	cacheHit
	// happens if someone writes directly to the overlay without
	// going through this union
	cacheLocal
)

// entryIdentity returns a stable identity for the directory entry backing
// name, or nil when the file system cannot provide one.
func entryIdentity(fs Fs, name string) interface{} {
	if m, ok := fs.(*MemMapFs); ok {
		fd, err := m.open(name)
		if err != nil {
			return nil
		}
		return fd
	}
	return nil
}

// currentGeneration snapshots the version of the base entry backing name.
func (u *CacheOnReadFs) currentGeneration(name string) (sourceGeneration, bool) {
	bfi, err := u.base.Stat(name)
	if err != nil {
		return sourceGeneration{}, false
	}
	return sourceGeneration{
		identity: entryIdentity(u.base, name),
		modTime:  bfi.ModTime(),
		size:     bfi.Size(),
	}, true
}

// rememberGeneration binds the cached copy of name to the base version
// currently committed.
func (u *CacheOnReadFs) rememberGeneration(name string) {
	gen, ok := u.currentGeneration(name)
	if !ok {
		return
	}
	u.genMu.Lock()
	if u.generations == nil {
		u.generations = map[string]sourceGeneration{}
	}
	u.generations[name] = gen
	u.genMu.Unlock()
}

func (u *CacheOnReadFs) forgetGeneration(name string) {
	u.genMu.Lock()
	delete(u.generations, name)
	u.genMu.Unlock()
}

// renameGeneration moves the recorded version when a cached entry is renamed.
func (u *CacheOnReadFs) renameGeneration(oldname, newname string) {
	u.genMu.Lock()
	if gen, ok := u.generations[oldname]; ok {
		if u.generations == nil {
			u.generations = map[string]sourceGeneration{}
		}
		u.generations[newname] = gen
		delete(u.generations, oldname)
	}
	u.genMu.Unlock()
}

// generationStale reports whether the base entry of name no longer matches
// the version the cached copy was made from.
func (u *CacheOnReadFs) generationStale(name string) bool {
	u.genMu.Lock()
	gen, ok := u.generations[name]
	u.genMu.Unlock()
	if !ok {
		return false
	}
	current, exists := u.currentGeneration(name)
	if !exists {
		// Base lost the file: the cached copy is now overlay-local.
		return false
	}
	if gen.identity != nil && current.identity != gen.identity {
		return true
	}
	return !current.modTime.Equal(gen.modTime) || current.size != gen.size
}

func (u *CacheOnReadFs) cacheStatus(name string) (state cacheState, fi os.FileInfo, err error) {
	var lfi, bfi os.FileInfo
	lfi, err = u.layer.Stat(name)
	if err == nil {
		if u.generationStale(name) {
			// The cached copy is bound to an older source generation.
			if bfi, berr := u.base.Stat(name); berr == nil {
				return cacheStale, bfi, nil
			}
		}
		if u.cacheTime == 0 {
			return cacheHit, lfi, nil
		}
		if lfi.ModTime().Add(u.cacheTime).Before(time.Now()) {
			bfi, err = u.base.Stat(name)
			if err != nil {
				return cacheLocal, lfi, nil
			}
			if bfi.ModTime().After(lfi.ModTime()) {
				return cacheStale, bfi, nil
			}
		}
		return cacheHit, lfi, nil
	}

	if err == syscall.ENOENT || os.IsNotExist(err) {
		return cacheMiss, nil, nil
	}

	return cacheMiss, nil, err
}

func (u *CacheOnReadFs) copyToLayer(name string) error {
	if err := copyToLayer(u.base, u.layer, name); err != nil {
		return err
	}
	u.rememberGeneration(name)
	return nil
}

func (u *CacheOnReadFs) copyFileToLayer(name string, flag int, perm os.FileMode) error {
	// Refreshing the cache only needs to read the committed base version;
	// opening the base with the caller's write flags would be rejected by a
	// read-only base filter and serves no purpose for the copy itself.
	if err := copyToLayer(u.base, u.layer, name); err != nil {
		return err
	}
	u.rememberGeneration(name)
	return nil
}

func (u *CacheOnReadFs) Chtimes(name string, atime, mtime time.Time) error {
	st, _, err := u.cacheStatus(name)
	if err != nil {
		return err
	}
	switch st {
	case cacheLocal:
	case cacheHit:
		err = u.base.Chtimes(name, atime, mtime)
	case cacheStale, cacheMiss:
		if err := u.copyToLayer(name); err != nil {
			return err
		}
		err = u.base.Chtimes(name, atime, mtime)
	}
	if err != nil {
		return err
	}
	return u.layer.Chtimes(name, atime, mtime)
}

func (u *CacheOnReadFs) Chmod(name string, mode os.FileMode) error {
	st, _, err := u.cacheStatus(name)
	if err != nil {
		return err
	}
	switch st {
	case cacheLocal:
	case cacheHit:
		err = u.base.Chmod(name, mode)
	case cacheStale, cacheMiss:
		if err := u.copyToLayer(name); err != nil {
			return err
		}
		err = u.base.Chmod(name, mode)
	}
	if err != nil {
		return err
	}
	return u.layer.Chmod(name, mode)
}

func (u *CacheOnReadFs) Chown(name string, uid, gid int) error {
	st, _, err := u.cacheStatus(name)
	if err != nil {
		return err
	}
	switch st {
	case cacheLocal:
	case cacheHit:
		err = u.base.Chown(name, uid, gid)
	case cacheStale, cacheMiss:
		if err := u.copyToLayer(name); err != nil {
			return err
		}
		err = u.base.Chown(name, uid, gid)
	}
	if err != nil {
		return err
	}
	return u.layer.Chown(name, uid, gid)
}

func (u *CacheOnReadFs) Stat(name string) (os.FileInfo, error) {
	st, fi, err := u.cacheStatus(name)
	if err != nil {
		return nil, err
	}
	switch st {
	case cacheMiss:
		return u.base.Stat(name)
	case cacheStale:
		// Keep the cache index in sync before reporting the new version.
		// Directories are merged lazily on Open, so just report the base
		// metadata instead of trying to copy the directory as a file.
		if !fi.IsDir() {
			if err := u.copyToLayer(name); err != nil {
				return nil, err
			}
			if lfi, lerr := u.layer.Stat(name); lerr == nil {
				return lfi, nil
			}
		}
		return fi, nil
	default: // cacheHit and cacheLocal use the layer os.FileInfo
		return fi, nil
	}
}

func (u *CacheOnReadFs) Rename(oldname, newname string) error {
	st, _, err := u.cacheStatus(oldname)
	if err != nil {
		return err
	}
	switch st {
	case cacheLocal:
	case cacheHit:
		err = u.base.Rename(oldname, newname)
	case cacheStale, cacheMiss:
		if err := u.copyToLayer(oldname); err != nil {
			return err
		}
		err = u.base.Rename(oldname, newname)
	}
	if err != nil {
		// The base commit did not happen: keep both views on the old path.
		return err
	}
	if lerr := u.layer.Rename(oldname, newname); lerr != nil {
		return lerr
	}
	u.renameGeneration(oldname, newname)
	return nil
}

func (u *CacheOnReadFs) Remove(name string) error {
	st, _, err := u.cacheStatus(name)
	if err != nil {
		return err
	}
	switch st {
	case cacheLocal:
	case cacheHit, cacheStale, cacheMiss:
		err = u.base.Remove(name)
	}
	if err != nil {
		return err
	}
	if lerr := u.layer.Remove(name); lerr != nil && !os.IsNotExist(lerr) {
		return lerr
	}
	u.forgetGeneration(name)
	return nil
}

func (u *CacheOnReadFs) RemoveAll(name string) error {
	st, _, err := u.cacheStatus(name)
	if err != nil {
		return err
	}
	switch st {
	case cacheLocal:
	case cacheHit, cacheStale, cacheMiss:
		err = u.base.RemoveAll(name)
	}
	if err != nil {
		return err
	}
	if lerr := u.layer.RemoveAll(name); lerr != nil && !os.IsNotExist(lerr) {
		return lerr
	}
	u.genMu.Lock()
	for p := range u.generations {
		if p == name || hasPathPrefix(p, name) {
			delete(u.generations, p)
		}
	}
	u.genMu.Unlock()
	return nil
}

// hasPathPrefix reports whether path is name itself or a descendant of name.
func hasPathPrefix(path, name string) bool {
	prefix := name
	if prefix[len(prefix)-1] != FilePathSeparator[0] {
		prefix += FilePathSeparator
	}
	return len(path) >= len(prefix) && path[:len(prefix)] == prefix
}

func (u *CacheOnReadFs) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	st, _, err := u.cacheStatus(name)
	if err != nil {
		return nil, err
	}
	write := flag&(os.O_WRONLY|syscall.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0
	switch st {
	case cacheLocal, cacheHit:
	default:
		// On a cache miss with create flags the base entry does not exist
		// yet: it is created by the base OpenFile below, so there is nothing
		// to copy up. Otherwise refresh the cache from the committed base.
		if !(st == cacheMiss && write) {
			if err := u.copyFileToLayer(name, flag, perm); err != nil {
				return nil, err
			}
		}
	}
	if write {
		// Commit to the base first. Only when that succeeds may the cache
		// layer take the write; a base failure therefore cannot leave the
		// two layers forked.
		bfi, err := u.base.OpenFile(name, flag, perm)
		if err != nil {
			return nil, err
		}
		if st == cacheMiss {
			// A brand new base entry needs its parent directory in the layer
			// before the layer open can succeed.
			if err := u.layer.MkdirAll(filepath.Dir(name), 0o777); err != nil {
				bfi.Close()
				return nil, err
			}
		}
		lfi, err := u.layer.OpenFile(name, flag, perm)
		if err != nil {
			bfi.Close()
			return nil, err
		}
		return &UnionFile{
			Base:     bfi,
			Layer:    lfi,
			onCommit: func() { u.forgetGeneration(name) },
		}, nil
	}
	return u.layer.OpenFile(name, flag, perm)
}

func (u *CacheOnReadFs) Open(name string) (File, error) {
	st, fi, err := u.cacheStatus(name)
	if err != nil {
		return nil, err
	}

	switch st {
	case cacheLocal:
		return u.layer.Open(name)

	case cacheMiss:
		bfi, err := u.base.Stat(name)
		if err != nil {
			return nil, err
		}
		if bfi.IsDir() {
			return u.base.Open(name)
		}
		if err := u.copyToLayer(name); err != nil {
			return nil, err
		}
		return u.layer.Open(name)

	case cacheStale:
		if !fi.IsDir() {
			if err := u.copyToLayer(name); err != nil {
				return nil, err
			}
			return u.layer.Open(name)
		}
	case cacheHit:
		if !fi.IsDir() {
			return u.layer.Open(name)
		}
	}
	// the dirs from cacheHit, cacheStale fall down here:
	bfile, _ := u.base.Open(name)
	lfile, err := u.layer.Open(name)
	if err != nil && bfile == nil {
		return nil, err
	}
	return &UnionFile{Base: bfile, Layer: lfile}, nil
}

func (u *CacheOnReadFs) Mkdir(name string, perm os.FileMode) error {
	err := u.base.Mkdir(name, perm)
	if err != nil {
		return err
	}
	return u.layer.MkdirAll(name, perm) // yes, MkdirAll... we cannot assume it exists in the cache
}

func (u *CacheOnReadFs) Name() string {
	return "CacheOnReadFs"
}

func (u *CacheOnReadFs) MkdirAll(name string, perm os.FileMode) error {
	err := u.base.MkdirAll(name, perm)
	if err != nil {
		return err
	}
	return u.layer.MkdirAll(name, perm)
}

func (u *CacheOnReadFs) Create(name string) (File, error) {
	// Base commit first, cache update second.
	bfh, err := u.base.Create(name)
	if err != nil {
		return nil, err
	}
	lfh, err := u.layer.Create(name)
	if err != nil {
		bfh.Close()
		return nil, err
	}
	return &UnionFile{
		Base:     bfh,
		Layer:    lfh,
		onCommit: func() { u.forgetGeneration(name) },
	}, nil
}
