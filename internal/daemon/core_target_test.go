package daemon

import (
	"context"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The core sleeve's target (phase 1, read-only), witnessed on synthetic
// books: the mandate test server, its seeded histories, a synthetic IBKR
// answer for the fund's ISIN and a synthetic series on the listing
// exchange's calendar (Xetra-shaped unless a test says London).
// mandateTestNow is Wednesday 14 October 2026, 17:00 Berlin: the latest
// decision is 30 September, the next 30 October.

const coreTestConID = 9030

// testCoreConstitution names the fund by ISIN and currency only, as the
// owner's decision of 2026-10-18 has the policy do.
func testCoreConstitution() *risk.Constitution {
	c := testMandateConstitution()
	c.Mandate.Core = &risk.ConstitutionMandateCore{Rule: core.RuleID, ISIN: core.ISIN, Currency: core.Currency,
		ScalePct: new(100.0), BandPct: new(10.0), MinOrderBase: new(2500.0), MaxValuePct: new(40.0),
		// Live in the register, so the core reserves its allocation and
		// sizes its buys inside it; a live entry needs its kill bound
		// written (2026-10-19).
		SpecHash: core.SpecHash, Stage: risk.StageLive, KillDrawdownK: new(0.35)}
	c.Mandate.IndexGapShockPct = new(12.0) // the owner's D4
	c.Capital.BaseCurrency = "EUR"
	return c
}

// coreTestListing is IBKR's synthetic answer for the ISIN: one EUR line on
// Xetra, found by ISIN.
func coreTestListing() ibkrlib.Listing {
	return ibkrlib.Listing{ConID: coreTestConID, Symbol: core.Symbol, SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: core.Currency, TradingClass: core.Symbol,
		ISIN: core.ISIN, ValidExchanges: "SMART,IBIS", Forms: []string{"STK by ISIN on SMART"}}
}

// coreTestLondonListing is a synthetic London EUR line of the fund under
// another symbol and trading class.
func coreTestLondonListing() ibkrlib.Listing {
	return ibkrlib.Listing{ConID: 9031, Symbol: "IXSE", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSEETF", Currency: core.Currency, TradingClass: "EXXT",
		ValidExchanges: "SMART,LSEETF", Forms: []string{"STK by ISIN on SMART", "STK by ISIN, no exchange"}}
}

// coreAnswer makes IBKR answer the given listings and resolves the core
// again, as the background lane would.
func coreAnswer(t *testing.T, srv *Server, listings ...ibkrlib.Listing) *coreFakeListings {
	t.Helper()
	fake := &coreFakeListings{listings: listings}
	if len(listings) == 0 {
		fake.err = ibkrlib.ErrNoListing
	}
	srv.coreListingsForTest = fake.lookup
	srv.coreTargets.mu.Lock()
	srv.coreTargets.pins = nil
	srv.coreTargets.mu.Unlock()
	srv.coreResolve(t.Context(), srv.riskPolicies.snapshot().policy)
	return fake
}

// coreTestDates are 320 sessions to the last completed Xetra session:
// Xetra's own calendar where it is embedded, weekdays before.
func coreTestDates(t *testing.T) []string {
	t.Helper()
	return coreTestDatesOn(t, marketcal.MarketDEXetra)
}

// coreTestDatesOn are 320 sessions to the last completed session of a
// market: its embedded calendar from 2026, weekdays before.
func coreTestDatesOn(t *testing.T, market marketcal.Market) []string {
	t.Helper()
	last, _, ok := mandateLastSessionsIn(mandateTestNow, market)
	if !ok {
		t.Fatal("no completed session")
	}
	cal := marketcal.NewWithClock(func() time.Time { return mandateTestNow })
	var out []string
	for day := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC); day.Format(time.DateOnly) <= last; day = day.AddDate(0, 0, 1) {
		d := day.Format(time.DateOnly)
		if d < "2026-01-01" {
			if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
				out = append(out, d)
			}
			continue
		}
		res, err := cal.Query(marketcal.Query{Market: market, Date: d, Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		if res.Session.State == marketcal.StateRegular || res.Session.State == marketcal.StateEarlyClose {
			out = append(out, d)
		}
	}
	return out[len(out)-mandateHistoryKeep:]
}

// coreTestSeries is a seeded walk with an upward drift (the trend is on at
// the 30 September decision) and about 12% volatility (the 15% floor binds).
func coreTestSeries(t *testing.T, drift float64) *mandateHistorySeries {
	t.Helper()
	return coreTestSeriesOn(t, coreTestListing(), marketcal.MarketDEXetra, coreTestDates(t), drift)
}

// coreTestSeriesOn is the same walk for a listing, read for a market's
// calendar on the given dates.
func coreTestSeriesOn(t *testing.T, l ibkrlib.Listing, market marketcal.Market, dates []string, drift float64) *mandateHistorySeries {
	t.Helper()
	r := rand.New(rand.NewPCG(31, 32))
	s := &mandateHistorySeries{Version: mandateHistoryDocVer, Key: coreSeriesKey(l.ConID), Dates: dates, FetchedAt: mandateTestNow.Add(-time.Hour), Source: "test", Market: market,
		Contract: rpc.ContractParams{ConID: l.ConID, Symbol: l.Symbol, SecType: "STK", Currency: l.Currency, Exchange: l.PrimaryExch, PrimaryExch: l.PrimaryExch}}
	price := 80.0
	for range dates {
		s.Closes = append(s.Closes, math.Round(price*100)/100)
		price *= math.Exp(drift/252 + 0.12/math.Sqrt(252)*r.NormFloat64())
	}
	return s
}

// coreTestServer is the mandate test server with [mandate.core], a
// database for the frozen months, the book's histories, IBKR's answer for
// the ISIN (one Xetra line), resolved, and the core's series.
func coreTestServer(t *testing.T, c *risk.Constitution, pos *rpc.PositionsResult, drift float64) *Server {
	t.Helper()
	srv, _ := mandateTestServer(t, c)
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv.coreStore = store
	seedMandateHistory(t, srv, 0)
	if err := srv.saveMandateSeries(t.Context(), coreTestSeries(t, drift)); err != nil {
		t.Fatal(err)
	}
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: mandateTestNow}, srv.currentBrokerStateScope(), nil
	}
	coreAnswer(t, srv, coreTestListing())
	return srv
}

func coreEvaluate(t *testing.T, srv *Server) *rpc.CoreTargetResult {
	t.Helper()
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	return srv.evaluateCoreTarget(t.Context(), srv.nowUTC())
}

func withIBCF(pos *rpc.PositionsResult, shares, mark float64) *rpc.PositionsResult {
	out := *pos
	out.Stocks = append(slices.Clone(pos.Stocks), rpc.PositionView{Symbol: core.Symbol, SecType: rpc.SecTypeStock, ConID: coreTestConID, Currency: core.Currency, PrimaryExch: "IBIS", Quantity: shares, Multiplier: 1, Mark: mark})
	return &out
}

// With the owner's index gap (D4: 12%) the core's first buy is the whole
// rule target: the sleeve's 6% share over the 15% floor is 40% of the
// budget base, capped there, and both the budget and the gap cap keep
// their margin. The result is read-only and names the rule and the spec.
func TestCoreTargetFirstBuyAtTheRuleTarget(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	res := coreEvaluate(t, srv)
	if res.Status != rpc.CoreStatusReady || len(res.Reasons) != 0 {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	if res.ReadOnly || res.Rule != core.RuleID || res.SpecHash != core.SpecHash || res.PolicyRule != core.RuleID || res.NextDecision != "2026-10-30" {
		t.Fatalf("header %+v", res)
	}
	d := res.Decision
	if d == nil || d.Decision != "2026-09-30" || d.Status != core.StatusOK || !d.TrendOn || d.Binding != core.BindingFloor || d.FrozenAt.IsZero() || len(d.MonthEnds) != core.TrendMonths {
		t.Fatalf("decision %+v", d)
	}
	sz := res.Sizing
	base := 230000.0 // 300,000 equity − 70,000 tax reserve, at full room
	if sz == nil || math.Abs(sz.Current.SleeveBudget-0.06*base) > 1e-6 || math.Abs(sz.Current.Notional-0.4*base) > 1e-6 {
		t.Fatalf("sizing %+v", sz)
	}
	want := int(math.Floor(0.4 * base / d.Close))
	if sz.Target != want || sz.Frozen.TargetShares != want || sz.Holdable == nil || *sz.Holdable != want {
		t.Fatalf("target %d frozen %d holdable %v, want %d", sz.Target, sz.Frozen.TargetShares, sz.Holdable, want)
	}
	a := res.Action
	if a == nil || a.Kind != core.ActionBuy || a.Quantity != want || a.HeadroomCut || !strings.Contains(a.Why, "first buy") {
		t.Fatalf("action %+v", a)
	}
	if res.Holding == nil || res.Holding.Shares != 0 || res.Holding.PriceSource != "close" {
		t.Fatalf("holding %+v", res.Holding)
	}
	// The core's own series never enters the book's verdict, so it can never
	// make that verdict stale (review S7).
	if book := srv.evaluateMandate(t.Context(), false); slices.ContainsFunc(book.Evidence.Series, func(e rpc.MandateSeriesEvidence) bool { return strings.HasPrefix(e.Key, coreSeriesKeyPrefix) }) {
		t.Fatalf("the book reads the core series: %+v", book.Evidence.Series)
	}
	if res.Evidence == nil || res.Evidence.SeriesKey != coreSeriesKey(coreTestConID) || res.Evidence.Stale || res.Evidence.Calendar != "de_xetra" {
		t.Fatalf("evidence %+v", res.Evidence)
	}
	if in := res.Instrument; in == nil || !in.Resolved || in.ConID != coreTestConID || in.Exchange != "IBIS" || in.Calendar != "de_xetra" || in.CalendarLabel != "Xetra" {
		t.Fatalf("instrument %+v", res.Instrument)
	}
}

// Review B1: without the index gap the core at its target would breach the
// single-name gap cap and the forced reduction would sell it back, so the
// core is not ready until index_gap_shock_pct is written; it holds and
// says why.
func TestCoreTargetHoldsWithoutTheIndexGap(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.IndexGapShockPct = nil
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	res := coreEvaluate(t, srv)
	if res.Status != rpc.CoreStatusHold || !strings.Contains(strings.Join(res.Reasons, " "), "mandate.index_gap_shock_pct") || res.Action.Kind != core.ActionHold || res.Sizing != nil {
		t.Fatalf("status %s reasons %v action %+v sizing %+v", res.Status, res.Reasons, res.Action, res.Sizing)
	}
	if !slices.Contains(res.Policy.Unapproved, "mandate.index_gap_shock_pct") {
		t.Fatalf("policy view %+v", res.Policy)
	}
}

// Where the core's gap binds before its target (an index gap of 24%, so
// the target's gap loses more than 95% of the cap), the buy is cut to the
// largest holding that keeps the core's gap within 95% of its cap and the
// book within 95% of the budget, and the mandate gate admits it.
func TestCoreTargetBuyCutToTheGapCapMargin(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.IndexGapShockPct = new(24.0)
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	res := coreEvaluate(t, srv)
	if res.Status != rpc.CoreStatusReady {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	a := res.Action
	if a.Kind != core.ActionBuy || !a.HeadroomCut || a.Quantity >= res.Sizing.Target {
		t.Fatalf("action %+v target %d", a, res.Sizing.Target)
	}
	price := res.Holding.Price
	_, kept, _, _ := srv.mandateVerdicts.current()
	capBase := mandate.GapCapBase(kept.Policy, kept.Capital)
	if loss := float64(a.Quantity) * price * 0.24; loss > core.HeadroomFactor*capBase+1e-6 {
		t.Fatalf("the cut buy's gap loses %.0f, over 95%% of the cap %.0f", loss, capBase)
	}
	if loss := float64(a.Quantity+1) * price * 0.24; loss <= core.HeadroomFactor*capBase {
		t.Fatalf("one share more still fits: %.0f <= %.0f", loss, core.HeadroomFactor*capBase)
	}
	// The cut buy passes the mandate gate the order would meet.
	draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: a.Quantity, LimitPrice: price, Contract: rpc.ContractParams{ConID: coreTestConID, Symbol: core.Symbol, SecType: "STK", Currency: core.Currency, PrimaryExch: "IBIS"}}
	if blocker := srv.mandateBudgetGate(draft, mandateOpen); blocker != nil {
		t.Fatalf("the headroom-cut buy is refused by the gate: %+v", blocker)
	}
}

// Property (design acceptance test 20): on synthetic books, a core buy
// sized under the headroom never by itself yields a verdict over the
// budget or a gap-cap breach.
func TestCoreHeadroomNeverBreaksTheBudget(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for i := range 12 {
		c := testCoreConstitution()
		if i%2 == 0 {
			c.Mandate.IndexGapShockPct = new(24.0) // the gap binds before the target
		}
		srv := coreTestServer(t, c, mandateTestPositions(float64(50+r.IntN(400))), 0.25)
		res := coreEvaluate(t, srv)
		if res.Status != rpc.CoreStatusReady || res.Action.Kind != core.ActionBuy && res.Action.Kind != core.ActionWait {
			t.Fatalf("book %d: %s %v %+v", i, res.Status, res.Reasons, res.Action)
		}
		_, kept, _, _ := srv.mandateVerdicts.current()
		leg := mandate.Leg{ID: "con:9030", Symbol: core.Symbol, Underlying: core.Symbol, Kind: mandate.KindStock, Quantity: float64(res.Action.Quantity), Multiplier: 1, Currency: core.Currency, Spot: res.Holding.Price}
		with := mandate.Evaluate(mandate.WithLeg(*kept, leg))
		if with.Status != mandate.StatusWithin || with.Risk.SDAnnualBase > core.HeadroomFactor*with.Capital.BudgetAnnualSD+1e-6 {
			t.Fatalf("book %d: the core buy of %d leaves the book %s at %.0f of %.0f", i, res.Action.Quantity, with.Status, with.Risk.SDAnnualBase, with.Capital.BudgetAnnualSD)
		}
	}
}

// A month is frozen at its first clean evaluation and survives a restart;
// closes that later read differently hold the month with
// core_target_drift; a higher scale written mid-month never raises the
// frozen target, a lower one lowers it at once.
func TestCoreTargetFrozenMonthDriftAndCeiling(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	first := coreEvaluate(t, srv)
	if first.Status != rpc.CoreStatusReady {
		t.Fatalf("first: %s %v", first.Status, first.Reasons)
	}
	frozenAt, target := first.Decision.FrozenAt, first.Sizing.Target
	// A restart: the frozen month is read back from the database, once the
	// background lane has resolved the contract again.
	srv.coreTargets = coreTargetStore{}
	srv.now = func() time.Time { return mandateTestNow.Add(time.Hour) }
	coreAnswer(t, srv, coreTestListing())
	again := coreEvaluate(t, srv)
	if !again.Decision.FrozenAt.Equal(frozenAt) || again.Sizing.Target != target {
		t.Fatalf("after a restart: frozen %v target %d, want %v %d", again.Decision.FrozenAt, again.Sizing.Target, frozenAt, target)
	}
	// Half the scale lowers the target at once; double the room would not
	// raise it (the frozen size is the ceiling).
	c.Mandate.Core.ScalePct = new(50.0)
	if half := coreEvaluate(t, srv); half.Sizing.Target != half.Sizing.Current.TargetShares || half.Sizing.Target >= target {
		t.Fatalf("half scale: %+v", half.Sizing)
	}
	c.Mandate.Core.ScalePct = new(100.0)
	// A close inside the window reads differently: the month holds.
	series := srv.mandateHistorySeries(t.Context(), coreSeriesKey(coreTestConID))
	edited := *series
	edited.Closes = slices.Clone(series.Closes)
	i := slices.Index(edited.Dates, "2026-09-15")
	edited.Closes[i] *= 1.01
	if err := srv.saveMandateSeries(t.Context(), &edited); err != nil {
		t.Fatal(err)
	}
	drift := coreEvaluate(t, srv)
	if drift.Status != rpc.CoreStatusHold || !strings.Contains(strings.Join(drift.Reasons, " "), "core_target_drift") || !drift.Decision.Drift || drift.Action.Kind != core.ActionHold {
		t.Fatalf("drift: %s %v %+v", drift.Status, drift.Reasons, drift.Decision)
	}
}

// Trend off with a holding sells every share, the band and the minimum
// notwithstanding; a mandate verdict over budget leaves the core's buy
// sized inside its own allocation (owner, 2026-10-18 15:31 CEST; until
// 2026-10-19 it blocked the buy) and sales open; a holding inside the band
// does nothing.
func TestCoreTargetTrendOffSellsAllAndOverBudgetBuysInsideTheAllocation(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, withIBCF(mandateTestPositions(100), 10, 60), -0.6)
	res := coreEvaluate(t, srv)
	if res.Status != rpc.CoreStatusReady || res.Decision.TrendOn || res.Action.Kind != core.ActionSellAll || res.Action.Quantity != 10 || res.Holding.PriceSource != "mark" {
		t.Fatalf("trend off: %s %v decision %+v action %+v", res.Status, res.Reasons, res.Decision, res.Action)
	}

	srv = coreTestServer(t, c, withIBCF(mandateTestPositions(3000), 100, 100), 0.25)
	res = coreEvaluate(t, srv)
	if res.Mandate == nil || res.Mandate.Status == mandate.StatusWithin || res.Action.Kind != core.ActionBuy || !res.Action.HeadroomCut || res.Sizing.Holdable == nil || strings.Contains(res.Action.Why, "mandate verdict") {
		t.Fatalf("over budget, the core inside its allocation: %+v %+v %+v", res.Mandate, res.Action, res.Sizing)
	}

	srv = coreTestServer(t, c, mandateTestPositions(100), 0.25)
	target := coreEvaluate(t, srv).Sizing.Target
	srv = coreTestServer(t, c, withIBCF(mandateTestPositions(100), float64(target-target/20), 100), 0.25)
	if res := coreEvaluate(t, srv); res.Action.Kind != core.ActionNone || !res.Action.InsideBand {
		t.Fatalf("inside the band: %+v", res.Action)
	}
}

// Every missing or suspect input holds and names itself: unwritten keys,
// another rule, a missing or stale series, the decision day's close not
// in yet, a contract that is not the pinned one, a second line of the
// fund, and a base currency other than EUR.
func TestCoreTargetHolds(t *testing.T) {
	cases := []struct {
		name string
		edit func(*risk.Constitution, *Server)
		want string
	}{
		{"unwritten", func(c *risk.Constitution, _ *Server) { c.Mandate.Core.BandPct, c.Mandate.Sleeves.CoreVolPct = nil, nil }, "mandate.core.band_pct, mandate.sleeves.core_vol_pct"},
		{"another rule", func(c *risk.Constitution, _ *Server) { c.Mandate.Core.Rule = "core-ibcf-v2" }, `rule "core-ibcf-v2"`},
		{"another ISIN", func(c *risk.Constitution, _ *Server) { c.Mandate.Core.ISIN = "IE00B5BMR087" }, "compiled for IE00B3ZW0K18"},
		{"base currency", func(c *risk.Constitution, _ *Server) { c.Capital.BaseCurrency = "USD" }, "base currency USD is not EUR"},
		{"no series", func(c *risk.Constitution, s *Server) {
			// The owner pins another line IBKR lists: no series is kept for it yet.
			c.Mandate.Core.ConID = new(9031)
			other := coreTestListing()
			other.ConID = 9031
			coreAnswer(t, s, coreTestListing(), other)
		}, "no IBCF daily history yet; the desk reads it on IBIS"},
		{"not resolved", func(_ *risk.Constitution, s *Server) { s.coreTargets.pins = nil }, "the core's contract is not resolved yet"},
		{"stale series", func(_ *risk.Constitution, s *Server) {
			series := *s.mandateHistorySeries(context.Background(), coreSeriesKey(coreTestConID))
			series.Dates, series.Closes = series.Dates[:len(series.Dates)-3], series.Closes[:len(series.Closes)-3]
			_ = s.saveMandateSeries(context.Background(), &series)
		}, "stale"},
		{"another contract", func(_ *risk.Constitution, s *Server) {
			series := *s.mandateHistorySeries(context.Background(), coreSeriesKey(coreTestConID))
			series.Contract.ConID = 9999
			_ = s.saveMandateSeries(context.Background(), &series)
		}, "is not the core's contract 9030"},
		{"second line", func(_ *risk.Constitution, s *Server) {
			pos := withIBCF(mandateTestPositions(100), 5, 100)
			pos.Stocks[len(pos.Stocks)-1].ConID = 9032
			s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
		}, "a second line of the fund (IBCF) is held under con:9032"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testCoreConstitution()
			srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
			tc.edit(c, srv)
			res := coreEvaluate(t, srv)
			if res.Status != rpc.CoreStatusHold || !strings.Contains(strings.Join(res.Reasons, " | "), tc.want) || res.Action == nil || res.Action.Kind != core.ActionHold {
				t.Fatalf("status %s reasons %v action %+v, want a hold naming %q", res.Status, res.Reasons, res.Action, tc.want)
			}
		})
	}
	// The decision day's close is not in before 18:00 Berlin on the day
	// (30 October 2026 is on CET, UTC+1).
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	if latest, _, _ := coreDecisionDays(time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC), marketcal.MarketDEXetra); latest != "2026-09-30" {
		t.Fatalf("before 18:00 on 30 October the latest decision is %s", latest)
	}
	if latest, next, _ := coreDecisionDays(time.Date(2026, 10, 30, 16, 59, 0, 0, time.UTC), marketcal.MarketDEXetra); latest != "2026-09-30" || next != "2026-10-30" {
		t.Fatalf("at 17:59 on 30 October: %s next %s", latest, next)
	}
	if latest, next, _ := coreDecisionDays(time.Date(2026, 10, 30, 17, 30, 0, 0, time.UTC), marketcal.MarketDEXetra); latest != "2026-10-30" || next != "2026-11-30" {
		t.Fatalf("after 18:00 on 30 October: %s next %s", latest, next)
	}
	srv.now = func() time.Time { return time.Date(2026, 10, 30, 17, 30, 0, 0, time.UTC) }
	if res := coreEvaluate(t, srv); res.Status != rpc.CoreStatusHold {
		t.Fatalf("the series ends before the decision day: %s %v", res.Status, res.Reasons)
	}
}

// Without [mandate.core] nothing changes: the target reads inactive, the
// background lane reads no core series, the book's needs are unchanged.
func TestCoreTargetAbsentChangesNothing(t *testing.T) {
	c := testMandateConstitution()
	srv, _ := mandateTestServer(t, c)
	fake := &coreFakeListings{listings: []ibkrlib.Listing{coreTestListing()}}
	srv.coreListingsForTest = fake.lookup
	if srv.coreResolve(t.Context(), c) != nil || len(fake.asked) != 0 || srv.registerFundLines(t.Context()) != nil || srv.coreContractIDs(t.Context()) != nil {
		t.Fatal("IBKR is asked for a core contract without [mandate.core]")
	}
	res, err := srv.handleCoreTarget(t.Context(), &rpc.Request{})
	if err != nil || res.Active || res.Status != rpc.CoreStatusInactive || !strings.Contains(res.Message, "no [mandate.core]") || !res.ReadOnly {
		t.Fatalf("inactive: %+v %v", res, err)
	}
	srv.refreshCoreTarget(t.Context())
	if len(srv.coreTargets.frozen) != 0 {
		t.Fatal("an inactive core froze a month")
	}
	// The core series is judged fresh on the calendar its read named, the
	// book's on the US one.
	if mandateSeriesMarket(&mandateHistorySeries{Key: coreSeriesKey(1), Market: marketcal.MarketUKLSE}) != marketcal.MarketUKLSE || mandateSeriesMarket(&mandateHistorySeries{Key: "u:SPY"}) != marketcal.MarketUSEquity {
		t.Fatal("series markets")
	}
	// A half-written table names its keys in the core only, never in the
	// constitution's approval list.
	c.Mandate.Core = &risk.ConstitutionMandateCore{}
	if all := strings.Join(c.UnapprovedKeys(), " "); strings.Contains(all, "mandate.core") {
		t.Fatalf("core keys reach the constitution's approval list: %s", all)
	}
}
