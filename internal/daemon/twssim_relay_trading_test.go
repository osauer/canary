//go:build trading

package daemon

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// A read tool passes on the daemon's bytes instead of this build's encoding
// of them. Against the real daemon on a simulated book the two are the same
// value for every read a relayed tool makes, so a reader at the daemon's own
// version sees what it saw before.
func TestSimulatedTWSReadsEncodeAsTheirTypesDo(t *testing.T) {
	d := startSimDesk(t, twssim.Options{}, true)
	answered := map[string]bool{}
	same := func(method string, params any, typed any) {
		t.Helper()
		var sent string
		if params != nil {
			raw, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			sent = string(raw)
		}
		res := dispatchRaw(t, d.srv, method, sent)
		if !res.Ok {
			t.Logf("%s: not answered on the simulated book: %+v", method, res.Error)
			return
		}
		if err := json.Unmarshal(res.Result, typed); err != nil {
			t.Errorf("%s: this build's type refuses the daemon's own answer: %v", method, err)
			return
		}
		again, err := json.Marshal(typed)
		if err != nil {
			t.Errorf("%s: %v", method, err)
			return
		}
		if !sameJSON(t, res.Result, again) {
			t.Errorf("%s: the daemon's bytes and the typed encoding differ\ndaemon %s\ntyped  %s", method, res.Result, again)
			return
		}
		answered[method] = true
	}
	same(api.AccountSummary.Name, rpc.AccountSummaryParams{}, new(rpc.AccountResult))
	same(api.AccountIntraday.Name, nil, new(rpc.AccountIntradayResult))
	same(api.PositionsList.Name, rpc.PositionsListParams{}, new(rpc.PositionsResult))
	same(api.PositionsRisk.Name, rpc.PositionsListParams{}, new(rpc.PositionsRiskResult))
	same(api.PortfolioSnapshot.Name, nil, new(rpc.PortfolioSnapshotResult))
	same(api.PortfolioPlan.Name, nil, new(rpc.PortfolioPlanResult))
	same(api.BriefSnapshot.Name, rpc.BriefSnapshotParams{}, new(rpc.BriefResult))
	same(api.StatusHealth.Name, rpc.HealthParams{}, new(rpc.HealthResult))
	same(api.TradingStatus.Name, nil, new(rpc.TradingStatus))
	same(api.SettingsGet.Name, nil, new(rpc.PlatformSettings))
	same(api.DataCheck.Name, nil, new(rpc.DataCheckResult))
	same(api.MarketSnapshot.Name, nil, new(rpc.MarketSnapshotResult))
	same(api.MarketTape.Name, rpc.MarketTapeParams{}, new(rpc.MarketTapeResult))
	same(api.MarketRead.Name, nil, new(rpc.MarketReadResult))
	same(api.MarketCalendar.Name, rpc.MarketCalendarParams{}, new(rpc.MarketCalendarResult))
	same(api.MacroSnapshot.Name, rpc.MacroSnapshotParams{}, new(rpc.MacroSnapshotResult))
	same(api.RegimeSnapshot.Name, rpc.RegimeSnapshotParams{}, new(rpc.RegimeSnapshotResult))
	same(api.RegimeMonitor.Name, rpc.RegimeSnapshotParams{}, new(rpc.RegimeMonitorResult))
	same(api.StressSnapshot.Name, rpc.StressSnapshotParams{}, new(rpc.StressSnapshotResult))
	same(api.MandateRisk.Name, rpc.MandateRiskParams{}, new(rpc.MandateRiskResult))
	same(api.CoreTarget.Name, rpc.CoreTargetParams{}, new(rpc.CoreTargetResult))
	same(api.StrategyTarget.Name, rpc.StrategyTargetParams{ID: "core"}, new(rpc.StrategyTargetResult))
	same(api.StrategyOne.Name, nil, new(rpc.StrategyOneResult))
	same(api.ScorecardRead.Name, rpc.ScorecardReadParams{}, new(rpc.ScorecardResult))
	same(api.EdgeSnapshot.Name, rpc.EdgeSnapshotParams{}, new(rpc.EdgeResult))
	same(api.RulesSnapshot.Name, rpc.RulesSnapshotParams{}, new(rpc.RulesResult))
	same(api.TradeProposalsSnapshot.Name, rpc.TradeProposalSnapshotParams{}, new(rpc.TradeProposalSnapshot))
	same(api.OpportunitiesSnapshot.Name, rpc.OpportunitySnapshotParams{}, new(rpc.OpportunitySnapshot))
	same(api.OrdersOpen.Name, rpc.OrdersOpenParams{}, new(rpc.OrdersOpenResult))
	same(api.Technical.Name, rpc.TechnicalParams{Symbols: []string{"SYNA"}}, new(rpc.TechnicalResult))

	// The reads Desk keeps on screen must be among those compared.
	for _, method := range []string{
		api.AccountSummary.Name, api.PositionsList.Name, api.BriefSnapshot.Name, api.StatusHealth.Name,
		api.RegimeSnapshot.Name, api.RegimeMonitor.Name, api.StressSnapshot.Name, api.RulesSnapshot.Name,
		api.TradeProposalsSnapshot.Name, api.OrdersOpen.Name, api.MarketSnapshot.Name,
	} {
		if !answered[method] {
			t.Errorf("%s was not compared", method)
		}
	}
	t.Logf("%d reads compared", len(answered))
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	decode := func(raw []byte) any {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		return v
	}
	return reflect.DeepEqual(decode(a), decode(b))
}
