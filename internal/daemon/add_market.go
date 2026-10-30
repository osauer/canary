package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func (s *Server) handleAddMarket(ctx context.Context, req *rpc.Request) (*rpc.AddMarketResult, error) {
	var p rpc.AddMarketParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	u := p.Underlying
	currencyValid := len(u.Currency) == 3 && strings.IndexFunc(u.Currency, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0
	if u.Symbol == "" || len(u.Symbol) > 32 || u.SecType != "STK" || !currencyValid || u.Exchange != "SMART" || u.ConID < 0 || u.Expiry != "" || u.Strike != 0 || u.Right != "" {
		return nil, errBadRequest("Add requires an exact SMART stock underlying and trading currency")
	}
	if p.Right != "" && p.Right != "C" && p.Right != "P" {
		return nil, errBadRequest("Choose right C or P")
	}
	side := s.addSide(p.Right)
	if u.Currency != "USD" && (p.Discover || p.Selected != nil || len(p.Refresh) > 0) {
		return nil, errBadRequest("The " + side.noun() + " picker supports standard USD " + side.noun() + "s")
	}
	if len(p.Refresh) > len(side.horizons) || p.Discover && len(p.Refresh) > 0 {
		return nil, errBadRequest(fmt.Sprintf("Choose discovery or at most %d exact %s refreshes", len(side.horizons), side.noun()))
	}
	seen := map[int]bool{}
	for _, item := range p.Refresh {
		if !slices.Contains(side.horizonDays(), item.Horizon) || seen[item.Horizon] || !validAddCallSelection(side, item.Contract, u.Symbol) {
			return nil, errBadRequest("Invalid " + side.noun() + " horizon or identity")
		}
		seen[item.Horizon] = true
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return nil, s.gatewayUnavailableError()
	}
	authority := s.setupQuoteAuthority(c, binding)
	if authority == nil {
		return nil, s.gatewayUnavailableError()
	}
	// One log line per discovery says where its time went.
	started, mark := time.Now(), time.Now()
	phase := map[string]time.Duration{}
	lap := func(name string) { phase[name], mark = time.Since(mark), time.Now() }
	exact, err := c.ResolveOrderContractForSession(ctx, binding, ibkr.Contract{ConID: u.ConID, Symbol: u.Symbol, SecType: "STK", Currency: u.Currency, Exchange: "SMART"}, 8*time.Second)
	lap("underlying")
	if err != nil {
		return nil, err
	}
	if exact.Contract.Symbol != u.Symbol || exact.Contract.SecType != "STK" || exact.Contract.Currency != u.Currency || u.ConID > 0 && exact.Contract.ConID != u.ConID {
		return nil, errBadRequest("underlying identity changed")
	}
	now := s.orderNow()
	session := addOptionsCalendar(now)
	class := &addClassSession{calendar: session, now: now}
	// Keep the exact underlying's shared read subscription alive while metadata
	// loads and TWS computes option models. The snapshot below consumes this same
	// cache; a private exact-session quote line would leave it waiting on a second
	// subscription. Option quote/model evidence remains independently session-bound.
	if session.mode != "unavailable" && (p.Discover || p.Selected != nil || len(p.Refresh) > 0) {
		_, release, err := s.subs.HoldContract(ctx, exact.Contract)
		if err != nil {
			return nil, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release(cleanup)
		}()
	}
	out := &rpc.AddMarketResult{Version: 1, Underlying: setupOptionContract(exact.Contract), OptionsSession: session.current, OptionsLastSession: session.last, OptionMode: session.mode, OptionCap: s.addOptionCap()}
	out.PriceIncrements = addPriceRules(ctx, c, binding, exact)
	lap("rules")
	var expiries []rpc.SetupOptionExpiry
	var listing map[string][]ibkr.ExpiryClassedStrikes
	if p.Discover && session.mode != "unavailable" {
		listing, err = c.FetchSetupOptionStrikes(ctx, binding, exact.Contract, 10*time.Second)
		if err != nil {
			return nil, err
		}
		expiries, _ = setupOptionExpiries(listing, time.Now())
	}
	lap("listing")
	// Read after metadata so callbacks received during those broker round trips
	// reach strike ranking instead of retaining an earlier empty snapshot.
	raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: out.Underlying, TimeoutMs: 3000})
	out.Quote, err = s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	lap("spot")
	if err != nil {
		return nil, err
	}
	if p.Selected != nil {
		requested := *p.Selected
		if !validAddCallSelection(side, requested, u.Symbol) {
			return nil, errBadRequest("choose an exact standard " + side.noun())
		}
		row := s.addCallMarket(ctx, side, c, binding, exact.Contract, requested.Expiry, requested.Strike, class)
		if row.Contract != nil && !sameAddCall(*row.Contract, requested) {
			return nil, errBadRequest("selected " + side.noun() + " identity changed")
		}
		out.Selected = &row
	}
	rows := make([]rpc.AddCallCandidate, 0, len(side.horizons))
	if len(p.Refresh) > 0 {
		rows = make([]rpc.AddCallCandidate, len(p.Refresh))
		addParallel(len(p.Refresh), addHorizonSlots, func(i int) {
			item := p.Refresh[i]
			row := s.addCallMarket(ctx, side, c, binding, exact.Contract, item.Contract.Expiry, item.Contract.Strike, class)
			if row.Contract != nil && !sameAddCall(*row.Contract, item.Contract) {
				row = rpc.AddCallCandidate{Unavailable: side.Noun() + " identity changed"}
			}
			row.Horizon = item.Horizon
			_, row.DTE = addHorizonExpiry([]rpc.SetupOptionExpiry{{Date: item.Contract.Expiry}}, item.Horizon, 8, 400, time.Now())
			row.DeltaBand = side.bandFor(&item.Contract).rpc()
			rows[i] = row
		})
	}
	if p.Discover {
		spot, _ := quoteCurrentPrice(out.Quote)
		// The stock's implied volatility aims capped horizons straight at
		// their target delta; the live option delta still decides.
		stockIV := 0.0
		if q := out.Quote; q != nil && q.IV != nil && positiveFinite(*q.IV) {
			stockIV = *q.IV
		}
		rows = make([]rpc.AddCallCandidate, len(side.horizons))
		timing := make([]string, len(side.horizons))
		addParallel(len(side.horizons), addHorizonSlots, func(i int) {
			h := side.horizons[i]
			target := h.Days
			date, dte := addHorizonExpiry(expiries, target, h.DTE[0], h.DTE[1], time.Now())
			row := rpc.AddCallCandidate{Horizon: target, DTE: dte, Unavailable: "No eligible " + side.noun()}
			if session.mode == "unavailable" {
				row.Unavailable = "Options calendar unavailable"
			}
			if date != "" && spot != nil && *spot > 0 {
				started := time.Now()
				near := func(ref float64) ([]ibkr.ResolvedOrderContract, error) {
					if calls := s.addNearestExpiryCalls(ctx, side, c, binding, exact.Contract, listing, date, ref); len(calls) >= 3 {
						return calls, nil
					}
					// Too few listed strikes resolved for this expiry: take the
					// broker's complete inventory, slower but exhaustive.
					return c.FetchSetupExpiryOptions(ctx, binding, exact.Contract, date, side.right, 30*time.Second)
				}
				quote := func(call ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
					return s.addResolvedCallForSession(ctx, side, c, binding, exact.Contract, call, class.of(call))
				}
				var spent string
				var err error
				row, spent, err = selectAddCallAimed(side, *spot, stockIV, dte, quote, near)
				timing[i] = fmt.Sprintf("%d:%s", target, spent)
				if err == nil {
					row.Horizon, row.DTE = target, dte
				} else {
					s.warnAddInventoryFailure(u.Symbol, date, target, time.Since(started), err)
					row.Unavailable = side.Noun() + " contracts unavailable"
				}
			}
			if dte > 0 {
				row.DeltaBand = side.bandForDTE(dte).rpc()
			}
			rows[i] = row
		})
		lap("horizons")
		// The default log keeps warnings only; a slow discovery is one.
		if time.Since(started) > 8*time.Second {
			s.warnf("add.slow symbol=%s underlying_ms=%d rules_ms=%d listing_ms=%d spot_ms=%d horizons_ms=%d %s total_ms=%d", u.Symbol, phase["underlying"].Milliseconds(), phase["rules"].Milliseconds(), phase["listing"].Milliseconds(), phase["spot"].Milliseconds(), phase["horizons"].Milliseconds(), strings.Join(timing, " "), time.Since(started).Milliseconds())
		}
	}
	if len(rows) > 0 {
		if side.right == "P" {
			out.Puts = rows
		} else {
			out.Calls = rows
		}
	}
	if side.right == "P" {
		// Discovery can take several broker round trips. Read remaining cover
		// after them so earlier capacity is not returned as current.
		out.PutCover = s.readAddPutCover(ctx, out.Underlying)
	}
	if !c.HistoricalSessionCurrent(binding) || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return nil, fmt.Errorf("broker session changed during Add discovery")
	}
	settled := class.result()
	out.OptionsSession, out.OptionsLastSession, out.OptionMode = settled.current, settled.last, settled.mode
	out.AsOf = time.Now()
	return out, nil
}

func (s *Server) warnAddInventoryFailure(symbol, expiry string, horizon int, elapsed time.Duration, err error) {
	s.warnf("add.inventory symbol=%s expiry=%s horizon=%d elapsed_ms=%d reason=%s", symbol, expiry, horizon, elapsed.Milliseconds(), addInventoryFailureReason(err))
}

// Inventory failures contain both typed errors and fixed local error strings.
// Only this closed vocabulary reaches the normal log, never arbitrary error or
// broker text. Keep unknown or changed formats unclassified rather than echoing.
func addInventoryFailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ibkr.ErrContractDetailsTimeout):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ibkr.ErrContractNoDefinition):
		return "no_definition"
	case errors.Is(err, ibkr.ErrIBKRUnavailable):
		return "session_unavailable"
	}
	if rejected, ok := errors.AsType[*ibkr.ContractDetailsRejection](err); ok && rejected.Code > 0 {
		return "broker_" + strconv.Itoa(rejected.Code)
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		switch cause.Error() {
		case "expiry call inventory contains a mismatched contract":
			return "identity_mismatch"
		case "expiry call inventory contains conflicting identities":
			return "identity_conflict"
		case "expiry call inventory exceeds 2048 responses":
			return "response_limit"
		case "broker session changed during expiry call discovery":
			return "session_changed"
		case "exact US stock session and expiry required":
			return "invalid_session_or_identity"
		case "rate limiter stopped":
			return "limiter_stopped"
		}
		// Ordinary contract-details rejections currently expose only this fixed
		// numeric format; the text-bearing typed rejection is bond-specific.
		if codeText, ok := strings.CutPrefix(cause.Error(), "contract details request failed (IBKR "); ok {
			if codeText, ok = strings.CutSuffix(codeText, ")"); ok {
				if code, err := strconv.Atoi(codeText); err == nil && code > 0 && strconv.Itoa(code) == codeText {
					return "broker_" + codeText
				}
			}
		}
	}
	return "read_failed"
}

// Quote only the nearest actual contracts for this expiry. The broker's
// separate SecDef expiry/strike sets must never consume these three slots.
func selectAddCall(side addSide, calls []ibkr.ResolvedOrderContract, spot float64, quote func(ibkr.ResolvedOrderContract) rpc.AddCallCandidate) rpc.AddCallCandidate {
	row, _ := selectAddCallNear(side, calls, spot, quote)
	return row
}

// selectAddCallNear quotes the three actual contracts nearest ref at once and
// keeps the best of them (pickAddCall). It also returns an implied
// volatility it observed, to aim a further round.
func selectAddCallNear(side addSide, calls []ibkr.ResolvedOrderContract, ref float64, quote func(ibkr.ResolvedOrderContract) rpc.AddCallCandidate) (rpc.AddCallCandidate, float64) {
	calls = slices.Clone(calls)
	slices.SortFunc(calls, func(a, b ibkr.ResolvedOrderContract) int {
		return compareDistance(a.Contract.Strike, b.Contract.Strike, ref)
	})
	candidates := make([]rpc.AddCallCandidate, min(3, len(calls)))
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Go(func() { candidates[i] = quote(calls[i]) })
	}
	wg.Wait()
	iv := 0.0
	for _, c := range candidates {
		if q := c.Quote; iv == 0 && q != nil && q.IV != nil && positiveFinite(*q.IV) {
			iv = *q.IV
		}
	}
	return pickAddCall(side, candidates...), iv
}

// pickAddCall keeps, in order: a live in-range option nearest its band's
// target, a closed-market option whose last delta was in range, then a
// closed-market option without a delta. Earlier candidates win ties.
func pickAddCall(side addSide, candidates ...rpc.AddCallCandidate) rpc.AddCallCandidate {
	var best, unqualified *rpc.AddCallCandidate
	closer := func(a, b *rpc.AddCallCandidate) bool {
		target := side.bandFor(a.Contract).Target
		return b == nil || math.Abs(side.size(*a.Quote.Delta)-target) < math.Abs(side.size(*b.Quote.Delta)-target)
	}
	for i := range candidates {
		c := &candidates[i]
		switch {
		case eligibleAddCall(side, *c, time.Now()):
			if best == nil || best.Planning != nil || closer(c, best) {
				best = c
			}
		case selectableAddPlanningCall(*c) && c.Planning.DeltaStatus == "in_range":
			if best == nil || best.Planning != nil && closer(c, best) {
				best = c
			}
		case selectableAddPlanningCall(*c) && unqualified == nil:
			unqualified = c
		}
	}
	if best == nil {
		best = unqualified
	}
	if best == nil {
		return rpc.AddCallCandidate{Unavailable: "No eligible " + side.noun()}
	}
	return *best
}

// selectAddCallAimed searches one expiry in at most two rounds. The first
// starts at spot, or for a capped range at the strike the stock's implied
// volatility puts at the target delta. Without a live in-range call, a
// second round aims with the volatility the options themselves showed, or
// repeats the first when no quote returned one (a missed quote window). The
// better call of the two rounds is kept, so a miss in one cannot empty the
// row. It also says what each round took, in milliseconds.
func selectAddCallAimed(side addSide, spot, stockIV float64, dte int, quote func(ibkr.ResolvedOrderContract) rpc.AddCallCandidate, near func(float64) ([]ibkr.ResolvedOrderContract, error)) (rpc.AddCallCandidate, string, error) {
	band := side.bandForDTE(dte)
	// The aim formula solves for a call's delta; a put of delta -d sits
	// where the call's delta is 1-d.
	aim := band.Target
	if side.right == "P" {
		aim = 1 - band.Target
	}
	notes := []string{}
	round := func(ref float64) (rpc.AddCallCandidate, float64, error) {
		started := time.Now()
		calls, err := near(ref)
		looked := time.Since(started)
		if err != nil {
			return rpc.AddCallCandidate{}, 0, err
		}
		started = time.Now()
		row, iv := selectAddCallNear(side, calls, ref, quote)
		notes = append(notes, fmt.Sprintf("resolve=%d,quote=%d", looked.Milliseconds(), time.Since(started).Milliseconds()))
		return row, iv, nil
	}
	ref := spot
	if band.Target < .5 && stockIV > 0 {
		ref = addAimStrike(spot, stockIV, dte, aim)
	}
	first, iv, err := round(ref)
	// A closed market has no live call; its planning row stands as found.
	if err != nil || eligibleAddCall(side, first, time.Now()) || selectableAddPlanningCall(first) {
		return first, strings.Join(notes, "+"), err
	}
	if iv > 0 {
		ref = addAimStrike(spot, iv, dte, aim)
	}
	second, _, err := round(ref)
	if err != nil {
		return first, strings.Join(notes, "+"), nil
	}
	return pickAddCall(side, first, second), strings.Join(notes, "+"), nil
}

// addDeltaBand is one horizon's delta range and the delta selection aims
// at, as the opportunity policy's [add] table sets them (addSide); a put's
// is written without its sign.
type addDeltaBand struct{ Min, Max, Target float64 }

func (b addDeltaBand) holds(delta float64) bool { return delta >= b.Min && delta <= b.Max }

func (b addDeltaBand) rpc() *rpc.AddDeltaBand {
	return &rpc.AddDeltaBand{Min: b.Min, Max: b.Max, Target: b.Target}
}

// addAimStrike estimates the strike whose call delta is the target, from
// Black-Scholes with zero rates: delta = N(d1).
func addAimStrike(spot, iv float64, dte int, delta float64) float64 {
	t := float64(max(dte, 1)) / 365
	d1 := math.Sqrt2 * math.Erfinv(2*delta-1)
	return spot * math.Exp(-d1*iv*math.Sqrt(t)+iv*iv*t/2)
}

func compareDistance(a, b, spot float64) int {
	da, db := math.Abs(a-spot), math.Abs(b-spot)
	if da < db || da == db && a < b {
		return -1
	}
	if da > db || a > b {
		return 1
	}
	return 0
}
func addHorizonExpiry(expiries []rpc.SetupOptionExpiry, target, minDTE, maxDTE int, now time.Time) (string, int) {
	loc, _ := time.LoadLocation("America/New_York")
	day := now.In(loc).Format("20060102")
	today, _ := time.Parse("20060102", day)
	best, dte, distance := "", 0, 10000
	for _, e := range expiries {
		date, err := time.Parse("20060102", e.Date)
		if err != nil {
			continue
		}
		n := int(date.Sub(today).Hours() / 24)
		delta := int(math.Abs(float64(n - target)))
		if n >= minDTE && n <= maxDTE && (delta < distance || delta == distance && e.Date < best) {
			best, dte, distance = e.Date, n, delta
		}
	}
	return best, dte
}
func addPriceRules(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, exact ibkr.ResolvedOrderContract) []rpc.SetupOptionPriceIncrement {
	rules, err := c.OrderPriceIncrements(ctx, binding, exact)
	if err != nil {
		return nil
	}
	out := make([]rpc.SetupOptionPriceIncrement, 0, len(rules))
	for _, r := range rules {
		out = append(out, rpc.SetupOptionPriceIncrement{LowEdge: r.LowEdge, Increment: r.Increment})
	}
	return out
}
func (s *Server) addCallMarket(ctx context.Context, side addSide, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under ibkr.Contract, expiry string, strike float64, class *addClassSession) rpc.AddCallCandidate {
	row := rpc.AddCallCandidate{Unavailable: "Exact " + side.noun() + " evidence unavailable"}
	if class.calendar.mode == "unavailable" {
		return rpc.AddCallCandidate{Unavailable: "Options calendar unavailable"}
	}
	parsed, err := time.Parse("20060102", expiry)
	if err != nil || parsed.Before(time.Now().Add(7*24*time.Hour)) || !positiveFinite(strike) {
		return row
	}
	requested := ibkr.Contract{Symbol: under.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: expiry, Strike: strike, Right: side.right, Multiplier: 100, TradingClass: under.Symbol}
	exact, err := c.ResolveOrderContractForSession(ctx, binding, requested, 5*time.Second)
	if err != nil || ibkr.ValidateSetupOptionResolution(exact, under, expiry, strike, side.right) != nil {
		return row
	}
	row = s.addResolvedCallForSession(ctx, side, c, binding, under, exact, class.of(exact))
	if row.Planning != nil && !selectableAddPlanningCall(row) {
		return rpc.AddCallCandidate{Unavailable: side.Noun() + " delta outside its range"}
	}
	return row
}

func (s *Server) addResolvedCallMarket(ctx context.Context, side addSide, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under ibkr.Contract, exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
	row := rpc.AddCallCandidate{Unavailable: "Exact " + side.noun() + " evidence unavailable"}
	call := exact.Contract
	parsed, err := time.Parse("20060102", call.Expiry)
	if err != nil || parsed.Before(time.Now().Add(7*24*time.Hour)) || !positiveFinite(call.Strike) || !c.SessionCurrent(binding) || ibkr.ValidateSetupOptionResolution(exact, under, call.Expiry, call.Strike, side.right) != nil {
		return row
	}
	contract := setupOptionContract(exact.Contract)
	contract.MinTick = exact.MinTick
	row.Contract = &contract
	row.PriceIncrements = addPriceRules(ctx, c, binding, exact)
	row.Quote = s.setupCallQuote(ctx, c, binding, contract)
	if eligibleAddCall(side, row, time.Now()) {
		row.Unavailable = ""
	}
	return row
}
func eligibleAddCall(side addSide, row rpc.AddCallCandidate, now time.Time) bool {
	q := row.Quote
	return row.Planning == nil && row.Contract != nil && len(row.PriceIncrements) > 0 && q != nil && q.Status == rpc.SetupQuoteQuoted && q.DataType == rpc.MarketDataLive && q.ModelDataType == rpc.MarketDataLive && q.Delta != nil && side.holds(row.Contract, *q.Delta) && q.IV != nil && *q.IV > 0 && !q.AsOf.IsZero() && !q.ModelAsOf.IsZero() && !q.AsOf.After(now.Add(time.Second)) && !q.ModelAsOf.After(now.Add(time.Second)) && now.Sub(q.AsOf) <= 15*time.Second && now.Sub(q.ModelAsOf) <= 15*time.Second
}

type addOptionsCalendarState struct {
	current rpc.MarketSession
	last    *rpc.MarketSession
	mode    string
}

func addOptionsCalendar(now time.Time) addOptionsCalendarState {
	completed, current, completedKnown := lastCompletedOptionsSessionWindow(now)
	out := addOptionsCalendarState{current: marketSessionToRPC(current), mode: "unavailable"}
	if current.Market != marketcal.MarketUSOptions || current.State == marketcal.StateUnknown {
		return out
	}
	out.mode = "planning"
	if current.IsOpen {
		out.mode = "live"
	}
	if completedKnown {
		last := marketSessionToRPC(completed)
		out.last = &last
	}
	return out
}

// addClassSession settles one Add read's options session from the first
// resolved call, so every row and the reported session share one class close.
type addClassSession struct {
	calendar addOptionsCalendarState
	now      time.Time
	once     sync.Once
	settled  *addOptionsCalendarState
}

func (a *addClassSession) of(call ibkr.ResolvedOrderContract) addOptionsCalendarState {
	a.once.Do(func() {
		windows, ok := call.SessionWindows()
		settled := addClassOptionsCalendar(a.calendar, windows, ok, a.now)
		a.settled = &settled
	})
	return *a.settled
}

// result is read after every row has finished; a read without a resolved
// call reports the calendar.
func (a *addClassSession) result() addOptionsCalendarState {
	if a.settled == nil {
		return a.calendar
	}
	return *a.settled
}

// addClassOptionsCalendar narrows the calendar's open options session to one
// option class's close. The calendar closes at the late 16:15 New York close
// of SPY-style classes, while single-stock options close with the stock
// market. The class's broker hours decide, never later than the calendar;
// without a broker window for the session, the stock market's close does.
func addClassOptionsCalendar(calendar addOptionsCalendarState, windows []ibkr.TradingWindow, hoursOK bool, now time.Time) addOptionsCalendarState {
	if calendar.mode != "live" {
		return calendar
	}
	session := calendar.current
	closeAt, found := time.Time{}, false
	for _, w := range windows {
		if hoursOK && w.Open.Before(session.Close) && w.Close.After(session.Open) && (!found || w.Close.After(closeAt)) {
			closeAt, found = w.Close, true
		}
	}
	if !found {
		closeAt = session.Close.Add(-15 * time.Minute)
		if stock, err := marketcal.NewWithClock(func() time.Time { return now }).SessionAt(marketcal.MarketUSEquity, now); err == nil && stock.Date == session.Date && !stock.Close.IsZero() && !stock.Close.After(session.Close) {
			closeAt = stock.Close
		}
	}
	if closeAt.After(session.Close) {
		return calendar
	}
	if now.Before(closeAt) {
		calendar.current = addSessionClosingAt(session, closeAt)
		return calendar
	}
	// Read the calendar as it stands after the latest class close: the session
	// is complete and reopens at the next one; only its close is the class's.
	closed := addOptionsCalendar(session.Close)
	if closed.mode != "planning" || closed.current.Date != session.Date || closed.last == nil || closed.last.Date != session.Date {
		return addOptionsCalendarState{current: calendar.current, mode: "unavailable"}
	}
	closed.current = addSessionClosingAt(closed.current, closeAt)
	last := addSessionClosingAt(*closed.last, closeAt)
	closed.last = &last
	return closed
}

func addSessionClosingAt(session rpc.MarketSession, closeAt time.Time) rpc.MarketSession {
	session.Close = closeAt
	windows := make([]rpc.MarketWindow, 0, len(session.Windows))
	for _, w := range session.Windows {
		if w.Open.Before(closeAt) {
			if w.Close.After(closeAt) {
				w.Close = closeAt
			}
			windows = append(windows, w)
		}
	}
	session.Windows = windows
	return session
}

// A missing delta retains a provisional near-ATM planning row. A known delta
// outside the owner's range is never a suggested contract, even while closed.
func selectableAddPlanningCall(row rpc.AddCallCandidate) bool {
	return row.Contract != nil && row.Planning != nil && (row.Planning.DeltaStatus == "in_range" || row.Planning.DeltaStatus == "unavailable")
}

func addPlanningObservation(side addSide, q *rpc.SetupOptionQuote, call *rpc.ContractParams) *rpc.AddCallPlanning {
	p := &rpc.AddCallPlanning{DeltaStatus: "unavailable"}
	if q == nil {
		return p
	}
	if q.Delta != nil {
		p.DeltaStatus = "out_of_range"
		if side.holds(call, *q.Delta) {
			p.DeltaStatus = "in_range"
		}
	}
	if q.Status == rpc.SetupQuoteQuoted && q.Bid != nil && q.Ask != nil && positiveFinite(*q.Bid) && positiveFinite(*q.Ask) && *q.Bid <= *q.Ask {
		p.Price, p.PriceSource, p.ObservedAt = new((*q.Bid+*q.Ask)/2), "broker_midpoint", q.AsOf
	}
	return p
}

func (s *Server) addResolvedCallForSession(ctx context.Context, side addSide, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under ibkr.Contract, exact ibkr.ResolvedOrderContract, session addOptionsCalendarState) rpc.AddCallCandidate {
	if session.mode == "live" {
		return s.addResolvedCallMarket(ctx, side, c, binding, under, exact)
	}
	call := exact.Contract
	date, err := time.Parse("20060102", call.Expiry)
	if session.mode != "planning" || err != nil || date.Before(time.Now().Add(7*24*time.Hour)) || !c.SessionCurrent(binding) || ibkr.ValidateSetupOptionResolution(exact, under, call.Expiry, call.Strike, side.right) != nil {
		return rpc.AddCallCandidate{Unavailable: "Exact " + side.noun() + " evidence unavailable"}
	}
	contract := setupOptionContract(call)
	contract.MinTick = exact.MinTick
	row := rpc.AddCallCandidate{Contract: &contract, PriceIncrements: addPriceRules(ctx, c, binding, exact)}
	row.Quote = s.addPlanningCallQuote(ctx, c, binding, contract)
	row.Planning = addPlanningObservation(side, row.Quote, row.Contract)
	if row.Planning.Price == nil {
		row.Unavailable = "Planning price unavailable"
	}
	return row
}

// The broker's frozen closing quote is the only closed-session price: it
// returned no option daily bars on 2026-10-22, and after a daemon restart its
// frozen quote took longer than 1.5 s, so this read gets the setup budget.
func (s *Server) addPlanningCallQuote(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, call rpc.ContractParams) *rpc.SetupOptionQuote {
	authority := s.setupQuoteAuthority(c, binding)
	if authority == nil {
		return &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}
	}
	snap, evidence, err := s.exactSessionContractQuoteModel(ctx, authority, call, setupQuoteBudget, true, true, true)
	if err != nil {
		return &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}
	}
	// The read stamps the snapshot with the server clock; judge it on that clock.
	quote, _ := setupOptionQuote(snap, s.orderNow())
	if quote.Status == rpc.SetupQuoteQuoted {
		quote.BidSize, quote.AskSize = evidence.Bid, evidence.Ask
		if m := evidence.Model; m != nil && m.Contract.ConID == call.ConID && !m.ReceivedAt.Before(m.RequestedAt) && !m.ReceivedAt.After(time.Now()) && time.Since(m.ReceivedAt) <= setupQuoteMaxAge {
			quote.IV, quote.Delta, quote.ModelAsOf, quote.ModelDataType = m.IV, m.Delta, m.ReceivedAt, marketDataTypeName(m.DataType)
		}
	}
	return &quote
}

const (
	// All four horizons run at once: with three quotes each, discovery holds at
	// most twelve option lines, well inside the broker's market-data allowance.
	addHorizonSlots = 4
	// Listed strikes resolved per expiry. SecDef pools strikes across expiries,
	// so some nearest listed strikes do not exist for this one; eight leaves
	// room for those and still yields the three quote candidates.
	addResolveWidth = 8
	// Batches before the complete expiry inventory becomes the fallback.
	addResolveBatches = 4
)

func addParallel(n, slots int, read func(int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, slots)
	for i := range n {
		wg.Go(func() { sem <- struct{}{}; defer func() { <-sem }(); read(i) })
	}
	wg.Wait()
}

// addNearestExpiryCalls resolves the listed strikes nearest spot as fully
// specified contracts, as TWS does, instead of waiting for the broker to list
// every call in the expiry. Only broker-resolved contracts are returned.
func (s *Server) addNearestExpiryCalls(ctx context.Context, side addSide, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under ibkr.Contract, listing map[string][]ibkr.ExpiryClassedStrikes, expiry string, spot float64) []ibkr.ResolvedOrderContract {
	listed, err := setupListedStrikes(listing, under.Symbol, expiry)
	if err != nil {
		return nil
	}
	return nearestResolvedCalls(listed, spot, func(k float64) (ibkr.ResolvedOrderContract, error) {
		requested := ibkr.Contract{Symbol: under.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: expiry, Strike: k, Right: side.right, Multiplier: 100, TradingClass: under.Symbol}
		exact, err := c.ResolveOrderContractForSession(ctx, binding, requested, 5*time.Second)
		if err == nil {
			err = ibkr.ValidateSetupOptionResolution(exact, under, expiry, k, side.right)
		}
		return exact, err
	})
}

// nearestResolvedCalls resolves listed strikes nearest spot in parallel
// batches of addResolveWidth and returns those that exist, nearest first. A
// phantom strike fails its own lookup and never takes a quote slot. SecDef
// mixes every expiry's strike spacing (1, 2.5 and 5 USD for one stock), so a batch
// can hold fewer than three real strikes; the next batch then continues
// outward, up to addResolveBatches.
func nearestResolvedCalls(listed []float64, spot float64, resolve func(float64) (ibkr.ResolvedOrderContract, error)) []ibkr.ResolvedOrderContract {
	strikes := slices.Clone(listed)
	slices.SortFunc(strikes, func(a, b float64) int { return compareDistance(a, b, spot) })
	out := []ibkr.ResolvedOrderContract{}
	for start := 0; start < len(strikes) && start < addResolveWidth*addResolveBatches && len(out) < 3; start += addResolveWidth {
		batch := strikes[start:min(start+addResolveWidth, len(strikes))]
		found := make([]*ibkr.ResolvedOrderContract, len(batch))
		var wg sync.WaitGroup
		for i, k := range batch {
			wg.Go(func() {
				if exact, err := resolve(k); err == nil {
					found[i] = &exact
				}
			})
		}
		wg.Wait()
		for _, r := range found {
			if r != nil {
				out = append(out, *r)
			}
		}
	}
	return out
}
func validAddCallSelection(side addSide, c rpc.ContractParams, symbol string) bool {
	return c.Symbol == symbol && c.SecType == "OPT" && c.Right == side.right && c.Multiplier == 100 && c.ConID > 0 && c.Currency == "USD" && c.Exchange == "SMART" && c.TradingClass != "" && positiveFinite(c.Strike)
}
func sameAddCall(a, b rpc.ContractParams) bool {
	return a.ConID == b.ConID && a.Symbol == b.Symbol && a.SecType == b.SecType && a.Expiry == b.Expiry && a.Strike == b.Strike && a.Right == b.Right && a.TradingClass == b.TradingClass && a.Multiplier == b.Multiplier && a.Currency == b.Currency && a.Exchange == b.Exchange
}
