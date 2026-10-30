package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

// ReplaceStatementProjection atomically replaces the complete current
// inventory/winner projection while retaining every distinct file-content and
// derived-day version as append-only evidence. A same-name restatement is a
// new version when its SHA-256 changes, regardless of file size.
//
// A row version is stored only the first time its content is seen, from the
// earliest report that showed it; a restatement with different content is
// always stored. The daily Flex report covers the year to date, and copying
// every unchanged row once per file made the version tables the largest
// unbounded grower in daemon.db. Which later files repeated a row stays in
// the retained XML. Owner instruction 2026-10-30 10:24 CET: "ok go for B1".
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
	// Offer the copies oldest report first, so the first-seen insert below
	// keeps the earliest report's copy whatever order the caller used.
	byReport := func(a, b StatementRecord) int {
		if c := a.GeneratedAt.Compare(b.GeneratedAt); c != 0 {
			return c
		}
		return strings.Compare(a.StatementFileKey, b.StatementFileKey)
	}
	versionRecords, versionMetadata := partitionStatementMetadata(slices.SortedStableFunc(slices.Values(recordVersions), byReport))
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
SELECT ?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM statement_equity_day_versions
 WHERE scope_key=? AND account_key=? AND day=? AND equity_base_text=? AND raw_sha256=?)`,
				scopeKey, day.AccountKey, day.Day, day.EquityBaseText, day.StatementFileKey, file.SHA256[:], formatTime(day.GeneratedAt), day.RawJSON, rawDigest[:], stamp,
				scopeKey, day.AccountKey, day.Day, day.EquityBaseText, rawDigest[:]); err != nil {
				return fmt.Errorf("append statement equity-day version: %w", err)
			}
		}
		for _, record := range versionRecords {
			file := fileByKey[record.StatementFileKey]
			rawDigest := sha256.Sum256(record.RawJSON)
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_record_versions
(scope_key,record_kind,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM statement_record_versions
 WHERE scope_key=? AND record_kind=? AND record_key=? AND account_key=? AND effective_at=? AND raw_sha256=?)`,
				scopeKey, record.Kind, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON, rawDigest[:], stamp,
				scopeKey, record.Kind, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), rawDigest[:]); err != nil {
				return fmt.Errorf("append statement record version: %w", err)
			}
		}
		for _, record := range versionMetadata {
			file := fileByKey[record.StatementFileKey]
			rawDigest := sha256.Sum256(record.RawJSON)
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_metadata_versions
(scope_key,record_key,account_key,effective_at,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
SELECT ?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM statement_metadata_versions
 WHERE scope_key=? AND record_key=? AND account_key=? AND effective_at=? AND raw_sha256=?)`,
				scopeKey, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON, rawDigest[:], stamp,
				scopeKey, record.RecordKey, record.AccountKey, formatTime(record.EffectiveAt), rawDigest[:]); err != nil {
				return fmt.Errorf("append statement metadata version: %w", err)
			}
		}
		// These are current projections only. Reconciling them never touches
		// the immutable version tables above.
		fileRows := make([]currentRow, 0, len(files))
		for _, file := range files {
			fileRows = append(fileRows, currentRow{key: []any{file.FileKey},
				values: []any{file.SizeBytes, file.SHA256[:], file.Status, nullableTime(file.StatementGeneratedAt), nullableTime(file.IngestedAt)}})
		}
		dayRows := make([]currentRow, 0, len(days))
		for _, day := range days {
			file := fileByKey[day.StatementFileKey]
			dayRows = append(dayRows, currentRow{key: []any{day.AccountKey, day.Day},
				values: []any{day.EquityBaseText, day.StatementFileKey, file.SHA256[:], formatTime(day.GeneratedAt), day.RawJSON}})
		}
		recordRows := make([]currentRow, 0, len(currentRecords))
		for _, record := range currentRecords {
			file := fileByKey[record.StatementFileKey]
			recordRows = append(recordRows, currentRow{key: []any{record.Kind, record.RecordKey},
				values: []any{record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON}})
		}
		metadataRows := make([]currentRow, 0, len(currentMetadata))
		for _, record := range currentMetadata {
			file := fileByKey[record.StatementFileKey]
			metadataRows = append(metadataRows, currentRow{key: []any{record.RecordKey},
				values: []any{record.AccountKey, formatTime(record.EffectiveAt), record.StatementFileKey, file.SHA256[:], formatTime(record.GeneratedAt), record.RawJSON}})
		}
		children := []struct {
			table currentTable
			rows  []currentRow
		}{
			{statementEquityDaysTable, dayRows},
			{statementRecordsTable, recordRows},
			{statementMetadataTable, metadataRows},
		}
		// Foreign keys hold after every statement: a row whose file version
		// leaves the inventory is deleted first and written back once the new
		// version exists. A row moving to another retained file is updated.
		inventory := make(map[string]bool, len(files))
		for _, file := range files {
			inventory[encodeCurrentValues([]any{file.FileKey, file.SHA256[:]})] = true
		}
		for _, child := range children {
			if err := retractCurrentTx(ctx, tx, scopeKey, child.table, inventory); err != nil {
				return err
			}
		}
		if err := reconcileCurrentTx(ctx, tx, scopeKey, statementFilesTable, fileRows, stamp); err != nil {
			return err
		}
		for _, child := range children {
			if err := reconcileCurrentTx(ctx, tx, scopeKey, child.table, child.rows, stamp); err != nil {
				return err
			}
		}
		_, err := advanceHeadTx(ctx, tx, 0, now)
		return err
	})
}

// currentTable names one current-winner projection: the columns that identify
// a row within its scope, the content columns a refresh compares, and the
// subset of those that references a statement_files version.
type currentTable struct {
	name, label  string
	keys, values []string
	refs         []string
}

var (
	statementFilesTable = currentTable{name: "statement_files", label: "statement inventory",
		keys: []string{"file_key"}, values: []string{"size_bytes", "sha256", "status", "statement_generated_at", "ingested_at"}}
	statementEquityDaysTable = currentTable{name: "statement_equity_days", label: "statement equity-day winner",
		keys: []string{"account_key", "day"}, values: []string{"equity_base_text", "statement_file_key", "statement_file_sha256", "generated_at", "raw_json"},
		refs: []string{"statement_file_key", "statement_file_sha256"}}
	statementRecordsTable = currentTable{name: "statement_records", label: "statement record",
		keys: []string{"record_kind", "record_key"}, values: []string{"account_key", "effective_at", "statement_file_key", "statement_file_sha256", "generated_at", "raw_json"},
		refs: []string{"statement_file_key", "statement_file_sha256"}}
	statementMetadataTable = currentTable{name: "statement_metadata", label: "statement metadata",
		keys: []string{"record_key"}, values: []string{"account_key", "effective_at", "statement_file_key", "statement_file_sha256", "generated_at", "raw_json"},
		refs: []string{"statement_file_key", "statement_file_sha256"}}
)

// currentRow holds one desired row as SQL arguments, in currentTable column order.
type currentRow struct{ key, values []any }

// storedRow is one existing current row, encoded for comparison.
type storedRow struct {
	key          []any
	values, refs string
}

func (t currentTable) where() string {
	return "scope_key=? AND " + strings.Join(t.keys, "=? AND ") + "=?"
}

// refsOf encodes the referencing columns of values, which are in t.values order.
func (t currentTable) refsOf(values []any) string {
	picked := make([]any, 0, len(t.refs))
	for _, ref := range t.refs {
		picked = append(picked, values[slices.Index(t.values, ref)])
	}
	return encodeCurrentValues(picked)
}

func loadCurrentTx(ctx context.Context, tx *sql.Tx, scopeKey string, table currentTable) (map[string]storedRow, error) {
	columns := append(slices.Clone(table.keys), table.values...)
	cursor, err := tx.QueryContext(ctx, `SELECT `+strings.Join(columns, ",")+` FROM `+table.name+` WHERE scope_key=?`, scopeKey)
	if err != nil {
		return nil, fmt.Errorf("read current %s: %w", table.label, err)
	}
	defer cursor.Close()
	stored := map[string]storedRow{}
	for cursor.Next() {
		row := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range row {
			dest[i] = &row[i]
		}
		if err := cursor.Scan(dest...); err != nil {
			return nil, fmt.Errorf("read current %s: %w", table.label, err)
		}
		values := row[len(table.keys):]
		stored[encodeCurrentValues(row[:len(table.keys)])] = storedRow{key: row[:len(table.keys)], values: encodeCurrentValues(values), refs: table.refsOf(values)}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("read current %s: %w", table.label, err)
	}
	return stored, nil
}

func deleteCurrentTx(ctx context.Context, tx *sql.Tx, scopeKey string, table currentTable, stored map[string]storedRow, keys []string) error {
	slices.Sort(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table.name+` WHERE `+table.where(), append([]any{scopeKey}, stored[key].key...)...); err != nil {
			return fmt.Errorf("retract current %s: %w", table.label, err)
		}
	}
	return nil
}

// retractCurrentTx deletes the rows of table that reference a file version
// outside inventory, so that version can be replaced or removed;
// reconcileCurrentTx writes back any of them that remain current.
func retractCurrentTx(ctx context.Context, tx *sql.Tx, scopeKey string, table currentTable, inventory map[string]bool) error {
	stored, err := loadCurrentTx(ctx, tx, scopeKey, table)
	if err != nil {
		return err
	}
	var gone []string
	for key, row := range stored {
		if !inventory[row.refs] {
			gone = append(gone, key)
		}
	}
	return deleteCurrentTx(ctx, tx, scopeKey, table, stored, gone)
}

// reconcileCurrentTx makes table's rows for scopeKey equal rows. It inserts
// new keys, updates changed rows in place and deletes absent keys; unchanged
// rows are not written. Rewriting the whole projection on every Flex file
// held one transaction of the table's full size in the WAL.
func reconcileCurrentTx(ctx context.Context, tx *sql.Tx, scopeKey string, table currentTable, rows []currentRow, stamp string) error {
	stored, err := loadCurrentTx(ctx, tx, scopeKey, table)
	if err != nil {
		return err
	}
	columns := append(slices.Clone(table.keys), table.values...)
	update := `UPDATE ` + table.name + ` SET ` + strings.Join(table.values, "=?,") + `=?,updated_at=? WHERE ` + table.where()
	insert := `INSERT INTO ` + table.name + ` (scope_key,` + strings.Join(columns, ",") + `,updated_at) VALUES(?` + strings.Repeat(",?", len(columns)+1) + `)`
	for _, row := range rows {
		key := encodeCurrentValues(row.key)
		old, found := stored[key]
		delete(stored, key)
		switch {
		case !found:
			args := append(append(append([]any{scopeKey}, row.key...), row.values...), stamp)
			if _, err := tx.ExecContext(ctx, insert, args...); err != nil {
				return fmt.Errorf("write current %s: %w", table.label, err)
			}
		case old.values != encodeCurrentValues(row.values):
			args := append(append(append(slices.Clone(row.values), stamp), scopeKey), row.key...)
			if _, err := tx.ExecContext(ctx, update, args...); err != nil {
				return fmt.Errorf("write current %s: %w", table.label, err)
			}
		}
	}
	return deleteCurrentTx(ctx, tx, scopeKey, table, stored, slices.Collect(maps.Keys(stored)))
}

// encodeCurrentValues gives one comparable string for SQL values, keeping the
// type so a TEXT and a BLOB with equal bytes never compare equal.
func encodeCurrentValues(values []any) string {
	var b strings.Builder
	for _, v := range values {
		switch v := v.(type) {
		case nil:
			b.WriteString("n;")
		case []byte:
			fmt.Fprintf(&b, "b%d:%s;", len(v), v)
		case string:
			fmt.Fprintf(&b, "s%d:%s;", len(v), v)
		case int64:
			fmt.Fprintf(&b, "i%d;", v)
		default:
			fmt.Fprintf(&b, "%T:%v;", v, v)
		}
	}
	return b.String()
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
	rows, err := q.QueryContext(ctx, `SELECT equity_day_id,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json FROM statement_equity_days WHERE `+strings.Join(clauses, " AND ")+` ORDER BY day,account_key,equity_day_id LIMIT ?`, args...)
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
