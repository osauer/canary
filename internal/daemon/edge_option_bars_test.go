package daemon

import (
	"testing"
	"time"

	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
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
