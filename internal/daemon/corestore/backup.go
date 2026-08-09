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

	"modernc.org/sqlite"
)

type onlineBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

// Backup creates a verified, consistent backup without replacing an existing
// destination. The returned head is read from the backup snapshot itself.
func (s *Store) Backup(ctx context.Context, destination string) (BackupInfo, error) {
	if destination == "" {
		return BackupInfo{}, errorsf("backup path is required")
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("resolve backup path: %w", err)
	}
	if destination == s.path {
		return BackupInfo{}, errorsf("backup path must differ from authority path")
	}
	if err := ensurePrivateParent(filepath.Dir(destination)); err != nil {
		return BackupInfo{}, err
	}
	if _, err := os.Lstat(destination); err == nil {
		return BackupInfo{}, errorsf("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupInfo{}, fmt.Errorf("inspect backup destination: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return BackupInfo{}, fmt.Errorf("create backup temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer cleanupBackupTemp(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return BackupInfo{}, err
	}
	if err := temp.Close(); err != nil {
		return BackupInfo{}, err
	}

	s.writeMu.Lock()
	if !s.Health().Ready {
		s.writeMu.Unlock()
		return BackupInfo{}, ErrBlocked
	}
	minimum, err := readAuthorityHead(ctx, s.db)
	if err == nil {
		err = s.runOnlineBackup(ctx, tempPath)
	}
	s.writeMu.Unlock()
	if err != nil {
		return BackupInfo{}, fmt.Errorf("online backup: %w", err)
	}
	if err := normalizeBackupJournal(ctx, tempPath); err != nil {
		return BackupInfo{}, err
	}
	if err := removeBackupSidecars(tempPath); err != nil {
		return BackupInfo{}, err
	}
	if err := syncFile(tempPath); err != nil {
		return BackupInfo{}, err
	}
	info, err := VerifyBackup(ctx, tempPath, minimum)
	if err != nil {
		return BackupInfo{}, err
	}
	if err := os.Link(tempPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupInfo{}, errorsf("backup destination already exists")
		}
		return BackupInfo{}, fmt.Errorf("publish backup: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return BackupInfo{}, err
	}
	info.Path = destination
	return info, nil
}

// normalizeBackupJournal makes the unpublished backup self-contained. Online
// backup copies from the WAL authority, so the destination header can otherwise
// continue to require WAL sidecars that are not part of the published artifact.
func normalizeBackupJournal(ctx context.Context, path string) error {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(defaultBusyTimeout.Milliseconds(), 10)+")")
	q.Set("_dqs", "0")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("open backup for journal normalization: %w", err)
	}
	db.SetMaxOpenConns(1)
	var journal string
	queryErr := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&journal)
	closeErr := db.Close()
	if queryErr != nil {
		return fmt.Errorf("normalize backup journal mode: %w", queryErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close normalized backup: %w", closeErr)
	}
	if strings.ToLower(journal) != "delete" {
		return fmt.Errorf("normalize backup journal mode: got %q, want delete", journal)
	}
	return nil
}

func removeBackupSidecars(path string) error {
	var joined error
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, fmt.Errorf("remove backup%s: %w", suffix, err))
		}
	}
	return joined
}

func cleanupBackupTemp(path string) {
	_ = removeBackupSidecars(path)
	_ = os.Remove(path)
}

func (s *Store) runOnlineBackup(ctx context.Context, destination string) error {
	return runOnlineBackupDB(ctx, s.db, destination)
}

func runOnlineBackupDB(ctx context.Context, db *sql.DB, destination string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(onlineBackuper)
		if !ok {
			return errorsf("sqlite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(256)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		finished = true
		return backup.Finish()
	})
}

// VerifyBackup performs read-only schema/checksum, integrity, foreign-key,
// and minimum-head checks. It never upgrades or repairs the candidate.
func VerifyBackup(ctx context.Context, path string, minimum AuthorityHead) (BackupInfo, error) {
	plan := currentMigrationPlan()
	return verifyBackupWithPlan(ctx, path, minimum, len(plan), plan)
}

func verifyBackupWithPlan(ctx context.Context, path string, minimum AuthorityHead, expectedVersion int, plan []migration) (BackupInfo, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return BackupInfo{}, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		return BackupInfo{}, fmt.Errorf("open backup for verification: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return BackupInfo{}, fmt.Errorf("open backup for verification: %w", err)
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, expectedVersion, plan); err != nil {
		return BackupInfo{}, fmt.Errorf("verify backup schema: %w", err)
	}
	report, err := checkIntegrityDB(ctx, db)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("verify backup integrity: %w", err)
	}
	if !report.OK() {
		return BackupInfo{}, integrityFailure(report)
	}
	head, err := readAuthorityHead(ctx, db)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("verify backup head: %w", err)
	}
	if err := requireMinimumHead(head, minimum); err != nil {
		return BackupInfo{}, err
	}
	return BackupInfo{Path: path, SchemaVersion: expectedVersion, Head: head, Integrity: report}, nil
}

func validateSchemaLedgerWithPlan(ctx context.Context, db *sql.DB, expectedVersion int, plan []migration) error {
	if err := validateMigrationPlan(plan); err != nil {
		return err
	}
	if expectedVersion < 1 || expectedVersion > len(plan) {
		return errorsf("unsupported schema version")
	}
	var version, appID int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&appID); err != nil {
		return err
	}
	if version != expectedVersion {
		return fmt.Errorf("schema version %d does not match expected %d", version, expectedVersion)
	}
	if appID != applicationID {
		return errorsf("application identity mismatch")
	}
	rows, err := db.QueryContext(ctx, `SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var v int
		var name, checksum string
		if err := rows.Scan(&v, &name, &checksum); err != nil {
			return err
		}
		if v != seen+1 || v > expectedVersion {
			return errorsf("migration ledger is non-contiguous")
		}
		want := plan[v-1]
		if name != want.name || checksum != migrationChecksum(want) {
			return fmt.Errorf("migration checksum drift at version %d", v)
		}
		seen = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if seen != expectedVersion {
		return errorsf("migration ledger does not match schema version")
	}
	return validateSchemaObjectsWithPlan(ctx, db, expectedVersion, plan)
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	return f.Sync()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
