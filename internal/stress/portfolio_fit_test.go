package stress

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// A portfolio fit of "low" must be a measurement, never a default reached
// because the exposure-measuring signals were blocked or data-quality.
func TestStressPortfolioFitUnknownWhenExposureSignalsAreDataBlocked(t *testing.T) {
	p := StressPortfolioSummary{NetLiquidation: 100000}
	greeksBlind := []risk.Signal{{ID: risk.SignalOptionGreeksDegraded, Direction: risk.DirectionDataQuality}}
	if got := stressPortfolioFit(p, greeksBlind); got != stressPortfolioFitUnknown {
		t.Fatalf("data-quality greeks signal must yield unknown fit, got %q", got)
	}
	blockedExposure := []risk.Signal{{ID: risk.SignalGrossExposureHigh, BlockedBy: []string{"positions_stale"}}}
	if got := stressPortfolioFit(p, blockedExposure); got != stressPortfolioFitUnknown {
		t.Fatalf("blocked exposure signal must yield unknown fit, got %q", got)
	}
	if got := stressPortfolioFit(p, nil); got != stressPortfolioFitLow {
		t.Fatalf("a measurable quiet book stays low, got %q", got)
	}
	measured := []risk.Signal{{ID: risk.SignalMarginCushionLow}}
	if got := stressPortfolioFit(p, measured); got != stressPortfolioFitMedium {
		t.Fatalf("measured medium signal must yield medium fit, got %q", got)
	}
}

func TestStressUnknownFitNeverClaimsLowExposure(t *testing.T) {
	r := StressResult{Action: stressActionWatch, PortfolioFit: stressPortfolioFitUnknown, MarketConfirmation: stressMarketPartial}
	sum := stressDecisionSummary(r)
	if strings.Contains(sum, "exposure is low") || strings.Contains(sum, "no reductions needed") {
		t.Fatalf("unknown fit must not claim low exposure: %q", sum)
	}
	if !strings.Contains(sum, "could not be measured") {
		t.Fatalf("unknown fit summary must disclose the blind spot: %q", sum)
	}
	if dir, sev := stressDecisionState(stressMarketPartial, stressPortfolioFitUnknown, stressInputDegraded, StressMarketSummary{}, nil); dir != risk.DirectionDefensive || sev != risk.SeverityWatch {
		t.Fatalf("partial market with unknown fit must stay defensive watch, got %v/%v", dir, sev)
	}
}

// A never-fetched positions snapshot (zero AsOf) against a real account must
// be a positions source issue, not a silent pass through the staleness gate:
// with GrossPositionValue missing and a real stock-only book, fit must derive
// unknown via the data-blocked path, never fall through to low.
func TestComputeStressNeverFetchedPositionsIsSourceIssueNotLowFit(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.GrossPositionValue = 0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Positions: rpc.PositionsResult{ // real stock-only book, AsOf never stamped
			Stocks: []rpc.PositionView{{Symbol: "AAPL", SecType: "STK", Quantity: 200}},
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "AAPL", MarketValueBase: 40_000, MarketValuePctNLV: new(40.0),
				}},
			},
		},
		Regime: healthyStressRegime(),
	})
	if res.PortfolioFit != stressPortfolioFitUnknown {
		t.Fatalf("portfolio_fit = %q, want unknown when the positions snapshot was never fetched", res.PortfolioFit)
	}
	if res.InputHealth != stressInputDegraded {
		t.Fatalf("input_health = %q, want degraded for never-fetched positions", res.InputHealth)
	}
	if res.Action != stressActionConfirmInputs {
		t.Fatalf("action = %q, want confirm_inputs instead of trusting an untimestamped book", res.Action)
	}
	blocked := false
	for _, sig := range res.Signals {
		if sig.ID == risk.SignalSingleNameExposureHigh && slices.Contains(sig.BlockedBy, "positions") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected the single-name exposure signal blocked by positions, signals: %+v", res.Signals)
	}
}
