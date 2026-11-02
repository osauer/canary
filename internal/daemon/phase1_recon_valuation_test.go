package daemon

import (
	"fmt"
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A foreign unit rate is the suspicious provider value for which the normal
// account projection already requires independent FX resolution. Capturing it
// as an observed field must not promote it into historical valuation proof.
func TestOperationalReconCaptureForeignUnitFXNeedsIndependentEvidence(t *testing.T) {
	_, _, sample := reconValuationFixture()
	raw := &ibkrlib.RawAccountSummary{
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		NetLiquidation: new(sample.EquityBase),
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{
			"EUR": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true},
			"USD": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true,
				StockMarketValue: 10000, StockMarketValueText: "10000",
				ExchangeRate: 1, ExchangeRateObserved: true},
		},
	}
	positions := []*ibkrlib.RawPosition{{ValuationAt: sample.AsOf,
		Contract: ibkrlib.Contract{ConID: 101, SecType: "STK", Currency: "USD"},
		Position: 100, MarketPrice: 100, MarketValue: 10000}}
	got := captureReconValuation(raw, positions, sample.AsOf)
	if got == nil {
		t.Fatal("missing basis must retain the original account reading")
	}
	if got.EquityBase != sample.EquityBase || got.Currencies["USD"].FXRate != 1 || got.Currencies["USD"].Equity != 50000 {
		t.Fatalf("raw broker evidence was rewritten: %+v", got)
	}
	if got.Reason == "" {
		t.Error("foreign unit FX with no independent proof was admitted as complete historical basis")
	}
}

// Old durable samples predate the capture guard. Their empty Reason must not
// grandfather suspect FX into the automatic policy-extension decision.
func TestOperationalReconLegacyForeignUnitFXCannotEraseEquityDifference(t *testing.T) {
	for _, equity := range []float64{100000, 98000} {
		t.Run(fmt.Sprintf("equity_%g", equity), func(t *testing.T) {
			row, statement, legacy := reconValuationFixture()
			row.TotalBase, statement.FX.NAV = 95000, 95000
			statement.Equity[0] = row
			statement.FX.Rates["USD"] = .9
			legacy.EquityBase = equity
			legacy.Currencies["USD"] = rpc.ReconNativeValuation{Equity: 50000, FXRate: 1}
			legacy.Reason = ""

			check := compareReconValuation(row, statement, legacy)
			wantRaw := (legacy.EquityBase - row.TotalBase) / row.TotalBase * 100
			if check.RuntimeEquityBase == nil || *check.RuntimeEquityBase != legacy.EquityBase ||
				check.RawDivergencePct == nil || math.Abs(*check.RawDivergencePct-wantRaw) > 1e-10 ||
				check.Evidence == nil || check.Evidence.Runtime.Currencies["USD"].FXRate != 1 ||
				check.Evidence.UnallocatedRuntimeBase != equity-100000 {
				t.Fatalf("unverified comparison lost the raw difference, residual or broker evidence: %+v", check)
			}
			if check.Comparability == "comparable" || check.DivergencePct != nil || check.ComparedRuntimeBase != nil {
				t.Errorf("unverified foreign unit FX manufactured a policy comparison: %+v", check)
			}
			pol := newReconV3TestServer(t).riskPolicies.snapshot().policy
			report := cleanReconReport(legacy.AsOf)
			report.Equity = check
			hold, held := autoExtendHoldFor(rpc.RiskPolicyStatusActive, pol, report, legacy.AsOf)
			if !held || hold.State != rpc.ReconEvaluationStateWaiting || hold.Reason != rpc.ReconEvaluationReasonAccountValuePending {
				t.Errorf("unverified legacy basis must wait for evidence, not extend or assert a proved mismatch: held=%t hold=%+v", held, hold)
			}
		})
	}
}
