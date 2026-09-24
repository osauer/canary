package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestBudgetGovernorRequiresObservedFunds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*rpc.AccountResult)
		state string
		rows  int
	}{
		{"missing_funds", func(a *rpc.AccountResult) { a.Authority.Fields.AvailableFunds = false }, rpc.BudgetStateAccountUnavailable, 0},
		{"missing_nlv", func(a *rpc.AccountResult) { a.Authority.Fields.NetLiquidation = false }, rpc.BudgetStateAccountUnavailable, 0},
		{"missing_currency", func(a *rpc.AccountResult) { a.Authority.Fields.BaseCurrency = false }, rpc.BudgetStateAccountUnavailable, 0},
		{"missing_authority", func(a *rpc.AccountResult) { a.Authority = nil }, rpc.BudgetStateAccountUnavailable, 0},
		{"missing_fields", func(a *rpc.AccountResult) { a.Authority.Fields = nil }, rpc.BudgetStateAccountUnavailable, 0},
		{"unavailable", func(a *rpc.AccountResult) { a.Authority.Availability = rpc.AccountDataUnavailable }, rpc.BudgetStateAccountUnavailable, 0},
		{"stale", func(a *rpc.AccountResult) { a.Authority.Freshness = rpc.AccountDataFreshnessStale }, rpc.BudgetStateAccountUnavailable, 0},
		{"unstamped_cache", func(a *rpc.AccountResult) { a.Authority.Freshness = rpc.AccountDataFreshnessUnknown }, rpc.BudgetStateAccountUnavailable, 0},
		{"wrong_account", func(a *rpc.AccountResult) { a.AccountID = "DU0000000" }, rpc.BudgetStateAccountUnavailable, 0},
		{"unknown_mode", func(a *rpc.AccountResult) { a.Authority.Scope.AccountMode = "" }, rpc.BudgetStateAccountUnavailable, 0},
		{"invalid_funds", func(a *rpc.AccountResult) { a.AvailableFunds = math.NaN() }, rpc.BudgetStateAccountUnavailable, 0},
		{"invalid_nlv", func(a *rpc.AccountResult) { a.NetLiquidation = math.Inf(1) }, rpc.BudgetStateAccountUnavailable, 0},
		{"observed_zero", func(*rpc.AccountResult) {}, rpc.BudgetStateOverBudget, 1},
		{"observed_negative", func(a *rpc.AccountResult) { a.AvailableFunds = -100 }, rpc.BudgetStateOverBudget, 1},
		{"within_reserve", func(a *rpc.AccountResult) { a.AvailableFunds = 80000 }, rpc.BudgetStateWithinBudget, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := optionExitTestTime()
			account := &rpc.AccountResult{AccountID: "DU1234567", BaseCurrency: "USD", NetLiquidation: 100000, AsOf: now,
				Authority: &rpc.AccountDataAuthority{
					Scope: rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: rpc.AccountModePaper}, AsOf: now,
					Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
					Fields: &rpc.AccountFieldAvailability{NetLiquidation: true, BaseCurrency: true, AvailableFunds: true},
				},
			}
			tc.edit(account)
			engine := &proposalEngine{}
			policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 40, 15)
			policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
			pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"}, Options: []rpc.PositionView{budgetOptionLeg("EXAMPLE", 1, "C", 2, 1000, -50)}}
			input := engine.budgetGovernorInput(account, now)
			rows, status := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, account, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			if status.State != tc.state || len(rows) != tc.rows {
				t.Fatalf("state=%s rows=%d, want state=%s rows=%d", status.State, len(rows), tc.state, tc.rows)
			}
			if tc.rows > 0 && (rows[0].Quantity != 2 || !rows[0].AutomaticEligible()) {
				t.Fatalf("observed cash deficit must retain the existing reduction: %+v", rows[0])
			}
			if tc.name == "missing_funds" && input.AvailableFundsBase != nil {
				t.Fatal("missing funds became a numeric value")
			}
		})
	}
}
