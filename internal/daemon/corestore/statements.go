package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReplaceStatementProjection atomically replaces the complete current
// inventory/winner projection while retaining every distinct file-content and
// derived-day version as append-only evidence. A same-name restatement is a
// new version when its SHA-256 changes, regardless of file size.
func (s *Store) ReplaceStatementProjection(ctx context.Context, scopeKey string, files []StatementFileRecord, days []StatementEquityDayRecord, records, recordVersions []StatementRecord) error {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return err
	}
	fileByKey := make(map[string]StatementFileRecord, len(files))
	for i := range files {
		if files[i].ScopeKey != "" && files[i].ScopeKey != scopeKey {
			return errorsf("statement file scope mismatch")
		}
		if err := validateStatementFile(files[i]); err != nil {
			return err
		}
		if _, duplicate := fileByKey[files[i].FileKey]; duplicate {
			return errorsf("duplicate statement file key")
		}
		fileByKey[files[i].FileKey] = files[i]
	}
	winners := make(map[string]struct{}, len(days))
	for i := range days {
		if days[i].ScopeKey != "" && days[i].ScopeKey != scopeKey {
			return errorsf("statement day scope mismatch")
		}
		if err := validateStatementDay(days[i]); err != nil {
			return err
		}
		file, ok := fileByKey[days[i].StatementFileKey]
		if !ok {
			return errorsf("statement day references a file outside the complete inventory")
		}
		if days[i].StatementFileSHA256 != ([sha256.Size]byte{}) && days[i].StatementFileSHA256 != file.SHA256 {
			return errorsf("statement day file digest does not match inventory")
		}
		winnerKey := days[i].AccountKey + "\x00" + days[i].Day
		if _, duplicate := winners[winnerKey]; duplicate {
			return errorsf("duplicate current statement equity-day winner")
		}
		winners[winnerKey] = struct{}{}
	}
	recordWinners := make(map[string]struct{}, len(records))
	for i := range records {
		if records[i].ScopeKey != "" && records[i].ScopeKey != scopeKey {
			return errorsf("statement record scope mismatch")
		}
		if err := validateStatementRecord(records[i]); err != nil {
			return err
		}
		file, ok := fileByKey[records[i].StatementFileKey]
		if !ok {
			return errorsf("statement record references a file outside the complete inventory")
		}
		if records[i].StatementFileSHA256 != ([sha256.Size]byte{}) && records[i].StatementFileSHA256 != file.SHA256 {
			return errorsf("statement record file digest does not match inventory")
		}
		winnerKey := records[i].Kind + "\x00" + records[i].RecordKey
		if _, duplicate := recordWinners[winnerKey]; duplicate {
			return errorsf("duplicate current statement record winner")
		}
		recordWinners[winnerKey] = struct{}{}
	}
	// A caller that has no separate all-version inventory can preserve the
	// former behavior by passing nil. The daemon projection passes every typed
	// row from every retained XML source here, including rows that lost the
	// deterministic current-winner comparison.
	if recordVersions == nil {
		recordVersions = records
	}
	for i := range recordVersions {
		if recordVersions[i].ScopeKey != "" && recordVersions[i].ScopeKey != scopeKey {
			return errorsf("statement record-version scope mismatch")
		}
		if err := validateStatementRecord(recordVersions[i]); err != nil {
			return err
		}
		file, ok := fileByKey[recordVersions[i].StatementFileKey]
		if !ok {
			return errorsf("statement record version references a file outside the complete inventory")
		}
		if recordVersions[i].StatementFileSHA256 != ([sha256.Size]byte{}) && recordVersions[i].StatementFileSHA256 != file.SHA256 {
			return errorsf("statement record-version file digest does not match inventory")
		}
	}
	currentRecords, currentMetadata := partitionStatementMetadata(records)
	versionRecords, versionMetadata := partitionStatementMetadata(recordVersions)
	return s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		stamp := formatTime(now)
		for _, file := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_file_versions
(scope_key,file_key,sha256,size_bytes,status,statement_generated_at,ingested_at,recorded_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(scope_key,file_key,sha256) DO NOTHING`, scopeKey, file.FileKey, file.SHA256[:], file.SizeBytes, file.Status, nullableTime(file.StatementGeneratedAt), nullableTime(file.IngestedAt), stamp); err != nil {
				return fmt.Errorf("append statement file version: %w", err)
			}
		}
		for _, day := range days {
			file := fileByKey[day.StatementFileKey]
			rawDigest := sha256.Sum256(day.RawJSON)
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_equity_day_versions
(scope_key,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, scopeKey, day.AccountKey, day.Day, day.EquityBaseText, day.StatementFileKey, file.SHA256[:], formatTime(day.GeneratedAt), day.RawJSON, rawDigest[:], stamp); err != nil {
				return fmt.Errorf("append statement equity-day version: %w", err)
			}
		}
		for _, record := range versionRecords {
			file := fileByKey[record.StatementFileKey]
			rawDigest := sha256.Sum256(record.RawJSON)
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_record_versions
(scope_key,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, scopeKey, record.Kind, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON, rawDigest[:], stamp); err != nil {
				return fmt.Errorf("append statement record version: %w", err)
			}
		}
		for _, record := range versionMetadata {
			file := fileByKey[record.StatementFileKey]
			rawDigest := sha256.Sum256(record.RawJSON)
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_metadata_versions
(scope_key,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, scopeKey, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON, rawDigest[:], stamp); err != nil {
				return fmt.Errorf("append statement metadata version: %w", err)
			}
		}
		// These are current projections only. Deleting/replacing them never
		// touches the immutable version tables above.
		if _, err := tx.ExecContext(ctx, `DELETE FROM statement_equity_days WHERE scope_key=?`, scopeKey); err != nil {
			return fmt.Errorf("replace current statement days: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM statement_records WHERE scope_key=?`, scopeKey); err != nil {
			return fmt.Errorf("replace current statement records: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM statement_metadata WHERE scope_key=?`, scopeKey); err != nil {
			return fmt.Errorf("replace current statement metadata: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM statement_files WHERE scope_key=?`, scopeKey); err != nil {
			return fmt.Errorf("replace current statement inventory: %w", err)
		}
		for _, file := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_files
(scope_key,file_key,size_bytes,sha256,status,statement_generated_at,ingested_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`, scopeKey, file.FileKey, file.SizeBytes, file.SHA256[:], file.Status, nullableTime(file.StatementGeneratedAt), nullableTime(file.IngestedAt), stamp); err != nil {
				return fmt.Errorf("write current statement inventory: %w", err)
			}
		}
		for _, day := range days {
			file := fileByKey[day.StatementFileKey]
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_equity_days
(scope_key,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, scopeKey, day.AccountKey, day.Day, day.EquityBaseText, day.StatementFileKey, file.SHA256[:], formatTime(day.GeneratedAt), day.RawJSON, stamp); err != nil {
				return fmt.Errorf("write current statement equity-day winner: %w", err)
			}
		}
		for _, record := range currentRecords {
			file := fileByKey[record.StatementFileKey]
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_records
(scope_key,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`, scopeKey, record.Kind, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON, stamp); err != nil {
				return fmt.Errorf("write current statement record: %w", err)
			}
		}
		for _, record := range currentMetadata {
			file := fileByKey[record.StatementFileKey]
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_metadata
(scope_key,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, scopeKey, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON, stamp); err != nil {
				return fmt.Errorf("write current statement metadata: %w", err)
			}
		}
		_, err := advanceHeadTx(ctx, tx, 0, now)
		return err
	})
}

// LoadStatementRecords returns current typed Flex records ordered by kind,
// effective time, and stable record key. An empty kind list selects all kinds.
func (s *Store) LoadStatementRecords(ctx context.Context, scopeKey string, kinds []string, limit int) ([]StatementRecord, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 250000 {
		return nil, errorsf("statement record limit is invalid")
	}
	return loadStatementRecordsIncludingMetadataQuery(ctx, s.db, scopeKey, kinds, limit)
}

type statementRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadStatementRecordsQuery(ctx context.Context, q statementRowsQueryer, scopeKey string, kinds []string, limit int) ([]StatementRecord, error) {
	clauses := []string{"scope_key=?"}
	args := []any{scopeKey}
	if len(kinds) > 0 {
		placeholders := make([]string, 0, len(kinds))
		seen := map[string]bool{}
		for _, kind := range kinds {
			if !validStatementRecordKind(kind) {
				return nil, errorsf("statement record kind is invalid")
			}
			if seen[kind] {
				continue
			}
			seen[kind] = true
			placeholders = append(placeholders, "?")
			args = append(args, kind)
		}
		if len(placeholders) > 0 {
			clauses = append(clauses, "record_kind IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `SELECT record_id,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json FROM statement_records WHERE `+strings.Join(clauses, " AND ")+` ORDER BY record_kind,effective_at,record_key LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatementRecord{}
	for rows.Next() {
		var item StatementRecord
		var effectiveAt, generatedAt string
		var digest []byte
		item.ScopeKey = scopeKey
		if err := rows.Scan(&item.ID, &item.Kind, &item.RecordKey, &item.AccountKey, &effectiveAt, &item.StatementFileKey, &digest, &generatedAt, &item.RawJSON); err != nil {
			return nil, err
		}
		if len(digest) != sha256.Size {
			return nil, errorsf("stored statement-record file digest is invalid")
		}
		copy(item.StatementFileSHA256[:], digest)
		item.EffectiveAt, err = parseTime(effectiveAt)
		if err != nil {
			return nil, err
		}
		item.GeneratedAt, err = parseTime(generatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func loadStatementRecordsIncludingMetadataQuery(ctx context.Context, q statementRowsQueryer, scopeKey string, kinds []string, limit int) ([]StatementRecord, error) {
	metadataSelected := len(kinds) == 0
	baseKinds := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if !validStatementRecordKind(kind) {
			return nil, errorsf("statement record kind is invalid")
		}
		if kind == StatementRecordMetadata {
			metadataSelected = true
			continue
		}
		baseKinds = append(baseKinds, kind)
	}
	loadBase := len(kinds) == 0 || len(baseKinds) > 0
	out := []StatementRecord{}
	var err error
	if loadBase {
		out, err = loadStatementRecordsQuery(ctx, q, scopeKey, baseKinds, limit)
		if err != nil {
			return nil, err
		}
	}
	if metadataSelected && len(out) < limit {
		metadata, err := loadStatementMetadataQuery(ctx, q, scopeKey, limit-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, metadata...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if !out[i].EffectiveAt.Equal(out[j].EffectiveAt) {
			return out[i].EffectiveAt.Before(out[j].EffectiveAt)
		}
		return out[i].RecordKey < out[j].RecordKey
	})
	return out, nil
}

func loadStatementMetadataQuery(ctx context.Context, q statementRowsQueryer, scopeKey string, limit int) ([]StatementRecord, error) {
	rows, err := q.QueryContext(ctx, `SELECT metadata_id,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json FROM statement_metadata WHERE scope_key=? ORDER BY effective_at,record_key LIMIT ?`, scopeKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatementRecord{}
	for rows.Next() {
		item := StatementRecord{ScopeKey: scopeKey, Kind: StatementRecordMetadata}
		var effectiveAt, generatedAt string
		var digest []byte
		if err := rows.Scan(&item.ID, &item.RecordKey, &item.AccountKey, &effectiveAt, &item.StatementFileKey, &digest, &generatedAt, &item.RawJSON); err != nil {
			return nil, err
		}
		if len(digest) != sha256.Size {
			return nil, errorsf("stored statement-metadata file digest is invalid")
		}
		copy(item.StatementFileSHA256[:], digest)
		item.EffectiveAt, err = parseTime(effectiveAt)
		if err != nil {
			return nil, err
		}
		item.GeneratedAt, err = parseTime(generatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadStatementFiles returns the current complete statement inventory for one
// scope in file-key order.
func (s *Store) LoadStatementFiles(ctx context.Context, scopeKey string) ([]StatementFileRecord, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT file_key,size_bytes,sha256,status,statement_generated_at,ingested_at,updated_at FROM statement_files WHERE scope_key=? ORDER BY file_key`, scopeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementFileRecord
	for rows.Next() {
		var item StatementFileRecord
		var digest []byte
		var generated, ingested sql.NullString
		var updated string
		item.ScopeKey = scopeKey
		if err := rows.Scan(&item.FileKey, &item.SizeBytes, &digest, &item.Status, &generated, &ingested, &updated); err != nil {
			return nil, err
		}
		if len(digest) != len(item.SHA256) {
			return nil, errorsf("stored statement digest is invalid")
		}
		copy(item.SHA256[:], digest)
		if generated.Valid {
			v, err := parseTime(generated.String)
			if err != nil {
				return nil, err
			}
			item.StatementGeneratedAt = &v
		}
		if ingested.Valid {
			v, err := parseTime(ingested.String)
			if err != nil {
				return nil, err
			}
			item.IngestedAt = &v
		}
		item.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadStatementEquityDays returns current statement-derived winners in
// ascending day and row-ID order. Day bounds are inclusive; a zero limit
// defaults to 1,000 rows.
func (s *Store) LoadStatementEquityDays(ctx context.Context, scopeKey, fromDay, toDay string, limit int) ([]StatementEquityDayRecord, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return nil, err
	}
	if limit < 0 || limit > 10000 {
		return nil, errorsf("statement day limit is invalid")
	}
	if limit == 0 {
		limit = 1000
	}
	return loadStatementEquityDaysQuery(ctx, s.db, scopeKey, fromDay, toDay, limit)
}

func loadStatementEquityDaysQuery(ctx context.Context, q statementRowsQueryer, scopeKey, fromDay, toDay string, limit int) ([]StatementEquityDayRecord, error) {
	clauses := []string{"scope_key=?"}
	args := []any{scopeKey}
	if fromDay != "" {
		clauses = append(clauses, "day>=?")
		args = append(args, fromDay)
	}
	if toDay != "" {
		clauses = append(clauses, "day<=?")
		args = append(args, toDay)
	}
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `SELECT equity_day_id,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json FROM statement_equity_days WHERE `+strings.Join(clauses, " AND ")+` ORDER BY day,equity_day_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementEquityDayRecord
	for rows.Next() {
		var item StatementEquityDayRecord
		var fileDigest []byte
		var generated string
		item.ScopeKey = scopeKey
		if err := rows.Scan(&item.ID, &item.AccountKey, &item.Day, &item.EquityBaseText, &item.StatementFileKey, &fileDigest, &generated, &item.RawJSON); err != nil {
			return nil, err
		}
		if len(fileDigest) != sha256.Size {
			return nil, errorsf("stored statement-day file digest is invalid")
		}
		copy(item.StatementFileSHA256[:], fileDigest)
		item.GeneratedAt, err = parseTime(generated)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadStatementProjectionSnapshot reads records, equity days, and their
// authority head through one SQLite read transaction. This prevents a caller
// from calculating from one projection revision and fingerprinting another.
func (s *Store) LoadStatementProjectionSnapshot(ctx context.Context, scopeKey string, recordLimit, dayLimit int) (StatementProjectionSnapshot, error) {
	var out StatementProjectionSnapshot
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return out, err
	}
	if recordLimit <= 0 || recordLimit > 250000 {
		return out, errorsf("statement record limit is invalid")
	}
	if dayLimit <= 0 || dayLimit > 10000 {
		return out, errorsf("statement day limit is invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	out.Head, err = readAuthorityHead(ctx, tx)
	if err != nil {
		return out, err
	}
	out.Records, err = loadStatementRecordsIncludingMetadataQuery(ctx, tx, scopeKey, nil, recordLimit)
	if err != nil {
		return out, err
	}
	out.EquityDays, err = loadStatementEquityDaysQuery(ctx, tx, scopeKey, "", "", dayLimit)
	if err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func partitionStatementMetadata(records []StatementRecord) (base, metadata []StatementRecord) {
	for _, record := range records {
		if record.Kind == StatementRecordMetadata {
			metadata = append(metadata, record)
		} else {
			base = append(base, record)
		}
	}
	return base, metadata
}

func validateStatementFile(v StatementFileRecord) error {
	if err := validateKey("statement file key", v.FileKey, 512); err != nil {
		return err
	}
	if v.SizeBytes < 0 {
		return errorsf("statement file size must not be negative")
	}
	if v.SHA256 == ([32]byte{}) {
		return errorsf("statement file digest is required")
	}
	return validateKey("statement file status", v.Status, 64)
}
func validateStatementDay(v StatementEquityDayRecord) error {
	for _, item := range []struct {
		label, value string
		limit        int
	}{{"statement account key", v.AccountKey, 256}, {"statement day", v.Day, 32}, {"equity value", v.EquityBaseText, 128}, {"statement file key", v.StatementFileKey, 512}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return err
		}
	}
	if v.GeneratedAt.IsZero() {
		return errorsf("statement generation time is required")
	}
	if !json.Valid(v.RawJSON) {
		return errorsf("statement equity payload must be valid JSON")
	}
	return nil
}

func validateStatementRecord(v StatementRecord) error {
	if !validStatementRecordKind(v.Kind) {
		return errorsf("statement record kind is invalid")
	}
	for _, item := range []struct {
		label, value string
		limit        int
	}{{"statement record key", v.RecordKey, 512}, {"statement record account key", v.AccountKey, 256}, {"statement record file key", v.StatementFileKey, 512}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return err
		}
	}
	if v.EffectiveAt.IsZero() || v.GeneratedAt.IsZero() {
		return errorsf("statement record times are required")
	}
	if !json.Valid(v.RawJSON) {
		return errorsf("statement record payload must be valid JSON")
	}
	return nil
}

func validStatementRecordKind(kind string) bool {
	switch kind {
	case StatementRecordTrade, StatementRecordInstrument, StatementRecordPosition, StatementRecordOptionEvent,
		StatementRecordCorporateAction, StatementRecordTransfer, StatementRecordCash, StatementRecordFXRate,
		StatementRecordMetadata:
		return true
	default:
		return false
	}
}
func nullableTime(v *time.Time) any {
	if v == nil || v.IsZero() {
		return nil
	}
	return formatTime(*v)
}
