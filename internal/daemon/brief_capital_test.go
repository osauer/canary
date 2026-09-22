package daemon

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// approvedTestConstitution is a synthetic, fully decided version-1 policy:
// every material key carries a value, so UnapprovedKeys is empty. The numbers
// are fixture arithmetic only, never a recommendation.
func approvedTestConstitution() *risk.Constitution {
	return &risk.Constitution{
		Kind: risk.ConstitutionKind, SchemaVersion: 1, PolicyID: "risk-constitution", PolicyVersion: 1,
		Capital: risk.ConstitutionCapital{
			BaseCurrency: "EUR", ProtectedFloor: new(80000.0), DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240), MaxUnreconciledDays: new(45),
		},
		Drawdown: risk.ConstitutionDrawdown{WarnConsumedPct: new(10.0), BlockConsumedPct: new(20.0), BlockEnforcement: risk.EnforcementAdvisory},
		Override: risk.ConstitutionOverride{MaxDurationHours: new(72)},
		Recon: risk.ConstitutionRecon{
			AmountTolerancePct: new(1.0), AmountToleranceMin: new(25.0), DateWindowBusinessDays: new(3), MaxReportAgeDays: new(7),
		},
	}
}

func TestBriefCapitalRowCarriesConstitutionFiguresOnlyWhenApproved(t *testing.T) {
	now := time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC)
	effective := 42000.0
	report := rpc.CapitalStateReport{Tier: risk.CapitalTierOK, Enforcement: risk.EnforcementAdvisory, EffectiveRiskCapitalBase: &effective, BaseCurrency: "EUR"}

	// Approved: every figure is served and the effective base comes from the verdict.
	approved := composeBriefRisk(&rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusActive, Capital: report}, approvedTestConstitution(), now)
	row := approved.Capital
	if row.WarnPct == nil || row.BlockPct == nil || row.ProtectedFloorBase == nil || row.DeclaredRiskCapitalBase == nil || row.EffectiveRiskCapitalBase == nil {
		t.Fatalf("approved constitution left figures nil: %+v", row)
	}
	if *row.WarnPct != 10 || *row.BlockPct != 20 || *row.ProtectedFloorBase != 80000 || *row.DeclaredRiskCapitalBase != 50000 || *row.EffectiveRiskCapitalBase != 42000 {
		t.Fatalf("figures = warn %v block %v floor %v declared %v effective %v", *row.WarnPct, *row.BlockPct, *row.ProtectedFloorBase, *row.DeclaredRiskCapitalBase, *row.EffectiveRiskCapitalBase)
	}
	// The JSON names are the wire contract Desk reads.
	raw, _ := json.Marshal(row)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"warn_pct", "block_pct", "protected_floor_base", "declared_risk_capital_base", "effective_risk_capital_base"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("capital row JSON lacks %s: %s", key, raw)
		}
	}

	// Unapproved: one missing material key hides every figure, including the
	// ones the file does carry, and the tier reads unapproved.
	partial := approvedTestConstitution()
	partial.Capital.DeclaredRiskCapital = nil
	unapprovedReport := report
	unapprovedReport.Tier, unapprovedReport.EffectiveRiskCapitalBase = risk.CapitalTierUnapproved, nil
	unapproved := composeBriefRisk(&rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusActive, Capital: unapprovedReport, Unapproved: partial.UnapprovedKeys()}, partial, now)
	row = unapproved.Capital
	if row.WarnPct != nil || row.BlockPct != nil || row.ProtectedFloorBase != nil || row.DeclaredRiskCapitalBase != nil || row.EffectiveRiskCapitalBase != nil {
		t.Fatalf("unapproved constitution served figures: %+v", row)
	}
	raw, _ = json.Marshal(row)
	wire = map[string]any{}
	_ = json.Unmarshal(raw, &wire)
	for _, key := range []string{"warn_pct", "block_pct", "protected_floor_base", "declared_risk_capital_base", "effective_risk_capital_base"} {
		if _, ok := wire[key]; ok {
			t.Fatalf("unapproved capital row JSON still carries %s: %s", key, raw)
		}
	}

	// Absent: no constitution at all leaves the row unavailable and bare.
	absent := composeBriefRisk(&rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusAbsent}, nil, now)
	if absent.Capital.DeclaredRiskCapitalBase != nil || absent.Capital.Status != rpc.BriefStatusUnavailable {
		t.Fatalf("absent constitution row = %+v", absent.Capital)
	}
}

func TestBriefPremiumAtRiskPctOfRiskCapital(t *testing.T) {
	declared, premium := 50000.0, 12500.0
	capital := rpc.BriefCapitalRow{DeclaredRiskCapitalBase: &declared}
	row := rpc.BriefMoneyCoverageRow{AmountBase: &premium}
	ready := composeBriefReady(rpc.BriefMarketSection{}, rpc.BriefCalendarSection{}, rpc.BriefRiskSection{Capital: capital},
		rpc.BriefPortfolioSection{PremiumAtRisk: row}, rpc.BriefProcessSection{}, rpc.BriefReadyProposalsRow{})
	if ready.PremiumAtRisk.PctOfRiskCapital == nil || math.Abs(*ready.PremiumAtRisk.PctOfRiskCapital-25) > 1e-9 {
		t.Fatalf("pct_of_risk_capital = %v, want 25", ready.PremiumAtRisk.PctOfRiskCapital)
	}
	// A hedge-cost row shares the type and never carries the percentage.
	if ready.HedgeCost.PctOfRiskCapital != nil {
		t.Fatalf("hedge cost row grew a risk-capital percentage: %+v", ready.HedgeCost)
	}
	// Either side missing: nil, not zero.
	for name, in := range map[string]struct {
		capital rpc.BriefCapitalRow
		premium rpc.BriefMoneyCoverageRow
	}{
		"no premium amount":  {capital: capital},
		"no declared budget": {premium: row},
	} {
		out := composeBriefReady(rpc.BriefMarketSection{}, rpc.BriefCalendarSection{}, rpc.BriefRiskSection{Capital: in.capital},
			rpc.BriefPortfolioSection{PremiumAtRisk: in.premium}, rpc.BriefProcessSection{}, rpc.BriefReadyProposalsRow{})
		if out.PremiumAtRisk.PctOfRiskCapital != nil {
			t.Fatalf("%s: pct_of_risk_capital = %v, want nil", name, *out.PremiumAtRisk.PctOfRiskCapital)
		}
	}
	// A measured zero premium over an approved budget is a genuine 0%.
	zero := 0.0
	out := composeBriefReady(rpc.BriefMarketSection{}, rpc.BriefCalendarSection{}, rpc.BriefRiskSection{Capital: capital},
		rpc.BriefPortfolioSection{PremiumAtRisk: rpc.BriefMoneyCoverageRow{AmountBase: &zero}}, rpc.BriefProcessSection{}, rpc.BriefReadyProposalsRow{})
	if out.PremiumAtRisk.PctOfRiskCapital == nil || *out.PremiumAtRisk.PctOfRiskCapital != 0 {
		t.Fatalf("zero premium pct = %v, want 0", out.PremiumAtRisk.PctOfRiskCapital)
	}
}
