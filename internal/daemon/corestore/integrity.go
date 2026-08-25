package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
)

// CheckIntegrity verifies SQLite structure, foreign keys, and application-level
// content hashes without modifying the store.
func (s *Store) CheckIntegrity(ctx context.Context) (IntegrityReport, error) {
	return checkIntegrityDB(ctx, s.db)
}

func checkIntegrityDB(ctx context.Context, db *sql.DB) (IntegrityReport, error) {
	var report IntegrityReport
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return report, fmt.Errorf("quick_check: %w", err)
	}
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			rows.Close()
			return report, fmt.Errorf("quick_check result: %w", err)
		}
		report.QuickCheckResults = append(report.QuickCheckResults, result)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, fmt.Errorf("quick_check results: %w", err)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return report, fmt.Errorf("foreign_key_check: %w", err)
	}
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKey int64
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			rows.Close()
			return report, fmt.Errorf("foreign_key_check result: %w", err)
		}
		violation := ForeignKeyViolation{Table: table, ParentTable: parent, ForeignKey: foreignKey}
		if rowID.Valid {
			value := rowID.Int64
			violation.RowID = &value
		}
		report.ForeignKeyViolations = append(report.ForeignKeyViolations, violation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, fmt.Errorf("foreign_key_check results: %w", err)
	}
	rows.Close()
	if err := checkApplicationHashes(ctx, db); err != nil {
		return report, err
	}
	return report, nil
}

type applicationHashCheck struct {
	table         string
	payloadColumn string
	digestColumn  string
}

func checkApplicationHashes(ctx context.Context, db *sql.DB) error {
	checks := []applicationHashCheck{
		{table: "state_documents", payloadColumn: "document_json", digestColumn: "document_sha256"},
		{table: "event_log", payloadColumn: "payload_json", digestColumn: "payload_sha256"},
		{table: "observations", payloadColumn: "payload", digestColumn: "payload_sha256"},
		{table: "statement_equity_day_versions", payloadColumn: "raw_json", digestColumn: "raw_sha256"},
		{table: "statement_record_versions", payloadColumn: "raw_json", digestColumn: "raw_sha256"},
		{table: "statement_metadata_versions", payloadColumn: "raw_json", digestColumn: "raw_sha256"},
	}
	for _, check := range checks {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, check.table).Scan(&exists); err != nil {
			return fmt.Errorf("inspect application hash table %s: %w", check.table, err)
		}
		// A newly created, unmigrated authority file has no application tables.
		// The schema manifest independently rejects missing objects after migrate.
		if exists == 0 {
			continue
		}
		query := fmt.Sprintf(`SELECT %s,%s FROM %s`, check.payloadColumn, check.digestColumn, check.table)
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("scan application hashes for %s: %w", check.table, err)
		}
		mismatches := 0
		for rows.Next() {
			var payload, stored []byte
			if err := rows.Scan(&payload, &stored); err != nil {
				rows.Close()
				return fmt.Errorf("scan application hash row for %s: %w", check.table, err)
			}
			digest := sha256.Sum256(payload)
			if len(stored) != sha256.Size || !bytes.Equal(stored, digest[:]) {
				mismatches++
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("scan application hashes for %s: %w", check.table, err)
		}
		rows.Close()
		if mismatches != 0 {
			return fmt.Errorf("application content hash mismatch: table=%s rows=%d", check.table, mismatches)
		}
	}
	return nil
}

func integrityFailure(report IntegrityReport) error {
	return fmt.Errorf("authority database integrity failed: quick_check_rows=%d foreign_key_violations=%d", len(report.QuickCheckResults), len(report.ForeignKeyViolations))
}
