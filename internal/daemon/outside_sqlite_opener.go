package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/procinfo"
	"github.com/osauer/canary/v2/internal/xdgcache"
)

// Only the daemon opens daemon.db with SQLite. The store keeps the log's index
// in the daemon's memory and holds an exclusive lock on the file, so any other
// SQLite opener is refused as locked and no daemon.db-shm exists. That lock is
// a POSIX lock: closing any descriptor of daemon.db in this process drops it.
// An opener that then gets in leaves a trace the watch can see: it creates
// daemon.db-shm, or it checkpoints and removes the log the daemon writes to.
// Either way the daemon's index no longer describes the file, so the watch
// latches the store fail-closed and names the process holding the index file.
// The store's own connection also replaces the log when its pool reopens it;
// the store tells that apart (corestore.Store.LogChanged).
const (
	// A minute let one-shot openers slip between checks; a second also
	// bounds how long the daemon writes on after its lock is lost.
	outsideOpenerCheckEvery = time.Second
	// shmDMSByte is SQLite's dead-man-switch lock byte in the -shm file,
	// (22+SQLITE_SHM_NLOCK)*4+SQLITE_SHM_NLOCK. Every connection using the
	// file holds it shared.
	shmDMSByte = 128
)

// outsideOpenerWatch is touched only by its loop. It holds no descriptor of
// the store's files.
type outsideOpenerWatch struct {
	warned  map[int]bool
	latched bool
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

// checkOutsideSQLiteOpeners latches the store once when another process has
// got past its exclusive lock, and warns once per process found holding
// daemon.db-shm. F_GETLK never reports this process's own locks, and the
// daemon holds none on that file, so any holder is another process.
func (s *Server) checkOutsideSQLiteOpeners(ctx context.Context) {
	if s.coreStore == nil || s.coreStorePath == "" {
		return
	}
	w := &s.outsideOpeners
	if w.warned == nil {
		w.warned = map[int]bool{}
	}
	reason := ""
	if changed, err := s.coreStore.LogChanged(ctx); err == nil && changed != "" {
		reason = "daemon.db-wal was " + changed
	}
	shmPath := s.coreStorePath + "-shm"
	if _, err := os.Lstat(shmPath); err == nil {
		reason = cmp.Or(reason, "daemon.db-shm appeared")
		s.nameIndexHolder(ctx, shmPath)
	}
	if reason != "" && !w.latched {
		w.latched = true
		s.coreStore.LatchDamaged("outside_opener", fmt.Errorf("%s while the daemon holds daemon.db exclusively: another process opened it, so the daemon's in-memory index may no longer match the file", reason))
	}
}

// nameIndexHolder warns once per process holding a lock on daemon.db-shm.
func (s *Server) nameIndexHolder(ctx context.Context, shmPath string) {
	f, err := os.Open(shmPath)
	if err != nil {
		return
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: shmDMSByte, Len: 1}
	lockErr := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lock)
	_ = f.Close()
	pid := int(lock.Pid)
	if lockErr != nil || lock.Type == syscall.F_UNLCK || pid <= 0 || s.outsideOpeners.warned[pid] {
		return
	}
	s.outsideOpeners.warned[pid] = true
	name := procinfo.Names(ctx, []int{pid})[pid]
	s.warnf("daemon.db: another process (pid %d %s) has it open with SQLite; only the daemon may. Read account data through canary commands", pid, nonEmptyString(name, "unknown"))
}
