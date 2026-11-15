package daemon

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The core on the line IBKR lists under its ISIN, on that line's own
// exchange (owner decision 2026-10-18 08:06 CEST): synthetic London lines,
// synthetic closes, no account data.

// coreLondonServer is the core test server with a London line resolved
// and, unless series is false, its series on London's calendar.
func coreLondonServer(t *testing.T, pos *rpc.PositionsResult, series bool) *Server {
	t.Helper()
	c := testCoreConstitution()
	srv, _ := mandateTestServer(t, c)
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv.coreStore = store
	seedMandateHistory(t, srv, 0)
	if series {
		if err := srv.saveMandateSeries(t.Context(), coreTestSeriesOn(t, coreTestLondonListing(), marketcal.MarketUKLSE, coreTestDatesOn(t, marketcal.MarketUKLSE), 0.25)); err != nil {
			t.Fatal(err)
		}
	}
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	coreAnswer(t, srv, coreTestLondonListing())
	return srv
}

// The London line is decided on London's calendar and reads ready; the
// same closes judged on Xetra's would hold every month (London trades on 1
// May when Xetra is shut, and is shut on 4 May, 25 May and 31 August when
// Xetra trades), which is why the calendar follows the listing.
func TestCoreTargetOnTheLondonLine(t *testing.T) {
	srv := coreLondonServer(t, mandateTestPositions(100), true)
	res := coreEvaluate(t, srv)
	if res.Status != rpc.CoreStatusReady || len(res.Reasons) != 0 {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	in := res.Instrument
	if in == nil || !in.Resolved || in.ConID != 9031 || in.Symbol != "IXSE" || in.Exchange != "LSEETF" || in.TradingClass != "EXXT" || in.Calendar != "uk_lse" || in.CalendarLabel != "London SETS" {
		t.Fatalf("instrument %+v", in)
	}
	if res.Evidence == nil || res.Evidence.SeriesKey != coreSeriesKey(9031) || res.Evidence.Calendar != "uk_lse" || res.Evidence.Stale {
		t.Fatalf("evidence %+v", res.Evidence)
	}
	if res.Decision == nil || res.Decision.Decision != "2026-09-30" || res.NextDecision != "2026-10-30" || res.Action.Kind != core.ActionBuy {
		t.Fatalf("decision %+v next %s action %+v", res.Decision, res.NextDecision, res.Action)
	}
	// The same closes on Xetra's calendar: a bar on a day Xetra was shut
	// and a missing close on a day it traded.
	series := srv.mandateHistoryForNeed(t.Context(), coreSeriesNeed(srv.coreContractInUse()))
	sessions, off := coreCalendar(series.Dates, "2026-09-30", mandateTestNow, marketcal.MarketDEXetra)
	if !slices.Contains(off, "2026-05-01") {
		t.Fatalf("Xetra's calendar takes London's 1 May bar: %v", off)
	}
	if sig, _ := core.Measure(sessions, series.Dates, series.Closes, "2026-09-30"); sig.Status != core.StatusHold || !strings.Contains(sig.Reason, "no close on 2026-05-04") {
		t.Fatalf("London closes on Xetra's calendar: %+v", sig)
	}
	// Month-ends differ: London's last August session is the 28th.
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if london, _, _ := coreDecisionDays(at, marketcal.MarketUKLSE); london != "2026-08-28" {
		t.Fatalf("London's August decision %s", london)
	}
	if xetra, _, _ := coreDecisionDays(at, marketcal.MarketDEXetra); xetra != "2026-08-31" {
		t.Fatalf("Xetra's August decision %s", xetra)
	}
	// A London close counts from half an hour after its 16:30 London close.
	if latest, _, _ := coreDecisionDays(time.Date(2026, 9, 30, 15, 59, 0, 0, time.UTC), marketcal.MarketUKLSE); latest != "2026-08-28" {
		t.Fatalf("before 17:00 London on 30 September: %s", latest)
	}
	if latest, _, _ := coreDecisionDays(time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC), marketcal.MarketUKLSE); latest != "2026-09-30" {
		t.Fatalf("from 17:00 London on 30 September: %s", latest)
	}
	// A series kept for another calendar is not used: it is read again.
	stale := *series
	stale.Market = marketcal.MarketDEXetra
	if err := srv.saveMandateSeries(t.Context(), &stale); err != nil {
		t.Fatal(err)
	}
	if res := coreEvaluate(t, srv); res.Status != rpc.CoreStatusHold || !strings.Contains(strings.Join(res.Reasons, " "), "was read for the de_xetra calendar, not London SETS; the desk reads it again on LSEETF") {
		t.Fatalf("another calendar's series: %s %v", res.Status, res.Reasons)
	}
}

// A series read IBKR refuses is named in the hold, with when it came, and
// shown in the evidence until a read succeeds. A gateway that is down is
// not IBKR's answer and is not kept.
func TestCoreHoldNamesTheRefusedRead(t *testing.T) {
	srv := coreLondonServer(t, mandateTestPositions(100), false)
	k := srv.coreContractInUse()
	refusal := errors.New("No security definition has been found for the request")
	srv.mandateHistoryFetchForTest = func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return nil, refusal
	}
	if srv.readMandateSeries(t.Context(), coreSeriesNeed(k), mandateTestNow) {
		t.Fatal("a refused read succeeded")
	}
	res := coreEvaluate(t, srv)
	want := "no IXSE daily history: the last read of contract 9031 IXSE (class EXXT) on LSEETF failed at 2026-10-14 15:00:00 UTC: No security definition has been found for the request"
	if res.Status != rpc.CoreStatusHold || !slices.Contains(res.Reasons, want) {
		t.Fatalf("reasons %v, want %q", res.Reasons, want)
	}
	if e := res.Evidence; e == nil || !e.Missing || e.ReadError != refusal.Error() || !e.ReadErrorAt.Equal(mandateTestNow) {
		t.Fatalf("evidence %+v", res.Evidence)
	}
	// A down gateway leaves the refusal standing; a read that succeeds
	// clears it.
	srv.mandateHistoryFetchForTest = func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	srv.readMandateSeries(t.Context(), coreSeriesNeed(k), mandateTestNow.Add(time.Minute))
	if err, at := srv.mandateSeriesFailure(bindMandateHistoryNeed(coreSeriesNeed(k)).Key); err != refusal.Error() || !at.Equal(mandateTestNow) {
		t.Fatalf("after a down gateway: %q at %s", err, at)
	}
	dates := coreTestDatesOn(t, marketcal.MarketUKLSE)
	srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, _ time.Time) (*rpc.MarketHistoryResult, error) {
		out := &rpc.MarketHistoryResult{Interval: "1 day", AsOf: mandateTestNow, Source: "test", Contract: p.Contract}
		for i, d := range dates[len(dates)-5:] {
			day, _ := time.Parse(time.DateOnly, d)
			out.Points = append(out.Points, rpc.MarketHistoryPoint{At: day, Value: 100 + float64(i)})
		}
		return out, nil
	}
	if !srv.readMandateSeries(t.Context(), coreSeriesNeed(k), mandateTestNow.Add(2*time.Minute)) {
		t.Fatal("the read did not succeed")
	}
	if err, _ := srv.mandateSeriesFailure(bindMandateHistoryNeed(coreSeriesNeed(k)).Key); err != "" {
		t.Fatalf("a successful read left the refusal: %q", err)
	}
	if got := srv.mandateHistoryForNeed(t.Context(), coreSeriesNeed(k)); got == nil || got.Market != marketcal.MarketUKLSE {
		t.Fatalf("the read is not stamped with London's calendar: %+v", got)
	}
}

// Held, the fund's London line is the fund (design review B1 and S5): it
// takes the 12% broad-index gap and moves on SPY's returns, matched by the
// contract IBKR lists under the ISIN, not by the symbol; the book reads no
// series of its own for it, and an order on it is measured. Without
// [mandate.core] the same symbol is a single name with its own series.
func TestMandateMeasuresTheFundsLondonLineAsTheIndex(t *testing.T) {
	pos := mandateTestPositions(100)
	pos.Stocks = append(pos.Stocks, rpc.PositionView{Symbol: "IXSE", SecType: rpc.SecTypeStock, ConID: 9031, Currency: "EUR", PrimaryExch: "LSEETF", Quantity: 200, Multiplier: 1, Mark: 50})
	srv := coreLondonServer(t, pos, true)
	for _, need := range srv.mandateHistoryNeedsWithRequests(t.Context(), pos, "EUR", mandateTestNow) {
		if need.Key == "u:IXSE" {
			t.Fatalf("the book reads a series for the fund's line: %+v", need)
		}
	}
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book with the London line: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	var line *mandate.UnderlyingReport
	for i, u := range res.Verdict.Underlyings {
		if u.Underlying == "IXSE" {
			line = &res.Verdict.Underlyings[i]
		}
	}
	if line == nil || !line.BroadIndex || line.GapShockPct != 12 || math.Abs(line.Beta-1) > 1e-12 {
		t.Fatalf("the London line is not measured as the index: %+v", line)
	}
	buy := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, LimitPrice: 50, Contract: rpc.ContractParams{ConID: 9031, Symbol: "IXSE", SecType: "STK", Currency: "EUR", PrimaryExch: "LSEETF"}}
	if blocker := srv.mandateBudgetGate(buy, mandateOpen); blocker != nil {
		t.Fatalf("a buy of the London line: %+v", blocker)
	}
	if got := srv.mandateHistoryRequests(mandateTestNow); len(got) != 0 {
		t.Fatalf("a buy of the London line requested a series: %+v", got)
	}
	// The core's own target sees the holding.
	if core := coreEvaluate(t, srv); core.Holding == nil || core.Holding.Shares != 200 || core.Holding.PriceSource != "mark" {
		t.Fatalf("core holding %+v", core.Holding)
	}
	// Without the core table the symbol is a single name.
	plain, _ := mandateTestServer(t, testMandateConstitution())
	if needs := plain.mandateHistoryNeedsWithRequests(t.Context(), pos, "EUR", mandateTestNow); !slices.ContainsFunc(needs, func(n mandateHistoryNeed) bool { return n.Key == "u:IXSE" }) {
		t.Fatalf("without the core the line reads its own series: %+v", needs)
	}
	if mandate.IsBroadIndexLine("IXSE", "EUR", "") || !mandate.IsBroadIndexLine("IXSE", "EUR", core.ISIN) || mandate.ReturnsSeriesFor("IXSE", "EUR", core.ISIN) != mandate.IndexSymbol || mandate.ReturnsSeriesFor("IXSE", "GBP", core.ISIN) != "IXSE" {
		t.Fatal("ISIN matching")
	}
}

// D5 (owner decision 2026-10-18 08:06 CEST): protection's automatic sales
// leave out the core's contract. Its trailing stop is still served for the
// owner's hand but gets no automatic record and does not read
// pre-authorised; a record made before the core used the line is
// superseded; the mandate's forced reduction on the same line keeps its
// record; another line's stop is unchanged.
func TestProtectionLeavesTheCoreToTheForcedReduction(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, `pre_authorised = ["trailing_stop", "mandate_reduction"]`)
	policy, status := rig.server.protectionPolicies.Active()
	coreRow := automaticTestStockRow()
	coreRow.ConID, coreRow.Symbol = 103, "SYC"
	coreStop, ok := trailingStopStockProposal(policy, status, coreRow, rpc.TradeProposalSourceFingerprints{}, rig.now, true, 0.01)
	if !ok {
		t.Fatal("no trailing stop for the core line")
	}
	otherStop := rig.stopProposal()
	// Before the core uses the line, its stop gets a record like any other.
	revision := rig.install(coreStop, otherStop)
	rig.cycle()
	if rec := rig.record(coreStop.Key, revision); rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("before the core: %+v", rec)
	}
	// The core resolves the line.
	rig.server.riskPolicies = &riskPolicyManager{active: testCoreConstitution()}
	if err := rig.server.rememberCore(t.Context(), &coreContract{Version: coreContractDocVer, ISIN: core.ISIN, ConID: 103, Symbol: "SYC", PrimaryExch: "IBIS", Currency: core.Currency, Market: marketcal.MarketDEXetra}); err != nil {
		t.Fatal(err)
	}
	rig.advance(time.Minute)
	rig.cycle()
	if rec := rig.record(coreStop.Key, revision); rec.State != rpc.TradeProposalAutomaticSuperseded || rec.Reason != automaticCoreExcludedReason {
		t.Fatalf("the core's waiting stop: %+v", rec)
	}
	if rec := rig.record(otherStop.Key, revision); rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("another line's stop: %+v", rec)
	}
	// A new revision of the core's stop gets no record and reads manual.
	coreStop.Quantity--
	reduction := rig.mandateProposal(coreRow, 40, 1, 1)
	revision = rig.install(coreStop, otherStop, reduction)
	rig.cycle()
	rig.noRecord(coreStop.Key, revision)
	if rec := rig.record(reduction.Key, revision); rec.State != rpc.TradeProposalAutomaticPending || rec.Bucket != preAuthorisedBucketMandateReduction {
		t.Fatalf("the forced reduction on the core line: %+v", rec)
	}
	for _, p := range rig.engine.Snapshot(false).Proposals {
		if p.Key == coreStop.Key && p.Automatic != nil && p.Automatic.PreAuthorised {
			t.Fatalf("the core's stop reads pre-authorised: %+v", p.Automatic)
		}
	}
	// The final gate refuses it too.
	if blockers := rig.engine.automaticSubmitBlockers(coreStop); len(blockers) == 0 {
		t.Fatal("the submit gate admits an automatic sale of the core line")
	}
}
