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

// No option close is requested from the broker. The connector's daily-bar
// read is keyed by symbol: without one it refuses before sending, which is
// what the first version did on every rebuild without anyone seeing it, and
// with the underlying's it would settle on the stock's cached contract and
// return the stock's closes as the option's. Until an exact-contract read
// exists, the statement marks are the only option closes.
func TestEdgeOptionCloseIsNotRequestedFromTheBroker(t *testing.T) {
	asked := 0
	srv := &Server{edgeFetchBarsFn: func(context.Context, ibkrlib.Contract, int) ([]ibkrlib.HistoricalBar, error) {
		asked++
		return nil, nil
	}}
	if _, err := srv.fetchEdgeBars(context.Background(), edgeContractSpec{ConID: 2, Symbol: "SYNX 260320C00100000", Currency: "USD", Option: true}, edgeFullLookbackDays); err == nil {
		t.Fatal("an option close fetch reported success")
	}
	if asked != 0 {
		t.Fatalf("the broker was asked %d times for an option's closes", asked)
	}
	// A stock still goes to the broker (the stub's empty answer is refused
	// afterwards, which is not what this test is about).
	if _, _ = srv.fetchEdgeBars(context.Background(), edgeContractSpec{ConID: 1, Symbol: "SYNX", Currency: "USD"}, edgeFullLookbackDays); asked != 1 {
		t.Fatalf("the broker was asked %d times for a stock's closes, want once", asked)
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
