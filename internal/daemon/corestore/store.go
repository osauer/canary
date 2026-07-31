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
	"time"

	"modernc.org/sqlite"
)

const defaultBusyTimeout = 5 * time.Second

// Store is the daemon-owned authoritative handle. Its database handle is
// intentionally private so all writes pass through typed transactions.
type Store struct {
	db             *sql.DB
	path           string
	busyTimeout    time.Duration
	commitObserver func(AuthorityHead) error

	writeMu   sync.Mutex
	healthMu  sync.RWMutex
	health    Health
	closeOnce sync.Once
	closeErr  error
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

	dsn := sqliteDSN(path, timeout, false)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite authority: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("connect sqlite authority: %w", err))
	}
	if err := verifyPragmas(ctx, db, timeout); err != nil {
		return fail(err)
	}
	if report, err := checkIntegrityDB(ctx, db); err != nil {
		return fail(fmt.Errorf("startup integrity check: %w", err))
	} else if !report.OK() {
		return fail(integrityFailure(report))
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
	return &Store{
		db: db, path: path, busyTimeout: timeout,
		commitObserver: opts.CommitObserver,
		health:         Health{Ready: true},
	}, nil
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

func sqliteDSN(path string, busy time.Duration, readOnly bool) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "foreign_keys(ON)")
		q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
		q.Set("_dqs", "0")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Add("_pragma", "fullfsync(ON)")
		q.Add("_pragma", "checkpoint_fullfsync(ON)")
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
	var synchronous, foreignKeys, busyMS, fullfsync, checkpointFullfsync int64
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

// Health returns the process-lifetime mutation-health latch. A false Ready
// value blocks later critical mutations until the store is closed and reopened.
func (s *Store) Health() Health {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health
}

// AuthorityHead reads the current rollback identity and monotonic write head.
func (s *Store) AuthorityHead(ctx context.Context) (AuthorityHead, error) {
	return readAuthorityHead(ctx, s.db)
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

func (s *Store) criticalMutation(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Health().Ready {
		return ErrBlocked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		err = fn(tx)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if err == nil && s.commitObserver != nil {
		headCtx, cancel := context.WithTimeout(context.Background(), s.busyTimeout)
		head, headErr := readAuthorityHead(headCtx, s.db)
		cancel()
		if headErr != nil {
			s.latchCritical(headErr)
			if s.Health().Ready {
				s.latchHealth("head_watermark")
			}
			err = fmt.Errorf("read committed authority head: %w", headErr)
		} else if observerErr := s.commitObserver(head); observerErr != nil {
			s.latchHealth("head_watermark")
			err = fmt.Errorf("persist committed authority head: %w", observerErr)
		}
	}
	if err != nil {
		s.latchCritical(err)
	}
	return err
}

func (s *Store) latchCritical(err error) {
	code, critical := criticalSQLiteCode(err)
	if !critical {
		return
	}
	s.latchHealth(code)
}

func (s *Store) latchHealth(code string) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if !s.health.Ready {
		return
	}
	s.health = Health{Ready: false, Code: code, BlockedAt: time.Now().UTC()}
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Health().Ready {
		return CheckpointResult{}, ErrBlocked
	}
	var result CheckpointResult
	err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&result.Busy, &result.LogFrames, &result.CheckpointedFrames)
	if err != nil {
		s.latchCritical(err)
		return CheckpointResult{}, fmt.Errorf("checkpoint authority WAL: %w", err)
	}
	if result.Busy != 0 {
		return result, ErrCheckpointBusy
	}
	if err := enforcePrivateModes(s.path); err != nil {
		return result, err
	}
	return result, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
