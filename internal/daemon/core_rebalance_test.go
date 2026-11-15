package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/register"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The core sleeve's monthly rows (core-sleeve.md phase 2, option A),
// witnessed on synthetic books: the core test server (a Xetra line, the
// seeded series with the 30 September decision) and the proposal engine on
// top of it. coreDecisionNow is the evening of that decision day, once its
// close is in; coreWindowSessions the sessions its row is served for.

var (
	coreDecisionNow    = time.Date(2026, 9, 30, 16, 30, 0, 0, time.UTC) // 18:30 Berlin
	coreWindowSessions = []string{"2026-10-01", "2026-10-02", "2026-10-05", "2026-10-06", "2026-10-07"}
)

// coreRowServer is the core test server at a clock, with an engine on it.
func coreRowServer(t *testing.T, c *risk.Constitution, pos *rpc.PositionsResult, drift float64, now time.Time) (*Server, *proposalEngine) {
	t.Helper()
	srv := coreTestServer(t, c, pos, drift)
	srv.now = func() time.Time { return now }
	engine := &proposalEngine{server: srv, now: srv.now, queued: &queuedAuthStore{}, store: &proposalStore{}}
	srv.tradeProposals = engine
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	return srv, engine
}

// coreRows generates one proposal set and returns the core's rows and status.
func coreRows(t *testing.T, srv *Server, engine *proposalEngine, pos *rpc.PositionsResult) ([]rpc.TradeProposal, *rpc.TradeProposalCoreStatus) {
	t.Helper()
	rows, _, _, _, _, st := engine.generateBook(t.Context(), defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, srv.currentBrokerStateScope(), srv.now())
	var out []rpc.TradeProposal
	for _, p := range rows {
		if p.Bucket == rpc.TradeProposalBucketCoreRebalance {
			out = append(out, p)
		}
	}
	return out, st
}

// A decision day creates exactly one row: once the decision day's close is
// in, the live core's first buy is one core_rebalance BUY of the sized
// quantity for the next session, a DAY limit at the live mid, with the
// decision's figures and the window on it, open (no blocker), never
// pre-authorisable, and the month's decision is recorded once with its key.
func TestCoreRowOnTheDecisionDayCreatesExactlyOneRow(t *testing.T) {
	pos := mandateTestPositions(100)
	srv, engine := coreRowServer(t, testCoreConstitution(), pos, 0.25, coreDecisionNow)
	res := srv.evaluateCoreTarget(t.Context(), srv.now())
	if res.Status != rpc.CoreStatusReady || res.Action.Kind != core.ActionBuy || res.Decision.Decision != "2026-09-30" || res.ReadOnly {
		t.Fatalf("verdict %s %v action %+v decision %+v read-only %v", res.Status, res.Reasons, res.Action, res.Decision, res.ReadOnly)
	}
	rows, st := coreRows(t, srv, engine, pos)
	if len(rows) != 1 {
		t.Fatalf("rows %+v, want exactly one", rows)
	}
	p := rows[0]
	if p.Action != rpc.OrderActionBuy || p.Quantity != res.Action.Quantity || p.MaxQuantity != p.Quantity || p.PositionEffect != rpc.OrderPositionEffectOpen ||
		p.OrderType != rpc.OrderTypeLMT || p.TIF != rpc.OrderTIFDay || p.Contract.ConID != coreTestConID || p.Contract.Exchange != "SMART" || p.Contract.PrimaryExch != "IBIS" ||
		p.Contract.Currency != core.Currency || p.Contract.SecType != "STK" || p.State != rpc.TradeProposalStateGenerated || len(p.Blockers) != 0 || p.Shadow {
		t.Fatalf("row %+v", p)
	}
	c := p.Core
	if c == nil || c.Decision != "2026-09-30" || !c.TrendOn || c.Kind != core.ActionBuy || c.Target != res.Sizing.Target || c.Held != 0 || c.Session != "2026-10-01" ||
		c.Sleeve != "core" || c.Stage != "live" || c.ConID != coreTestConID || c.DecisionClose != res.Decision.Close || c.VerdictStatus != "within" {
		t.Fatalf("core figures %+v", c)
	}
	if c.Terms.OrderType != rpc.OrderTypeLMT || c.Terms.TIF != rpc.OrderTIFDay || !slices.Equal(c.Terms.Window, coreWindowSessions) || c.Terms.PrimaryExchange != "IBIS" || !strings.HasPrefix(c.Terms.Episode, "core:") || !strings.HasSuffix(c.Terms.Episode, ":2026-09") {
		t.Fatalf("terms %+v", c.Terms)
	}
	if !strings.Contains(p.Reason, "first buy") || !strings.Contains(strings.Join(p.Details, " "), "confirm this row on your device") {
		t.Fatalf("reason %q details %v", p.Reason, p.Details)
	}
	if st == nil || !st.Served || st.RowKey != p.Key || st.State != "live" || st.Kind != core.ActionBuy || st.Decision != "2026-09-30" || !slices.Equal(st.Window, coreWindowSessions) {
		t.Fatalf("status %+v", st)
	}
	if validPreAuthorisedBucket(rpc.TradeProposalBucketCoreRebalance) || automaticBucketFor(p) != "" || engine.automaticBucket(p) != "" {
		t.Fatal("a core row is pre-authorisable")
	}
	// The month's decision is recorded once, with the row's key, and served
	// as the last decision; a second refresh changes nothing.
	for range 2 {
		srv.refreshCoreTarget(t.Context())
	}
	f := srv.coreFrozen(t.Context(), coreScope(srv.currentBrokerStateScope(), coreTestConID))
	if f == nil || f.Row == nil || f.Row.Kind != core.ActionBuy || f.Row.Quantity != p.Quantity || !f.Row.Served || f.Row.RowKey != p.Key || f.Row.Stage != "live" || !slices.Equal(f.Row.Window, coreWindowSessions) {
		t.Fatalf("recorded decision %+v", f.Row)
	}
	// The engine's served snapshot shows the row on the verdict.
	engine.snapshot = rpc.TradeProposalSnapshot{Kind: "trade-proposals", Proposals: rows, AccountID: srv.currentBrokerStateScope().Account, AccountMode: srv.currentBrokerStateScope().Mode}
	again := srv.evaluateCoreTarget(t.Context(), srv.now())
	if again.Row == nil || again.Row.Key != p.Key || again.Row.Quantity != p.Quantity || again.Row.Session != "2026-10-01" || again.Row.Action != rpc.OrderActionBuy {
		t.Fatalf("row view %+v", again.Row)
	}
	if d := again.LastDecision; d == nil || d.Decision != "2026-09-30" || d.Kind != core.ActionBuy || !d.Outcome.Served || d.Outcome.Submitted || d.RowKey != p.Key {
		t.Fatalf("last decision %+v", again.LastDecision)
	}
	if len(coreShadowRecords(t, srv)) != 0 {
		t.Fatal("a live core wrote shadow records")
	}
}

// A non-decision day creates none: on 14 October the September window has
// ended, so the rule's standing buy is served as no row, and before the
// decision day's close is in the previous month's window has ended too.
func TestCoreRowOnANonDecisionDayCreatesNone(t *testing.T) {
	pos := mandateTestPositions(100)
	for _, now := range []time.Time{mandateTestNow, time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)} {
		srv, engine := coreRowServer(t, testCoreConstitution(), pos, 0.25, now)
		if res := srv.evaluateCoreTarget(t.Context(), now); res.Action == nil || res.Action.Kind != core.ActionBuy {
			t.Fatalf("at %s the rule's action is %+v", now, res.Action)
		}
		rows, st := coreRows(t, srv, engine, pos)
		if len(rows) != 0 || st == nil || st.Served || !strings.Contains(strings.Join(st.Reasons, " "), "window after") {
			t.Fatalf("at %s: rows %+v status %+v", now, rows, st)
		}
	}
}

// In shadow the decision reaches the shadow record only: no row, and the
// intent carries the order terms the live row would, from the one builder.
func TestCoreRowInShadowCreatesNoneAndTheIntentCarriesTheTerms(t *testing.T) {
	pos := mandateTestPositions(100)
	c := testCoreConstitution()
	c.Mandate.Core.Stage = risk.StageShadow
	srv, engine := coreRowServer(t, c, pos, 0.25, coreDecisionNow)
	rows, st := coreRows(t, srv, engine, pos)
	if len(rows) != 0 || st == nil || st.Served || st.State != "shadow" || !strings.Contains(strings.Join(st.Reasons, " "), "runs in shadow") {
		t.Fatalf("rows %+v status %+v", rows, st)
	}
	srv.refreshCoreTarget(t.Context())
	records := coreShadowRecords(t, srv)
	i := slices.IndexFunc(records, func(r rpc.ShadowRecord) bool { return r.Kind == rpc.ShadowKindIntent })
	if i < 0 || records[i].Intent == nil || records[i].Intent.Terms == nil {
		t.Fatalf("shadow records %+v, want an intent with terms", records)
	}
	in := records[i].Intent
	live, _ := coreRowServer(t, testCoreConstitution(), pos, 0.25, coreDecisionNow)
	liveRows, _ := coreRows(t, live, live.tradeProposals, pos)
	if len(liveRows) != 1 || liveRows[0].Core == nil {
		t.Fatalf("live rows %+v", liveRows)
	}
	if want := liveRows[0].Core.Terms; !slices.Equal(in.Terms.Window, want.Window) || in.Terms.OrderType != want.OrderType || in.Terms.TIF != want.TIF || in.Terms.Limit != want.Limit || in.Terms.Episode != want.Episode || in.Terms.PrimaryExchange != want.PrimaryExchange {
		t.Fatalf("shadow terms %+v differ from the live row's %+v", *in.Terms, want)
	}
	if in.ForSession != liveRows[0].Core.Session {
		t.Fatalf("shadow intent for %s, the live row for %s", in.ForSession, liveRows[0].Core.Session)
	}
	if res := srv.evaluateCoreTarget(t.Context(), srv.now()); !res.ReadOnly || res.Row != nil || res.LastDecision != nil {
		t.Fatalf("a shadow verdict serves a row: %+v %+v", res.Row, res.LastDecision)
	}
}

// The band and the minimum hold: a holding inside the band serves no row,
// and a buy below the minimum order waits; each is named on the status.
func TestCoreRowHoldsInsideTheBandAndBelowTheMinimum(t *testing.T) {
	pos := mandateTestPositions(100)
	srv, _ := coreRowServer(t, testCoreConstitution(), pos, 0.25, coreDecisionNow)
	target := srv.evaluateCoreTarget(t.Context(), srv.now()).Sizing.Target
	inside := withIBCF(pos, float64(target-target/20), 100)
	srv, engine := coreRowServer(t, testCoreConstitution(), inside, 0.25, coreDecisionNow)
	rows, st := coreRows(t, srv, engine, inside)
	if len(rows) != 0 || st == nil || st.Served || st.Kind != core.ActionNone || !strings.Contains(st.Why, "inside the band") {
		t.Fatalf("inside the band: rows %+v status %+v", rows, st)
	}
	c := testCoreConstitution()
	c.Mandate.Core.MinOrderBase = new(1e9)
	srv, engine = coreRowServer(t, c, pos, 0.25, coreDecisionNow)
	rows, st = coreRows(t, srv, engine, pos)
	if len(rows) != 0 || st == nil || st.Served || st.Kind != core.ActionWait || !strings.Contains(st.Why, "below the minimum order") {
		t.Fatalf("below the minimum: rows %+v status %+v", rows, st)
	}
}

// A standing reduction of the line serves the buy blocked, never open (an
// open breach episode did too until 2026-10-19; a sleeve inside its
// allocation now adds while the reduction runs); the typed exception admits
// only the row's own buy within its sized quantity near the decision close,
// and a core sale stays an ordinary reduction.
func TestCoreRowServesBuysBlockedBesideReductionsAndWorkingOrders(t *testing.T) {
	pos := mandateTestPositions(100)
	srv, engine := coreRowServer(t, testCoreConstitution(), pos, 0.25, coreDecisionNow)
	base := srv.coreRowInput(t.Context(), srv.now())
	for _, tc := range []struct {
		name string
		edit func(*coreRowInput)
		code string
	}{
		{"reduction stands", func(in *coreRowInput) { in.ReductionStands = true }, coreBuyBesideReductionCode},
	} {
		in := base
		tc.edit(&in)
		got, st := engine.coreRebalanceProposals(t.Context(), defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, in, rpc.TradeProposalSourceFingerprints{}, srv.now())
		if len(got) != 1 || got[0].State != rpc.TradeProposalStateBlocked || !slices.ContainsFunc(got[0].Blockers, func(b rpc.TradingBlocker) bool { return b.Code == tc.code }) || !st.Served {
			t.Fatalf("%s: rows %+v status %+v, want the buy served blocked with %s", tc.name, got, st, tc.code)
		}
	}
	// A working order for the contract, the owner's included, blocks the row.
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Account: srv.currentBrokerStateScope().Account,
			ConID: coreTestConID, Symbol: core.Symbol, SecType: "STK", Action: rpc.OrderActionSell, TotalQuantity: 5, Remaining: 5, OrderType: "LMT"}
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.now(), Orders: []ibkrlib.OrderLifecycleEvent{order}}, srv.currentBrokerStateScope(), nil
	}
	rows, _ := coreRows(t, srv, engine, pos)
	if len(rows) != 1 || rows[0].State != rpc.TradeProposalStateBlocked || !slices.ContainsFunc(rows[0].Blockers, func(b rpc.TradingBlocker) bool { return b.Code == reductionOrderExistingCode }) {
		t.Fatalf("beside a working sale of the line: %+v", rows)
	}

	// The typed exception: the row's own buy, within its sized quantity.
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.now()}, srv.currentBrokerStateScope(), nil
	}
	rows, _ = coreRows(t, srv, engine, pos)
	if len(rows) != 1 || len(rows[0].Blockers) != 0 {
		t.Fatalf("rows %+v", rows)
	}
	row := rows[0]
	x, ok := coreOpenExceptionFor(row)
	if !ok || x.ConID != coreTestConID || x.MaxQuantity != row.Quantity || x.Currency != core.Currency {
		t.Fatalf("exception %+v %v", x, ok)
	}
	preview := func(edit func(*rpc.OrderPreviewResult)) *rpc.OrderPreviewResult {
		p := &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: row.Quantity, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: row.Core.DecisionClose * 1.01,
			TriggerMethod: rpc.OrderTriggerMethodDefault, Source: proposalOrderSource, Contract: row.Contract}, Position: rpc.OrderPositionImpact{Before: 0, After: float64(row.Quantity), Effect: rpc.OrderPositionEffectOpen}}
		edit(p)
		return p
	}
	if b := proposalPreviewSafetyBlockers(row, preview(func(*rpc.OrderPreviewResult) {})); len(b) != 0 {
		t.Fatalf("the row's own buy is refused: %+v", b)
	}
	for _, tc := range []struct {
		name string
		edit func(*rpc.OrderPreviewResult)
		code string
	}{
		{"one share more", func(p *rpc.OrderPreviewResult) { p.Draft.Quantity++; p.Position.After++ }, "preview_effect_not_close_reduce"},
		{"another contract", func(p *rpc.OrderPreviewResult) { p.Draft.Contract.ConID = 4242 }, "preview_effect_not_close_reduce"},
		{"a limit far from the decision close", func(p *rpc.OrderPreviewResult) { p.Draft.LimitPrice = row.Core.DecisionClose * 1.2 }, coreLimitDistanceCode},
	} {
		if b := proposalPreviewSafetyBlockers(row, preview(tc.edit)); !slices.ContainsFunc(b, func(b rpc.TradingBlocker) bool { return b.Code == tc.code }) {
			t.Fatalf("%s: blockers %+v, want %s", tc.name, b, tc.code)
		}
	}
	// Past Q − H the exception does not apply, whatever the row says.
	wider := row
	wider.Quantity, wider.MaxQuantity = row.Core.Target-row.Core.Held+1, row.Core.Target-row.Core.Held+1
	if _, ok := coreOpenExceptionFor(wider); ok {
		t.Fatal("a buy past the target is admitted")
	}
	// Any other bucket's buy stays close-or-reduce only.
	other := row
	other.Bucket = rpc.TradeProposalBucketRiskReduction
	if b := proposalPreviewSafetyBlockers(other, preview(func(*rpc.OrderPreviewResult) {})); !slices.ContainsFunc(b, func(b rpc.TradingBlocker) bool { return b.Code == "proposal_effect_not_close_reduce" }) {
		t.Fatalf("another bucket's buy passes: %+v", b)
	}
}

// Trend off sells every share: the row is a SELL of the holding, closing
// it, an ordinary reduction, and past the window it continues in the next
// session the US also trades until the holding is zero.
func TestCoreRowSellsAllAndContinuesPastTheWindow(t *testing.T) {
	pos := withIBCF(mandateTestPositions(100), 10, 60)
	for _, tc := range []struct {
		now     time.Time
		session string
	}{{coreDecisionNow, "2026-10-01"}, {mandateTestNow, "2026-10-14"}} {
		srv, engine := coreRowServer(t, testCoreConstitution(), pos, -0.6, tc.now)
		rows, st := coreRows(t, srv, engine, pos)
		if len(rows) != 1 || rows[0].Action != rpc.OrderActionSell || rows[0].Quantity != 10 || rows[0].PositionEffect != rpc.OrderPositionEffectClose || rows[0].Core.Kind != core.ActionSellAll || rows[0].Core.Session != tc.session || !st.Served {
			t.Fatalf("at %s: rows %+v status %+v", tc.now, rows, st)
		}
		if _, ok := coreOpenExceptionFor(rows[0]); ok {
			t.Fatal("a core sale qualifies for the buy exception")
		}
	}
}

// The window: the first five sessions after the decision on the line's
// calendar that the US also trades. July 2026 skips Independence Day as
// observed on Friday 3 July; the London line's August 2026 decision is the
// 28th and its window skips Labor Day.
func TestCoreWindowSkipsSessionsTheUSIsClosed(t *testing.T) {
	if got := coreWindow("2026-06-30", marketcal.MarketDEXetra, mandateTestNow); !slices.Equal(got, []string{"2026-07-01", "2026-07-02", "2026-07-06", "2026-07-07", "2026-07-08"}) {
		t.Fatalf("Xetra, July 2026: %v", got)
	}
	if got := coreWindow("2026-08-28", marketcal.MarketUKLSE, mandateTestNow); !slices.Equal(got, []string{"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04", "2026-09-08"}) {
		t.Fatalf("London, September 2026: %v", got)
	}
	if got := coreWindow("2026-09-30", marketcal.MarketDEXetra, mandateTestNow); !slices.Equal(got, coreWindowSessions) {
		t.Fatalf("Xetra, October 2026: %v", got)
	}
	// Xetra's second segment (IBIS2, the trend fund's EUR line) quotes on
	// the Xetra calendar; before 2026-10-19 it fell through to the US one.
	if market := quoteMarketForStockContract(rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS2", Currency: "EUR"}); market != marketcal.MarketDEXetra {
		t.Fatalf("an IBIS2 line quotes on %s", market)
	}
	if market := quoteMarketForStockContract(rpc.ContractParams{Symbol: "IXSE", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSEETF", Currency: "EUR"}); market != marketcal.MarketUKLSE {
		t.Fatalf("a London line reads the %s calendar", market)
	}
}

// A core row's fill is the core sleeve's in the scorecard, under its rule,
// never the owner's and never the generic rule sleeve.
func TestScorecardSleeveResolverKeepsCoreFillsInTheCoreSleeve(t *testing.T) {
	t.Parallel()
	events := []orderJournalEvent{{ExecID: "e-core", OrderRef: "canary-core-1", Origin: rpc.OrderOriginPairedDevice}}
	proposals := []proposalEvent{{Type: "submitted", OrderRef: "canary-core-1", Bucket: rpc.TradeProposalBucketCoreRebalance}}
	got := scorecardSleeveResolver(events, proposals)(edgecore.PositionChange{ExecutionIDs: []string{"e-core"}})
	if got.Name != rpc.ScorecardSleeveCore || got.Origin != "proposal:core_rebalance" || got.StrategyRef != core.RuleID {
		t.Fatalf("a core fill resolved to %+v", got)
	}
	if scorecardRuleBucket(rpc.TradeProposalBucketCoreRebalance) {
		t.Fatal("the core's bucket is the generic rule sleeve's")
	}
	if rule, _ := register.Find(register.Catalogue(), core.RuleID); !rule.Sends || rule.Bucket != rpc.TradeProposalBucketCoreRebalance {
		t.Fatalf("catalogue %+v", rule)
	}
}

// The month's record follows the row the window serves: a decision first
// recorded as "no buy" (here below the minimum order; until 2026-10-19 the
// verdict reading reduce) is re-recorded when a later refresh inside the
// window serves the buy, with its key, so `canary core` reads the row's
// outcome; a refresh that changes nothing re-records nothing.
func TestCoreDecisionRecordFollowsTheRowServedLaterInTheWindow(t *testing.T) {
	pos := withIBCF(mandateTestPositions(100), 100, 100)
	c := testCoreConstitution()
	c.Mandate.Core.MinOrderBase = new(1e9)
	srv, engine := coreRowServer(t, c, pos, 0.25, coreDecisionNow)
	srv.refreshCoreTarget(t.Context())
	first := srv.evaluateCoreTarget(t.Context(), srv.now()).LastDecision
	if first == nil || first.Kind != core.ActionWait || first.Outcome.Served || first.RowKey != "" {
		t.Fatalf("first record %+v", first)
	}
	// The owner lowers the minimum inside the window: the buy is served.
	c.Mandate.Core.MinOrderBase = new(2500.0)
	srv.now = func() time.Time { return coreDecisionNow.Add(90 * time.Minute) }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	rows, _ := coreRows(t, srv, engine, pos)
	if len(rows) != 1 || rows[0].Action != rpc.OrderActionBuy {
		t.Fatalf("rows %+v, want the buy", rows)
	}
	srv.refreshCoreTarget(t.Context())
	second := srv.evaluateCoreTarget(t.Context(), srv.now()).LastDecision
	if second == nil || second.Kind != core.ActionBuy || !second.Outcome.Served || second.RowKey != rows[0].Key || !second.DecidedAt.After(first.DecidedAt) {
		t.Fatalf("record after the buy is served %+v", second)
	}
	srv.refreshCoreTarget(t.Context())
	if again := srv.evaluateCoreTarget(t.Context(), srv.now()).LastDecision; again == nil || !again.DecidedAt.Equal(second.DecidedAt) {
		t.Fatalf("an unchanged decision was re-recorded: %+v", again)
	}
}

// A send that lands between the engine serving the buy and the strategy
// cadence re-recording the month (the controller sends at its first ready
// read; the record follows a minute later) is read: the journal is searched
// since the month was frozen, not since the row's re-record, so the record
// reads submitted and is final.
func TestCoreDecisionRecordReadsASendBeforeItsReRecord(t *testing.T) {
	pos := withIBCF(mandateTestPositions(100), 100, 100)
	c := testCoreConstitution()
	c.Mandate.Core.MinOrderBase = new(1e9)
	srv, engine := coreRowServer(t, c, pos, 0.25, coreDecisionNow)
	srv.refreshCoreTarget(t.Context())
	c.Mandate.Core.MinOrderBase = new(2500.0)
	srv.now = func() time.Time { return coreDecisionNow.Add(90 * time.Minute) }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	rows, _ := coreRows(t, srv, engine, pos)
	if len(rows) != 1 || rows[0].Action != rpc.OrderActionBuy {
		t.Fatalf("rows %+v, want the buy", rows)
	}
	// The controller sends the served row before the strategy cadence
	// re-records the month.
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), srv.coreStore); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.store.bindCore(t.Context(), srv.coreStore); err != nil {
		t.Fatal(err)
	}
	sent := proposalEventForProposal("submitted", rows[0], srv.now(), "", "canary-core-1", "sent under the standing mandate")
	sent.AccountID, sent.AccountMode = srv.currentBrokerStateScope().Account, srv.currentBrokerStateScope().Mode
	if err := engine.appendEvent(sent); err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return coreDecisionNow.Add(91 * time.Minute) }
	srv.refreshCoreTarget(t.Context())
	got := srv.evaluateCoreTarget(t.Context(), srv.now()).LastDecision
	if got == nil || got.Kind != core.ActionBuy || !got.Outcome.Served || got.RowKey != rows[0].Key || !got.Outcome.Submitted || got.Outcome.OrderRef != "canary-core-1" {
		t.Fatalf("the send before the re-record is not read: %+v", got)
	}
	// Final: a later change of the terms re-records nothing.
	c.Mandate.Core.MinOrderBase = new(1e9)
	srv.now = func() time.Time { return coreDecisionNow.Add(92 * time.Minute) }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
	srv.refreshCoreTarget(t.Context())
	if again := srv.evaluateCoreTarget(t.Context(), srv.now()).LastDecision; again == nil || !again.DecidedAt.Equal(got.DecidedAt) || !again.Outcome.Submitted {
		t.Fatalf("a submitted record was rewritten: %+v", again)
	}
}
