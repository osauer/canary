package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// History RPC surfaces (regime.history, rules.history, stress.history,
// recon.equity) render journal evidence from daemon.db and must never feed
// submit eligibility, freeze, or any broker-write path.

const (
	historyIndexDefaultLookback = 7 * 24 * time.Hour
	historyIndexDefaultLimit    = 50
	historyIndexMaxLimit        = 500

	// recon.equity deviates deliberately: the series is daily-granular, so
	// the window and caps are wider (D6).
	reconEquityDefaultLookback = 90 * 24 * time.Hour
	reconEquityDefaultLimit    = 200
	reconEquityMaxLimit        = 1000
	// reconEquityEventsCap hard-caps interleaved capital events (newest
	// first, disclosed via events_truncated).
	reconEquityEventsCap = 500
)

// errHistoryIndexUnavailable is the classified operator-facing failure for
// a missing or broken authority. Deliberately a plain error (maps to
// internal): the remediation is always the same because the history read
// model is derived state.
var errHistoryIndexUnavailable = errors.New("authoritative history storage unavailable (daemon.db; inspect daemon storage health and logs)")

func (s *Server) handleRegimeHistory(req *rpc.Request) (*rpc.RegimeHistoryResult, error) {
	var p rpc.RegimeHistoryParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	since, until, err := historyIndexRange(p.Since, p.Until, now)
	if err != nil {
		return nil, err
	}
	limit, err := historyIndexLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	if s.coreStore == nil {
		return nil, errHistoryIndexUnavailable
	}
	entries, total, err := s.sqliteRegimeHistory(context.Background(), since, until, strings.TrimSpace(p.Stage), limit)
	if err != nil {
		s.logger.Warnf("daemon authority: regime history query failed: %v", err)
		return nil, errHistoryIndexUnavailable
	}
	if entries == nil {
		entries = []rpc.RegimeHistoryEntry{} // JSON [] like orders.history, never null
	}
	return &rpc.RegimeHistoryResult{
		AsOf:       now,
		Since:      since,
		Until:      until,
		Entries:    entries,
		Count:      len(entries),
		TotalCount: total,
		Limit:      limit,
		Truncated:  total > len(entries),
	}, nil
}

func (s *Server) handleRulesHistory(req *rpc.Request) (*rpc.RulesHistoryResult, error) {
	var p rpc.RulesHistoryParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	since, until, err := historyIndexRange(p.Since, p.Until, now)
	if err != nil {
		return nil, err
	}
	limit, err := historyIndexLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	if s.coreStore == nil {
		return nil, errHistoryIndexUnavailable
	}
	entries, total, err := s.sqliteRulesHistory(context.Background(), since, until, strings.TrimSpace(p.Rule), limit)
	if err != nil {
		s.logger.Warnf("daemon authority: rules history query failed: %v", err)
		return nil, errHistoryIndexUnavailable
	}
	if entries == nil {
		entries = []rpc.RuleTransitionEntry{} // JSON [] like orders.history, never null
	}
	return &rpc.RulesHistoryResult{
		AsOf:       now,
		Since:      since,
		Until:      until,
		Entries:    entries,
		Count:      len(entries),
		TotalCount: total,
		Limit:      limit,
		Truncated:  total > len(entries),
	}, nil
}

// historyIndexRange resolves the since/until window: default 7-day
// lookback, YYYY-MM-DD as whole UTC days, RFC3339 exact. Mirrors
// orderHistoryRange; the ~12-line grammar is duplicated locally by design
// (D5) instead of refactoring parseOrderHistoryTime.
func historyIndexRange(sinceRaw, untilRaw string, now time.Time) (time.Time, time.Time, error) {
	return historyIndexRangeLookback(sinceRaw, untilRaw, now, historyIndexDefaultLookback)
}

// historyIndexRangeLookback is historyIndexRange with a caller-chosen
// default lookback (recon.equity uses 90 days).
func historyIndexRangeLookback(sinceRaw, untilRaw string, now time.Time, lookback time.Duration) (time.Time, time.Time, error) {
	until := now
	if raw := strings.TrimSpace(untilRaw); raw != "" {
		parsed, dateOnly, err := historyIndexTime(raw)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		until = parsed
		if dateOnly {
			until = until.Add(24 * time.Hour)
		}
	}
	since := until.Add(-lookback)
	if raw := strings.TrimSpace(sinceRaw); raw != "" {
		parsed, _, err := historyIndexTime(raw)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		since = parsed
	}
	if !since.Before(until) {
		return time.Time{}, time.Time{}, errBadRequest("history: since must be before until")
	}
	return since, until, nil
}

// historyIndexTime parses one boundary; the bool reports the YYYY-MM-DD
// (whole UTC day) form.
func historyIndexTime(raw string) (time.Time, bool, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), false, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, time.UTC); err == nil {
		return t.UTC(), true, nil
	}
	return time.Time{}, false, errBadRequest("history: time boundaries must be YYYY-MM-DD or RFC3339")
}

func historyIndexLimit(limit int) (int, error) {
	return historyIndexLimitBounded(limit, historyIndexDefaultLimit, historyIndexMaxLimit)
}

func historyIndexLimitBounded(limit, def, maxLimit int) (int, error) {
	if limit == 0 {
		return def, nil
	}
	if limit < 0 || limit > maxLimit {
		return 0, errBadRequest(fmt.Sprintf("history: limit must be between 1 and %d", maxLimit))
	}
	return limit, nil
}

func (s *Server) handleStressHistory(req *rpc.Request) (*rpc.StressHistoryResult, error) {
	var p rpc.StressHistoryParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	since, until, err := historyIndexRange(p.Since, p.Until, now)
	if err != nil {
		return nil, err
	}
	limit, err := historyIndexLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	if s.coreStore == nil {
		return nil, errHistoryIndexUnavailable
	}
	entries, total, err := s.sqliteStressHistory(context.Background(), since, until, strings.TrimSpace(p.Severity), strings.TrimSpace(p.Action), limit)
	if err != nil {
		s.logger.Warnf("daemon authority: stress history query failed: %v", err)
		return nil, errHistoryIndexUnavailable
	}
	if entries == nil {
		entries = []rpc.StressHistoryEntry{} // JSON [] like orders.history, never null
	}
	return &rpc.StressHistoryResult{
		AsOf:       now,
		Since:      since,
		Until:      until,
		Entries:    entries,
		Count:      len(entries),
		TotalCount: total,
		Limit:      limit,
		Truncated:  total > len(entries),
	}, nil
}

func (s *Server) handleReconEquity(req *rpc.Request) (*rpc.ReconEquityResult, error) {
	var p rpc.ReconEquityParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	since, until, err := historyIndexRangeLookback(p.Since, p.Until, now, reconEquityDefaultLookback)
	if err != nil {
		return nil, err
	}
	limit, err := historyIndexLimitBounded(p.Limit, reconEquityDefaultLimit, reconEquityMaxLimit)
	if err != nil {
		return nil, err
	}
	if s.coreStore == nil {
		return nil, errHistoryIndexUnavailable
	}
	days, total, err := s.sqliteStatementEquityDays(context.Background(), since, until, limit)
	var (
		events          []rpc.CapitalEventEntry
		eventsTruncated bool
		stmtHealth      rpc.HistoryIndexHealth
	)
	if err == nil {
		events, eventsTruncated, err = s.sqliteCapitalEvents(context.Background(), since, until, reconEquityEventsCap)
	}
	if err == nil {
		stmtHealth, err = s.sqliteStatementsHealth(context.Background())
	}
	if err != nil {
		s.logger.Warnf("daemon authority: recon equity query failed: %v", err)
		return nil, errHistoryIndexUnavailable
	}
	if days == nil {
		days = []rpc.EquityDayEntry{} // JSON [] never null
	}
	if events == nil {
		events = []rpc.CapitalEventEntry{}
	}
	return &rpc.ReconEquityResult{
		AsOf:            now,
		Since:           since,
		Until:           until,
		Days:            days,
		Count:           len(days),
		TotalCount:      total,
		Limit:           limit,
		Truncated:       total > len(days),
		Events:          events,
		EventsTruncated: eventsTruncated,
		Statements:      stmtHealth,
	}, nil
}
