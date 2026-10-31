package corestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// EarningsEvent is one issuer earnings date a provider published, kept after
// the day. The earnings cache forgets a date the moment it elapses, so without
// this record every look after a print read "no date published" as if the
// print had never been known (two held names, 2026-10-29). A withdrawn event is
// a future date the same provider later pulled; it is kept, never deleted.
type EarningsEvent struct {
	Symbol string
	// Date is the US Eastern calendar date, YYYY-MM-DD.
	Date string
	// Source is the provider that published the date.
	Source      string
	TimeOfDay   string
	Estimated   bool
	FirstSeen   time.Time
	LastSeen    time.Time
	WithdrawnAt *time.Time
}

// EarningsEventRecord is one change to the event table: a date a provider
// published at At (first seen on insert, last seen on every confirmation; a
// confirmation reinstates a withdrawn event), or, with Withdrawn, the
// provider pulling that date at At.
type EarningsEventRecord struct {
	Symbol    string
	Date      string
	Source    string
	TimeOfDay string
	Estimated bool
	At        time.Time
	Withdrawn bool
}

const earningsEventSymbolMaxLen = 32

func validateEarningsEventRecord(r EarningsEventRecord) error {
	if r.Symbol == "" || len(r.Symbol) > earningsEventSymbolMaxLen || r.Symbol != strings.ToUpper(strings.TrimSpace(r.Symbol)) {
		return errorsf("invalid earnings event symbol")
	}
	if d, err := time.Parse(time.DateOnly, r.Date); err != nil || d.Format(time.DateOnly) != r.Date {
		return errorsf("invalid earnings event date")
	}
	if err := validateKey("earnings event source", r.Source, 64); err != nil {
		return err
	}
	if r.TimeOfDay != "" && r.TimeOfDay != "amc" && r.TimeOfDay != "bmo" {
		return errorsf("invalid earnings event time of day")
	}
	if r.At.IsZero() {
		return errorsf("earnings event record has no time")
	}
	return nil
}

// RecordEarningsEvents applies the records in one durable mutation. It is
// idempotent: recording the same date twice confirms it, withdrawing a date
// that is already withdrawn or unknown changes nothing.
func (s *Store) RecordEarningsEvents(ctx context.Context, records []EarningsEventRecord) error {
	for _, r := range records {
		if err := validateEarningsEventRecord(r); err != nil {
			return err
		}
	}
	if len(records) == 0 {
		return nil
	}
	return s.criticalMutation(ctx, func(tx *sql.Tx) error {
		for _, r := range records {
			at := formatTime(r.At)
			var err error
			if r.Withdrawn {
				_, err = tx.ExecContext(ctx, `UPDATE earnings_events SET withdrawn_at=?
WHERE symbol=? AND event_date=? AND source=? AND withdrawn_at IS NULL`, at, r.Symbol, r.Date, r.Source)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO earnings_events(symbol, event_date, source, time_of_day, estimated, first_seen, last_seen, withdrawn_at)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
ON CONFLICT(symbol, event_date, source) DO UPDATE SET
  time_of_day=excluded.time_of_day, estimated=excluded.estimated, last_seen=excluded.last_seen, withdrawn_at=NULL`,
					r.Symbol, r.Date, r.Source, r.TimeOfDay, boolInt(r.Estimated), at, at)
			}
			if err != nil {
				return fmt.Errorf("record earnings event: %w", err)
			}
		}
		_, err := advanceHeadTx(ctx, tx, 0, time.Now().UTC())
		return err
	})
}

// ListEarningsEvents returns every recorded event, symbol then date then
// source ascending.
func (s *Store) ListEarningsEvents(ctx context.Context) ([]EarningsEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT symbol, event_date, source, time_of_day, estimated, first_seen, last_seen, withdrawn_at
FROM earnings_events ORDER BY symbol, event_date, source`)
	if err != nil {
		return nil, fmt.Errorf("list earnings events: %w", err)
	}
	defer rows.Close()
	var out []EarningsEvent
	for rows.Next() {
		var ev EarningsEvent
		var estimated int
		var first, last string
		var withdrawn sql.NullString
		if err := rows.Scan(&ev.Symbol, &ev.Date, &ev.Source, &ev.TimeOfDay, &estimated, &first, &last, &withdrawn); err != nil {
			return nil, fmt.Errorf("scan earnings event: %w", err)
		}
		ev.Estimated = estimated != 0
		if ev.FirstSeen, err = parseTime(first); err != nil {
			return nil, fmt.Errorf("decode earnings event first seen: %w", err)
		}
		if ev.LastSeen, err = parseTime(last); err != nil {
			return nil, fmt.Errorf("decode earnings event last seen: %w", err)
		}
		if withdrawn.Valid {
			at, err := parseTime(withdrawn.String)
			if err != nil {
				return nil, fmt.Errorf("decode earnings event withdrawal: %w", err)
			}
			ev.WithdrawnAt = &at
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read earnings events: %w", err)
	}
	return out, nil
}
