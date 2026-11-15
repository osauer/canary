package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/register"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The strategy evaluator (internal-docs/design/strategy-evaluator.md),
// witnessed on a second register entry: a synthetic trend fund on the
// parameterised rule, in the overlay, on a London line, in shadow beside
// the core. No market data and no account figure.

const (
	trendFundID    = "trend-fund"
	trendFundISIN  = "ZZ0000000001"
	trendFundConID = 9040
	trendFundHash  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// testTrendFundConstitution is the core's constitution with a complete
// [mandate.strategies.trend-fund] entry on timed-holding-v1.
func testTrendFundConstitution() *risk.Constitution {
	c := testCoreConstitution()
	c.Mandate.Strategies = map[string]*risk.ConstitutionMandateStrategy{trendFundID: {
		Rule: register.TimedHoldingRuleID, SpecHash: trendFundHash, Sleeve: risk.OverlaySleeve, Stage: risk.StageShadow, ScalePct: new(25.0),
		ISIN: trendFundISIN, Currency: "EUR", Signal: core.TrendSMA10.ID, BandPct: new(10.0), MinOrderBase: new(2500.0), MaxValuePct: new(40.0)}}
	return c
}

// trendFundListing is IBKR's synthetic answer for the fund's ISIN: one EUR
// line on London.
func trendFundListing() ibkrlib.Listing {
	return ibkrlib.Listing{ConID: trendFundConID, Symbol: "SYNF", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSEETF", Currency: "EUR", TradingClass: "SYNF",
		ISIN: trendFundISIN, ValidExchanges: "SMART,LSEETF", Forms: []string{"STK by ISIN on SMART"}}
}

// byISINListings answers each request with the listings under its ISIN.
type byISINListings struct {
	listings []ibkrlib.Listing
	asked    []ibkrlib.ListingRequest
}

func (f *byISINListings) lookup(_ context.Context, r ibkrlib.ListingRequest) ([]ibkrlib.Listing, *ibkrlib.BondLookupTrace, error) {
	f.asked = append(f.asked, r)
	var out []ibkrlib.Listing
	for _, l := range f.listings {
		if l.ISIN == r.ISIN {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return nil, nil, ibkrlib.ErrNoListing
	}
	return out, nil, nil
}

// trendFundServer is the core test server with the trend fund entry, IBKR's
// answer for both ISINs and the fund's series on the London calendar under
// its own key, the kept verdict measured.
func trendFundServer(t *testing.T, c *risk.Constitution, pos *rpc.PositionsResult) *Server {
	t.Helper()
	// The core runs in shadow here, as the live file runs it; the entry
	// under test is the trend fund.
	c.Mandate.Core.Stage = risk.StageShadow
	srv := coreTestServer(t, c, pos, 0.25)
	srv.coreListingsForTest = (&byISINListings{listings: []ibkrlib.Listing{coreTestListing(), trendFundListing()}}).lookup
	series := coreTestSeriesOn(t, trendFundListing(), marketcal.MarketUKLSE, coreTestDatesOn(t, marketcal.MarketUKLSE), 0.25)
	series.Key = strategySeriesKey(trendFundID, trendFundConID)
	if err := srv.saveMandateSeries(t.Context(), series); err != nil {
		t.Fatal(err)
	}
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	return srv
}

// withSYNF adds the owner's real units of the trend fund's line to a book.
func withSYNF(pos *rpc.PositionsResult, shares, mark float64) *rpc.PositionsResult {
	out := *pos
	out.Stocks = append(slices.Clone(pos.Stocks), rpc.PositionView{Symbol: "SYNF", SecType: rpc.SecTypeStock, ConID: trendFundConID, Currency: "EUR", PrimaryExch: "LSEETF", Quantity: shares, Multiplier: 1, Mark: mark})
	return &out
}

func trendFundTarget(t *testing.T, srv *Server) *rpc.StrategyTargetResult {
	t.Helper()
	res, err := srv.handleStrategyTarget(t.Context(), &rpc.Request{Params: []byte(`{"id":"` + trendFundID + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A second entry runs end to end without Go of its own (design §1, §8):
// its contract is resolved by ISIN on the background lane, its series read
// under its own key on the London calendar, its month decided under the
// library's filter and sized against the overlay's allocation at its scale,
// and its shadow record written once (decision, intent, fill) beside the
// core's, from a zero paper position, while the owner's real units of the
// fund stay the owner's; its series never enters the book's verdict, and
// the kept verdict is untouched.
func TestStrategyEntryRunsEndToEndInShadow(t *testing.T) {
	srv := trendFundServer(t, testTrendFundConstitution(), withSYNF(mandateTestPositions(100), 500, 90))
	// The owner holds the fund, so the book reads its closes too: the same
	// closes the entry's series carries, as one contract has.
	own := *srv.mandateHistorySeries(t.Context(), strategySeriesKey(trendFundID, trendFundConID))
	own.Key = "u:SYNF"
	if err := srv.saveMandateSeries(t.Context(), &own); err != nil {
		t.Fatal(err)
	}
	book := srv.evaluateMandate(t.Context(), false)
	keptRes, keptIn, keptAt, keptFP := srv.mandateVerdicts.current()
	for range 3 {
		srv.refreshStrategies(t.Context())
	}
	records, err := srv.shadowRecords(t.Context(), trendFundID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0].Kind != rpc.ShadowKindDecision || records[1].Kind != rpc.ShadowKindIntent || records[2].Kind != rpc.ShadowKindFill {
		t.Fatalf("records %+v, want decision, intent and fill once each", records)
	}
	d, in, fill := records[0], records[1].Intent, records[2].Fill
	if d.Session != "2026-09-30" || d.Rule != register.TimedHoldingRuleID || d.SpecHash != trendFundHash || !d.SpecCited || d.Stage != "shadow" || d.Decision == nil || !d.Decision.TrendOn || d.Decision.ScalePct != 25 || d.Decision.Close <= 0 || d.Decision.SleeveBudget <= 0 {
		t.Fatalf("decision %+v %+v", d, d.Decision)
	}
	if in == nil || in.ForSession != "2026-10-01" || in.Kind != core.ActionBuy || in.Quantity <= 0 || in.HoldingBefore != 0 || !strings.Contains(in.Why, "first buy") {
		t.Fatalf("intent %+v", in)
	}
	if fill == nil || fill.Quantity != in.Quantity || fill.Price <= 0 || fill.HoldingAfter != in.Quantity {
		t.Fatalf("fill %+v", fill)
	}
	if coreRecords := coreShadowRecords(t, srv); len(coreRecords) != 3 || coreRecords[0].Strategy != risk.CoreStrategyID {
		t.Fatalf("the core's own records %+v", coreRecords)
	}
	if res, inp, at, fp := srv.mandateVerdicts.current(); res != keptRes || inp != keptIn || at != keptAt || fp != keptFP {
		t.Fatal("the shadow's hypothetical book reached the kept verdict")
	}
	// The sleeve's allocation sized the target: 4% of the budget base over
	// the 15% floor, at scale 25, in whole shares.
	res := trendFundTarget(t, srv)
	if res.Status != rpc.CoreStatusReady || res.Strategy != trendFundID || res.Sleeve != risk.OverlaySleeve || res.Rule != register.TimedHoldingRuleID || res.SpecHash != trendFundHash || res.Signal != core.TrendSMA10.ID || res.SpecID != "" {
		t.Fatalf("header %+v reasons %v", res, res.Reasons)
	}
	if res.Sizing == nil || res.Sizing.CurrentOn.AllocationPct != 4 || res.Sizing.CurrentOn.ScalePct != 25 || res.Sizing.Target != d.Decision.TargetShares || res.Sizing.Target != in.Quantity {
		t.Fatalf("sizing %+v, decision %+v, intent %+v", res.Sizing, d.Decision, in)
	}
	if res.Instrument == nil || !res.Instrument.Resolved || res.Instrument.ConID != trendFundConID || res.Instrument.Calendar != "uk_lse" || res.Instrument.Symbol != "SYNF" {
		t.Fatalf("instrument %+v", res.Instrument)
	}
	if res.Holding == nil || res.Holding.Shares != 0 || res.Holding.Price != 90 || res.Holding.PriceSource != "mark" {
		t.Fatalf("the owner's 500 units are the owner's; holding %+v", res.Holding)
	}
	if res.Action == nil || res.Action.Kind != core.ActionWait || !strings.Contains(res.Action.Why, "the trend-fund strategy runs in shadow") {
		t.Fatalf("action %+v", res.Action)
	}
	if res.Shadow == nil || res.Shadow.Strategy != trendFundID || res.Shadow.Holding != in.Quantity || res.Shadow.Records != 3 {
		t.Fatalf("shadow %+v", res.Shadow)
	}
	if res.Kill == nil || res.Kill.MaxDrawdownK != nil || !res.Kill.Measured || res.Kill.Decisions != 1 || res.Kill.DrawdownK != 0 || !strings.Contains(res.Kill.Note, "cannot fire") {
		t.Fatalf("kill %+v", res.Kill)
	}
	if res.Evidence == nil || res.Evidence.SeriesKey != strategySeriesKey(trendFundID, trendFundConID) || res.Evidence.Calendar != "uk_lse" {
		t.Fatalf("evidence %+v", res.Evidence)
	}
	if slices.ContainsFunc(book.Evidence.Series, func(e rpc.MandateSeriesEvidence) bool { return strings.HasPrefix(e.Key, strategySeriesKeyPrefix) }) {
		t.Fatalf("the book reads a strategy series: %+v", book.Evidence.Series)
	}
	if i := slices.IndexFunc(book.Register, func(v rpc.MandateStrategyView) bool { return v.ID == trendFundID }); i < 0 || book.Register[i].ISIN != trendFundISIN || book.Register[i].Signal != core.TrendSMA10.ID || book.Register[i].State != "shadow" {
		t.Fatalf("register view %+v", book.Register)
	}
	// The core by id is the core's own target.
	if coreRes, err := srv.handleStrategyTarget(t.Context(), &rpc.Request{Params: []byte(`{"id":"core"}`)}); err != nil || coreRes.Strategy != risk.CoreStrategyID || coreRes.Rule != core.RuleID {
		t.Fatalf("core by id: %+v %v", coreRes, err)
	}
	// An unknown id is refused with the register's ids, and a rule's id
	// (`canary mandate` prints id and rule side by side) names the entry
	// that runs it.
	if _, err := srv.handleStrategyTarget(t.Context(), &rpc.Request{Params: []byte(`{"id":"nobody"}`)}); err == nil || !strings.Contains(err.Error(), `no strategy "nobody" in the register; its ids are core, `+trendFundID) {
		t.Fatalf("an unknown id: %v", err)
	}
	if _, err := srv.handleStrategyTarget(t.Context(), &rpc.Request{Params: []byte(`{"id":"` + core.RuleID + `"}`)}); err == nil || !strings.Contains(err.Error(), `"`+core.RuleID+`" is a rule, not a strategy id; the strategy running it is core (`+"`canary strategy core`)") {
		t.Fatalf("the core's rule as an id: %v", err)
	}
}

// An unknown capital holds the strategy once: mandate.Evaluate copies the
// capital's reasons into the verdict's, and the verdict hold does not say
// them again after the capital hold has.
func TestStrategyTargetNamesAnUnknownCapitalOnce(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	st := srv.riskCapital
	st.mu.Lock()
	m := st.state.Mandate
	m.FlowSuspect = &mandateFlowSuspect{At: mandateTestNow.Add(-time.Minute), Base: mandateFlowBase{At: mandateTestNow.Add(-3 * time.Minute), EquityBase: 280000},
		Unexplained: 20000, ThresholdAt: 3000, MoveBase: 20000}
	st.mu.Unlock()
	res := coreEvaluate(t, srv)
	if res.Status != rpc.CoreStatusHold || res.Mandate == nil || res.Mandate.Status != "unknown" {
		t.Fatalf("status %s mandate %+v reasons %v", res.Status, res.Mandate, res.Reasons)
	}
	var capital, verdict, flow int
	for _, r := range res.Reasons {
		switch {
		case strings.HasPrefix(r, "the mandate's capital is unknown: "):
			capital++
		case strings.HasPrefix(r, "the mandate verdict is unknown"):
			verdict++
		}
		if strings.Contains(r, "a cash flow is suspected") {
			flow++
		}
	}
	if capital != 1 || verdict != 0 || flow != 1 {
		t.Fatalf("the suspicion is named %d times (capital %d, verdict %d): %v", flow, capital, verdict, res.Reasons)
	}
}

// What an entry lacks holds it and is named: an unwritten key, a currency
// other than the account's base, and an ISIN IBKR lists nothing under;
// nothing is decided and no record is written.
func TestStrategyEntryHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*risk.ConstitutionMandateStrategy)
		want string
	}{
		"unwritten key":  {func(e *risk.ConstitutionMandateStrategy) { e.BandPct = nil }, "mandate.strategies.trend-fund.band_pct unwritten"},
		"other currency": {func(e *risk.ConstitutionMandateStrategy) { e.Currency = "USD" }, "the account base currency EUR is not USD: the trend-fund strategy trades in USD"},
		"unknown signal": {func(e *risk.ConstitutionMandateStrategy) { e.Signal = "trend-sma12m" }, "signal trend-sma12m is not in this Canary's library"},
		"no listing":     {func(e *risk.ConstitutionMandateStrategy) { e.ISIN = "ZZ0000000002" }, "IBKR lists no EUR line under ZZ0000000002"},
	} {
		t.Run(name, func(t *testing.T) {
			c := testTrendFundConstitution()
			tc.edit(c.Mandate.Strategies[trendFundID])
			srv := trendFundServer(t, c, mandateTestPositions(100))
			srv.refreshStrategies(t.Context())
			res := trendFundTarget(t, srv)
			if res.Status != rpc.CoreStatusHold || !strings.Contains(strings.Join(res.Reasons, " | "), tc.want) || res.Action == nil || res.Action.Kind != core.ActionHold || res.Sizing != nil {
				t.Fatalf("status %s reasons %v action %+v, want a hold naming %q", res.Status, res.Reasons, res.Action, tc.want)
			}
			if records, _ := srv.shadowRecords(t.Context(), trendFundID); slices.ContainsFunc(records, func(r rpc.ShadowRecord) bool { return r.Kind != rpc.ShadowKindHold }) {
				t.Fatalf("a held entry wrote %+v", records)
			}
		})
	}
}

// The kill rule's drawdown test (design §3): on a record whose paper buy
// was filled far above the next decision's close, the drawdown in units of
// K is past a written bound, so the strategy holds, names it and freezes
// nothing; unwritten, the same record cannot fire the rule.
func TestStrategyKillBoundHoldsPastTheDrawdown(t *testing.T) {
	seed := func(t *testing.T, srv *Server) {
		t.Helper()
		base := rpc.ShadowRecord{Version: 1, Strategy: trendFundID, Rule: register.TimedHoldingRuleID, SpecHash: trendFundHash, SpecCited: true, Stage: "shadow", RecordedAt: mandateTestNow.Add(-40 * 24 * time.Hour)}
		decision, intent, fill := base, base, base
		decision.Kind, decision.Session, decision.Decision = rpc.ShadowKindDecision, "2026-08-28", &rpc.ShadowDecision{TrendOn: true, TargetShares: 100, ScalePct: 25, Close: 1000, SleeveBudget: 9200}
		intent.Kind, intent.Session, intent.Intent = rpc.ShadowKindIntent, "2026-08-28", &rpc.ShadowIntent{ForSession: "2026-09-01", Kind: core.ActionBuy, Quantity: 100}
		fill.Kind, fill.Session, fill.Fill = rpc.ShadowKindFill, "2026-09-01", &rpc.ShadowFill{Quantity: 100, Price: 1000, HoldingAfter: 100, Convention: rpc.ShadowFillConvention}
		for _, r := range []rpc.ShadowRecord{decision, intent, fill} {
			if err := srv.appendShadowRecord(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := testTrendFundConstitution()
	c.Mandate.Strategies[trendFundID].KillDrawdownK = new(0.5)
	srv := trendFundServer(t, c, mandateTestPositions(100))
	seed(t, srv)
	srv.refreshStrategies(t.Context())
	res := trendFundTarget(t, srv)
	if res.Status != rpc.CoreStatusHold || res.Kill == nil || !res.Kill.Breached || res.Kill.DrawdownK < 1 || res.Kill.Decisions != 2 {
		t.Fatalf("status %s kill %+v reasons %v", res.Status, res.Kill, res.Reasons)
	}
	if !strings.Contains(strings.Join(res.Reasons, " | "), "kill rule: the drawdown on the shadow record is") || res.Decision == nil || !res.Decision.FrozenAt.IsZero() {
		t.Fatalf("reasons %v decision %+v, want the kill hold and no frozen month", res.Reasons, res.Decision)
	}
	if records, _ := srv.shadowRecords(t.Context(), trendFundID); len(records) != 4 || records[3].Kind != rpc.ShadowKindHold || !strings.Contains(strings.Join(records[3].Reasons, " "), "kill rule") {
		t.Fatalf("records %+v, want the month recorded as a kill hold", records)
	}
	// Unwritten, the same loss cannot fire the rule.
	free := trendFundServer(t, testTrendFundConstitution(), mandateTestPositions(100))
	seed(t, free)
	free.refreshStrategies(t.Context())
	if res := trendFundTarget(t, free); res.Status != rpc.CoreStatusReady || res.Kill == nil || res.Kill.Breached || res.Kill.DrawdownK < 1 || res.Kill.MaxDrawdownK != nil {
		t.Fatalf("unwritten bound: status %s kill %+v reasons %v", res.Status, res.Kill, res.Reasons)
	}
}
