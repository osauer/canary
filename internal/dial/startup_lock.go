package dial

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/xdgcache"
)

type startupLockKey struct{}

type startupReservation struct {
	path string
	file *os.File
}

// WithStartupLock reserves daemon startup until release is called. Restart
// holds this across discovery, stop, exact-executable startup and app resume;
// ordinary autospawn rechecks the daemon after acquiring it. The scope matches
// the daemon instance lock, including sockets sharing the same directory.
// Nested calls for the same scope borrow the reservation through ctx.
func WithStartupLock(ctx context.Context, socketPath string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	path := LockPath(socketPath) + ".startup"
	if held, ok := ctx.Value(startupLockKey{}).(*startupReservation); ok && held.path == path {
		return ctx, func() {}, nil
	}
	// Match the daemon's private socket directory on first startup; the
	// general cache-lock helper otherwise creates a world-readable directory.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ctx, nil, fmt.Errorf("create daemon startup directory: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, StartupBudget())
	defer cancel()
	for {
		if err := waitCtx.Err(); err != nil {
			return ctx, nil, err
		}
		lock, err := xdgcache.OpenLock(path)
		if err == nil {
			held := &startupReservation{path: path, file: lock.File()}
			return context.WithValue(ctx, startupLockKey{}, held), func() { _ = lock.Release() }, nil
		}
		if !errors.Is(err, xdgcache.ErrLocked) {
			return ctx, nil, fmt.Errorf("reserve daemon startup: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return ctx, nil, waitCtx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// LockDaemonStartup joins the startup reservation at the daemon boundary, so
// even an older client invoking `canary daemon` cannot steal a restart's gap.
// The selected child inherits its parent's reservation as descriptor 3 via
// exec.Cmd.ExtraFiles. Accept it only if it names this scope's lock inode and
// can hold its flock; unrelated inherited descriptors are left untouched.
// Release immediately after acquiring the daemon instance lock.
func LockDaemonStartup(ctx context.Context, socketPath string) (release func(), err error) {
	const inheritedFD = 3
	path := LockPath(socketPath) + ".startup"
	var inherited, expected syscall.Stat_t
	if syscall.Fstat(inheritedFD, &inherited) == nil && syscall.Stat(path, &expected) == nil &&
		inherited.Dev == expected.Dev && inherited.Ino == expected.Ino {
		if err := syscall.Flock(inheritedFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, fmt.Errorf("inherit daemon startup reservation: %w", err)
		}
		syscall.CloseOnExec(inheritedFD)
		file := os.NewFile(inheritedFD, path)
		// Close only: LOCK_UN would unlock the parent's shared reservation
		// before exact-PID verification and app resume have completed.
		return func() { _ = file.Close() }, nil
	}
	_, release, err = WithStartupLock(ctx, socketPath)
	return release, err
}
