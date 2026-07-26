package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

type retainedStatementFile struct {
	name       string
	size       int64
	sha256     string
	data       []byte
	statements []flexstmt.Statement
	equityDays int
}

type recordedStatementFile struct {
	size   int64
	sha256 string
}

type statementEquityWinner struct {
	accountID     string
	day           string
	equityBase    float64
	sourceStmt    string
	whenGenerated time.Time
}

// ingestStatements reconciles the derived statement tables against the
// complete retained *.xml file set. Content hashes, rather than file size,
// detect in-place corrections. Whenever the set changes, every current file
// is parsed before one transaction replaces both bookkeeping and equity rows;
// a read or parse failure therefore preserves the last complete snapshot and
// retries on the next pass. Statements themselves are never modified.
// Callers hold ingestMu.
func (s *Store) ingestStatements(ctx context.Context) error {
	recorded, err := s.recordedStatementFiles()
	if err != nil {
		return err
	}
	files, err := s.readRetainedStatementFiles(ctx)
	if err != nil {
		return err
	}
	if statementFileSetMatches(recorded, files) {
		return nil
	}
	if err := parseRetainedStatementFiles(files); err != nil {
		return err
	}
	return s.replaceStatementDerivation(ctx, files)
}

func (s *Store) recordedStatementFiles() (map[string]recordedStatementFile, error) {
	rows, err := s.db.Query(`SELECT name, size, sha256 FROM statement_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recorded := make(map[string]recordedStatementFile)
	for rows.Next() {
		var name string
		var file recordedStatementFile
		if err := rows.Scan(&name, &file.size, &file.sha256); err != nil {
			return nil, err
		}
		recorded[name] = file
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return recorded, nil
}

// readRetainedStatementFiles fingerprints files in descending filename order,
// matching the recon engine's deterministic newest-file-first tie break when
// two statements carry the same generation timestamp. Parsing is deferred
// until a changed fingerprint is found, avoiding repeated XML work on kicks
// caused by unrelated journal appends.
func (s *Store) readRetainedStatementFiles(ctx context.Context) ([]retainedStatementFile, error) {
	entries, err := os.ReadDir(s.opts.StatementsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".xml") {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	files := make([]retainedStatementFile, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(s.opts.StatementsDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		digest := sha256.Sum256(data)
		files = append(files, retainedStatementFile{
			name:   name,
			size:   int64(len(data)),
			sha256: hex.EncodeToString(digest[:]),
			data:   data,
		})
	}
	return files, nil
}

func parseRetainedStatementFiles(files []retainedStatementFile) error {
	for i := range files {
		statements, err := flexstmt.Parse(files[i].data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", files[i].name, err)
		}
		files[i].data = nil
		files[i].statements = statements
		for _, statement := range statements {
			files[i].equityDays += len(statement.Equity)
		}
	}
	return nil
}

func statementFileSetMatches(recorded map[string]recordedStatementFile, files []retainedStatementFile) bool {
	if len(recorded) != len(files) {
		return false
	}
	for _, file := range files {
		got, ok := recorded[file.name]
		if !ok || got.size != file.size || got.sha256 != file.sha256 {
			return false
		}
	}
	return true
}

// replaceStatementDerivation rebuilds the per-day winners from the current
// retained set. The newest whenGenerated value wins. Equal generations keep
// the first row in descending filename/XML order, the same deterministic tie
// break used by mergeRetainedStatements.
func (s *Store) replaceStatementDerivation(ctx context.Context, files []retainedStatementFile) error {
	winners := make(map[string]statementEquityWinner)
	for _, file := range files {
		for _, statement := range file.statements {
			when := statement.WhenGenerated.UTC()
			for _, row := range statement.Equity {
				if err := ctx.Err(); err != nil {
					return err
				}
				day := row.ReportDate.UTC().Format("2006-01-02")
				key := statement.AccountID + "\x00" + day
				current, ok := winners[key]
				if ok && !when.After(current.whenGenerated) {
					continue
				}
				winners[key] = statementEquityWinner{
					accountID:     statement.AccountID,
					day:           day,
					equityBase:    row.TotalBase,
					sourceStmt:    file.name,
					whenGenerated: when,
				}
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM statement_equity_days`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM statement_files`); err != nil {
		return err
	}
	ingestedAt := nowUTC()
	for _, file := range files {
		if _, err := tx.Exec(`INSERT INTO statement_files (name, size, sha256, ingested_at, equity_days)
VALUES (?, ?, ?, ?, ?)`, file.name, file.size, file.sha256, ingestedAt, file.equityDays); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		winner := winners[key]
		if _, err := tx.Exec(`INSERT INTO statement_equity_days
  (account_id, day, equity_base, source_stmt, when_generated)
VALUES (?, ?, ?, ?, ?)`, winner.accountID, winner.day, winner.equityBase, winner.sourceStmt,
			winner.whenGenerated.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
