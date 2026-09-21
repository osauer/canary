package daemon

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The governor carries no default that acts: the embedded default has no
// budget_reduction table, a file that writes the table without enabled is
// disabled, and the mode falls to shadow when left empty.
func TestBudgetReductionAbsentFromDefaultAndDisabledByDefault(t *testing.T) {
	if defaultProtectionPolicy().Buckets.BudgetReduction != nil {
		t.Fatal("embedded default carries a budget_reduction table")
	}
	raw, err := DefaultPolicyTOML("protection")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "budget_reduction") {
		t.Fatalf("canary policy default protection prints the governor:\n%s", raw)
	}
	// Adding a nil pointer field must not move an existing policy fingerprint.
	before := fingerprintProtectionPolicy(defaultProtectionPolicy())
	withNil := defaultProtectionPolicy()
	withNil.Buckets.BudgetReduction = nil
	if fingerprintProtectionPolicy(withNil).Key != before.Key {
		t.Fatal("nil budget_reduction changed the protection policy fingerprint")
	}

	var p protectionPolicy
	md, err := toml.Decode(`
kind = "ibkr.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = 7
[authority]
close_reduce_only = true
auto_submit = false
[buckets.budget_reduction]
premium_at_risk_pct_of_risk_capital = 40
per_line_pct_of_risk_capital = 15
max_order_notional = 10000
`, &p)
	if err != nil {
		t.Fatal(err)
	}
	applyProtectionPolicyDefaults(&p, &md)
	if err := validateProtectionPolicy(p); err != nil {
		t.Fatalf("table without enabled must validate: %v", err)
	}
	bucket := p.Buckets.BudgetReduction
	if bucket == nil || bucket.enabled() || bucket.effectiveMode() != rpc.BudgetReductionModeShadow {
		t.Fatalf("written table = %+v; want present, disabled, shadow", bucket)
	}
	if fingerprintProtectionPolicy(p).Key == before.Key {
		t.Fatal("a written budget_reduction table must enter the fingerprint")
	}
}

func TestBudgetReductionValidation(t *testing.T) {
	base := func() protectionPolicy {
		p := defaultProtectionPolicy()
		p.Buckets.BudgetReduction = &protectionBudgetPolicy{Enabled: true, PremiumAtRiskPctOfRiskCapital: 40, PerLinePctOfRiskCapital: 15, MaxOrderNotional: 10000}
		return p
	}
	if err := validateProtectionPolicy(base()); err != nil {
		t.Fatalf("well-formed governor rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(*protectionBudgetPolicy)
		want   string
	}{
		"mode":              {func(b *protectionBudgetPolicy) { b.Mode = "hard" }, "mode"},
		"total zero":        {func(b *protectionBudgetPolicy) { b.PremiumAtRiskPctOfRiskCapital = 0 }, "premium_at_risk_pct_of_risk_capital"},
		"total above 100":   {func(b *protectionBudgetPolicy) { b.PremiumAtRiskPctOfRiskCapital = 100.5 }, "premium_at_risk_pct_of_risk_capital"},
		"per line zero":     {func(b *protectionBudgetPolicy) { b.PerLinePctOfRiskCapital = 0 }, "per_line_pct_of_risk_capital"},
		"per line negative": {func(b *protectionBudgetPolicy) { b.PerLinePctOfRiskCapital = -1 }, "per_line_pct_of_risk_capital"},
		"per line > total":  {func(b *protectionBudgetPolicy) { b.PerLinePctOfRiskCapital = 41 }, "must not exceed"},
		"notional":          {func(b *protectionBudgetPolicy) { b.MaxOrderNotional = 0 }, "max_order_notional"},
	} {
		t.Run(name, func(t *testing.T) {
			p := base()
			tc.change(p.Buckets.BudgetReduction)
			err := validateProtectionPolicy(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
	// Disabled: unset caps are fine, a malformed written cap is not.
	disabled := base()
	disabled.Buckets.BudgetReduction = &protectionBudgetPolicy{}
	if err := validateProtectionPolicy(disabled); err != nil {
		t.Fatalf("disabled empty table rejected: %v", err)
	}
	disabled.Buckets.BudgetReduction.PerLinePctOfRiskCapital = 150
	if err := validateProtectionPolicy(disabled); err == nil {
		t.Fatal("malformed cap accepted while disabled")
	}
	// Mode is case-insensitive on read and resolves to the two constants.
	active := base()
	active.Buckets.BudgetReduction.Mode = "Active"
	if err := validateProtectionPolicy(active); err != nil || active.Buckets.BudgetReduction.effectiveMode() != rpc.BudgetReductionModeActive {
		t.Fatalf("active mode: err=%v mode=%q", err, active.Buckets.BudgetReduction.effectiveMode())
	}
	var nilBucket *protectionBudgetPolicy
	if nilBucket.enabled() || nilBucket.effectiveMode() != rpc.BudgetReductionModeShadow {
		t.Fatal("nil bucket must read disabled and shadow")
	}
}
