// Package xdgcache provides shared filesystem primitives for user data and
// caches: same-directory temp-and-rename replacement, non-blocking process
// locks, and XDG-aware cache-directory resolution.
package xdgcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// WriteAtomic replaces path with data by renaming a temporary file created in
// the same directory. Readers therefore observe either the old or new name on
// supported local filesystems. The parent directory is created with mode 0755;
// the resulting inode retains the temporary file's mode, normally 0600.
// WriteAtomic does not fsync the file or directory and therefore does not claim
// power-loss durability.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// On any non-success path past this point, remove the orphaned temp
	// file so the cache dir doesn't accumulate junk. tmp is set to nil
	// after successful close so the defer's Close becomes a no-op when
	// we got that far.
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil // signal defer that Close already happened
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// Lock is a held flock. Released by calling Release; safe to call
// multiple times. The underlying file descriptor stays open for the
// lock's lifetime — closing the file releases the flock as a side
// effect, but Release does so explicitly to surface unlock errors.
type Lock struct {
	path string
	f    *os.File
}

// ErrLocked is returned by OpenLock when the lock file is held by
// another process. Callers that want to queue rather than fail-fast
// can implement their own retry loop; this package does not.
var ErrLocked = errors.New("xdgcache: lock held by another process")

// OpenLock takes a non-blocking exclusive flock on path. The parent
// directory is created if missing. Returns ErrLocked if the lock is
// already held by another process (LOCK_NB EWOULDBLOCK), or a wrapped
// error for any other failure mode.
//
// The lock file is never deleted by this package — the inode is the
// lock identity, so unlinking it under contention would let a second
// caller create a fresh file at the same path and acquire it
// independently. Callers wanting cleanup should remove the file at a
// known-quiescent point.
func OpenLock(path string) (*Lock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return &Lock{path: path, f: f}, nil
}

// File exposes the held descriptor for inheritance via exec.Cmd.ExtraFiles.
// The caller must not close or unlock it; Release retains ownership.
func (l *Lock) File() *os.File { return l.f }

// Release unlocks and closes the underlying file. Safe to call
// multiple times; the second call is a no-op. The lock file itself
// is intentionally NOT removed (see OpenLock comment).
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	switch {
	case unlockErr != nil:
		return fmt.Errorf("unlock %s: %w", l.path, unlockErr)
	case closeErr != nil:
		return fmt.Errorf("close lock %s: %w", l.path, closeErr)
	}
	return nil
}

// CacheDir returns the daemon's on-disk cache root for sub, resolving
// $XDG_CACHE_HOME first and falling back to $HOME/.cache when XDG isn't
// set (the XDG spec's documented default). sub is the per-feature
// subdirectory ("spx-members", "update", "gamma-zero" etc.) — kept as a
// required argument so call sites read self-documentingly and the
// shared root is never returned by accident.
//
// Returns an error only when both XDG_CACHE_HOME and HOME are unset,
// which on a real OS user account doesn't happen. Tests should pass
// t.TempDir() directly to whichever helper needs a cache path rather
// than relying on this function.
func CacheDir(sub string) (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", sub), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", sub), nil
}
