package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/history"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// History-index glue: wiring for the derived evidence index
// (internal/daemon/history, internal-docs/design/history-index.md). The history
// RPC surfaces render journal evidence and must never feed submit
// eligibility, freeze, or any broker-write path; the phase-2 indexed
// order reads (order_index_read.go) are read-path substitutions with an
// automatic journal-scan fallback.

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

	// historyMaintenanceEvery is the rotation scheduler cadence; one pass
	// also runs at startup after crash recovery.
	historyMaintenanceEvery = 24 * time.Hour
)

// errHistoryIndexUnavailable is the classified operator-facing failure for
// a nil or broken index. Deliberately a plain error (maps to internal):
// the remediation is always the same because the index is derived state.
var errHistoryIndexUnavailable = errors.New("authoritative history storage unavailable (daemon.db; inspect daemon storage health and logs)")

// installHistoryIndex resolves history.db, journal, statement, and
// archive paths at construction time only. It must not open the DB: New
// runs in every autospawn race loser before the instance flock, and only
// the flock winner (Start) may touch history.db.
func (s *Server) installHistoryIndex() {
	resolve := func(name string) (string, bool) {
		path, err := defaultTradingStatePath(name)
		if err != nil {
			s.logger.Warnf("history index: resolve %s path: %v (index disabled)", name, err)
			return "", false
		}
		return path, true
	}
	dbPath, ok := resolve("history.db")
	if !ok {
		return
	}
	regimePath, err := regimeDecisionsDefaultPath()
	if err != nil {
		s.logger.Warnf("history index: resolve regime journal path: %v (index disabled)", err)
		return
	}
	rulesPath, ok := resolve("rules-decisions.jsonl")
	if !ok {
		return
	}
	canaryPath, err := canaryDecisionsDefaultPath()
	if err != nil {
		s.logger.Warnf("history index: resolve canary journal path: %v (index disabled)", err)
		return
	}
	capitalPath, ok := resolve(capitalEventsJournalFile)
	if !ok {
		return
	}
	riskPolicyPath, ok := resolve(riskPolicyJournalFile)
	if !ok {
		return
	}
	outcomesPath, err := defaultProposalOutcomesPath()
	if err != nil {
		s.logger.Warnf("history index: resolve proposal outcomes path: %v (index disabled)", err)
		return
	}
	orderPath, err := defaultOrderJournalPath()
	if err != nil {
		s.logger.Warnf("history index: resolve order journal path: %v (index disabled)", err)
		return
	}
	statementsDir, err := flexStatementsDirPath()
	if err != nil {
		s.logger.Warnf("history index: resolve statements dir: %v (index disabled)", err)
		return
	}
	rotatedDir, ok := resolve("rotated")
	if !ok {
		return
	}
	s.historyIndexOpts = &history.Options{
		DBPath:                dbPath,
		RegimeJournalPath:     regimePath,
		RulesJournalPath:      rulesPath,
		CanaryJournalPath:     canaryPath,
		CapitalJournalPath:    capitalPath,
		RiskPolicyJournalPath: riskPolicyPath,
		ProposalOutcomesPath:  outcomesPath,
		OrderJournalPath:      orderPath,
		ValidateOrderLine:     validateOrderJournalLine,
		StatementsDir:         statementsDir,
		RotatedDir:            rotatedDir,
		Logf:                  s.logger.Warnf,
		Infof:                 s.logger.Infof,
	}
}

// startHistoryIndex opens the index, resolves any rotation left pending
// by a crash (before writer traffic — RPC serving has not started yet),
// and launches the ingest and rotation-maintenance goroutines on
// serverCtx. Open failure degrades to a warning and a nil store — the
// history RPCs return a classified error while journaling continues
// untouched.
func (s *Server) startHistoryIndex(ctx context.Context) {
	if s.historyIndexOpts == nil {
		return
	}
	store, err := history.Open(*s.historyIndexOpts)
	if err != nil {
		s.logger.Warnf("history index: %v (history RPCs unavailable; journals unaffected)", err)
		return
	}
	store.RecoverRotations(s.historyRotationSources())
	s.historyIndex.Store(store)
	go store.Run(ctx)
	go s.runHistoryMaintenanceLoop(ctx)
}

// historyRotationSources binds the three rotatable decision journals to
// their daemon-side writer locks. A journal whose writer failed to
// install is omitted (its rotation is skipped rather than run unlocked).
func (s *Server) historyRotationSources() []history.RotationSource {
	var sources []history.RotationSource
	if s.regimeDecisions != nil {
		sources = append(sources, history.RotationSource{Name: "regime", Locker: &s.regimeDecisions.mu})
	}
	sources = append(sources, history.RotationSource{Name: "rules", Locker: &s.rulesJournalMu})
	if s.canaryDecisions != nil {
		sources = append(sources, history.RotationSource{Name: "stress", Locker: &s.canaryDecisions.mu})
	}
	return sources
}

// runHistoryMaintenanceLoop schedules journal rotation: one pass at start
// (crash recovery already ran) and then daily. Each pass re-reads the
// runtime settings; per-source failures are warned inside RotateAll and
// never propagate — journaling is never blocked.
func (s *Server) runHistoryMaintenanceLoop(ctx context.Context) {
	s.historyMaintenancePass(ctx)
	t := time.NewTicker(historyMaintenanceEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.historyMaintenancePass(ctx)
		}
	}
}

// historyMaintenancePass runs one rotation pass, honoring the runtime
// enable switch and keep-window setting. Returns whether rotation ran
// (false = disabled or no index), for tests.
func (s *Server) historyMaintenancePass(ctx context.Context) bool {
	store := s.historyIndex.Load()
	if store == nil {
		return false
	}
	enabled, keepMonths := s.historyRotationSettings()
	if !enabled {
		return false
	}
	store.RotateAll(ctx, s.historyRotationSources(), keepMonths, time.Now())
	return true
}

// kickHistoryIndex nudges the ingest goroutine after a journal append.
// Nil-safe and non-blocking: the kick carries no data (the journal file
// is the only ingest input), so evidence always lands before the index
// reads it.
func (s *Server) kickHistoryIndex() {
	if s == nil {
		return
	}
	if store := s.historyIndex.Load(); store != nil {
		store.Kick()
	}
}

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
	var (
		entries []rpc.RegimeHistoryEntry
		total   int
		health  rpc.HistoryIndexHealth
	)
	if s.coreStore != nil {
		entries, total, err = s.sqliteRegimeHistory(context.Background(), since, until, strings.TrimSpace(p.Stage), limit)
		if err != nil {
			s.logger.Warnf("daemon authority: regime history query failed: %v", err)
			return nil, errHistoryIndexUnavailable
		}
	} else {
		// Legacy importer/test-oracle compatibility only. Production Start
		// requires daemon.db before publishing a socket and never installs
		// history.db.
		store := s.historyIndex.Load()
		if store == nil {
			return nil, errHistoryIndexUnavailable
		}
		entries, total, err = store.RegimeHistory(history.RegimeQuery{
			Since: since, Until: until, Stage: strings.TrimSpace(p.Stage), Limit: limit,
		})
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
		health, err = store.Health("regime")
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
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
		Index:      health,
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
	var (
		entries []rpc.RuleTransitionEntry
		total   int
		health  rpc.HistoryIndexHealth
	)
	if s.coreStore != nil {
		entries, total, err = s.sqliteRulesHistory(context.Background(), since, until, strings.TrimSpace(p.Rule), limit)
		if err != nil {
			s.logger.Warnf("daemon authority: rules history query failed: %v", err)
			return nil, errHistoryIndexUnavailable
		}
	} else {
		store := s.historyIndex.Load()
		if store == nil {
			return nil, errHistoryIndexUnavailable
		}
		entries, total, err = store.RulesHistory(history.RulesQuery{
			Since: since, Until: until, Rule: strings.TrimSpace(p.Rule), Limit: limit,
		})
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
		health, err = store.Health("rules")
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
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
		Index:      health,
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
	var (
		entries []rpc.StressHistoryEntry
		total   int
		health  rpc.HistoryIndexHealth
	)
	if s.coreStore != nil {
		entries, total, err = s.sqliteStressHistory(context.Background(), since, until, strings.TrimSpace(p.Severity), strings.TrimSpace(p.Action), limit)
		if err != nil {
			s.logger.Warnf("daemon authority: stress history query failed: %v", err)
			return nil, errHistoryIndexUnavailable
		}
	} else {
		store := s.historyIndex.Load()
		if store == nil {
			return nil, errHistoryIndexUnavailable
		}
		entries, total, err = store.StressHistory(history.StressQuery{
			Since: since, Until: until, Severity: strings.TrimSpace(p.Severity), Action: strings.TrimSpace(p.Action), Limit: limit,
		})
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
		health, err = store.Health("stress")
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
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
		Index:      health,
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
	var (
		days            []rpc.EquityDayEntry
		events          []rpc.CapitalEventEntry
		total           int
		eventsTruncated bool
		health          rpc.HistoryIndexHealth
		stmtHealth      rpc.HistoryIndexHealth
	)
	if s.coreStore != nil {
		days, total, err = s.sqliteStatementEquityDays(context.Background(), since, until, limit)
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
	} else {
		// Legacy importer/test-oracle compatibility only; production never
		// opens history.db after the SQLite authority cutover.
		store := s.historyIndex.Load()
		if store == nil {
			return nil, errHistoryIndexUnavailable
		}
		days, total, err = store.EquityDays(history.EquityQuery{Since: since, Until: until, Limit: limit})
		if err == nil {
			events, eventsTruncated, err = store.CapitalEvents(since, until, reconEquityEventsCap)
		}
		if err == nil {
			health, err = store.Health("capital")
		}
		if err == nil {
			stmtHealth, err = store.StatementsHealth()
		}
		if err != nil {
			return nil, errHistoryIndexUnavailable
		}
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
		Index:           health,
		Statements:      stmtHealth,
	}, nil
}
