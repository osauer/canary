package canary

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// A portfolio fit of "low" must be a measurement, never a default reached
// because the exposure-measuring signals were blocked or data-quality.
func TestCanaryPortfolioFitUnknownWhenExposureSignalsAreDataBlocked(t *testing.T) {
	p := StressPortfolioSummary{NetLiquidation: 100000}
	greeksBlind := []risk.Signal{{ID: risk.SignalOptionGreeksDegraded, Direction: risk.DirectionDataQuality}}
	if got := canaryPortfolioFit(p, greeksBlind); got != canaryPortfolioFitUnknown {
		t.Fatalf("data-quality greeks signal must yield unknown fit, got %q", got)
	}
	blockedExposure := []risk.Signal{{ID: risk.SignalGrossExposureHigh, BlockedBy: []string{"positions_stale"}}}
	if got := canaryPortfolioFit(p, blockedExposure); got != canaryPortfolioFitUnknown {
		t.Fatalf("blocked exposure signal must yield unknown fit, got %q", got)
	}
	if got := canaryPortfolioFit(p, nil); got != canaryPortfolioFitLow {
		t.Fatalf("a measurable quiet book stays low, got %q", got)
	}
	measured := []risk.Signal{{ID: risk.SignalMarginCushionLow}}
	if got := canaryPortfolioFit(p, measured); got != canaryPortfolioFitMedium {
		t.Fatalf("measured medium signal must yield medium fit, got %q", got)
	}
}

func TestCanaryUnknownFitNeverClaimsLowExposure(t *testing.T) {
	r := StressResult{Action: canaryActionWatch, PortfolioFit: canaryPortfolioFitUnknown, MarketConfirmation: canaryMarketPartial}
	sum := canaryDecisionSummary(r)
	if strings.Contains(sum, "exposure is low") || strings.Contains(sum, "no reductions needed") {
		t.Fatalf("unknown fit must not claim low exposure: %q", sum)
	}
	if !strings.Contains(sum, "could not be measured") {
		t.Fatalf("unknown fit summary must disclose the blind spot: %q", sum)
	}
	if dir, sev := canaryDecisionState(canaryMarketPartial, canaryPortfolioFitUnknown, canaryInputDegraded, StressMarketSummary{}, nil); dir != risk.DirectionDefensive || sev != risk.SeverityWatch {
		t.Fatalf("partial market with unknown fit must stay defensive watch, got %v/%v", dir, sev)
	}
}

// A never-fetched positions snapshot (zero AsOf) against a real account must
// be a positions source issue, not a silent pass through the staleness gate:
// with GrossPositionValue missing and a real stock-only book, fit must derive
// unknown via the data-blocked path, never fall through to low.
func TestComputeCanaryNeverFetchedPositionsIsSourceIssueNotLowFit(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.GrossPositionValue = 0
	res := ComputeStress(StressInput{Now: canaryTestNow,
		Account: acct,
		Positions: rpc.PositionsResult{ // real stock-only book, AsOf never stamped
			Stocks: []rpc.PositionView{{Symbol: "AAPL", SecType: "STK", Quantity: 200}},
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "AAPL", MarketValueBase: 40_000, MarketValuePctNLV: new(40.0),
				}},
			},
		},
		Regime: healthyCanaryRegime(),
	})
	if res.PortfolioFit != canaryPortfolioFitUnknown {
		t.Fatalf("portfolio_fit = %q, want unknown when the positions snapshot was never fetched", res.PortfolioFit)
	}
	if res.InputHealth != canaryInputDegraded {
		t.Fatalf("input_health = %q, want degraded for never-fetched positions", res.InputHealth)
	}
	if res.Action != canaryActionConfirmInputs {
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
