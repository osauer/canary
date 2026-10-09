package risk

import "testing"

func existingPolicyAddFixture() StockAddInput {
	in := stockAddFixture()
	in.Policy = nil
	// Each witness isolates the policy measurement it exercises.
	for id := range in.Rulebook.Modes {
		in.Rulebook.Modes[id] = RuleModeOff
	}
	in.Rulebook.Modes[RuleSingleNameExposure] = RuleModeTrack
	in.Rulebook.Modes[RuleDeltaSwing] = RuleModeTrack
	in.Rulebook.Modes[RuleLossBudget] = RuleModeTrack
	return in
}

func TestStockAddUsesExistingPolicyWithoutAllocationTable(t *testing.T) {
	in := existingPolicyAddFixture()
	if got := SizeStockAdd(in); got.Quantity != 199 {
		t.Fatalf("existing policy should permit cash-funded sizing: %+v", got)
	}
}

func TestStockAddIncludesExistingIssuerDeltaBoundary(t *testing.T) {
	in := existingPolicyAddFixture()
	in.Rulebook.DeltaSwingWatchPct = 5
	if got := SizeStockAdd(in); got.Quantity != 49 {
		t.Fatalf("must stop before existing 5%% warning: %+v", got)
	}
}

func TestStockAddUnrelatedIssuerDoesNotConsumeSelectedIssuerAllowance(t *testing.T) {
	in := existingPolicyAddFixture()
	in.Rules.Names = []NameInput{stockLine("SYNB", 900, 100)}
	if got := SizeStockAdd(in); got.Quantity != 199 {
		t.Fatalf("unrelated issuer finding must not block selected issuer: %+v", got)
	}
	in.Rulebook.IssuerGroups = map[string][]string{"synthetic issuer": {"SYNA", "SYNB"}}
	if got := SizeStockAdd(in); got.Quantity != 0 {
		t.Fatalf("same issuer must retain the warning: %+v", got)
	}
}

func TestStockAddManualWarningsNeverOverrideCashOrMissingEvidence(t *testing.T) {
	in := existingPolicyAddFixture()
	in.Manual = true
	in.FreeCash = 50000
	in.OrderCapBase = 50000
	in.Requested = 400
	in.BrokerMargin = &StockAddMargin{Quantity: 400, ExcessLiquidityBase: 50000, InitialMarginBase: 50000, MaintenanceMarginBase: 40000}
	got := CheckStockAdd(in)
	if got.Quantity != 400 || len(got.Warnings) == 0 || got.Warnings[0].Code != RuleDeltaSwing {
		t.Fatalf("manual warning: quantity=%d warnings=%+v blockers=%+v", got.Quantity, got.Warnings, got.Blockers)
	}
	in.FreeCash = 1000
	if got := CheckStockAdd(in); got.Quantity != 0 {
		t.Fatal("warning override bypassed cash")
	}
	in.FreeCash = 50000
	in.Rules.RiskCapital = nil
	if got := CheckStockAdd(in); got.Quantity != 0 {
		t.Fatal("warning override bypassed missing evidence")
	}
}

func TestStockAddProtectionIsMeasuredAndModesAreRespected(t *testing.T) {
	in := existingPolicyAddFixture()
	in.Rulebook.Modes[RuleHedgeIntegrity] = RuleModeTrack
	if got := SizeStockAdd(in); got.Quantity != 0 {
		t.Fatal("new unhedged exposure bypassed protection finding")
	}
	in.Rulebook.Modes[RuleHedgeIntegrity] = RuleModeOff
	if got := SizeStockAdd(in); got.Quantity == 0 {
		t.Fatalf("disabled protection rule silently enforced: %+v", got.Blockers)
	}
}

func TestStockAddOnlyAffectedClustersConstrainEntry(t *testing.T) {
	in := existingPolicyAddFixture()
	in.Rulebook.Modes[RuleClusterStress] = RuleModeTrack
	in.Rules.Names = []NameInput{stockLine("SYNB", 900, 100)}
	in.Rulebook.Clusters = map[string][]string{"unrelated": {"SYNB"}}
	if got := SizeStockAdd(in); got.Quantity == 0 {
		t.Fatalf("unrelated cluster blocked entry: %+v", got.Blockers)
	}
	in.Rulebook.Clusters = map[string][]string{"related": {"SYNA", "SYNB"}}
	if got := SizeStockAdd(in); got.Quantity != 0 {
		t.Fatal("related cluster breach not applied")
	}
}
