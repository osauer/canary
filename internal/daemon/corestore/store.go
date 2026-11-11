package corestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

const defaultBusyTimeout = 5 * time.Second

// walSizeLimit caps the WAL file left behind once a checkpoint resets it.
// SQLite's default (-1) rewinds the WAL but keeps the file at its largest
// size, so one large transaction pinned a 140 MB WAL until restart
// (2026-10-30). 64 MiB keeps routine state-document churn from shrinking and
// regrowing the file on every reset; it bounds disk only, never content.
const walSizeLimit = 64 << 20

// Store is the daemon-owned authoritative handle. Its database handle is
// intentionally private so all writes pass through typed transactions.
type Store struct {
	db             *sql.DB
	path           string
	busyTimeout    time.Duration
	commitObserver func(AuthorityHead) error
	healthObserver func(Health, error)
	readHead       func(context.Context) (AuthorityHead, error)
	checkIntegrity func(context.Context) (IntegrityReport, error)

	writeMu             sync.Mutex
	lastObservedHead    AuthorityHead
	recoveryMinimumHead AuthorityHead
	healthMu            sync.RWMutex
	health              Health
	closeOnce           sync.Once
	closeErr            error
}

// Open validates or creates the authority at opts.Path and returns a
// single-connection store. Existing older schemas return UpgradeRequiredError;
// future, corrupt, rolled-back, or otherwise invalid authorities are never
// repaired or recreated.
func Open(ctx context.Context, opts Options) (*Store, error) {
	return openWithPlan(ctx, opts, currentMigrationPlan())
}

func openWithPlan(ctx context.Context, opts Options, plan []migration) (*Store, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return nil, err
	}
	targetVersion := len(plan)
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errorsf("database path is required")
	}
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := ensurePrivateParent(filepath.Dir(path)); err != nil {
		return nil, err
	}
	timeout := opts.BusyTimeout
	if timeout <= 0 {
		timeout = defaultBusyTimeout
	}
	existing := false
	if info, err := os.Lstat(path); err == nil {
		existing = true
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errorsf("database path must not be a symbolic link")
		}
		if !info.Mode().IsRegular() {
			return nil, errorsf("database path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect database path: %w", err)
	}
	if existing {
		version, err := readSchemaVersionOnly(ctx, path, timeout)
		if err != nil {
			return nil, fmt.Errorf("read existing authority schema version: %w", err)
		}
		if version > targetVersion {
			return nil, fmt.Errorf("future schema version %d exceeds supported %d", version, targetVersion)
		}
		if version > 0 && version < targetVersion {
			inspection, inspectErr := inspectWithPlan(ctx, InspectOptions{Path: path, MinimumHead: opts.MinimumHead}, plan)
			if inspectErr != nil {
				return nil, inspectErr
			}
			return nil, &UpgradeRequiredError{CurrentVersion: inspection.SchemaVersion, TargetVersion: inspection.TargetVersion}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open authority file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure authority file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close authority file: %w", err)
	}

	db, err := openAuthorityDB(ctx, path, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect sqlite authority: %w", err)
	}
	fail := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	if err := verifyPragmas(ctx, db, timeout); err != nil {
		return fail(err)
	}
	if report, err := checkIntegrityDB(ctx, db); err != nil {
		if code, _ := criticalSQLiteCode(err); code == "corrupt" || errors.Is(err, errContentHashMismatch) {
			err = fmt.Errorf("%w: %w", ErrDamaged, err)
		}
		return fail(fmt.Errorf("startup integrity check: %w", err))
	} else if !report.OK() {
		return fail(fmt.Errorf("%w: %w", ErrDamaged, integrityFailure(report)))
	}
	if err := requireIndexInMemory(path); err != nil {
		return fail(err)
	}
	var preMigrationVersion int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&preMigrationVersion); err != nil {
		return fail(fmt.Errorf("read pre-migration version: %w", err))
	}
	if preMigrationVersion > targetVersion {
		return fail(fmt.Errorf("future schema version %d exceeds supported %d", preMigrationVersion, targetVersion))
	}
	if preMigrationVersion > 0 && preMigrationVersion < targetVersion {
		if err := validateSchemaLedgerWithPlan(ctx, db, preMigrationVersion, plan); err != nil {
			return fail(fmt.Errorf("validate pre-upgrade authority schema: %w", err))
		}
		head, err := readAuthorityHead(ctx, db)
		if err != nil {
			return fail(fmt.Errorf("read pre-upgrade authority head: %w", err))
		}
		if opts.MinimumHead != nil {
			if err := requireMinimumHead(head, *opts.MinimumHead); err != nil {
				return fail(err)
			}
		}
		return fail(&UpgradeRequiredError{CurrentVersion: preMigrationVersion, TargetVersion: targetVersion})
	}
	applied, err := migrate(ctx, db, plan, time.Now().UTC())
	if err != nil {
		return fail(fmt.Errorf("open authority database: %w", err))
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, targetVersion, plan); err != nil {
		return fail(fmt.Errorf("validate authority schema: %w", err))
	}
	// Only re-verify what a migration could have changed. The startup check
	// above already proved this file, and on an ordinary open no migration
	// runs, so a second pass would re-read and re-hash bytes nothing touched —
	// half the pre-socket wait on a large authority, buying no coverage.
	if applied > 0 {
		if report, err := checkIntegrityDB(ctx, db); err != nil {
			return fail(fmt.Errorf("post-migration integrity check: %w", err))
		} else if !report.OK() {
			return fail(integrityFailure(report))
		}
	}
	head, err := readAuthorityHead(ctx, db)
	if err != nil {
		return fail(fmt.Errorf("read authority head: %w", err))
	}
	if opts.MinimumHead != nil {
		if err := requireMinimumHead(head, *opts.MinimumHead); err != nil {
			return fail(err)
		}
	}
	if err := enforcePrivateModes(path); err != nil {
		return fail(err)
	}
	store := &Store{
		db: db, path: path, busyTimeout: timeout,
		commitObserver:   opts.CommitObserver,
		healthObserver:   opts.HealthObserver,
		lastObservedHead: head,
		health:           Health{Ready: true},
	}
	store.readHead = func(ctx context.Context) (AuthorityHead, error) {
		return readAuthorityHead(ctx, store.db)
	}
	store.checkIntegrity = func(ctx context.Context) (IntegrityReport, error) {
		return checkIntegrityDB(ctx, store.db)
	}
	return store, nil
}

func ensurePrivateParent(parent string) error {
	info, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create authority directory: %w", err)
		}
		info, err = os.Lstat(parent)
	}
	if err != nil {
		return fmt.Errorf("inspect authority directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errorsf("authority directory must be a real directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errorsf("authority directory must not be group or world accessible")
	}
	return nil
}

// openAuthorityDB opens path read-write on one connection with the store's
// pragmas and enters WAL with the log's index in this process's memory.
//
// SQLite keeps the index in the heap only when the connection is in exclusive
// locking mode before it first uses the log. It then holds an exclusive lock on
// the database file for the life of the connection, creates no -shm file, and
// any other opener, read-only or not, is refused as locked. With the index in a
// shared -shm instead, any process that opened or rebuilt that file could hand
// this connection an older snapshot than it had committed, which the store saw
// as lost writes although every commit was intact in the log.
//
// The driver applies _pragma values with busy_timeout first and the rest in
// alphabetical order, so journal_mode would run before locking_mode. The DSN
// therefore carries locking_mode and every other setting, none of which reads
// the file, and WAL is entered here once the connection exists. WAL is recorded
// in the file header, so a connection the pool opens again is in WAL too.
//
// An index file left by an earlier build is removed first, unless another
// process holds it. After WAL is entered no -shm may exist; that, not the
// locking_mode pragma, proves the index is private.
func openAuthorityDB(ctx context.Context, path string, busy time.Duration) (*sql.DB, error) {
	if err := removeIdleSharedIndex(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, busy, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&journal); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enter WAL: %w", err)
	}
	if strings.ToLower(journal) != "wal" {
		_ = db.Close()
		return nil, fmt.Errorf("enter WAL: got %q", journal)
	}
	// Entering WAL on a new file does not yet open the log; a read does, and
	// takes the exclusive lock with it.
	var objects int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM main.sqlite_schema`).Scan(&objects); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open the WAL: %w", err)
	}
	if err := requireIndexInMemory(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// requireIndexInMemory fails when path has a -shm file while this process
// holds the authority open: the log's index is then shared, not private.
func requireIndexInMemory(path string) error {
	if _, err := os.Lstat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the log's index is in a shared file, not in this process's memory: %s-shm (%v)", filepath.Base(path), err)
	}
	return nil
}

// shmDMSByte is SQLite's dead-man-switch byte in a -shm file,
// (22+SQLITE_SHM_NLOCK)*4+SQLITE_SHM_NLOCK. Every connection that uses the
// file holds it shared.
const shmDMSByte = 128

// removeIdleSharedIndex removes path's -shm when no other process uses it. A
// build that kept the index in that file leaves it behind; nothing reads it
// once the index lives in memory, and leaving it would look like an outside
// opener. A file another process holds is refused instead: that process has
// the authority open, and the exclusive open would fail as locked anyway.
// F_GETLK never reports this process's own locks, so the caller must have no
// other connection open on path.
func removeIdleSharedIndex(path string) error {
	shm := path + "-shm"
	info, err := os.Lstat(shm)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect authority index file: %w", err)
	}
	if info.Mode().IsRegular() {
		f, err := os.Open(shm)
		if err != nil {
			return fmt.Errorf("inspect authority index file: %w", err)
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: shmDMSByte, Len: 1}
		lockErr := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lock)
		_ = f.Close()
		if lockErr != nil {
			return fmt.Errorf("inspect authority index file lock: %w", lockErr)
		}
		if lock.Type != syscall.F_UNLCK {
			return fmt.Errorf("corestore: another process (pid %d) has the authority open", lock.Pid)
		}
	}
	if err := os.Remove(shm); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the earlier index file: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

func sqliteDSN(path string, busy time.Duration, readOnly bool) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "foreign_keys(ON)")
		q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
		q.Set("_dqs", "0")
	} else {
		// journal_mode(WAL) is set by openAuthorityDB, after locking_mode.
		q.Add("_pragma", "locking_mode(EXCLUSIVE)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Add("_pragma", "fullfsync(ON)")
		q.Add("_pragma", "checkpoint_fullfsync(ON)")
		q.Add("_pragma", "journal_size_limit("+strconv.Itoa(walSizeLimit)+")")
		q.Add("_pragma", "foreign_keys(ON)")
		q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
		q.Set("_txlock", "immediate")
		q.Set("_dqs", "0")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func verifyPragmas(ctx context.Context, db *sql.DB, busy time.Duration) error {
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return fmt.Errorf("verify journal mode: %w", err)
	}
	if strings.ToLower(journal) != "wal" {
		return fmt.Errorf("verify journal mode: got %q, want wal", journal)
	}
	var locking string
	if err := db.QueryRowContext(ctx, `PRAGMA main.locking_mode`).Scan(&locking); err != nil {
		return fmt.Errorf("verify locking mode: %w", err)
	}
	if strings.ToLower(locking) != "exclusive" {
		return fmt.Errorf("verify locking mode: got %q, want exclusive", locking)
	}
	var synchronous, foreignKeys, busyMS, fullfsync, checkpointFullfsync, journalLimit int64
	if err := db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		return fmt.Errorf("verify synchronous FULL: value=%d error=%w", synchronous, err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		return fmt.Errorf("verify foreign keys: value=%d error=%w", foreignKeys, err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyMS); err != nil || busyMS != busy.Milliseconds() {
		return fmt.Errorf("verify busy timeout: value=%d error=%w", busyMS, err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA fullfsync`).Scan(&fullfsync); err != nil || fullfsync != 1 {
		return fmt.Errorf("verify fullfsync: value=%d error=%w", fullfsync, err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA checkpoint_fullfsync`).Scan(&checkpointFullfsync); err != nil || checkpointFullfsync != 1 {
		return fmt.Errorf("verify checkpoint fullfsync: value=%d error=%w", checkpointFullfsync, err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&journalLimit); err != nil || journalLimit != walSizeLimit {
		return fmt.Errorf("verify journal size limit: value=%d error=%w", journalLimit, err)
	}
	return nil
}

func enforcePrivateModes(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure sqlite file: %w", err)
		}
	}
	return nil
}

// Close releases the database handle and reapplies private modes to the
// authority and any SQLite sidecars. It is idempotent.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
		if err := enforcePrivateModes(s.path); s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// Health returns mutation health. A false Ready value blocks critical
// mutations. Only the explicitly eligible head-watermark latches (a post-commit
// head-read timeout or a CommitObserver failure after the committed head was
// read) can be proof-recovered in process; every other latch requires an
// explicit reopen.
func (s *Store) Health() Health {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health
}

// AuthorityHead reads the current rollback identity and monotonic write head.
func (s *Store) AuthorityHead(ctx context.Context) (AuthorityHead, error) {
	head, err := readAuthorityHead(ctx, s.db)
	return head, s.latchCorruptRead(err)
}

func readAuthorityHead(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (AuthorityHead, error) {
	var h AuthorityHead
	err := q.QueryRowContext(ctx, `SELECT authority_epoch, head_generation, last_event_seq, signer_generation FROM store_meta WHERE singleton=1`).Scan(
		&h.AuthorityEpoch, &h.HeadGeneration, &h.LastEventSeq, &h.SignerGeneration,
	)
	return h, err
}

func requireMinimumHead(got, minimum AuthorityHead) error {
	if minimum.AuthorityEpoch != "" && got.AuthorityEpoch != minimum.AuthorityEpoch {
		return fmt.Errorf("%w: authority epoch differs", ErrRollback)
	}
	if got.HeadGeneration < minimum.HeadGeneration || got.LastEventSeq < minimum.LastEventSeq || got.SignerGeneration < minimum.SignerGeneration {
		return fmt.Errorf("%w: write head is older than required", ErrRollback)
	}
	return nil
}

// latchNotice is one Ready-to-blocked transition captured under the store
// locks and delivered to the health observer only after they are released.
type latchNotice struct {
	health Health
	cause  error
}

func (s *Store) latchNoticeFor(latched bool, cause error) *latchNotice {
	if !latched {
		return nil
	}
	return &latchNotice{health: s.Health(), cause: cause}
}

// notifyHealth reports one health transition to the configured observer. It
// runs after writeMu and healthMu are released so the observer may read
// Health or log without deadlocking the store.
func (s *Store) notifyHealth(health Health, cause error) {
	if s.healthObserver != nil {
		s.healthObserver(health, cause)
	}
}

func (s *Store) notifyLatch(notice *latchNotice) {
	if notice != nil {
		s.notifyHealth(notice.health, notice.cause)
	}
}

func (s *Store) criticalMutation(ctx context.Context, fn func(*sql.Tx) error) error {
	notice, err := s.criticalMutationLocked(ctx, fn)
	s.notifyLatch(notice)
	return err
}

// criticalMutationLocked runs one mutation under writeMu. A non-nil notice
// means this mutation latched the store; the caller delivers it once the lock
// is released.
func (s *Store) criticalMutationLocked(ctx context.Context, fn func(*sql.Tx) error) (*latchNotice, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Health().Ready {
		return nil, ErrBlocked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil && s.commitObserver != nil {
		err = s.headNotBackTx(ctx, tx)
	}
	if err == nil {
		err = fn(tx)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	latched := false
	if err == nil && s.commitObserver != nil {
		headCtx, cancel := context.WithTimeout(context.Background(), s.busyTimeout)
		head, headErr := s.readCommittedHead(headCtx)
		cancel()
		if headErr != nil {
			if errors.Is(headErr, context.DeadlineExceeded) {
				latched = s.latchRecoverableHeadWatermarkTimeout()
			} else {
				latched = s.latchCritical(headErr) || s.latchHealth("head_watermark")
			}
			err = fmt.Errorf("read committed authority head: %w", headErr)
		} else if observerErr := s.commitObserver(head); observerErr != nil {
			// The mutation is durable and its head was read; only the external
			// watermark is unproven, so the committed head is the recovery floor.
			latched = s.latchRecoverableObserverFailure(head)
			err = fmt.Errorf("persist committed authority head: %w", observerErr)
		} else {
			s.lastObservedHead = head
		}
	}
	if err != nil {
		latched = s.latchCritical(err) || latched
		if conflict, ok := errors.AsType[*RevisionConflictError](err); ok && conflict.LostWrite() {
			latched = s.latchHealth("lost_write") || latched
		}
		if errors.Is(err, ErrHeadWentBack) {
			latched = s.latchHealth("lost_write") || latched
		}
	}
	return s.latchNoticeFor(latched, err), err
}

// headNotBackTx refuses a mutation when the commit counter reads older than a
// commit this store saw land. Without it the next commit reuses the lost number
// and the watermark agrees again, so a lost tail leaves no trace. A store whose
// head stayed put while a revision went back is then a different loss, one row
// or the caller's belief, and the log says which it was. The caller holds
// writeMu.
func (s *Store) headNotBackTx(ctx context.Context, tx *sql.Tx) error {
	head, err := readAuthorityHead(ctx, tx)
	if err != nil {
		return err
	}
	last := s.lastObservedHead
	if head.AuthorityEpoch == last.AuthorityEpoch && head.HeadGeneration < last.HeadGeneration {
		return fmt.Errorf("%w: the last commit seen was generation %d, the database reads %d", ErrHeadWentBack, last.HeadGeneration, head.HeadGeneration)
	}
	return nil
}

func (s *Store) readCommittedHead(ctx context.Context) (AuthorityHead, error) {
	if s.readHead != nil {
		return s.readHead(ctx)
	}
	return readAuthorityHead(ctx, s.db)
}

// latchCritical latches a permanent SQLite failure class. It reports whether
// this call moved the store from Ready to blocked.
func (s *Store) latchCritical(err error) bool {
	code, critical := criticalSQLiteCode(err)
	if !critical {
		return false
	}
	return s.latchHealth(code)
}

// LatchDamaged blocks critical mutations because the owning process saw the
// authority's files changed outside SQLite's own errors, such as another
// process opening the file after this one lost its lock. Only a reopen clears
// it.
func (s *Store) LatchDamaged(code string, cause error) {
	s.notifyLatch(s.latchNoticeFor(s.latchHealth(code), cause))
}

// latchCorruptRead latches the store when a read finds the authority damaged
// and returns err unchanged. A mutation latches on its own failure; a read that
// meets a damaged page is the same evidence. Left Ready, the store kept taking
// writes into a damaged table while the reader only marked its own feature
// unavailable (2026-11-03: three calendar rows on cross-linked pages, a lost
// proposal write, then a daemon that refused to start). Callers must not hold
// writeMu, since the observer runs here.
func (s *Store) latchCorruptRead(err error) error {
	if code, _ := criticalSQLiteCode(err); code == "corrupt" {
		s.notifyLatch(s.latchNoticeFor(s.latchHealth(code), err))
	}
	return err
}

// latchHealth records a permanent latch that only an explicit reopen clears.
// It reports whether this call moved the store from Ready to blocked.
func (s *Store) latchHealth(code string) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if !s.health.Ready {
		return false
	}
	s.health = Health{Ready: false, Code: code, BlockedAt: time.Now().UTC()}
	s.recoveryMinimumHead = AuthorityHead{}
	return true
}

// latchRecoverableHeadWatermark records the one recovery-eligible latch class:
// the mutation is durable but its external head watermark is unproven.
// minimum is the newest head known durable; the proof must read a head at
// least that new and persist it before writes may reopen.
func (s *Store) latchRecoverableHeadWatermark(minimum AuthorityHead) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if !s.health.Ready {
		return false
	}
	s.recoveryMinimumHead = minimum
	s.health = Health{
		Ready: false, Code: "head_watermark", BlockedAt: time.Now().UTC(), RecoveryEligible: true,
	}
	return true
}

// latchRecoverableHeadWatermarkTimeout covers a post-commit head read that hit
// its bounded deadline. The caller holds writeMu, so lastObservedHead is the
// exact head whose watermark was known durable before the just-committed
// mutation; the committed head itself is unknown.
func (s *Store) latchRecoverableHeadWatermarkTimeout() bool {
	return s.latchRecoverableHeadWatermark(s.lastObservedHead)
}

// latchRecoverableObserverFailure covers a CommitObserver that could not
// persist a successfully read committed head. That head is the floor: the
// proof must read a head at least that new and persist it through the same
// observer before writes reopen, exactly as for the timeout class.
func (s *Store) latchRecoverableObserverFailure(head AuthorityHead) bool {
	return s.latchRecoverableHeadWatermark(head)
}

// RecoverTransientHeadWatermark attempts the only supported in-process
// authority recovery. The write lock keeps every mutation blocked throughout
// the proof. Success requires intact content, the exact authority epoch, a
// head no older than the latch's recovery floor, and a successful synchronous
// persistence of the current head through CommitObserver. A successful proof
// reports a Ready health to the configured HealthObserver.
func (s *Store) RecoverTransientHeadWatermark(ctx context.Context) (bool, error) {
	if s == nil {
		return false, ErrRecoveryNotEligible
	}
	recovered, err := s.recoverTransientHeadWatermarkLocked(ctx)
	if recovered {
		s.notifyHealth(Health{Ready: true}, nil)
	}
	return recovered, err
}

func (s *Store) recoverTransientHeadWatermarkLocked(ctx context.Context) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	health := s.Health()
	if health.Ready {
		return false, nil
	}
	if health.Code != "head_watermark" || !health.RecoveryEligible || s.commitObserver == nil {
		return false, ErrRecoveryNotEligible
	}

	check := s.checkIntegrity
	if check == nil {
		check = func(ctx context.Context) (IntegrityReport, error) { return checkIntegrityDB(ctx, s.db) }
	}
	report, err := check(ctx)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			if _, critical := criticalSQLiteCode(err); critical {
				s.disableTransientRecoveryForCritical(err)
			} else {
				s.disableTransientRecovery("integrity")
			}
		}
		return false, fmt.Errorf("verify authority integrity for recovery: %w", err)
	}
	if !report.OK() {
		s.disableTransientRecovery("corrupt")
		return false, integrityFailure(report)
	}

	head, err := s.readCommittedHead(ctx)
	if err != nil {
		s.disableTransientRecoveryForCritical(err)
		return false, fmt.Errorf("read authority head for recovery: %w", err)
	}
	if err := requireMinimumHead(head, s.recoveryMinimumHead); err != nil {
		s.disableTransientRecovery("rollback")
		return false, err
	}
	if err := s.commitObserver(head); err != nil {
		// Persistence is still uncertain. Keep the narrow latch eligible so a
		// later background proof can retry, but never reopen mutations now.
		return false, fmt.Errorf("persist recovered authority head: %w", err)
	}

	s.healthMu.Lock()
	if !s.health.Ready && s.health.Code == "head_watermark" && s.health.RecoveryEligible {
		s.health = Health{Ready: true}
		s.recoveryMinimumHead = AuthorityHead{}
		s.lastObservedHead = head
		s.healthMu.Unlock()
		return true, nil
	}
	s.healthMu.Unlock()
	return false, ErrRecoveryNotEligible
}

func (s *Store) disableTransientRecoveryForCritical(err error) {
	if code, critical := criticalSQLiteCode(err); critical {
		s.disableTransientRecovery(code)
	}
}

func (s *Store) disableTransientRecovery(code string) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.health.Ready {
		return
	}
	if strings.TrimSpace(code) != "" {
		s.health.Code = code
	}
	s.health.RecoveryEligible = false
	s.recoveryMinimumHead = AuthorityHead{}
}

func criticalSQLiteCode(err error) (string, bool) {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return "", false
	}
	switch sqliteErr.Code() & 0xff {
	case 5:
		return "busy", true
	case 8:
		return "readonly", true
	case 10:
		return "ioerr", true
	case 11, 26:
		return "corrupt", true
	case 13:
		return "full", true
	default:
		return "", false
	}
}

func advanceHeadTx(ctx context.Context, tx *sql.Tx, lastEventSeq int64, now time.Time) (AuthorityHead, error) {
	var h AuthorityHead
	err := tx.QueryRowContext(ctx, `UPDATE store_meta
SET head_generation=head_generation+1,
    last_event_seq=CASE WHEN last_event_seq < ? THEN ? ELSE last_event_seq END,
    updated_at=?
WHERE singleton=1
RETURNING authority_epoch, head_generation, last_event_seq, signer_generation`,
		lastEventSeq, lastEventSeq, formatTime(now)).Scan(&h.AuthorityEpoch, &h.HeadGeneration, &h.LastEventSeq, &h.SignerGeneration)
	return h, err
}

// AdvanceSignerGeneration atomically advances the preview-token signer
// generation from expected to a larger next value and advances the authority
// head. A concurrent or stale expected value returns ErrAuthorityMismatch.
func (s *Store) AdvanceSignerGeneration(ctx context.Context, expected, next int64) (AuthorityHead, error) {
	if expected < 1 || next <= expected {
		return AuthorityHead{}, errorsf("signer generation must increase")
	}
	var head AuthorityHead
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, `UPDATE store_meta
SET signer_generation=?, head_generation=head_generation+1, updated_at=?
WHERE singleton=1 AND signer_generation=?`, next, formatTime(now), expected)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrAuthorityMismatch
		}
		head, err = readAuthorityHead(ctx, tx)
		return err
	})
	return head, err
}

// Checkpoint quiesces in-process writers and fully checkpoints/truncates the
// WAL. A busy result is explicit; callers must not publish a cutover snapshot.
func (s *Store) Checkpoint(ctx context.Context) (CheckpointResult, error) {
	result, notice, err := s.checkpointLocked(ctx)
	s.notifyLatch(notice)
	return result, err
}

func (s *Store) checkpointLocked(ctx context.Context) (CheckpointResult, *latchNotice, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Health().Ready {
		return CheckpointResult{}, nil, ErrBlocked
	}
	var result CheckpointResult
	err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&result.Busy, &result.LogFrames, &result.CheckpointedFrames)
	if err != nil {
		err = fmt.Errorf("checkpoint authority WAL: %w", err)
		return CheckpointResult{}, s.latchNoticeFor(s.latchCritical(err), err), err
	}
	if result.Busy != 0 {
		return result, nil, ErrCheckpointBusy
	}
	if err := enforcePrivateModes(s.path); err != nil {
		return result, nil, err
	}
	return result, nil, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
