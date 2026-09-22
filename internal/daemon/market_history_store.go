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
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Chart retention is independent of freshness. Failed acquisition never
// overwrites a successful document or advances its observed clock.
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
	}
	return nil
}

func isFinitePrice(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

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

func historyRefreshDue(saved *storedMarketHistory, p rpc.MarketHistoryParams, now time.Time) bool {
	if saved == nil {
		return true
	}
	// Request keys may still say SMART; only the retained broker identity
	// can choose the venue calendar and establish whether a prefix is missing.
	p.Contract = saved.Result.Contract
	if historyRequestStart(p, now).Before(saved.Result.RequestedStart) || now.Sub(saved.FullReadAt) >= 7*24*time.Hour {
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
	if saved.Result.Interval == "1 day" && now.Sub(saved.FullReadAt) >= 7*24*time.Hour {
		return -min(1830, max(1, int(math.Ceil(now.Sub(saved.Result.RequestedStart).Hours()/24))))
	}
	if historyRequestStart(p, now).Before(saved.Result.RequestedStart) || now.Sub(saved.FullReadAt) >= 7*24*time.Hour {
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
	if saved != nil && !historyRefreshDue(saved, p, now) {
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
	if saved != nil && tail <= 0 && historyLostStablePoints(saved.Result, *fresh, now) {
		s.logMarketHistoryFallback(p, saved, errors.New("response lacks sessions the recorded history has"))
		return selectStoredHistory(saved, storedAt, p, now, "cache", "IBKR returned incomplete history; previous range retained", true), nil
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
	if latest != nil && tail <= 0 && historyLostStablePoints(latest.Result, *fresh, now) {
		return selectStoredHistory(latest, latestAt, p, now, "cache", "IBKR returned incomplete history; previous range retained", true), nil
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

func historyLostStablePoints(old, fresh rpc.MarketHistoryResult, now time.Time) bool {
	points := make(map[int64]bool, len(fresh.Points))
	for _, p := range fresh.Points {
		points[p.At.Unix()] = true
	}
	for _, p := range old.Points {
		if !p.At.Before(fresh.RequestedStart) && p.At.Before(now.Add(-48*time.Hour)) && !points[p.At.Unix()] {
			return true
		}
	}
	return false
}

func mergeMarketHistory(key string, old *storedMarketHistory, fresh rpc.MarketHistoryResult, full bool, now time.Time) storedMarketHistory {
	next := storedMarketHistory{Version: 1, Identity: key, FullReadAt: now, Result: fresh}
	next.Result.Cache = nil
	points := make(map[int64]rpc.MarketHistoryPoint)
	if old != nil {
		next.FullReadAt = old.FullReadAt
		for _, p := range old.Result.Points {
			// A full overlapping fetch replaces its own interval, including
			// removed/corrected observations, while retaining older ranges.
			if !full || p.At.Before(fresh.RequestedStart) {
				points[p.At.Unix()] = p
			}
		}
		if old.Result.RequestedStart.Before(fresh.RequestedStart) {
			next.Result.RequestedStart = old.Result.RequestedStart
		}
	}
	if full && (old == nil || !fresh.RequestedStart.After(old.Result.RequestedStart)) {
		next.FullReadAt = now
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
	r.Cache = &rpc.MarketHistoryCache{Selected: selected, StoredAt: storedAt, FetchedAt: saved.Result.AsOf, CoveredThrough: r.End, Coverage: coverage, MissingSessions: missing, Detail: detail, RefreshFailed: failed, RefreshDue: historyRefreshDue(saved, p, now), PreviousWindow: previous}
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
