package daemon

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	setupMarkoutRegularMinutes = 30 * time.Minute
	// setupMarkoutSessionSearch bounds calendar walks; a longer exchange
	// closure than this leaves the target missing instead of guessing.
	setupMarkoutSessionSearch = 14
	setupMarkoutFillQueue     = 256
	setupMarkoutSweepEvery    = time.Second
	// setupMarkoutExecTimeSkew bounds how far a broker execution time may sit
	// after its local receipt before it is distrusted as malformed.
	setupMarkoutExecTimeSkew = time.Minute
)

// setupMarkoutRuntime connects order-journal fills to the durable schedule.
// The lifecycle callback only offers a fill to a bounded queue; persistence
// and quote reads happen on the markout worker so fill handling never waits.
type setupMarkoutRuntime struct {
	store *setupMarkoutStore
	fills chan orderJournalEvent
	// rescan latches when the queue overflowed; the worker then rebuilds
	// any unscheduled fills from the order journal.
	rescan atomic.Bool

	inFlightMu sync.Mutex
	inFlight   map[string]bool
	// reads is the semaphore for concurrent quote reads; wg tracks them.
	reads chan struct{}
	wg    sync.WaitGroup
	// quote replaces the exact-contract broker read in tests.
	quote func(context.Context, rpc.ContractParams, time.Duration) (setupMarkoutQuote, error)
}

func newSetupMarkoutRuntime(store *setupMarkoutStore) *setupMarkoutRuntime {
	return &setupMarkoutRuntime{store: store, fills: make(chan orderJournalEvent, setupMarkoutFillQueue), inFlight: map[string]bool{}, reads: make(chan struct{}, setupMarkoutMaxReads)}
}

// offerSetupMarkoutFill is called after a lifecycle event is durably
// journaled. It never blocks: a full queue latches a journal rescan.
func (s *Server) offerSetupMarkoutFill(ev orderJournalEvent) {
	if s == nil || s.setupMarkouts == nil || !setupMarkoutEligibleFill(ev) {
		return
	}
	select {
	case s.setupMarkouts.fills <- ev:
	default:
		s.setupMarkouts.rescan.Store(true)
	}
}

// setupMarkoutEligibleFill accepts executions of orders Canary placed through
// its own preview path. Manual TWS orders, bypass-preview flows and
// instruments other than stocks and options are not entry evidence.
func setupMarkoutEligibleFill(ev orderJournalEvent) bool {
	if ev.Type != orderJournalEventStatusUpdated || strings.TrimSpace(ev.ExecID) == "" || strings.TrimSpace(ev.OrderRef) == "" {
		return false
	}
	if strings.TrimSpace(ev.PreviewTokenID) == "" || ev.BypassPreview {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(ev.SecType)) {
	case "STK", "OPT":
		return true
	default:
		return false
	}
}

// setupMarkoutTargetsForFill derives both horizons for one execution from the
// journal event's own execution fields: ExecShares and ExecSide are this
// execution's quantity and broker side, LastFillPrice its price, while Filled
// stays the order's cumulative quantity. A fill
// whose identity, quantity, price or calendar cannot be proven yields missing
// targets with the failing condition; nothing is estimated.
func setupMarkoutTargetsForFill(ev orderJournalEvent, cal *marketcal.Calendar, now time.Time) []rpc.SetupMarkoutTarget {
	base := rpc.SetupMarkoutTarget{
		OrderRef: strings.TrimSpace(ev.OrderRef), ExecID: strings.TrimSpace(ev.ExecID), Status: rpc.SetupMarkoutPending,
		Mode: strings.ToLower(strings.TrimSpace(ev.Mode)),
		Contract: rpc.ContractParams{
			ConID: ev.ConID, Symbol: strings.ToUpper(strings.TrimSpace(ev.Symbol)), SecType: strings.ToUpper(strings.TrimSpace(ev.SecType)),
			Exchange: strings.TrimSpace(ev.Exchange), PrimaryExch: strings.TrimSpace(ev.PrimaryExch), Currency: strings.ToUpper(strings.TrimSpace(ev.Currency)),
			LocalSymbol: strings.TrimSpace(ev.LocalSymbol), TradingClass: strings.TrimSpace(ev.TradingClass),
			Expiry: strings.TrimSpace(ev.Expiry), Strike: ev.Strike, Right: strings.ToUpper(strings.TrimSpace(ev.Right)), Multiplier: ev.Multiplier,
		},
		FillQuantity: ev.ExecShares, FillPrice: ev.LastFillPrice, CommissionStatus: "not_recorded",
		ScheduledAt: now.UTC(), Currency: strings.ToUpper(strings.TrimSpace(ev.Currency)),
	}
	if base.Contract.SecType == "STK" {
		base.Contract.Multiplier = 1
	}
	base.Side = setupMarkoutSide(ev)
	switch base.Side {
	case rpc.OrderActionBuy:
		base.MarkSide = "bid"
	case rpc.OrderActionSell:
		base.MarkSide = "ask"
	}
	base.FillAt, base.FillTimeSource = setupMarkoutFillTime(ev)
	targets := []rpc.SetupMarkoutTarget{base, base}
	targets[0].Horizon, targets[1].Horizon = rpc.SetupMarkoutHorizonT30, rpc.SetupMarkoutHorizonNextClose
	reason := ""
	switch {
	case base.Contract.ConID <= 0 || base.Contract.Symbol == "":
		reason = "contract_identity_unknown"
	case base.Contract.SecType == "OPT" && (base.Contract.Multiplier <= 0 || base.Contract.Expiry == "" || base.Contract.Right == "" || base.Contract.Strike <= 0):
		reason = "option_identity_incomplete"
	case base.Currency == "":
		reason = "currency_unknown"
	case base.Side == "":
		reason = "fill_side_unknown"
	case !positiveFinite(base.FillQuantity):
		reason = "fill_quantity_unknown"
	case !positiveFinite(base.FillPrice):
		reason = "fill_price_unknown"
	}
	if reason != "" {
		for i := range targets {
			targets[i] = setupMarkoutMissing(targets[i], reason, now)
		}
		return targets
	}
	market, ok := setupMarkoutMarket(base.Contract)
	if !ok {
		for i := range targets {
			targets[i] = setupMarkoutMissing(targets[i], "exchange_calendar_unsupported", now)
		}
		return targets
	}
	for i := range targets {
		targets[i].Calendar = string(market)
		var at time.Time
		var ok bool
		if targets[i].Horizon == rpc.SetupMarkoutHorizonT30 {
			at, ok = setupMarkoutRegularMinutesAfter(cal, market, base.FillAt, setupMarkoutRegularMinutes)
		} else {
			at, ok = setupMarkoutNextClose(cal, market, base.FillAt)
		}
		switch {
		case !ok:
			targets[i] = setupMarkoutMissing(targets[i], "exchange_calendar_unavailable", now)
		case !now.Before(at):
			targets[i].TargetAt = at.UTC()
			targets[i] = setupMarkoutMissing(targets[i], "scheduled_after_target", now)
		default:
			targets[i].TargetAt = at.UTC()
		}
	}
	return targets
}

// setupMarkoutMarket selects the regular-session calendar. Options use the US
// equity session (09:30-16:00 ET), the single-name listed-option session; the
// generic options calendar closes at 16:15 for index classes, when single-name
// books are already closed and a receipt would look current.
func setupMarkoutMarket(c rpc.ContractParams) (marketcal.Market, bool) {
	if c.SecType == "OPT" {
		if c.Currency != "USD" {
			return "", false
		}
		return marketcal.MarketUSEquity, true
	}
	return quoteSessionMarketForContract(c)
}

func setupMarkoutSide(ev orderJournalEvent) string {
	switch strings.ToUpper(strings.TrimSpace(ev.ExecSide)) {
	case "BOT", "BUY":
		return rpc.OrderActionBuy
	case "SLD", "SELL":
		return rpc.OrderActionSell
	}
	switch strings.ToUpper(strings.TrimSpace(ev.Action)) {
	case "BUY":
		return rpc.OrderActionBuy
	case "SELL":
		return rpc.OrderActionSell
	}
	return ""
}

// setupMarkoutFillTime prefers the broker execution time when it carries an
// explicit zone; otherwise the durable local receipt anchors the schedule and
// the source says so.
func setupMarkoutFillTime(ev orderJournalEvent) (time.Time, string) {
	receipt := ev.At.UTC()
	if at, ok := parseIBKRExecTime(ev.ExecTime); ok && (receipt.IsZero() || !at.After(receipt.Add(setupMarkoutExecTimeSkew))) {
		return at.UTC(), "broker_exec_time"
	}
	return receipt, "journal_receipt"
}

// parseIBKRExecTime accepts "YYYYMMDD HH:MM:SS Zone" and the UTC form
// "YYYYMMDD-HH:MM:SS". A bare local time without a zone is ambiguous and is
// rejected.
func parseIBKRExecTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if at, err := time.Parse("20060102-15:04:05", raw); err == nil {
		return at.UTC(), true
	}
	fields := strings.Fields(raw)
	if len(fields) != 3 {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(fields[2])
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation("20060102 15:04:05", fields[0]+" "+fields[1], loc)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

func setupMarkoutTradingWindows(s marketcal.Session) []marketcal.Window {
	if s.State != marketcal.StateRegular && s.State != marketcal.StateEarlyClose {
		return nil
	}
	if len(s.Windows) > 0 {
		return s.Windows
	}
	if s.Open.IsZero() || !s.Close.After(s.Open) {
		return nil
	}
	return []marketcal.Window{{Open: s.Open, Close: s.Close}}
}

// setupMarkoutNextDay returns local midnight of the date after session s.
func setupMarkoutNextDay(s marketcal.Session) (time.Time, bool) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation(time.DateOnly, s.Date, loc)
	if err != nil {
		return time.Time{}, false
	}
	return day.AddDate(0, 0, 1), true
}

// setupMarkoutRegularMinutesAfter counts only scheduled regular-session time
// from start, carrying the remainder across closes, breaks, weekends and
// holidays to the next open. A fill outside the session starts counting at the
// next open. Unknown calendar coverage fails.
func setupMarkoutRegularMinutesAfter(cal *marketcal.Calendar, market marketcal.Market, start time.Time, span time.Duration) (time.Time, bool) {
	if cal == nil || start.IsZero() || span <= 0 {
		return time.Time{}, false
	}
	at, remaining := start, span
	for range setupMarkoutSessionSearch {
		s, err := cal.SessionAt(market, at)
		if err != nil || s.State == marketcal.StateUnknown {
			return time.Time{}, false
		}
		for _, w := range setupMarkoutTradingWindows(s) {
			if !at.Before(w.Close) {
				continue
			}
			from := w.Open
			if at.After(from) {
				from = at
			}
			avail := w.Close.Sub(from)
			if avail >= remaining {
				return from.Add(remaining), true
			}
			remaining -= avail
			at = w.Close
		}
		next, ok := setupMarkoutNextDay(s)
		if !ok {
			return time.Time{}, false
		}
		at = next
	}
	return time.Time{}, false
}

// setupMarkoutNextClose returns the scheduled close of the first regular
// session that opens after the fill: the next session for a fill during or
// after a session, and that day's session for a fill before its open.
func setupMarkoutNextClose(cal *marketcal.Calendar, market marketcal.Market, fill time.Time) (time.Time, bool) {
	if cal == nil || fill.IsZero() {
		return time.Time{}, false
	}
	at := fill
	for range setupMarkoutSessionSearch {
		s, err := cal.SessionAt(market, at)
		if err != nil || s.State == marketcal.StateUnknown {
			return time.Time{}, false
		}
		if windows := setupMarkoutTradingWindows(s); len(windows) > 0 && windows[0].Open.After(fill) {
			return windows[len(windows)-1].Close, true
		}
		next, ok := setupMarkoutNextDay(s)
		if !ok {
			return time.Time{}, false
		}
		at = next
	}
	return time.Time{}, false
}

func (s *Server) setupMarkoutNow() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) setupMarkoutCalendar() *marketcal.Calendar {
	return marketcal.NewWithClock(s.setupMarkoutNow)
}

// scheduleSetupMarkoutFill persists both horizons for one journaled fill.
func (s *Server) scheduleSetupMarkoutFill(ctx context.Context, ev orderJournalEvent) {
	rt := s.setupMarkouts
	if rt == nil || !setupMarkoutEligibleFill(ev) {
		return
	}
	now := s.setupMarkoutNow()
	if since := rt.store.trackingSince(); !ev.At.IsZero() && ev.At.Before(since) {
		return
	}
	if err := rt.store.schedule(ctx, setupMarkoutTargetsForFill(ev, s.setupMarkoutCalendar(), now), now); err != nil {
		s.warnf("setup markout schedule for order %s: %v", ev.OrderRef, err)
	}
}

// rescanSetupMarkoutFills schedules every eligible fill journaled since
// tracking began that the schedule does not know yet. It closes the gap left
// by a crash between journal commit and scheduling or by queue overflow; a
// target already past is recorded missing, never captured late.
func (s *Server) rescanSetupMarkoutFills(ctx context.Context) {
	rt := s.setupMarkouts
	if rt == nil || s.orderJournal == nil {
		return
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		rt.rescan.Store(true)
		s.warnf("setup markout journal rescan: %v", err)
		return
	}
	since := rt.store.trackingSince()
	for _, ev := range events {
		if ctx.Err() != nil {
			return
		}
		if ev.At.Before(since) || !setupMarkoutEligibleFill(ev) {
			continue
		}
		s.scheduleSetupMarkoutFill(ctx, ev)
	}
}

// sweepSetupMarkouts resolves every pending target whose clock has passed
// without a capture. A later quote is never backdated to the target.
func (s *Server) sweepSetupMarkouts(ctx context.Context, now time.Time) {
	rt := s.setupMarkouts
	if rt == nil {
		return
	}
	var missed []rpc.SetupMarkoutTarget
	for _, t := range rt.store.pending() {
		if now.After(t.TargetAt) && !rt.isInFlight(t) {
			missed = append(missed, setupMarkoutMissing(t, "capture_window_missed", now))
		}
	}
	if len(missed) == 0 {
		return
	}
	if err := rt.store.resolve(ctx, missed); err != nil {
		s.warnf("setup markout sweep: %v", err)
	}
}

func (rt *setupMarkoutRuntime) isInFlight(t rpc.SetupMarkoutTarget) bool {
	rt.inFlightMu.Lock()
	defer rt.inFlightMu.Unlock()
	return rt.inFlight[setupMarkoutKey(t)]
}

// runSetupMarkoutLoop owns scheduling and resolution. It starts with a
// journal rescan so a restart records missed targets with their reason.
func (s *Server) runSetupMarkoutLoop(ctx context.Context) {
	rt := s.setupMarkouts
	if rt == nil {
		return
	}
	s.rescanSetupMarkoutFills(ctx)
	ticker := time.NewTicker(setupMarkoutSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			rt.wg.Wait()
			return
		case ev := <-rt.fills:
			s.scheduleSetupMarkoutFill(ctx, ev)
		case <-ticker.C:
			if rt.rescan.Swap(false) {
				s.rescanSetupMarkoutFills(ctx)
			}
			now := s.setupMarkoutNow()
			s.captureDueSetupMarkouts(ctx, now)
			s.sweepSetupMarkouts(ctx, now)
		}
	}
}
