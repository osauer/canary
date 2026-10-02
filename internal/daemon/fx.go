package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

const fxMethod = "closing_native_book_v1"

// fxStatements binds reads to the accepted, active-query inventory and current
// account. Neither uploaded files nor unaccepted disk bytes certify FX values.
func (s *Server) fxStatements(ctx context.Context) ([]flexstmt.Statement, error) {
	if s == nil || s.cfg == nil || s.coreStore == nil || !s.cfg.Flex.Enabled {
		return nil, fmt.Errorf("reporting_authority_unavailable")
	}
	scope := s.currentBrokerStateScope()
	selection := s.flexEvidenceSelection()
	if !brokerScopeConcrete(scope) || !validFlexQueryFingerprint(selection.ActiveQueryFingerprint) {
		return nil, fmt.Errorf("reporting_scope_unavailable")
	}
	projectionScope := statementProjectionScopeForSelection(selection)
	recorded, err := s.coreStore.LoadStatementFiles(ctx, projectionScope)
	if err != nil {
		return nil, fmt.Errorf("accepted_inventory_unavailable")
	}
	files, err := readStatementProjectionFiles(ctx, selection)
	if err != nil || !statementProjectionInventoryMatches(recorded, files) {
		return nil, fmt.Errorf("accepted_inventory_changed")
	}
	out := []flexstmt.Statement{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := flexstmt.Parse(file.data)
		if err != nil {
			return nil, fmt.Errorf("accepted_statement_invalid")
		}
		out = append(out, rows...)
	}
	final, err := s.coreStore.LoadStatementFiles(ctx, projectionScope)
	if err != nil || !statementProjectionInventoryMatches(final, files) || selection != s.flexEvidenceSelection() || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return nil, fmt.Errorf("reporting_authority_changed")
	}
	out, _ = retainedStatementsForScope(out, scope)
	return out, nil
}

func (s *Server) handleFX(ctx context.Context) (*rpc.FXResult, error) {
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	rows, err := s.fxStatements(ctx)
	result := buildFX(rows, now)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		result.State = "unavailable"
		result.Reason = err.Error()
	}
	if s != nil {
		s.fxMu.Lock()
		result.Backfill.Running = s.fxRunning
		result.Backfill.Reason = s.fxReason
		s.fxMu.Unlock()
	}
	return &result, rpc.ValidateFXResult(result)
}

func buildFX(rows []flexstmt.Statement, now time.Time) rpc.FXResult {
	r := rpc.FXResult{SchemaVersion: rpc.FXSchemaVersion, AsOf: now.UTC(), State: "unavailable", Method: fxMethod, Days: []rpc.FXDay{}, Periods: []rpc.FXPeriod{}}
	start := fmt.Sprintf("%04d-01-01", now.Year())
	// Expected dates come from the broker's NAV series, including its opening
	// boundary. No weekends, holidays, missing closes or prices are invented.
	nav := map[string]float64{}
	generation := map[string]time.Time{}
	navConflict := map[string]bool{}
	ordered := append([]flexstmt.Statement(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].WhenGenerated.After(ordered[j].WhenGenerated) })
	snapshots := map[string]*flexstmt.FXSnapshot{}
	canonical := map[string][]byte{}
	snapshotGeneration := map[string]time.Time{}
	for _, st := range ordered {
		for _, row := range st.Equity {
			d := row.ReportDate.Format(performanceDayFormat)
			if existing, ok := nav[d]; ok && generation[d].Equal(st.WhenGenerated) && math.Abs(existing-row.TotalBase) > .02 {
				navConflict[d] = true
			}
			if _, ok := nav[d]; !ok {
				nav[d] = row.TotalBase
				generation[d] = st.WhenGenerated
			}
		}
		f := st.FX
		if f == nil || !f.SingleDay {
			continue
		}
		encoded, _ := json.Marshal(f)
		if old, ok := snapshots[f.Day]; ok {
			if snapshotGeneration[f.Day].Equal(st.WhenGenerated) && string(canonical[f.Day]) != string(encoded) {
				copy := *old
				copy.Reason = "conflicting_daily_statements"
				snapshots[f.Day] = &copy
			}
			continue
		}
		snapshots[f.Day] = f
		canonical[f.Day] = encoded
		snapshotGeneration[f.Day] = st.WhenGenerated
	}
	dates := make([]string, 0, len(nav))
	for d := range nav {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	latest := latestCompletedFlexDate(now).Format(performanceDayFormat)
	expectedSnapshots := map[string]bool{}
	for index, d := range dates {
		if d < start || d > latest {
			continue
		}
		r.Through = d
		expectedSnapshots[d] = true
		f := snapshots[d]
		day := rpc.FXDay{Day: d, Reason: "daily_snapshot_missing"}
		if f != nil {
			day.PreviousDay = f.PreviousDay
			day.Foreign = f.Foreign
			day.Reason = f.Reason
			expectedSnapshots[f.PreviousDay] = true
			if r.BaseCurrency == "" {
				r.BaseCurrency = f.BaseCurrency
			}
			if f.Reason == "" {
				day = attributeFX(f, snapshots[f.PreviousDay])
				if navConflict[d] || navConflict[f.PreviousDay] {
					day.Contribution = nil
					day.ReconciliationResidual = nil
					day.Reason = "conflicting_nav_evidence"
				}
				if index == 0 || f.PreviousDay != dates[index-1] {
					day.Contribution = nil
					day.ReconciliationResidual = nil
					day.Reason = "nav_boundary_mismatch"
				}
				if opening := snapshots[f.PreviousDay]; opening != nil && math.Abs(nav[f.PreviousDay]-opening.NAV) > .02 {
					day.Contribution = nil
					day.ReconciliationResidual = nil
					day.Reason = "restated_opening_nav_requires_daily_snapshot"
				}
				if math.Abs(nav[d]-f.NAV) > .02 {
					day.Contribution = nil
					day.ReconciliationResidual = nil
					day.Reason = "restated_nav_requires_daily_snapshot"
				}
			}
		} else {
			for i, x := range dates {
				if x == d && i > 0 {
					day.PreviousDay = dates[i-1]
					expectedSnapshots[dates[i-1]] = true
					break
				}
			}
		}
		r.Days = append(r.Days, day)
	}
	for d := range expectedSnapshots {
		if f := snapshots[d]; f != nil && f.Reason == "" {
			r.Backfill.SnapshotDays++
		}
	}
	r.Backfill.ExpectedSnapshots = len(expectedSnapshots)
	if r.Through == "" {
		r.Reason = "year_to_date_nav_missing"
		return r
	}
	through, _ := time.Parse(performanceDayFormat, r.Through)
	monday := through.AddDate(0, 0, -(int(through.Weekday())+6)%7).Format(performanceDayFormat)
	for _, p := range []struct{ key, from string }{{"day", r.Through}, {"week", monday}, {"month", r.Through[:7] + "-01"}, {"ytd", start}} {
		out := rpc.FXPeriod{Key: p.key, From: p.from, Through: r.Through, State: "unavailable", MissingDays: []string{}}
		total := 0.
		foreign := false
		for _, d := range r.Days {
			if d.Day < p.from {
				continue
			}
			out.ExpectedDays++
			if d.Contribution == nil {
				out.MissingDays = append(out.MissingDays, d.Day)
			} else {
				out.ObservedDays++
				total += *d.Contribution
				foreign = foreign || d.Foreign
			}
		}
		if out.ExpectedDays > 0 && out.ObservedDays == out.ExpectedDays {
			out.State = "available"
			if !foreign {
				out.State = "no_exposure"
			}
			out.Contribution = &total
		} else if out.ObservedDays > 0 {
			out.State = "partial"
		}
		r.Periods = append(r.Periods, out)
	}
	mixed := false
	for _, f := range snapshots {
		if f.Day >= start && f.Day <= r.Through && f.BaseCurrency != "" && f.BaseCurrency != r.BaseCurrency {
			mixed = true
		}
	}
	if mixed {
		for i := range r.Days {
			r.Days[i].Contribution = nil
			r.Days[i].ReconciliationResidual = nil
			r.Days[i].Reason = "base_currency_changed_within_year"
		}
		for i := range r.Periods {
			r.Periods[i].State = "unavailable"
			r.Periods[i].Contribution = nil
			r.Periods[i].ObservedDays = 0
			r.Periods[i].MissingDays = []string{}
			for _, d := range r.Days {
				if d.Day >= r.Periods[i].From {
					r.Periods[i].MissingDays = append(r.Periods[i].MissingDays, d.Day)
				}
			}
		}
		r.BaseCurrency = ""
		r.Reason = "base_currency_changed_within_year"
	}
	r.State = r.Periods[len(r.Periods)-1].State
	// No short retained window may claim a YTD total. The seed statement must
	// cover Jan 1 through the latest completed day, including its opening NAV.
	_, _, seeded := fxNextSeed(rows, time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC), latest)
	if !seeded {
		p := &r.Periods[len(r.Periods)-1]
		p.State = "partial"
		p.Contribution = nil
		r.State = "partial"
		r.Reason = "year_to_date_calendar_backfill_required"
	}
	return r
}

func attributeFX(end, start *flexstmt.FXSnapshot) rpc.FXDay {
	d := rpc.FXDay{Day: end.Day, PreviousDay: end.PreviousDay, Foreign: end.Foreign, Reason: "opening_snapshot_missing"}
	if start == nil {
		return d
	}
	d.Foreign = d.Foreign || start.ClosingForeign
	if start.Reason != "" {
		d.Reason = "opening_" + start.Reason
		return d
	}
	if start.Day != end.PreviousDay || start.BaseCurrency != end.BaseCurrency {
		d.Reason = "opening_boundary_mismatch"
		return d
	}
	for _, pair := range []struct{ a, b map[string]float64 }{{end.CashStart, start.CashEnd}, {end.InterestStart, start.InterestEnd}} {
		keys := map[string]bool{}
		for c := range pair.a {
			keys[c] = true
		}
		for c := range pair.b {
			keys[c] = true
		}
		for c := range keys {
			if math.Abs(pair.a[c]-pair.b[c]) > .02 {
				d.Reason = "native_balance_continuity_failed"
				return d
			}
		}
	}
	keys := map[string]bool{}
	for _, m := range []map[string]float64{start.Book, end.Book, end.Conversion, end.External} {
		for c := range m {
			keys[c] = true
		}
	}
	fx, other := 0., 0.
	for c := range keys {
		x0, ok0 := start.Rates[c]
		x1, ok1 := end.Rates[c]
		if !ok0 || !ok1 || x0 <= 0 || x1 <= 0 {
			d.Reason = "boundary_fx_rate_missing"
			return d
		}
		fx += end.Book[c]*(x1-x0) + (end.Conversion[c]+end.External[c])*x0
		other += (end.Book[c] - start.Book[c] - end.Conversion[c] - end.External[c]) * x0
	}
	fx -= end.ExternalBase
	residual := end.NAV - start.NAV - end.ExternalBase - fx - other
	if math.Abs(residual) > .03 {
		d.Reason = "nav_bridge_does_not_reconcile"
		return d
	}
	d.Contribution = &fx
	d.ReconciliationResidual = &residual
	d.Reason = ""
	return d
}

// The worker resumes from accepted daily statements. It shares the existing
// Flex request lane and waits ten seconds between jobs (at most six/minute).
// A read never starts a job; startup and the owner's CLI backfill do.
func (s *Server) startFXWorker(ctx context.Context) {
	if s == nil || s.cfg == nil || !s.cfg.Flex.Enabled || ctx == nil {
		return
	}
	s.fxMu.Lock()
	if s.fxWorker {
		select {
		case s.fxWake <- struct{}{}:
		default:
		}
		s.fxMu.Unlock()
		return
	}
	s.fxWorker = true
	s.fxRunning = true
	s.fxWake = make(chan struct{}, 1)
	wake := s.fxWake
	s.fxReason = ""
	s.fxMu.Unlock()
	go func() {
		attempted := map[string]bool{}
		activeQuery := ""
		defer func() { s.fxMu.Lock(); s.fxRunning = false; s.fxWorker = false; s.fxMu.Unlock() }()
		for ctx.Err() == nil {
			delay := 10 * time.Second
			attemptedDay := ""
			query := s.flexEvidenceSelection().ActiveQueryFingerprint
			if query != activeQuery {
				attempted = map[string]bool{}
				activeQuery = query
			}
			rows, err := s.fxStatements(ctx)
			if err == nil {
				now := time.Now()
				if s.now != nil {
					now = s.now()
				}
				latest := latestCompletedFlexDate(now)
				from := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
				seedFrom, seedTo, seeded := fxNextSeed(rows, from, latest.Format(performanceDayFormat))
				if !seeded {
					_, err = s.fetchFXStatement(ctx, seedFrom, seedTo)
				} else {
					result := buildFX(rows, now)
					required := map[string]bool{}
					for _, d := range result.Days {
						if d.Contribution == nil {
							required[d.Day] = true
							if d.PreviousDay != "" {
								required[d.PreviousDay] = true
							}
						}
					}
					complete := map[string]bool{}
					selected := map[string]bool{}
					sort.SliceStable(rows, func(i, j int) bool { return rows[i].WhenGenerated.After(rows[j].WhenGenerated) })
					for _, st := range rows {
						if st.FX != nil && st.FX.SingleDay && !selected[st.FX.Day] {
							selected[st.FX.Day] = true
							complete[st.FX.Day] = st.FX.Reason == ""
						}
					}
					for _, d := range result.Days {
						if d.Contribution == nil && d.Reason != "daily_snapshot_missing" && d.Reason != "opening_snapshot_missing" {
							complete[d.Day] = false
							complete[d.PreviousDay] = false
						}
					}
					dates := []string{}
					for day := range required {
						if !complete[day] && !attempted[day] {
							dates = append(dates, day)
						}
					}
					sort.Sort(sort.Reverse(sort.StringSlice(dates)))
					s.fxMu.Lock()
					s.fxRunning = len(dates) > 0
					s.fxMu.Unlock()
					if len(dates) == 0 {
						delay = time.Minute
					} else {
						attemptedDay = dates[0]
						attempted[dates[0]] = true
						day, _ := time.Parse(performanceDayFormat, dates[0])
						_, err = s.fetchFXStatement(ctx, day, day)
					}
				}
				if err == nil {
					err = s.refreshStatementProjection(ctx)
				}
			}
			s.fxMu.Lock()
			if err != nil {
				if attemptedDay != "" {
					delete(attempted, attemptedDay)
				}
				s.fxReason = "backfill_fetch_or_acceptance_failed"
				if failure, ok := errors.AsType[*flexFetchFailure](err); ok {
					s.fxReason = "backfill_" + failure.reason
				}
				s.warnf("FX backfill: %s", s.fxReason)
				delay = time.Minute
			} else {
				s.fxReason = ""
			}
			s.fxMu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-wake:
				timer.Stop()
				attempted = map[string]bool{}
			}
		}
	}()
}

// fetchFXStatement retains only a broker-authenticated response matching
// the requested historical range. It serializes on the same lane as ordinary Flex fetches.
func (s *Server) fetchFXStatement(ctx context.Context, from, to time.Time) (flexFetchOutcome, error) {
	if from.IsZero() || to.IsZero() || from.After(to) || int(to.Sub(from)/(24*time.Hour))+1 > 365 {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonQueryInvalid, detail: "historical FX request range invalid"}
	}
	s.flexBrokerMu.Lock()
	defer s.flexBrokerMu.Unlock()
	query := s.cfg.Flex.QueryID
	var raw []byte
	var err error
	if s.flexRawDateRangeLockedFn != nil {
		raw, err = s.flexRawDateRangeLockedFn(ctx, from, to, flexPollAttempts, query, s.cfg.Flex.TokenPath)
	} else {
		raw, err = fetchFlexRawDateRangeWithCredentialsLocked(ctx, from, to, flexPollAttempts, query, s.cfg.Flex.TokenPath)
	}
	if err != nil {
		return flexFetchOutcome{}, err
	}
	rows, err := flexstmt.Parse(raw)
	if err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportInvalid, detail: "daily FX report could not be parsed"}
	}
	for _, row := range rows {
		if !row.FromDate.Equal(from) || !row.ToDate.Equal(to) {
			return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportInvalid, detail: "historical FX report does not match requested range"}
		}
	}
	return retainFlexStatementWithGenerationPolicy(ctx, raw, flexEvidenceSelection{ActiveQueryFingerprint: flexQueryFingerprint(query)}, true)
}

func (s *Server) handleFXBackfill(ctx context.Context) (*rpc.FXResult, error) {
	if s.serverCtx == nil || s.serverCtx.Err() != nil {
		return nil, fmt.Errorf("daemon lifecycle unavailable")
	}
	s.startFXWorker(s.serverCtx)
	return s.handleFX(ctx)
}

// fxCalendarSeed requires the broker's full YTD NAV calendar and opening close,
// not just a statement envelope that happens to span those dates.
func fxCalendarSeed(st flexstmt.Statement, from time.Time, through string) bool {
	if st.FromDate.After(from) || st.ToDate.Format(performanceDayFormat) < through {
		return false
	}
	opening, closing := false, false
	for _, row := range st.Equity {
		d := row.ReportDate.Format(performanceDayFormat)
		if d < from.Format(performanceDayFormat) {
			opening = true
		}
		if d == through {
			closing = true
		}
	}
	return opening && closing
}

// fxNextSeed splits a leap-year calendar within Flex's 365-date request bound.
// Each window must include its opening NAV and last broker reporting close.
func fxNextSeed(rows []flexstmt.Statement, from time.Time, through string) (time.Time, time.Time, bool) {
	end, _ := time.Parse(performanceDayFormat, through)
	if end.Before(from) {
		return time.Time{}, time.Time{}, true
	}
	cap := from.AddDate(0, 0, 364)
	if end.Before(cap) {
		cap = end
	}
	for cap.Weekday() == time.Saturday || cap.Weekday() == time.Sunday {
		cap = cap.AddDate(0, 0, -1)
	}
	primary := false
	for _, st := range rows {
		if fxCalendarSeed(st, from, cap.Format(performanceDayFormat)) {
			primary = true
		}
	}
	if !primary {
		return from, cap, false
	}
	if !end.After(cap) {
		return time.Time{}, time.Time{}, true
	}
	tail := cap.AddDate(0, 0, 1)
	for _, st := range rows {
		if fxCalendarSeed(st, tail, through) {
			return time.Time{}, time.Time{}, true
		}
	}
	return tail, end, false
}
