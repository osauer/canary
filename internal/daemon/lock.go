package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/osauer/canary/v2/internal/dial"
)

// ErrAlreadyRunning means another Canary daemon holds the instance lock for this
// socket path. Callers (cmd/canaryd) treat this as an expected, non-fatal
// condition: a duplicate start, exit cleanly.
var ErrAlreadyRunning = errors.New("another Canary daemon holds the instance lock")

// instanceLock is a flock-backed pidfile. Lifetime is bound to the daemon
// process; on Stop() we release the flock and remove the pidfile.
type instanceLock struct {
	path string
	f    *os.File
}

// acquireInstanceLock takes a non-blocking exclusive flock on
// <socketDir>/ibkr.lock and writes the current PID. Returns
// ErrAlreadyRunning if the lock is contended.
func acquireInstanceLock(socketPath string) (*instanceLock, error) {
	path := dial.LockPath(socketPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write pid: %w", err)
	}
	return &instanceLock{path: path, f: f}, nil
}

// Release unlocks the flock, closes the file, and removes the pidfile.
// Safe to call multiple times.
func (l *instanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	_ = os.Remove(l.path)
	l.f = nil
}
