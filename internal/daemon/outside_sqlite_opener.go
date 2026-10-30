package daemon

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/procinfo"
	"github.com/osauer/canary/v2/internal/xdgcache"
)

// Only the daemon opens daemon.db with SQLite. An outside opener that finds no
// lock on the -shm's DMS byte resets the shared memory to 3 bytes, and a
// daemon still mapping it dies with SIGBUS: live 2026-10-29 21:29:45 CET,
// after which one committed risk-capital save was missing. The daemon names
// any process it finds holding the database open, so the next one is known.
const (
	outsideOpenerCheckEvery = time.Minute
	// shmDMSByte is SQLite's dead-man-switch lock byte in the -shm file,
	// (22+SQLITE_SHM_NLOCK)*4+SQLITE_SHM_NLOCK. Every open connection holds
	// it shared.
	shmDMSByte = 128
)

// outsideOpenerWatch is touched only by its loop.
type outsideOpenerWatch struct {
	// shm stays open for the life of the process: closing any descriptor of
	// daemon.db-shm drops every POSIX lock this process holds on it,
	// SQLite's included.
	shm    *os.File
	warned map[int]bool
}

func (s *Server) startOutsideOpenerWatch(ctx context.Context) {
	if s == nil || !s.productionStateDatabase {
		return
	}
	s.removeRetiredMoneyFile()
	go func() {
		ticker := time.NewTicker(outsideOpenerCheckEvery)
		defer ticker.Stop()
		for {
			s.checkOutsideSQLiteOpeners(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// removeRetiredMoneyFile deletes the live book's money figures that builds
// 7bffcc3d to 4e9f834e wrote in plaintext beside the daemon's cache; no other
// program writes that file. Their holdings-denylist stays: it is also the
// account-data gate's own cache, which the gate rewrites on every run. Owner
// instruction 2026-10-30 08:44 CET.
func (s *Server) removeRetiredMoneyFile() {
	path, err := xdgcache.CacheDir("money-denylist")
	if err != nil {
		return
	}
	if err := os.Remove(path); err == nil {
		s.infof("removed %s, written by an earlier build; the account-data check now asks the daemon", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		s.warnf("remove the retired money file %s: %v", path, err)
	}
}

// checkOutsideSQLiteOpeners warns once per process found holding a lock on
// daemon.db-shm. F_GETLK never reports this process's own locks, so any
// holder is another process with daemon.db open.
func (s *Server) checkOutsideSQLiteOpeners(ctx context.Context) {
	if s.coreStore == nil || s.coreStorePath == "" {
		return
	}
	w := &s.outsideOpeners
	if w.shm == nil {
		f, err := os.Open(s.coreStorePath + "-shm")
		if err != nil {
			return
		}
		w.shm = f
		w.warned = map[int]bool{}
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: shmDMSByte, Len: 1}
	if err := syscall.FcntlFlock(w.shm.Fd(), syscall.F_GETLK, &lock); err != nil {
		return
	}
	pid := int(lock.Pid)
	if lock.Type == syscall.F_UNLCK || pid <= 0 || w.warned[pid] {
		return
	}
	w.warned[pid] = true
	name := procinfo.Names(ctx, []int{pid})[pid]
	s.warnf("daemon.db: another process (pid %d %s) has it open with SQLite; only the daemon may, because an outside opener resets the daemon's shared memory if the daemon's lock on it is lost (crash of 2026-10-29). Read account data through canary commands", pid, nonEmptyString(name, "unknown"))
}
