package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Chart retention is independent of freshness. Failed acquisition never
// overwrites a successful document or advances its observed clock.
// FullReadAt is when a read last replaced everything the series can re-read;
// see mergeMarketHistory. It schedules reconciliation, never freshness.
type storedMarketHistory struct {
	Version    int                     `json:"version"`
	Identity   string                  `json:"identity"`
	FullReadAt time.Time               `json:"full_read_at"`
	Result     rpc.MarketHistoryResult `json:"result"`
}

func marketHistoryIdentity(p rpc.MarketHistoryParams) (string, rpc.MarketHistoryParams, error) {
	_, interval, err := marketHistoryWindow(p.Range, time.Now())
	if err != nil {
		return "", p, err
	}
	if isOptionQuoteContract(p.Contract) {
		return "", p, errors.New("option history is not supplied")
	}
	_, contract, _, err := normaliseStockQuoteContract(p.Contract)
	if err != nil {
		return "", p, err
	}
	switch contract.SecType {
	case "STK", "IND", "CASH", "FUT":
	default:
		return "", p, errors.New("unsupported history security type")
	}
	if contract.SecType == "FUT" && (contract.ConID <= 0 || contract.Expiry == "") {
		return "", p, errors.New("history requires an exact dated futures contract")
	}
	p.Contract = contract
	b, _ := json.Marshal(struct {
		Contract rpc.ContractParams
		Interval string
		Version  int
	}{contract, interval, 1})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), p, nil
}

func (s *Server) loadMarketHistory(ctx context.Context, key string) (*storedMarketHistory, time.Time, error) {
	if s.coreStore == nil {
		return nil, time.Time{}, nil
	}
	doc, ok, err := s.coreStore.LoadChartCache(ctx, key)
	if err != nil || !ok {
		return nil, time.Time{}, err
	}
	var saved storedMarketHistory
	if err := json.Unmarshal(doc.JSON, &saved); err != nil {
		return nil, time.Time{}, err
	}
	if saved.Version != 1 || saved.Identity != key || validStoredHistory(saved.Result) != nil {
		return nil, time.Time{}, errors.New("invalid retained chart history")
	}
	return &saved, doc.UpdatedAt, nil
}

func validStoredHistory(r rpc.MarketHistoryResult) error {
	if r.Contract.ConID <= 0 || r.Contract.Currency == "" || r.AsOf.IsZero() || r.RequestedStart.IsZero() || r.End.After(r.AsOf.Add(time.Minute)) || len(r.Points) == 0 || len(r.Points) > 8000 {
		return errors.New("chart identity, observation or size invalid")
	}
	if r.PriceBasis != "TRADES" && r.PriceBasis != "MIDPOINT" {
		return errors.New("chart price basis unavailable")
	}
	if (r.Contract.SecType == "CASH") != (r.PriceBasis == "MIDPOINT") || r.Start != r.Points[0].At || r.End != r.Points[len(r.Points)-1].At {
		return errors.New("chart basis or coverage bounds invalid")
	}
	if r.Interval != "5 mins" && r.Interval != "30 mins" && r.Interval != "1 day" {
		return errors.New("chart interval unavailable")
	}
	for i, p := range r.Points {
		if p.At.IsZero() || !isFinitePrice(p.Value) || p.Volume != nil && (*p.Volume < 0 || r.PriceBasis != "TRADES") || i > 0 && !p.At.After(r.Points[i-1].At) {
			return errors.New("invalid or unordered chart observations")
		}
		if p.Open != nil || p.High != nil || p.Low != nil {
			if p.Open == nil || p.High == nil || p.Low == nil || !validHistoryRange(*p.Open, *p.High, *p.Low, p.Value) {
				return errors.New("invalid chart range")
			}
		}
	}
	return nil
}

func isFinitePrice(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

func validHistoryRange(open, high, low, close float64) bool {
	return isFinitePrice(open) && isFinitePrice(high) && isFinitePrice(low) && isFinitePrice(close) && low <= min(open, close) && high >= max(open, close)
}

func historyRequestStart(p rpc.MarketHistoryParams, now time.Time) time.Time {
	if p.Range == "1D" && usChartCalendar(p.Contract) {
		last, current, ok := lastCompletedMarketSessionWindow(now, marketcal.MarketUSEquity)
		if ok {
			day := last.Date
			opens := current.Open
			if p.Contract.SecType == "STK" && !opens.IsZero() {
				loc, _ := time.LoadLocation("America/New_York")
				d := opens.In(loc)
				opens = time.Date(d.Year(), d.Month(), d.Day(), 4, 0, 0, 0, loc)
			}
			if !opens.IsZero() && !now.Before(opens) {
				day = current.Date
			}
			loc, _ := time.LoadLocation("America/New_York")
			start, _ := time.ParseInLocation("2006-01-02", day, loc)
			return start
		}
	}
	start := marketHistoryStart(p.Range, now)
	if p.Range != "1D" && p.Range != "5D" {
		return time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	}
	return start
}

func usChartCalendar(c rpc.ContractParams) bool {
	if c.Currency != "USD" || c.SecType != "STK" && c.SecType != "IND" {
		return false
	}
	for _, exchange := range []string{c.PrimaryExch, c.Exchange} {
		if slices.Contains([]string{"NYSE", "NASDAQ", "ARCA", "AMEX", "ISLAND", "BATS", "IEX", "CBOE"}, exchange) {
			return true
		}
	}
	return false
}

// globexContract reports a USD future on a CME Group venue. Those trade on
// Globex almost around the clock and have no embedded venue calendar; the
// one closure the daemon can name for them is the weekend.
func globexContract(c rpc.ContractParams) bool {
	return c.SecType == "FUT" && c.Currency == "USD" && slices.Contains([]string{"CME", "CBOT", "COMEX", "NYMEX", "GLOBEX"}, c.Exchange)
}

// globexWeekendSince returns when the Globex weekend closure containing now
// began, Friday 17:00 New York, or zero while Globex trades. Holidays are
// not modelled: Globex mostly trades through them, and a refresh on one only
// re-reads bars, so the ordinary cadence stands.
func globexWeekendSince(now time.Time) time.Time {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}
	}
	local := now.In(loc)
	daysSinceFriday := 0
	switch local.Weekday() {
	case time.Friday:
		if local.Hour() < 17 {
			return time.Time{}
		}
	case time.Saturday:
		daysSinceFriday = 1
	case time.Sunday:
		if local.Hour() >= 18 {
			return time.Time{}
		}
		daysSinceFriday = 2
	default:
		return time.Time{}
	}
	friday := local.AddDate(0, 0, -daysSinceFriday)
	return time.Date(friday.Year(), friday.Month(), friday.Day(), 17, 0, 0, 0, loc)
}

// marketHistoryReconcileEvery spaces the full reads that replace a series'
// re-readable range, catching corrections and corporate actions that an
// overlapping tail read cannot remove.
const marketHistoryReconcileEvery = 7 * 24 * time.Hour

// historyReconcileDue reports whether the series' periodic full read is due.
// It schedules a broker read but says nothing about the bars' freshness, so
// it never sets the served refresh_due.
func historyReconcileDue(saved *storedMarketHistory, now time.Time) bool {
	return saved != nil && now.Sub(saved.FullReadAt) >= marketHistoryReconcileEvery
}

// historyRefreshOverdue is the served refresh_due: the record is behind and
// the refresh worker has had its full cycle to catch it up, so a consumer
// can call the bars limited. The cycle starts where a read first became
// possible: the bars' cadence after the last read, or the end of a closure
// that held the record current (a US stock's 04:00 premarket or another
// intraday venue's open, a US daily series' fifteen minutes after the open,
// Globex's Sunday 18:00 reopen), whichever is later. So the record must
// already have been behind a whole cycle ago; judging a closure's end alone
// flipped the flag the instant it passed, before the worker could read. A
// record read within the cycle is overdue only when its view needs a prefix
// it lacks. A record inside its normal refresh cycle, or awaiting only its
// weekly reconciliation, is current.
func historyRefreshOverdue(saved *storedMarketHistory, p rpc.MarketHistoryParams, now time.Time) bool {
	cycleStart := now.Add(-marketHistoryRefreshGrace)
	return historyRefreshDue(saved, p, now) && (saved == nil || cycleStart.Before(saved.Result.AsOf) || historyRefreshDue(saved, p, cycleStart))
}

// historyRefreshDue reports whether the retained record is behind what the
// broker would now supply: the view needs a prefix the record lacks, or its
// bars are older than their refresh cadence while the venue may have traded
// since. readRetainedHistory reads the broker when this or
// historyReconcileDue holds.
func historyRefreshDue(saved *storedMarketHistory, p rpc.MarketHistoryParams, now time.Time) bool {
	if saved == nil {
		return true
	}
	// Request keys may still say SMART; only the retained broker identity
	// can choose the venue calendar and establish whether a prefix is missing.
	p.Contract = saved.Result.Contract
	if historyRequestStart(p, now).Before(saved.Result.RequestedStart) {
		return true
	}
	r := saved.Result
	if globexContract(r.Contract) {
		// Bars read after Friday's close stand until Sunday evening; a
		// weekend refresh would only ask the broker for the same session.
		if since := globexWeekendSince(now); !since.IsZero() && r.AsOf.After(since) {
			return false
		}
	}
	if usChartCalendar(r.Contract) && r.Interval == "1 day" {
		last, current, ok := lastCompletedMarketSessionWindow(now.Add(-15*time.Minute), marketcal.MarketUSEquity)
		if ok && !current.IsOpen && r.AsOf.After(last.Close.Add(15*time.Minute)) && r.End.Format("2006-01-02") >= last.Date {
			return false
		}
	}
	if usChartCalendar(r.Contract) && r.Interval != "1 day" {
		last, current, ok := lastCompletedMarketSessionWindow(now, marketcal.MarketUSEquity)
		if ok {
			finished, opens := last.Close, current.Open
			if r.Contract.SecType == "STK" {
				loc, _ := time.LoadLocation("America/New_York")
				d := finished.In(loc)
				finished = time.Date(d.Year(), d.Month(), d.Day(), 20, 15, 0, 0, loc)
				if !opens.IsZero() {
					d = opens.In(loc)
					opens = time.Date(d.Year(), d.Month(), d.Day(), 4, 0, 0, 0, loc)
				}
			} else {
				finished = finished.Add(15 * time.Minute)
			}
			closed := opens.IsZero() || now.Before(opens) || !current.Close.IsZero() && now.After(current.Close) && now.After(finished)
			if closed && now.After(finished) && r.AsOf.After(finished) {
				return false
			}
		}
	}
	interval := 5 * time.Minute
	if r.Interval == "30 mins" {
		interval = 30 * time.Minute
	} else if r.Interval == "1 day" {
		interval = time.Hour
	}
	return now.Sub(r.AsOf) >= interval
}

func historyTailDays(saved *storedMarketHistory, p rpc.MarketHistoryParams, now time.Time) int {
	if saved == nil {
		return 0
	}
	p.Contract = saved.Result.Contract
	if saved.Result.Interval == "1 day" && historyReconcileDue(saved, now) {
		return -min(1830, max(1, int(math.Ceil(now.Sub(saved.Result.RequestedStart).Hours()/24))))
	}
	if historyRequestStart(p, now).Before(saved.Result.RequestedStart) || historyReconcileDue(saved, now) {
		return 0 // Missing prefix or periodic corporate-action/correction reconciliation.
	}
	// IBKR durations are rounded days. Re-read the tail with an overlap rather
	// than the whole multi-year request. A changed older overlap forces a full read.
	days := int(math.Ceil(now.Sub(saved.Result.End).Hours()/24)) + 3
	return max(1, days)
}

func historicalOverlapChanged(old, next rpc.MarketHistoryResult) bool {
	if old.Interval != "1 day" {
		return false
	}
	values := make(map[int64]float64, len(old.Points))
	for _, p := range old.Points {
		values[p.At.Unix()] = p.Value
	}
	cutoff := old.End.AddDate(0, 0, -2)
	for _, p := range next.Points {
		if v, ok := values[p.At.Unix()]; ok && p.At.Before(cutoff) && v != p.Value {
			return true
		}
	}
	return false
}

// logMarketHistoryFallback records why a refresh served recorded history
// instead of fresh bars: the RPC carries only a fixed detail, and a silent
// refresh loop cannot be told apart from a broker that never answers.
func (s *Server) logMarketHistoryFallback(p rpc.MarketHistoryParams, saved *storedMarketHistory, cause error) {
	if s.logger == nil || saved == nil {
		return
	}
	if errors.Is(cause, ibkrlib.ErrIBKRUnavailable) && s.logGatewayDependency("history refresh requires the broker; recorded history remains available") {
		return
	}
	s.logger.Warnf("market history %s %s: IBKR refresh failed: %v; serving recorded history through %s", p.Contract.Symbol, p.Range, cause, saved.Result.End.UTC().Format("2006-01-02"))
}

// readRetainedHistory is the daemon-owned acquisition/selection path. Requests
// own fetch cancellation; the server owns the database and its short merge lock.
func (s *Server) readRetainedHistory(ctx context.Context, key string, p rpc.MarketHistoryParams, now time.Time, fetch func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error)) (*rpc.MarketHistoryResult, error) {
	saved, storedAt, err := s.loadMarketHistory(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("chart cache integrity: %w", err)
	}
	if saved != nil && !historyRefreshDue(saved, p, now) && !historyReconcileDue(saved, now) {
		return selectStoredHistory(saved, storedAt, p, now, "cache", "", false), nil
	}
	tail := historyTailDays(saved, p, now)
	fresh, fetchErr := fetch(ctx, p, tail, now)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if fetchErr == nil {
		if fresh == nil {
			fetchErr = errors.New("history response absent")
		} else {
			fetchErr = validStoredHistory(*fresh)
		}
	}
	if fetchErr == nil && !historyResponseMatches(p, *fresh, now) {
		fetchErr = errors.New("history response identity or clock changed")
	}
	if fetchErr == nil && tail > 0 && historicalOverlapChanged(saved.Result, *fresh) {
		tail = -min(1830, max(1, int(math.Ceil(now.Sub(saved.Result.RequestedStart).Hours()/24))))
		fresh, fetchErr = fetch(ctx, p, tail, now)
		if fetchErr == nil && fresh != nil {
			fetchErr = validStoredHistory(*fresh)
		} else if fetchErr == nil {
			fetchErr = errors.New("history response absent")
		}
		if fetchErr == nil && !historyResponseMatches(p, *fresh, now) {
			fetchErr = errors.New("history response identity or clock changed")
		}
	}
	if fetchErr != nil {
		if saved == nil {
			return nil, fetchErr
		}
		if errors.Is(fetchErr, ibkrlib.ErrContractNoDefinition) {
			// The served fallback carries only a fixed detail, so the
			// refresh worker cannot classify it. Relay the broker's verdict
			// to its memory here, which says it once instead of per attempt.
			s.rememberMarketHistoryDefinitionMiss(p.Contract, now, fetchErr)
		} else {
			s.logMarketHistoryFallback(p, saved, fetchErr)
		}
		return selectStoredHistory(saved, storedAt, p, now, "cache", "IBKR refresh unavailable; showing recorded history", true), nil
	}
	// The broker defined the contract and answered with bars, so a remembered
	// definition verdict for it no longer holds.
	s.clearMarketHistoryDefinitionMiss(p.Contract)
	if saved != nil && tail <= 0 {
		if compared, absent := historyAbsentRecorded(saved.Result, *fresh, now); historyResponseThin(*fresh, compared, absent) {
			s.logMarketHistoryFallback(p, saved, fmt.Errorf("response lacks sessions the recorded history has: %s", historySessionsLabel(*fresh, compared, absent)))
			return selectStoredHistory(saved, storedAt, p, now, "cache", "IBKR returned incomplete history; previous range retained", true), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Different range requests may finish together. Re-read under this short
	// lock so a shorter response cannot erase a concurrently retained long one.
	s.marketData.storeMu.Lock()
	defer s.marketData.storeMu.Unlock()
	latest, latestAt, err := s.loadMarketHistory(ctx, key)
	if err != nil {
		return nil, err
	}
	if latest != nil && latest.Result.AsOf.After(fresh.AsOf) {
		return selectStoredHistory(latest, latestAt, p, now, "cache", "", false), nil
	}
	if latest != nil && (latest.Result.Contract != fresh.Contract || latest.Result.PriceBasis != fresh.PriceBasis || latest.Result.RegularHoursOnly != fresh.RegularHoursOnly) {
		// A resolved identity or basis change starts a new series. Never splice
		// old listing/expiry/split conventions into the replacement.
		latest = nil
	}
	if latest != nil && tail <= 0 {
		compared, absent := historyAbsentRecorded(latest.Result, *fresh, now)
		if historyResponseThin(*fresh, compared, absent) {
			s.logMarketHistoryFallback(p, latest, fmt.Errorf("response lacks sessions the recorded history has: %s", historySessionsLabel(*fresh, compared, absent)))
			return selectStoredHistory(latest, latestAt, p, now, "cache", "IBKR returned incomplete history; previous range retained", true), nil
		}
		if len(absent) > 0 {
			// A futures read lacking a few older sessions: keep the recorded
			// bars for those sessions and reconcile the rest.
			kept := *fresh
			kept.Points = append(slices.Clone(fresh.Points), absent...)
			slices.SortFunc(kept.Points, func(a, b rpc.MarketHistoryPoint) int { return a.At.Compare(b.At) })
			fresh = &kept
			if s.logger != nil {
				log := s.logger.Warnf
				if len(historyCounted(*fresh, absent)) == 0 {
					log = s.logger.Infof // sessions without trades only: the expected artefact
				}
				log("market history %s %s: IBKR response lacks sessions the recorded history has: %s; kept the recorded bars", p.Contract.Symbol, p.Range, historySessionsLabel(*fresh, compared, absent))
			}
		}
	}
	next := mergeMarketHistory(key, latest, *fresh, tail <= 0, now)
	storedAt = time.Time{}
	detail := ""
	if s.coreStore != nil {
		payload, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		doc, err := s.coreStore.SaveChartCache(ctx, key, payload)
		if err == nil {
			storedAt = doc.UpdatedAt
		} else {
			detail = "Chart displayed; database save failed"
		}
	} else {
		detail = "Chart displayed; durable storage unavailable"
	}
	return selectStoredHistory(&next, storedAt, p, now, "ibkr", detail, false), nil
}

// historyAbsentRecorded compares a full read with the record it replaces:
// compared are the recorded sessions inside the span the read replaces (see
// historyReplaceFrom) that have stood for two days, and absent are those the
// read lacks, both oldest first.
func historyAbsentRecorded(old, fresh rpc.MarketHistoryResult, now time.Time) (compared, absent []rpc.MarketHistoryPoint) {
	points := make(map[int64]bool, len(fresh.Points))
	for _, p := range fresh.Points {
		points[p.At.Unix()] = true
	}
	from := historyReplaceFrom(fresh)
	for _, p := range old.Points {
		if p.At.Before(from) || !p.At.Before(now.Add(-48*time.Hour)) {
			continue
		}
		compared = append(compared, p)
		if !points[p.At.Unix()] {
			absent = append(absent, p)
		}
	}
	return compared, absent
}

// A dated futures contract's daily history is patchy a year back. A
// deferred contract month has many sessions without trades, which IBKR
// serves as zero-volume bars in one response and omits in the next; NQ's
// December contract recorded 133 of them in 250 sessions. Those are kept
// whenever a read lacks them and never count. Of
// the traded sessions a futures read may lack up to a tenth, as long as it
// carries the latest five; beyond that it is thin.
const (
	historyPatchyAbsentPercent  = 10
	historyPatchyRecentSessions = 5
)

// historyTradeless reports a recorded session without trades: zero volume.
// IBKR serves it as a flat bar at the settlement, which records carry with
// open, high and low; bars recorded before those were kept have none. Either
// way the volume says it.
func historyTradeless(p rpc.MarketHistoryPoint) bool {
	return p.Volume != nil && *p.Volume == 0
}

// historyCounted returns the sessions the patchy bound counts: for a dated
// futures contract those with trades, for every other type all of them.
func historyCounted(fresh rpc.MarketHistoryResult, points []rpc.MarketHistoryPoint) []rpc.MarketHistoryPoint {
	if fresh.Contract.SecType != "FUT" {
		return points
	}
	return slices.DeleteFunc(slices.Clone(points), historyTradeless)
}

// historyResponseThin reports whether a full read that lacks recorded
// sessions is incomplete, so it must not replace the record. For stocks,
// indices and FX any absent session is. For a dated futures contract only a
// read beyond the patchy bound is; within it the record keeps its bars for
// the absent sessions and the rest is reconciled.
func historyResponseThin(fresh rpc.MarketHistoryResult, compared, absent []rpc.MarketHistoryPoint) bool {
	if len(absent) == 0 {
		return false
	}
	if fresh.Contract.SecType != "FUT" {
		return true
	}
	compared, absent = historyCounted(fresh, compared), historyCounted(fresh, absent)
	if len(absent) == 0 {
		return false
	}
	if len(absent)*100 > len(compared)*historyPatchyAbsentPercent {
		return true
	}
	recent := compared[max(0, len(compared)-historyPatchyRecentSessions):]
	return !absent[len(absent)-1].At.Before(recent[0].At)
}

// historySessionsLabel names absent sessions in the log: how many of the
// counted sessions are absent and the first three, as session dates for
// daily bars, then how many sessions without trades are absent.
func historySessionsLabel(r rpc.MarketHistoryResult, compared, absent []rpc.MarketHistoryPoint) string {
	counted, missing := historyCounted(r, compared), historyCounted(r, absent)
	layout := "2006-01-02 15:04Z"
	if r.TimestampKind == "session_date" {
		layout = "2006-01-02"
	}
	var parts []string
	if len(missing) > 0 {
		names := make([]string, 0, 4)
		for _, p := range missing[:min(3, len(missing))] {
			names = append(names, p.At.UTC().Format(layout))
		}
		if len(missing) > 3 {
			names = append(names, "…")
		}
		parts = append(parts, fmt.Sprintf("%d of %d (%s)", len(missing), len(counted), strings.Join(names, ", ")))
	}
	if tradeless := len(absent) - len(missing); tradeless > 0 {
		parts = append(parts, fmt.Sprintf("%d without trades", tradeless))
	}
	return strings.Join(parts, " and ")
}

// historyReplaceFrom is where a full read starts to replace the sessions a
// record holds: the span it requested. IBKR serves a dated futures
// contract's bars only about a year back, a window that rolls forward with
// the clock, while the record keeps the sessions it already holds. Recorded
// sessions before the first bar served lie beyond that depth, not outside a
// thin response, so they are retained unreconciled; a dated contract's
// prices are never adjusted, so nothing is spliced. For every other type a
// recorded session the response lacks still marks it incomplete.
func historyReplaceFrom(fresh rpc.MarketHistoryResult) time.Time {
	if fresh.Contract.SecType == "FUT" && fresh.Start.After(fresh.RequestedStart) {
		return fresh.Start
	}
	return fresh.RequestedStart
}

func mergeMarketHistory(key string, old *storedMarketHistory, fresh rpc.MarketHistoryResult, full bool, now time.Time) storedMarketHistory {
	next := storedMarketHistory{Version: 1, Identity: key, FullReadAt: now, Result: fresh}
	next.Result.Cache = nil
	points := make(map[int64]rpc.MarketHistoryPoint)
	if old != nil {
		next.FullReadAt = old.FullReadAt
		from := historyReplaceFrom(fresh)
		for _, p := range old.Result.Points {
			// A full overlapping fetch replaces its own interval, including
			// removed/corrected observations, while retaining older ranges.
			if !full || p.At.Before(from) {
				points[p.At.Unix()] = p
			}
		}
		if old.Result.RequestedStart.Before(fresh.RequestedStart) {
			next.Result.RequestedStart = old.Result.RequestedStart
		}
	}
	for _, p := range fresh.Points {
		points[p.At.Unix()] = p
	}
	all := make([]rpc.MarketHistoryPoint, 0, len(points))
	for _, p := range points {
		all = append(all, p)
	}
	slices.SortFunc(all, func(a, b rpc.MarketHistoryPoint) int { return a.At.Compare(b.At) })
	var prunedBefore time.Time
	if fresh.Interval == "1 day" {
		cutoff := now.AddDate(-5, 0, 0).AddDate(0, 0, -1)
		if len(all) > 0 && all[0].At.Before(cutoff) {
			prunedBefore = cutoff
		}
		all = slices.DeleteFunc(all, func(p rpc.MarketHistoryPoint) bool { return p.At.Before(cutoff) })
	} else {
		// Keep twenty observed venue dates. The source's range and session
		// metadata remains explicit; missing dates are never called sessions.
		loc := time.UTC
		if usChartCalendar(fresh.Contract) {
			loc, _ = time.LoadLocation("America/New_York")
		}
		dates, prev, start := 0, "", 0
		for i, a := range slices.Backward(all) {
			date := a.At.In(loc).Format("2006-01-02")
			if date != prev {
				dates++
				prev = date
			}
			if dates > 20 {
				start = i + 1
				break
			}
		}
		all = all[start:]
		if start > 0 {
			prunedBefore = all[0].At
		}
	}
	if len(all) > 8000 {
		all = all[len(all)-8000:]
		prunedBefore = all[0].At
	}
	next.Result.Points = all
	if len(all) > 0 {
		next.Result.Start, next.Result.End = all[0].At, all[len(all)-1].At
		if next.Result.RequestedStart.Before(prunedBefore) {
			next.Result.RequestedStart = prunedBefore
		}
	}
	// A full read reconciles what it re-read. Daily reconciliation requests
	// the retained depth, so it counts once it reaches the start still
	// retained after pruning. Twenty retained venue dates outlast every
	// intraday request window and no read returns to them, so a full
	// intraday read of its window is the whole reconciliation that series
	// can have. Requiring the retained range there never advanced the clock,
	// and every refresh became a full read reported as refresh due.
	if full && (old == nil || fresh.Interval != "1 day" || !fresh.RequestedStart.After(next.Result.RequestedStart)) {
		next.FullReadAt = now
	}
	return next
}

func historyResponseMatches(p rpc.MarketHistoryParams, r rpc.MarketHistoryResult, now time.Time) bool {
	c := p.Contract
	_, interval, _ := marketHistoryWindow(p.Range, now)
	return (c.ConID == 0 || c.ConID == r.Contract.ConID) && c.Symbol == r.Contract.Symbol && c.Currency == r.Contract.Currency && c.SecType == r.Contract.SecType && (c.Expiry == "" || c.Expiry == r.Contract.Expiry) && r.Interval == interval && !r.AsOf.After(now.Add(time.Minute)) && !r.End.After(now.Add(time.Minute)) && r.RegularHoursOnly == (interval == "1 day") && r.TimestampKind == map[bool]string{true: "session_date", false: "instant"}[interval == "1 day"]
}

func selectStoredHistory(saved *storedMarketHistory, storedAt time.Time, p rpc.MarketHistoryParams, now time.Time, selected, detail string, failed bool) *rpc.MarketHistoryResult {
	r := saved.Result
	r.Range = p.Range
	p.Contract = r.Contract
	start := historyRequestStart(p, now)
	r.RequestedStart = start
	r.Points = slices.Clone(saved.Result.Points)
	r.Points = slices.DeleteFunc(r.Points, func(p rpc.MarketHistoryPoint) bool { return p.At.Before(start) })
	coverage := "observed"
	if start.Before(saved.Result.RequestedStart) {
		coverage = "partial"
	}
	missing := historyMissingSessions(r, start, now)
	if missing > 0 {
		coverage = "partial"
	}
	previous := len(r.Points) == 0
	if previous {
		// Preserve the last recorded window, explicitly identified as older.
		r.Points = slices.Clone(saved.Result.Points)
		previousStart := historyRequestStart(p, r.Points[len(r.Points)-1].At)
		r.Points = slices.DeleteFunc(r.Points, func(p rpc.MarketHistoryPoint) bool { return p.At.Before(previousStart) })
		coverage = "previous_window"
	}
	r.Start, r.End = r.Points[0].At, r.Points[len(r.Points)-1].At
	r.Cache = &rpc.MarketHistoryCache{Selected: selected, StoredAt: storedAt, FetchedAt: saved.Result.AsOf, CoveredThrough: r.End, Coverage: coverage, MissingSessions: missing, Detail: detail, RefreshFailed: failed, RefreshDue: historyRefreshOverdue(saved, p, now), PreviousWindow: previous}
	return &r
}

// historyMissingSessions checks known US daily sessions only. It does not infer
// expected intraday trades, listing dates, or unsupported venue calendars.
func historyMissingSessions(r rpc.MarketHistoryResult, start, now time.Time) int {
	if r.Interval != "1 day" || !usChartCalendar(r.Contract) {
		return 0
	}
	observed := make(map[string]bool, len(r.Points))
	for _, p := range r.Points {
		observed[p.At.UTC().Format("2006-01-02")] = true
	}
	loc, _ := time.LoadLocation("America/New_York")
	cal := marketcal.NewWithClock(func() time.Time { return now })
	day := time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, loc)
	missing := 0
	for i := 0; i < 1831 && day.Before(now); i++ {
		session, err := cal.SessionAt(marketcal.MarketUSEquity, day)
		if err == nil && !session.Close.IsZero() && session.Close.Add(15*time.Minute).Before(now) && !observed[session.Date] {
			missing++
		}
		day = day.AddDate(0, 0, 1)
	}
	return missing
}
