package edge

import (
	"testing"
	"time"
)

// The registered texts are frozen: a rule's hash is what an install recorded
// its registration instant against, so an edit to a rule is a new
// registration with a later instant. This test makes that a deliberate act.
// To change a rule, change its hash here in the same commit and say in the
// commit that its sample starts again.
func TestForwardTestRulesAreFrozen(t *testing.T) {
	want := map[string]string{
		"stock_sales_vs_nearby_day":              "rule_0af7db65e5e3f6dc4641cf80537d503b",
		"entries_vs_nearby_day":                  "rule_742d0cea4369ee2f041f2e71c936d15f",
		"option_buys_below_premium_line":         "rule_e1d8f6f61998ddfa89ecabd779895cce",
		"option_buys_after_regime_warning":       "rule_1f4b514c8ba19305e2520fd867497e48",
		"final_option_sales_vs_close_5_sessions": "rule_1680268bd2c3ccabf6c76ffa78242116",
		"floor_after_partial_sale":               "rule_fe3b796cd18c60c6fc791bd829d3dce3",
		"option_sales_after_adverse_move":        "rule_f82d08e64e6846534001fa0325963079",
	}
	tests := ForwardTests()
	if len(tests) != len(want) {
		t.Fatalf("rules=%d, want %d", len(tests), len(want))
	}
	for _, test := range tests {
		if test.Title == "" || test.Rule == "" {
			t.Fatalf("rule %q has no title or text", test.ID)
		}
		if got := test.RuleHash(); got != want[test.ID] {
			t.Errorf("rule %q hash=%s, registered as %s: its text changed", test.ID, got, want[test.ID])
		}
	}
	// The title is a label and may be reworded without re-registering.
	a := tests[0]
	b := a
	b.Title = "another label"
	if a.RuleHash() != b.RuleHash() {
		t.Fatal("the title is part of the rule hash")
	}
}

// A decision belongs to a rule's sample only if it was made strictly after
// the rule was registered. One made at the instant of registration, or
// before, is hindsight, and so is every decision while nothing is registered.
func TestForwardSampleStartsStrictlyAfterRegistration(t *testing.T) {
	registered := time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		executed time.Time
		want     bool
	}{
		{"a second before", registered.Add(-time.Second), false},
		{"the same instant", registered, false},
		{"a nanosecond after", registered.Add(time.Nanosecond), true},
		{"a day after", registered.AddDate(0, 0, 1), true},
	} {
		if got := InForwardSample(registered, tc.executed); got != tc.want {
			t.Errorf("%s: in sample=%v, want %v", tc.name, got, tc.want)
		}
	}
	if InForwardSample(time.Time{}, registered) {
		t.Error("a decision is in the sample of a rule that was never registered")
	}
}
