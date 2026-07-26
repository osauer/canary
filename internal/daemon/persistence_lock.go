package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ErrPersistenceInUse means another process owns the daemon state database.
// It is deliberately distinct from ErrAlreadyRunning: two daemons using
// different socket paths must still fail visibly when they resolve to the same
// daemon.db.
var ErrPersistenceInUse = errors.New("another Canary daemon owns the daemon state database")

// persistenceLock serializes every daemon that resolves to one authoritative
// state database. Unlike the socket-specific instance pidfile, this lock file
// is never removed: unlinking a flock file after unlock permits an inode race
// in which two later processes can each lock a different file at the same
// pathname.
type persistenceLock struct {
	path string
	f    *os.File
}

func acquirePersistenceLock(databasePath string) (*persistenceLock, error) {
	if databasePath == "" {
		return nil, errors.New("daemon database path is required")
	}
	path := databasePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir persistence lock dir: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("persistence lock must not be a symbolic link: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect persistence lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open persistence lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure persistence lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrPersistenceInUse
		}
		return nil, fmt.Errorf("flock persistence lock: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate persistence lock: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write persistence lock pid: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("sync persistence lock: %w", err)
	}
	return &persistenceLock{path: path, f: f}, nil
}

// Release unlocks and closes the lock. The stable lock pathname remains.
func (l *persistenceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
