package daemon

import (
	"errors"
	"github.com/osauer/canary/v2/internal/rpc"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Seven source dates, each with the latest rate and minimum rate actually
// observed on that date. A dip followed by a recovery resets older high dates;
// repeating the same provider timestamp cannot manufacture persistence.
type lendingBorrowDate struct {
	Date string                      `json:"date"`
	AsOf time.Time                   `json:"as_of"`
	Rows map[string]lendingBorrowDay `json:"rows"`
}
type lendingBorrowDay struct {
	ConID   string  `json:"con_id"`
	Rate    float64 `json:"rate"`
	Minimum float64 `json:"minimum"`
	Valid   bool    `json:"valid"`
	Broken  bool    `json:"broken,omitempty"`
}

func cloneLendingBorrowDates(in []lendingBorrowDate) []lendingBorrowDate {
	out := slices.Clone(in)
	for i := range out {
		out[i].Rows = maps.Clone(out[i].Rows)
	}
	return out
}

func mergeLendingBorrowDates(in []lendingBorrowDate, entry marketEventBorrowFeeEntry) []lendingBorrowDate {
	out := cloneLendingBorrowDates(in)
	if entry.AsOf.IsZero() || entry.AsOf.After(entry.FetchedAt) {
		return out
	}
	date := entry.AsOf.UTC().Format(time.DateOnly)
	cutoff := entry.AsOf.UTC().AddDate(0, 0, -6).Format(time.DateOnly)
	out = slices.DeleteFunc(out, func(d lendingBorrowDate) bool { return d.Date < cutoff })
	idx := slices.IndexFunc(out, func(d lendingBorrowDate) bool { return d.Date == date })
	if idx < 0 {
		out = append(out, lendingBorrowDate{Date: date, Rows: map[string]lendingBorrowDay{}})
		idx = len(out) - 1
	}
	d := &out[idx]
	if !entry.AsOf.After(d.AsOf) {
		return out
	}
	for symbol, rec := range entry.Symbols {
		if rec.Currency != "USD" || symbol != rec.Symbol {
			continue
		}
		valid := !rec.FeeRateUnpublished && rec.FeeRate >= 0 && !math.IsNaN(rec.FeeRate) && !math.IsInf(rec.FeeRate, 0)
		next := lendingBorrowDay{ConID: rec.ConID, Valid: valid, Broken: !valid}
		if valid {
			next.Rate = rec.FeeRate
			next.Minimum = rec.FeeRate
		}
		if old, ok := d.Rows[symbol]; ok && old.ConID == next.ConID {
			// An unknown rate breaks continuity conservatively, like a below-threshold
			// observation. Zero is an internal minimum only, never a displayed fee.
			next.Minimum = min(next.Minimum, old.Minimum)
			next.Broken = next.Broken || old.Broken
		}
		d.Rows[symbol] = next
	}
	d.AsOf = entry.AsOf
	slices.SortFunc(out, func(a, b lendingBorrowDate) int { return strings.Compare(a.Date, b.Date) })
	if len(out) > 7 {
		out = out[len(out)-7:]
	}
	return out
}

func lendingHighDates(dates []lendingBorrowDate, symbol, conID string, minRate float64, now time.Time) int {
	if id, _ := strconv.Atoi(conID); id <= 0 {
		return 0
	}
	cutoff := now.UTC().AddDate(0, 0, -6).Format(time.DateOnly)
	count := 0
	for _, d := range slices.Backward(dates) {
		if d.Date < cutoff || d.AsOf.After(now) {
			continue
		}
		r, ok := d.Rows[symbol]
		if !ok {
			continue
		}
		if r.ConID != conID || !r.Valid || r.Rate < minRate {
			break
		}
		count++
		if r.Minimum < minRate || r.Broken {
			break
		}
	}
	return count
}

func validateLendingBorrowDates(dates []lendingBorrowDate) error {
	bad := func() error { return errors.New("invalid borrowing source-date history") }
	if len(dates) > 7 {
		return bad()
	}
	for i, d := range dates {
		if d.AsOf.IsZero() || d.AsOf.UTC().Format(time.DateOnly) != d.Date || (i > 0 && dates[i-1].Date >= d.Date) || len(d.Rows) > 100000 {
			return bad()
		}
		for symbol, r := range d.Rows {
			if symbol == "" || symbol != normSym(symbol) || math.IsNaN(r.Rate) || math.IsInf(r.Rate, 0) || math.IsNaN(r.Minimum) || math.IsInf(r.Minimum, 0) || r.Rate < 0 || r.Minimum < 0 || r.Minimum > r.Rate {
				return bad()
			}
		}
	}
	return nil
}

func lendingFeeHistory(dates []lendingBorrowDate, symbol, conID string, now time.Time) []rpc.LendingFeeSample {
	out := []rpc.LendingFeeSample{}
	if id, _ := strconv.Atoi(conID); id <= 0 {
		return out
	}
	cutoff := now.UTC().AddDate(0, 0, -6).Format(time.DateOnly)
	for _, d := range slices.Backward(dates) {
		if d.Date < cutoff || d.AsOf.After(now) {
			continue
		}
		r, ok := d.Rows[symbol]
		if !ok {
			continue
		}
		if r.ConID != conID {
			break
		}
		if r.Valid {
			out = append(out, rpc.LendingFeeSample{AsOf: d.AsOf, FeeRate: r.Rate})
		}
	}
	slices.Reverse(out)
	return out
}
