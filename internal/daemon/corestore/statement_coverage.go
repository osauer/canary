package corestore

import (
	"context"
	"database/sql"
	"time"
)

// StatementCoverage reads the committed reporting summary without loading XML
// or large metadata payloads. An empty projection remains unavailable.
func (s *Store) StatementCoverage(ctx context.Context, scope string) (coverage, generated time.Time, valid bool, err error) {
	var end, stamp sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT MAX(effective_at), MAX(generated_at)
 FROM statement_metadata WHERE scope_key=?`, scope).Scan(&end, &stamp)
	if err != nil || !end.Valid || !stamp.Valid {
		return
	}
	coverage, err = parseTime(end.String)
	if err != nil {
		return
	}
	generated, err = parseTime(stamp.String)
	valid = err == nil && !coverage.IsZero() && !generated.IsZero()
	return
}
