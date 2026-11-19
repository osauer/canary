package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The broker keeps an option's daily closes only while the contract is
// alive, so Edge fetches them for live contracts and never for expired ones.
func TestEdgeContractsIncludeLiveOptionsOnly(t *testing.T) {
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	traded := now.AddDate(0, 0, -5)
	statement := flexstmt.Statement{
		Instruments: []flexstmt.Instrument{{ConID: 2, Expiry: "2026-03-20"}, {ConID: 3, Expiry: "20260227"}, {ConID: 4, Expiry: "2026-03-02"}},
		Trades: []flexstmt.Trade{
			{ConID: 1, Symbol: "SYNX", AssetClass: "STK", Currency: "USD", ExecutedAt: traded},
			{ConID: 2, Symbol: "SYNX 260320C00100000", AssetClass: "OPT", Currency: "USD", ExecutedAt: traded},
			{ConID: 3, Symbol: "SYNX 260227C00100000", AssetClass: "OPT", Currency: "USD", ExecutedAt: traded},
			{ConID: 4, Symbol: "SYNX 260302C00100000", AssetClass: "OPT", Currency: "USD", ExecutedAt: traded},
			{ConID: 5, Symbol: "SYNX 260320P00090000", AssetClass: "OPT", Currency: "USD", ExecutedAt: traded},
		},
	}
	got := edgeContracts([]flexstmt.Statement{statement}, now.AddDate(0, 0, -30), now)
	want := []edgeContractSpec{{ConID: 1, Symbol: "SYNX", Currency: "USD"}, {ConID: 2, Symbol: "SYNX 260320C00100000", Currency: "USD", Option: true}, {ConID: 4, Symbol: "SYNX 260302C00100000", Currency: "USD", Option: true}}
	if len(got) != len(want) {
		t.Fatalf("contracts=%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("contract %d=%+v want %+v", i, got[i], want[i])
		}
	}
}

// The recent list is newest first and bounded, and a publication written
// before verdicts existed reads as no verdict instead of failing validation.
func TestEdgeVerdictsListNewestFirst(t *testing.T) {
	impact := 12.0
	in := edgecore.Result{Verdicts: []edgecore.VerdictCount{{AssetClass: "OPT", Helped: 1, None: 1}}}
	for i := range rpc.MaxEdgeRecentVerdicts + 5 {
		change := edgecore.Change{ID: "change_" + time.Duration(i).String(), Action: edgecore.ActionAdd, Direction: edgecore.DirectionLong, AssetClass: "OPT", ExecutedAt: time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)}
		if i%2 == 0 {
			change.Verdict = edgecore.Verdict{Label: edgecore.VerdictHelped, Basis: edgecore.VerdictBasisHorizon, Sessions: 5, ImpactBase: &impact}
		}
		in.Changes = append(in.Changes, change)
	}
	out := rpcEdgeVerdicts(in)
	if len(out.Recent) != rpc.MaxEdgeRecentVerdicts || !out.Recent[0].ExecutedAt.After(out.Recent[1].ExecutedAt) || out.Recent[0].Verdict.Label != edgecore.VerdictHelped || out.Recent[1].Verdict.Label != edgecore.VerdictNone {
		t.Fatalf("recent=%+v", out.Recent[:2])
	}
	if len(out.Counts) != 1 || out.Counts[0].Helped != 1 {
		t.Fatalf("counts=%+v", out.Counts)
	}
}

// An option decision with no close on record is a missing horizon. It must
// not hold the publication in backfilling: nothing is left to fetch for it,
// and a console that waits for "current" would show no Edge at all.
func TestOptionWithoutACloseDoesNotKeepEdgeBackfilling(t *testing.T) {
	coverage := edgecore.Coverage{TradeChanges: 2, EligibleChanges: 2, PresentSections: []string{"trades"}, ReasonCounts: map[string]int{edgecore.ReasonMissingHorizon: 2}}
	if state, reason := edgePublicationStatus(map[string]edgecore.Result{"365d": {Coverage: coverage}}, 0); state != rpc.EdgeStateCurrent || reason != "" {
		t.Fatalf("state=%s reason=%s", state, reason)
	}
	quantity, price, multiplier, zero := 1.0, 2.0, 100.0, 0.0
	executed := time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)
	statement := flexstmt.Statement{
		AccountID: "U", FromDate: executed.AddDate(0, 0, -30), ToDate: executed.AddDate(0, 0, 20), WhenGenerated: executed.AddDate(0, 0, 21), ManifestVersion: flexstmt.ManifestVersion,
		Trades:    []flexstmt.Trade{{RecordID: "t1", AccountID: "U", ConID: 9, Symbol: "SYNX 260320C00100000", AssetClass: "OPT", Currency: "EUR", OrderID: "o1", ExecutionID: "e1", ExecutedAt: executed, Side: "BUY", OpenClose: "O", Quantity: &quantity, Price: &price, Multiplier: &multiplier, Commission: &zero, Taxes: &zero, LevelOfDetail: "EXECUTION"}},
		Positions: []flexstmt.OpenPosition{{RecordID: "p1", AccountID: "U", ConID: 9, AssetClass: "OPT", ReportDate: executed.AddDate(0, 0, 20), Quantity: &quantity}},
	}
	for _, section := range flexstmt.CanonicalQueryManifest() {
		statement.Coverage = append(statement.Coverage, flexstmt.SectionCoverage{Key: section.Key, Present: true})
	}
	result, err := edgecore.Analyze(edgecore.Input{AsOf: executed.AddDate(0, 0, 25), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{statement}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage.TradeChanges != 1 || result.Coverage.ReasonCounts[edgecore.ReasonMarketDataUnavailable] != 0 {
		t.Fatalf("coverage=%+v", result.Coverage)
	}
	if state, reason := edgePublicationStatus(map[string]edgecore.Result{"365d": result}, 0); state != rpc.EdgeStateCurrent {
		t.Fatalf("an option without a close kept Edge %s (%s)", state, reason)
	}
}

// A live option's closes are asked for by its contract id alone: no symbol,
// so the read cannot settle on the underlying stock's contract, and at most a
// year. A stock keeps its symbol-keyed read. (The first version blanked the
// symbol on the stock read, which refuses before sending; nothing was ever
// requested, and nobody saw it because an option's error is dropped.)
func TestEdgeOptionClosesAreRequestedByExactContract(t *testing.T) {
	var asked []ibkrlib.Contract
	var lookbacks []int
	srv := &Server{edgeFetchBarsFn: func(_ context.Context, contract ibkrlib.Contract, lookback int) ([]ibkrlib.HistoricalBar, error) {
		asked = append(asked, contract)
		lookbacks = append(lookbacks, lookback)
		return []ibkrlib.HistoricalBar{{Date: "20260302", Time: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), Close: 2.4}}, nil
	}}
	bars, err := srv.fetchEdgeBars(context.Background(), edgeContractSpec{ConID: 2, Symbol: "SYNX 260320C00100000", Currency: "USD", Option: true}, edgeFullLookbackDays)
	if err != nil || len(bars) != 1 || bars[0].ConID != 2 || bars[0].Close != 2.4 {
		t.Fatalf("option closes: bars %+v, err %v", bars, err)
	}
	if len(asked) != 1 || asked[0].ConID != 2 || asked[0].SecType != "OPT" || asked[0].Symbol != "" || asked[0].Currency != "USD" {
		t.Fatalf("the option was asked for as %+v, want its contract id, OPT and no symbol", asked)
	}
	if _, err := srv.fetchEdgeBars(context.Background(), edgeContractSpec{ConID: 1, Symbol: "SYNX", Currency: "USD"}, edgeFullLookbackDays); err != nil {
		t.Fatalf("stock closes: %v", err)
	}
	if len(asked) != 2 || asked[1].SecType != "STK" || asked[1].Symbol != "SYNX" || lookbacks[1] != edgeFullLookbackDays {
		t.Fatalf("the stock was asked for as %+v with lookback %v", asked[1], lookbacks)
	}
}

// An option whose closes could not be read is asked again after an hour, not
// on every rebuild: a rebuild runs on each statement fetch and each broker
// reconnect, and a failing contract would otherwise be asked for a year of
// bars each time. A stock is never held back this way.
func TestEdgeOptionFetchWaitsAfterAFailure(t *testing.T) {
	now := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	option := edgeContractSpec{ConID: 2, Option: true}
	for name, tc := range map[string]struct {
		spec   edgeContractSpec
		series edgeBarSeries
		want   bool
	}{
		"never asked":               {option, edgeBarSeries{}, true},
		"failed ten minutes ago":    {option, edgeBarSeries{FailedAt: now.Add(-10 * time.Minute), FailedRead: edgeOptionReadVersion}, false},
		"an older read just failed": {option, edgeBarSeries{FailedAt: now.Add(-10 * time.Minute), FailedRead: edgeOptionReadVersion - 1}, true},
		"failed two hours ago":      {option, edgeBarSeries{FailedAt: now.Add(-2 * time.Hour), FailedRead: edgeOptionReadVersion}, true},
		"a stock that just failed":  {edgeContractSpec{ConID: 1}, edgeBarSeries{FailedAt: now.Add(-time.Minute)}, true},
	} {
		if got := edgeFetchDueAfterFailure(now, tc.spec, tc.series); got != tc.want {
			t.Fatalf("%s: due=%v, want %v", name, got, tc.want)
		}
	}
}

// A switch to another account or mode starts the bar cache afresh, as it
// always did, but an option's closes are set aside, not dropped: the broker
// deletes them at expiry and they can never be fetched again. They come back
// when the first account does, and are never handed to another account's
// analysis.
func TestEdgeBarCacheKeepsOptionClosesAcrossAScopeSwitch(t *testing.T) {
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	live := edgeBarCache{Version: edgeBarCacheVersion, ScopeFingerprint: "live", LastFullRevalidation: day,
		Contracts: map[string]edgeBarSeries{
			"1": {ConID: 1, Symbol: "SYNX", Bars: []edgecore.DailyBar{{ConID: 1, Day: day, Close: 100}}},
			"2": {ConID: 2, Option: true, Bars: []edgecore.DailyBar{{ConID: 2, Day: day, Close: 2.4}}},
		},
		MarketContext: map[string]edgeBarSeries{"SPY": {Symbol: "SPY"}}}

	paper := edgeBarCacheForScope(live, "paper")
	if paper.ScopeFingerprint != "paper" || len(paper.Contracts) != 0 || len(paper.MarketContext) != 0 || !paper.LastFullRevalidation.IsZero() {
		t.Fatalf("the other scope did not start afresh: %+v", paper)
	}
	if kept := paper.Parked["live"]; len(kept) != 1 || kept["2"].Bars[0].Close != 2.4 {
		t.Fatalf("the option's closes were not set aside: %+v", paper.Parked)
	}

	back := edgeBarCacheForScope(paper, "live")
	if got := back.Contracts["2"]; len(back.Contracts) != 1 || !got.Option || got.Bars[0].Close != 2.4 {
		t.Fatalf("the option's closes did not come back: %+v", back.Contracts)
	}
	if len(back.Parked["live"]) != 0 {
		t.Fatalf("restored closes are still set aside: %+v", back.Parked)
	}
	if same := edgeBarCacheForScope(live, "live"); len(same.Contracts) != 2 || len(same.Parked) != 0 {
		t.Fatalf("an unchanged scope lost or parked series: %+v", same)
	}
}

// An option stays on the list through its expiry day in New York. Counted in
// UTC it left at 19:00 New York, three hours after the close its last bar
// forms at.
func TestEdgeContractsKeepAnOptionThroughItsExpiryDayInNewYork(t *testing.T) {
	traded := time.Date(2026, 2, 25, 15, 0, 0, 0, time.UTC)
	statement := flexstmt.Statement{
		Instruments: []flexstmt.Instrument{{ConID: 4, Expiry: "2026-03-02"}},
		Trades:      []flexstmt.Trade{{ConID: 4, Symbol: "SYNX 260302C00100000", AssetClass: "OPT", Currency: "USD", ExecutedAt: traded}},
	}
	evening := time.Date(2026, 3, 3, 2, 0, 0, 0, time.UTC) // 21:00 on 2 March in New York
	if got := edgeContracts([]flexstmt.Statement{statement}, traded.AddDate(0, 0, -30), evening); len(got) != 1 || !got[0].Option {
		t.Fatalf("the option left the list on its expiry evening: %+v", got)
	}
	nextDay := time.Date(2026, 3, 3, 6, 0, 0, 0, time.UTC) // 01:00 on 3 March in New York
	if got := edgeContracts([]flexstmt.Statement{statement}, traded.AddDate(0, 0, -30), nextDay); len(got) != 0 {
		t.Fatalf("an expired option is still listed: %+v", got)
	}
}

// A result that carries only a state serves empty lists, never null, for the
// verdicts and the option decisions, as it does for every other list.
func TestEdgeStateOnlyResultServesEmptyVerdictLists(t *testing.T) {
	raw, err := json.Marshal(edgeStateOnlyResult(rpc.EdgeStateBackfilling, "", "365d", 20, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"verdicts":{"counts":[],"recent":[]}`, `"decisions":{"changes":0,"with_outcome":0,"reasons":{},"actions":[]}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}

// When the first three option reads of a rebuild all fail and none has
// answered, the read itself is taken to be failing and the rest of the list
// is not sent after it. One answer keeps the list going: a thin contract
// with no trade is an ordinary failure.
func TestEdgeOptionReadsStopWhenTheFirstThreeAllFail(t *testing.T) {
	for _, tc := range []struct {
		failed, answered int
		want             bool
	}{{0, 0, false}, {2, 0, false}, {3, 0, true}, {20, 0, true}, {3, 1, false}, {20, 1, false}} {
		if got := edgeOptionReadLooksBroken(tc.failed, tc.answered); got != tc.want {
			t.Fatalf("failed=%d answered=%d: broken=%v, want %v", tc.failed, tc.answered, got, tc.want)
		}
	}
}
