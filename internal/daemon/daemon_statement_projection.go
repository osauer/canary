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
	statementProjectionVersion = 8
	statementProjectionStatus  = "parsed_v8"
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

type statementMetadataProjectionPayload struct {
	Version          int                          `json:"version"`
	QueryFingerprint string                       `json:"query_fingerprint,omitempty"`
	FromDate         time.Time                    `json:"from_date"`
	ToDate           time.Time                    `json:"to_date"`
	ManifestVersion  string                       `json:"manifest_version"`
	Coverage         []flexstmt.SectionCoverage   `json:"coverage"`
	PositionSnapshot []flexstmt.OpenPosition      `json:"position_snapshot"`
	Financing        *flexstmt.FinancingStatement `json:"financing,omitempty"`
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
	selection := s.flexEvidenceSelection()
	projectionScope := statementProjectionScopeForSelection(selection)
	files, err := readStatementProjectionFiles(ctx, selection)
	if err != nil {
		return err
	}
	recorded, err := s.coreStore.LoadStatementFiles(ctx, projectionScope)
	if err != nil {
		return fmt.Errorf("load statement projection inventory: %w", err)
	}
	if statementProjectionInventoryMatches(recorded, files) {
		return nil
	}
	if err := parseStatementProjectionFiles(ctx, files); err != nil {
		return err
	}
	fileRecords, days, records, recordVersions, err := buildStatementProjection(files, s.statementProjectionNow(), selection.ActiveQueryFingerprint)
	if err != nil {
		return err
	}
	if err := s.coreStore.ReplaceStatementProjection(ctx, projectionScope, fileRecords, days, records, recordVersions); err != nil {
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
func readStatementProjectionFiles(ctx context.Context, selection flexEvidenceSelection) ([]statementProjectionFile, error) {
	return readStatementProjectionFilesWithCache(ctx, selection, nil)
}

// cache must be locked through parsing: a verified warm file can omit its XML
// only while the matching parsed entry cannot be replaced by another reader.
func readStatementProjectionFilesWithCache(ctx context.Context, selection flexEvidenceSelection, cache *retainedFlexCache) ([]statementProjectionFile, error) {
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
		included, err := selection.includesRetainedFlexFile(entry.Name())
		if err != nil {
			return nil, err
		}
		if included {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	files := make([]statementProjectionFile, 0, len(names))
	var buffer []byte
	if cache != nil {
		buffer = make([]byte, 32*1024)
	}
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
		data, size, digest, readErr := readStatementProjectionData(ctx, f, name, cache, buffer)
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
			size != opened.Size() || afterRead.Size() != opened.Size() || !afterRead.ModTime().Equal(opened.ModTime()) {
			return nil, fmt.Errorf("statement %q changed while reading", name)
		}
		files = append(files, statementProjectionFile{
			name: name, size: size, digest: digest, data: data,
		})
	}
	return files, nil
}

// Hash every byte on every read, including same-size/same-time restatements.
// Reuse only the parser output; file identity and accepted inventory remain
// checked by the caller. Changed evidence still supplies exact bytes to parse.
func readStatementProjectionData(ctx context.Context, f *os.File, name string, cache *retainedFlexCache, buffer []byte) ([]byte, int64, [sha256.Size]byte, error) {
	if cache == nil {
		data, err := io.ReadAll(f)
		return data, int64(len(data)), sha256.Sum256(data), err
	}
	h := sha256.New()
	var size int64
	var digest [sha256.Size]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, digest, err
		}
		n, err := f.Read(buffer)
		size += int64(n)
		_, _ = h.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, digest, err
		}
	}
	h.Sum(digest[:0])
	if entry, ok := cache.rows[name]; ok && entry.digest == digest {
		return nil, size, digest, ctx.Err()
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, digest, err
	}
	data, err := io.ReadAll(f)
	if err == nil && (int64(len(data)) != size || sha256.Sum256(data) != digest) {
		err = fmt.Errorf("statement source changed while reading")
	}
	return data, size, digest, err
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
		statements, err := flexstmt.ParseContext(ctx, files[i].data)
		if err != nil {
			return fmt.Errorf("parse retained statement %q: %w", files[i].name, err)
		}
		files[i].statements = statements
		files[i].data = nil
	}
	return nil
}

func buildStatementProjection(files []statementProjectionFile, ingestedAt time.Time, queryFingerprint string) ([]corestore.StatementFileRecord, []corestore.StatementEquityDayRecord, []corestore.StatementRecord, []corestore.StatementRecord, error) {
	fileRecords := make([]corestore.StatementFileRecord, 0, len(files))
	winners := make(map[string]corestore.StatementEquityDayRecord)
	recordWinners := make(map[string]corestore.StatementRecord)
	recordVersions := []corestore.StatementRecord{}
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
					return nil, nil, nil, nil, fmt.Errorf("encode statement equity projection: %w", err)
				}
				winners[key] = corestore.StatementEquityDayRecord{
					AccountKey: statement.AccountID, Day: day, EquityBaseText: equityText,
					StatementFileKey: file.name, StatementFileSHA256: file.digest,
					GeneratedAt: generated, RawJSON: raw,
				}
			}
			if err := addStatementTypedRecords(recordWinners, &recordVersions, file, statement, queryFingerprint); err != nil {
				return nil, nil, nil, nil, err
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
	recordKeys := make([]string, 0, len(recordWinners))
	for key := range recordWinners {
		recordKeys = append(recordKeys, key)
	}
	sort.Strings(recordKeys)
	records := make([]corestore.StatementRecord, 0, len(recordKeys))
	for _, key := range recordKeys {
		records = append(records, recordWinners[key])
	}
	return fileRecords, days, records, recordVersions, nil
}

func addStatementTypedRecords(winners map[string]corestore.StatementRecord, versions *[]corestore.StatementRecord, file statementProjectionFile, statement flexstmt.Statement, queryFingerprint string) error {
	generated := statement.WhenGenerated.UTC()
	account := statement.AccountID
	add := func(kind, recordKey string, effectiveAt time.Time, payload any) error {
		if effectiveAt.IsZero() {
			effectiveAt = statement.ToDate
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode %s statement record: %w", kind, err)
		}
		projectionKey := statementProjectionRecordKey(account, recordKey)
		key := kind + "\x00" + projectionKey
		candidate := corestore.StatementRecord{
			Kind: kind, RecordKey: projectionKey, AccountKey: account,
			EffectiveAt: effectiveAt.UTC(), StatementFileKey: file.name, StatementFileSHA256: file.digest,
			GeneratedAt: generated, RawJSON: raw,
		}
		*versions = append(*versions, candidate)
		current, ok := winners[key]
		if !ok || generated.After(current.GeneratedAt) || (generated.Equal(current.GeneratedAt) && strings.Compare(hexDigest(file.digest), hexDigest(current.StatementFileSHA256)) > 0) {
			winners[key] = candidate
		}
		return nil
	}
	metadataKey := "statement:" + statement.FromDate.UTC().Format(time.DateOnly) + ":" + statement.ToDate.UTC().Format(time.DateOnly)
	positionSnapshot := []flexstmt.OpenPosition(nil)
	if statementSectionPresent(statement.Coverage, "open_positions") {
		positionSnapshot = append([]flexstmt.OpenPosition{}, statement.Positions...)
	}
	if err := add(corestore.StatementRecordMetadata, metadataKey, statement.ToDate, statementMetadataProjectionPayload{
		Version: statementProjectionVersion, QueryFingerprint: queryFingerprint,
		FromDate: statement.FromDate.UTC(), ToDate: statement.ToDate.UTC(),
		ManifestVersion: statement.ManifestVersion, Coverage: append([]flexstmt.SectionCoverage(nil), statement.Coverage...),
		PositionSnapshot: positionSnapshot, Financing: statement.Financing,
	}); err != nil {
		return err
	}
	for _, item := range statement.Trades {
		if err := add(corestore.StatementRecordTrade, item.RecordID, item.ExecutedAt, item); err != nil {
			return err
		}
	}
	for _, item := range statement.Instruments {
		if err := add(corestore.StatementRecordInstrument, item.RecordID, item.ReportDate, item); err != nil {
			return err
		}
	}
	for _, item := range statement.Positions {
		if err := add(corestore.StatementRecordPosition, item.RecordID, item.ReportDate, item); err != nil {
			return err
		}
	}
	for _, item := range statement.OptionEvents {
		if err := add(corestore.StatementRecordOptionEvent, item.RecordID, item.Date, item); err != nil {
			return err
		}
	}
	for _, item := range statement.CorporateActions {
		if err := add(corestore.StatementRecordCorporateAction, item.RecordID, item.Date, item); err != nil {
			return err
		}
	}
	for _, item := range statement.Transfers {
		if err := add(corestore.StatementRecordTransfer, item.ID, item.Date, item); err != nil {
			return err
		}
	}
	for _, item := range statement.Cash {
		if err := add(corestore.StatementRecordCash, item.ID, item.ValueDate, item); err != nil {
			return err
		}
	}
	for _, item := range statement.FXRates {
		if err := add(corestore.StatementRecordFXRate, item.RecordID, item.Date, item); err != nil {
			return err
		}
	}
	return nil
}

func statementSectionPresent(coverage []flexstmt.SectionCoverage, key string) bool {
	for _, section := range coverage {
		if section.Key == key && section.Present {
			return true
		}
	}
	return false
}

func hexDigest(value [sha256.Size]byte) string {
	return fmt.Sprintf("%x", value[:])
}

func statementProjectionRecordKey(account, recordKey string) string {
	digest := sha256.Sum256([]byte("canary.statement.record.v1\x00" + account + "\x00" + recordKey))
	return fmt.Sprintf("rec_%x", digest[:16])
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
	projectionScope := s.activeStatementProjectionScope()
	rows, err := s.coreStore.LoadStatementEquityDays(ctx, projectionScope, fromDay, toDay, statementProjectionMaxRows)
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
	selection := s.flexEvidenceSelection()
	files, err := s.coreStore.LoadStatementFiles(ctx, statementProjectionScopeForSelection(selection))
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
		included, err := selection.includesRetainedFlexFile(entry.Name())
		if err != nil {
			return health, err
		}
		if !included {
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
