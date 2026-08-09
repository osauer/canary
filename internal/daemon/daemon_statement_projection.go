package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	statementProjectionScope   = "statements"
	statementProjectionVersion = 1
	statementProjectionStatus  = "parsed_v1"
	statementProjectionMaxRows = 10000
)

type statementProjectionFile struct {
	name       string
	size       int64
	digest     [sha256.Size]byte
	data       []byte
	statements []flexstmt.Statement
}

type statementEquityProjectionPayload struct {
	Version    int    `json:"version"`
	ReportDate string `json:"report_date"`
	TotalBase  string `json:"total_base"`
}

// refreshStatementProjection fingerprints the complete retained Flex XML set,
// parses every changed source before publishing anything, and transactionally
// replaces only the current inventory/equity winners. The original XML remains
// the broker evidence; SQLite is its typed derived view.
//
// A read or parse failure leaves the last complete SQLite projection intact.
// Same-name, same-size restatements are detected by SHA-256, and a removed file
// retracts the rows that only that file supplied.
func (s *Server) refreshStatementProjection(ctx context.Context) error {
	if s == nil || s.coreStore == nil {
		return fmt.Errorf("statement SQLite authority is unavailable")
	}
	files, err := readStatementProjectionFiles(ctx)
	if err != nil {
		return err
	}
	recorded, err := s.coreStore.LoadStatementFiles(ctx, statementProjectionScope)
	if err != nil {
		return fmt.Errorf("load statement projection inventory: %w", err)
	}
	if statementProjectionInventoryMatches(recorded, files) {
		return nil
	}
	if err := parseStatementProjectionFiles(ctx, files); err != nil {
		return err
	}
	fileRecords, days, err := buildStatementProjection(files, s.statementProjectionNow())
	if err != nil {
		return err
	}
	if err := s.coreStore.ReplaceStatementProjection(ctx, statementProjectionScope, fileRecords, days); err != nil {
		return fmt.Errorf("replace statement projection: %w", err)
	}
	return nil
}

func (s *Server) statementProjectionNow() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// readStatementProjectionFiles returns a coherent, deterministic snapshot of
// regular XML files. Symlinks are rejected so mutable evidence outside the
// private statements directory cannot enter the authoritative projection.
func readStatementProjectionFiles(ctx context.Context) ([]statementProjectionFile, error) {
	dir, err := flexStatementsDirPath()
	if err != nil {
		return nil, err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect statements directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return nil, fmt.Errorf("statements path is not a regular directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read statements directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".xml") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, fmt.Errorf("statement source %q is not a regular non-symlink file", entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	files := make([]statementProjectionFile, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect statement %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("statement source %q is not a regular non-symlink file", name)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open statement %q: %w", name, err)
		}
		opened, statErr := f.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = f.Close()
			if statErr == nil {
				statErr = fmt.Errorf("source identity changed while opening")
			}
			return nil, fmt.Errorf("inspect opened statement %q: %w", name, statErr)
		}
		data, readErr := io.ReadAll(f)
		afterRead, afterStatErr := f.Stat()
		if readErr == nil {
			readErr = afterStatErr
		}
		closedErr := f.Close()
		if readErr == nil {
			readErr = closedErr
		}
		if readErr != nil {
			return nil, fmt.Errorf("read statement %q: %w", name, readErr)
		}
		current, currentErr := os.Lstat(path)
		if currentErr != nil || !os.SameFile(afterRead, current) ||
			int64(len(data)) != opened.Size() || afterRead.Size() != opened.Size() || !afterRead.ModTime().Equal(opened.ModTime()) {
			return nil, fmt.Errorf("statement %q changed while reading", name)
		}
		files = append(files, statementProjectionFile{
			name: name, size: int64(len(data)), digest: sha256.Sum256(data), data: data,
		})
	}
	return files, nil
}

func statementProjectionInventoryMatches(recorded []corestore.StatementFileRecord, files []statementProjectionFile) bool {
	if len(recorded) != len(files) {
		return false
	}
	byName := make(map[string]corestore.StatementFileRecord, len(recorded))
	for _, file := range recorded {
		byName[file.FileKey] = file
	}
	for _, file := range files {
		record, ok := byName[file.name]
		if !ok || record.SizeBytes != file.size || record.SHA256 != file.digest || record.Status != statementProjectionStatus {
			return false
		}
	}
	return true
}

func parseStatementProjectionFiles(ctx context.Context, files []statementProjectionFile) error {
	for i := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		statements, err := flexstmt.Parse(files[i].data)
		if err != nil {
			return fmt.Errorf("parse retained statement %q: %w", files[i].name, err)
		}
		files[i].statements = statements
		files[i].data = nil
	}
	return nil
}

func buildStatementProjection(files []statementProjectionFile, ingestedAt time.Time) ([]corestore.StatementFileRecord, []corestore.StatementEquityDayRecord, error) {
	fileRecords := make([]corestore.StatementFileRecord, 0, len(files))
	winners := make(map[string]corestore.StatementEquityDayRecord)
	for _, file := range files {
		var latestGenerated time.Time
		for _, statement := range file.statements {
			generated := statement.WhenGenerated.UTC()
			if generated.After(latestGenerated) {
				latestGenerated = generated
			}
			for _, row := range statement.Equity {
				day := row.ReportDate.UTC().Format("2006-01-02")
				key := statement.AccountID + "\x00" + day
				if current, ok := winners[key]; ok && !generated.After(current.GeneratedAt) {
					continue
				}
				equityText := strconv.FormatFloat(row.TotalBase, 'g', -1, 64)
				raw, err := json.Marshal(statementEquityProjectionPayload{
					Version: statementProjectionVersion, ReportDate: day, TotalBase: equityText,
				})
				if err != nil {
					return nil, nil, fmt.Errorf("encode statement equity projection: %w", err)
				}
				winners[key] = corestore.StatementEquityDayRecord{
					AccountKey: statement.AccountID, Day: day, EquityBaseText: equityText,
					StatementFileKey: file.name, StatementFileSHA256: file.digest,
					GeneratedAt: generated, RawJSON: raw,
				}
			}
		}
		generated := latestGenerated
		fileRecords = append(fileRecords, corestore.StatementFileRecord{
			FileKey: file.name, SizeBytes: file.size, SHA256: file.digest,
			Status: statementProjectionStatus, StatementGeneratedAt: &generated,
			IngestedAt: &ingestedAt,
		})
	}
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	days := make([]corestore.StatementEquityDayRecord, 0, len(keys))
	for _, key := range keys {
		days = append(days, winners[key])
	}
	return fileRecords, days, nil
}

// sqliteStatementEquityDays is the SQLite replacement for history.db's
// statement-equity query. Until is exclusive; returned rows are newest first.
func (s *Server) sqliteStatementEquityDays(ctx context.Context, since, until time.Time, limit int) ([]rpc.EquityDayEntry, int, error) {
	if s == nil || s.coreStore == nil {
		return nil, 0, fmt.Errorf("statement SQLite authority is unavailable")
	}
	if limit <= 0 || limit > reconEquityMaxLimit {
		return nil, 0, fmt.Errorf("statement equity limit is invalid")
	}
	fromDay := since.UTC().Format("2006-01-02")
	toDay := until.UTC().Add(-time.Nanosecond).Format("2006-01-02")
	rows, err := s.coreStore.LoadStatementEquityDays(ctx, statementProjectionScope, fromDay, toDay, statementProjectionMaxRows)
	if err != nil {
		return nil, 0, fmt.Errorf("load statement equity projection: %w", err)
	}
	if len(rows) == statementProjectionMaxRows {
		return nil, 0, fmt.Errorf("statement equity projection exceeds supported query size")
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Day != rows[j].Day {
			return rows[i].Day > rows[j].Day
		}
		return rows[i].AccountKey < rows[j].AccountKey
	})
	total := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	entries := make([]rpc.EquityDayEntry, 0, len(rows))
	for _, row := range rows {
		equity, err := strconv.ParseFloat(row.EquityBaseText, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("decode statement equity for %s: %w", row.Day, err)
		}
		entries = append(entries, rpc.EquityDayEntry{
			Day: row.Day, AccountID: row.AccountKey, EquityBase: equity,
			SourceStmt: row.StatementFileKey, WhenGenerated: row.GeneratedAt,
		})
	}
	return entries, total, nil
}

// sqliteStatementsHealth keeps the existing RPC health shape during the
// authority migration: projected bytes versus the current retained XML set.
func (s *Server) sqliteStatementsHealth(ctx context.Context) (rpc.HistoryIndexHealth, error) {
	var health rpc.HistoryIndexHealth
	if s == nil || s.coreStore == nil {
		return health, fmt.Errorf("statement SQLite authority is unavailable")
	}
	files, err := s.coreStore.LoadStatementFiles(ctx, statementProjectionScope)
	if err != nil {
		return health, fmt.Errorf("load statement projection health: %w", err)
	}
	for _, file := range files {
		health.IngestedBytes += file.SizeBytes
		if file.IngestedAt != nil && file.IngestedAt.After(health.LastIngestAt) {
			health.LastIngestAt = *file.IngestedAt
		}
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return health, err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return health, nil
		}
		return health, fmt.Errorf("inspect statement health directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return health, fmt.Errorf("statement health path is not a regular directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return health, nil
		}
		return health, fmt.Errorf("read statement health sources: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".xml") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return health, fmt.Errorf("inspect statement health source %q: %w", entry.Name(), err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return health, fmt.Errorf("statement health source %q is not a regular non-symlink file", entry.Name())
		}
		health.JournalBytes += info.Size()
	}
	return health, nil
}
