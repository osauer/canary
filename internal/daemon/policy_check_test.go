package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Synthetic policy files and a synthetic book: every number below is made
// up for the test, none is an account's.

const pcRulebookHead = `kind = "canary.rulebook_policy"
schema_version = 1
policy_id = "rulebook-test"
policy_version = 1
`

const pcProtectionHead = `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-test"
policy_version = 1

[authority]
close_reduce_only = true
auto_submit = false
`

const pcConstitutionHead = `kind = "canary.risk_policy"
schema_version = 1
policy_id = "constitution-test"
policy_version = 1
`

// pcSweep is an enabled, active cash sweep on the reserve design: every
// sizing number written, a cap in force of 9,000 EUR (max_order_notional
// beats 1% of 200,000), economic and NLV-proportionate against pcBook.
const pcSweep = `
[cash.sweep]
enabled = true
mode = "active"
reserve_floor_base = 10000.0
reserve_pct_nlv = 5.0
min_order_notional = 3000.0
max_order_pct_nlv = 1.0
max_order_notional = 9000.0
keep_cash = 4000.0
order_step_base = 1000.0
no_buy_while_borrowed = true

[cash.sweep.currency.EUR]
instruments = ["de_bubill"]
fallback = "none"
keep_cash = 6000.0
min_maturity_days = 91

[cash.sweep.currency.USD]
min_maturity_days = 91
`

// pcLeveling is enabled currency leveling at the written defaults.
const pcLeveling = `
[cash.leveling]
enabled = true
trigger_base = 10000.0
cushion_base = 250.0
max_slippage_bp = 2.0
`

// pcConstitution carries complete [order_limits]: against pcBook's 200,000
// NLV the cap in force is 10,000 EUR (5% of NLV meets the floor).
const pcConstitution = pcConstitutionHead + `
[capital]
base_currency = "EUR"
protected_floor = 120000.0
declared_risk_capital = 40000.0

[order_limits]
max_order_floor_base = 10000.0
max_order_pct_nlv = 5.0
max_order_ceiling_base = 100000.0
max_option_contracts = 5
allow_stock_short = false
allow_option_sell_to_open = false
`

type pcFiles struct {
	rulebook, protection, constitution, opportunity string
}

func pcWrite(t *testing.T, f pcFiles) PolicyFileSet {
	t.Helper()
	dir := t.TempDir()
	set := PolicyFileSet{}
	for _, w := range []struct {
		name, body string
		dst        *string
	}{
		{"rulebook-policy.toml", f.rulebook, &set.Rulebook},
		{"protection-policy.toml", f.protection, &set.Protection},
		{"risk-policy.toml", f.constitution, &set.Constitution},
		{"opportunity-policy.toml", f.opportunity, &set.Opportunity},
	} {
		path := filepath.Join(dir, w.name)
		*w.dst = path
		if w.body == "" {
			continue
		}
		if err := os.WriteFile(path, []byte(w.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return set
}

// pcBook is a synthetic EUR book of 200,000 with USD at 0.90.
func pcBook() *PolicyCheckBook {
	return &PolicyCheckBook{BaseCurrency: "EUR", NetLiquidation: 200000, AsOf: pcNow,
		Cash:     map[string]float64{"EUR": 20000, "USD": 5000},
		FXToBase: map[string]float64{"EUR": 1, "USD": 0.9},
		Positions: []PolicyCheckPosition{
			{Kind: policyCheckKindStock, Currency: "USD", Quantity: 100, MarketValueBase: 30000},
			{Kind: policyCheckKindOption, Currency: "USD", Quantity: 10, MarketValueBase: 20000},
		},
		PositionsKnown: true}
}

var pcNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func pcClean() pcFiles {
	return pcFiles{rulebook: pcRulebookHead, protection: pcProtectionHead + `
[buckets.risk_reduction]
enabled = true
max_order_notional = 9000.0
` + pcSweep + pcLeveling, constitution: pcConstitution}
}

func pcInput(t *testing.T, f pcFiles) PolicyCheckInput {
	t.Helper()
	tr := config.Trading{Mode: config.TradingModeLive}.WithDefaults()
	return PolicyCheckInput{Now: pcNow, Files: pcWrite(t, f), ConfigPath: "/x/config.toml", Trading: tr,
		Book: pcBook(), FileStatus: map[string]string{}}
}

func pcRules(r rpc.PolicyCheckReport) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Rule)
	}
	return out
}

func pcFinding(r rpc.PolicyCheckReport, rule string) (rpc.PolicyCheckFinding, bool) {
	for _, f := range r.Findings {
		if f.Rule == rule {
			return f, true
		}
	}
	return rpc.PolicyCheckFinding{}, false
}

func TestPolicyCheckCleanPolicyHasNoFindings(t *testing.T) {
	r := CheckPolicy(pcInput(t, pcClean()))
	if len(r.Findings) != 0 {
		for _, f := range r.Findings {
			t.Errorf("%s: %s", f.Rule, f.Message)
		}
	}
	if r.Status != "ok" || r.Book == nil || r.Book.NetLiquidation != 200000 {
		t.Fatalf("status %q book %+v", r.Status, r.Book)
	}
}

// TestPolicyCheckCatalogue drives every rule with synthetic files and a
// synthetic book: each case must produce its rule at its severity, and every
// catalogue entry must have a case.
func TestPolicyCheckCatalogue(t *testing.T) {
	clean := pcClean()
	replace := func(body, old, new string) string {
		if !strings.Contains(body, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(body, old, new, 1)
	}
	cases := []struct {
		name     string
		rule     string
		severity string
		edit     func(*pcFiles, *PolicyCheckInput)
		// absent lists rules that must not fire.
		absent []string
		// contains is a fragment the finding's message must carry.
		contains string
	}{
		{name: "file refused for an unknown key", rule: "file_refused", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) { f.protection += "\n[buckets.theta_hygiene]\nmax_dtee = 3\n" }, contains: "refuses protection-policy.toml"},
		{name: "sweep cap above the trading cap", rule: "cap_above_trading_max", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "max_order_notional = 9000.0\nkeep_cash", "max_order_notional = 15000.0\nkeep_cash")
			}, contains: "15,000 EUR"},
		{name: "contract-currency cap above the trading cap at the book's FX rate", rule: "cap_above_trading_max", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "[buckets.risk_reduction]\nenabled = true\nmax_order_notional = 9000.0", "[buckets.risk_reduction]\nenabled = true\nmax_order_notional = 12000.0")
			}, contains: "10,800 EUR"},
		{name: "sweep minimum above the sweep cap", rule: "sweep_minimum_above_cap", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "min_order_notional = 3000.0", "min_order_notional = 9500.0")
			}, contains: "no EUR order can be both"},
		{name: "legacy min_tranche above the sweep cap", rule: "sweep_minimum_above_cap", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "[cash.sweep.currency.USD]\n", "[cash.sweep.currency.USD]\nmin_tranche = 12000.0\n")
			}, contains: "from min_tranche"},
		{name: "percent-of-NLV cap above the trading cap", rule: "cap_above_trading_max", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "max_order_pct_nlv = 1.0", "max_order_pct_nlv = 6.0")
			}, contains: "12,000 EUR"},
		{name: "watch above act", rule: "watch_act_inverted", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.rulebook += "single_name_watch_pct = 45.0\nsingle_name_act_pct = 40.0\n"
			}, contains: "single_name_watch_pct (45) is above single_name_act_pct (40)"},
		{name: "margin headroom act above watch reads the wrong way round", rule: "watch_act_inverted", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.rulebook += "margin_headroom_watch_pct = 10.0\nmargin_headroom_act_pct = 20.0\n"
			}, contains: "alarms as it falls"},
		{name: "drawdown warn above block", rule: "watch_act_inverted", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution += "\n[drawdown]\nwarn_consumed_pct = 40.0\nblock_consumed_pct = 30.0\n"
			}, contains: "brake latches before you are ever warned"},
		{name: "premium budget loosens under stress", rule: "regime_loosens_under_stress", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.rulebook += "\n[regime_confirmed]\npremium_budget_watch_pct = 30.0\n"
			}, contains: "loosens as the regime worsens"},
		{name: "hedge band shrinks under stress", rule: "regime_loosens_under_stress", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.rulebook += "\n[regime_early_warning]\nhedge_band_min_pct = 20.0\n"
			}, contains: "hedge_band_min_pct is lower in [regime_early_warning]"},
		{name: "active sweep with order entry off", rule: "order_entry_off_for_active_bucket", severity: rpc.PolicyCheckError,
			edit: func(_ *pcFiles, in *PolicyCheckInput) { in.Trading.Mode = config.TradingModeDisabled }, contains: "can never be placed"},
		{name: "active currency leveling with order entry off", rule: "order_entry_off_for_active_bucket", severity: rpc.PolicyCheckError,
			edit: func(_ *pcFiles, in *PolicyCheckInput) { in.Trading.Mode = config.TradingModeDisabled }, contains: "[cash.leveling] enabled = true"},
		{name: "a debit inside the leveling band", rule: "leveling_debit_inside_band", severity: rpc.PolicyCheckWarn,
			edit: func(_ *pcFiles, in *PolicyCheckInput) { in.Book.Cash["USD"] = -3000 }, contains: "inside the 10,000 EUR band"},
		{name: "settlement route ended", rule: "settlement_route_expired", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "[cash.sweep.currency.USD]\n", "[cash.sweep.currency.USD]\nsettlement_valid_through = 2026-09-01\n")
			}, contains: "ended on 2026-09-01"},
		{name: "reserve percentage out of range", rule: "file_refused", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "reserve_pct_nlv = 5.0", "reserve_pct_nlv = 150.0")
			}, contains: "reserve_pct_nlv must be a percentage"},
		{name: "constitution base differs from the account's", rule: "base_currency_mismatch", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, `base_currency = "EUR"`, `base_currency = "USD"`)
			}, contains: "counts capital in USD"},
		{name: "one option contract above the trading cap", rule: "lot_above_trading_max", severity: rpc.PolicyCheckError,
			edit: func(_ *pcFiles, in *PolicyCheckInput) {
				in.Book.Positions = append(in.Book.Positions, PolicyCheckPosition{Kind: policyCheckKindOption, Currency: "USD", Quantity: 1, MarketValueBase: 12000})
			}, contains: "12,000 EUR"},
		{name: "sweep cap at the trading cap with a USD leg", rule: "cap_without_fx_headroom", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "max_order_notional = 9000.0\nkeep_cash", "max_order_notional = 10000.0\nkeep_cash")
			}, absent: []string{"cap_above_trading_max"}, contains: "sizes each USD order at the ledger rate"},
		{name: "trading cap tiny against NLV", rule: "order_cap_vs_nlv", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "max_order_floor_base = 10000.0\nmax_order_pct_nlv = 5.0", "max_order_floor_base = 3000.0\nmax_order_pct_nlv = 1.0")
			}, contains: "1.5% of NLV"},
		{name: "trading cap huge against NLV", rule: "order_cap_vs_nlv", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "max_order_floor_base = 10000.0", "max_order_floor_base = 150000.0")
				f.constitution = replace(f.constitution, "max_order_ceiling_base = 100000.0", "max_order_ceiling_base = 200000.0")
			}, contains: "more than half the book"},
		{name: "risk-reduction cap splits a trim", rule: "order_cap_splits_reduction", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "[buckets.risk_reduction]\nenabled = true\nmax_order_notional = 9000.0", "[buckets.risk_reduction]\nenabled = true\nmax_order_notional = 4000.0")
			}, contains: "into 6 orders"},
		{name: "keep_cash under 2% of NLV without the reserve design", rule: "cash_reserve_vs_nlv", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "reserve_floor_base = 10000.0\nreserve_pct_nlv = 5.0\n", "")
				f.protection = replace(f.protection, "keep_cash = 6000.0", "keep_cash = 1000.0")
				f.protection = replace(f.protection, "keep_cash = 4000.0", "keep_cash = 0.0")
			}, contains: "keep_cash across the swept currencies keeps back 1,000 EUR, 0.5% of NLV"},
		{name: "reserve design under 2% of NLV", rule: "cash_reserve_vs_nlv", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "reserve_floor_base = 10000.0\nreserve_pct_nlv = 5.0", "reserve_floor_base = 1000.0\nreserve_pct_nlv = 1.0")
				f.protection = replace(f.protection, "keep_cash = 6000.0", "keep_cash = 500.0")
				f.protection = replace(f.protection, "keep_cash = 4000.0", "keep_cash = 0.0")
			}, contains: "the reserve (2,000 EUR, held in EUR) and keep_cash in the other currencies keep back 2,000 EUR, 1% of NLV"},
		{name: "declared risk not all above the floor", rule: "protected_floor_vs_equity", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "protected_floor = 120000.0", "protected_floor = 180000.0")
			}, contains: "Only 20,000 EUR"},
		{name: "floor above equity", rule: "protected_floor_vs_equity", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "protected_floor = 120000.0", "protected_floor = 250000.0")
			}, contains: "effective risk capital is zero"},
		{name: "declared risk capital above NLV", rule: "declared_risk_vs_nlv", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "protected_floor = 120000.0\ndeclared_risk_capital = 40000.0", "declared_risk_capital = 300000.0")
			}, contains: "above NLV"},
		{name: "declared risk capital tiny", rule: "declared_risk_vs_nlv", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "declared_risk_capital = 40000.0", "declared_risk_capital = 2000.0")
			}, contains: "trip on noise"},
		{name: "sweep minimum below the commission", rule: "sweep_minimum_uneconomic", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "min_order_notional = 3000.0", "min_order_notional = 900.0")
				f.protection = replace(f.protection, "[cash.sweep.currency.USD]\nmin_maturity_days = 91", "[cash.sweep.currency.USD]\nmin_maturity_days = 28")
			}, contains: "1,000 USD (900 EUR, from min_order_notional) in a 28-day bill"},
		{name: "edited without a version bump", rule: "version_not_bumped", severity: rpc.PolicyCheckWarn,
			edit: func(_ *pcFiles, in *PolicyCheckInput) { in.FileStatus[PolicyFileRulebook] = "drift" }, contains: "did not rise"},
		{name: "cash interest assumption expired", rule: "dated_assumption_expired", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "[cash.sweep.currency.USD]\n", "[cash.sweep.currency.USD]\ncash_interest_rate_upper = 0.01\ncash_interest_valid_through = 2026-09-30\n")
			}, contains: "USD cash-interest assumption"},
		{name: "directional intent expired", rule: "dated_assumption_expired", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection += "\n[buckets.trailing_stop.options]\ndirectional_intents = [{ con_id = 1, reason = \"test\", approved_at = 2026-09-01T00:00:00Z, expires_at = 2026-10-01T00:00:00Z }]\n"
			}, contains: "overrides the standing defaults"},
		{name: "directional intent ending soon", rule: "dated_assumption_expiring", severity: rpc.PolicyCheckInfo,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection += "\n[buckets.trailing_stop.options]\ndirectional_intents = [{ con_id = 1, reason = \"test\", approved_at = 2026-09-01T00:00:00Z, expires_at = 2026-10-10T00:00:00Z }]\n"
			}, contains: "ends in 5 days"},
		{name: "edited file still marked unreviewed", rule: "file_unreviewed", severity: rpc.PolicyCheckInfo,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.rulebook = PolicyUnreviewedMarker + "\n" + f.rulebook + "option_line_act_pct = 15.0\n"
			}, contains: "1 of its limits differs from Canary's defaults (option_line_act_pct)"},
		{name: "an order limit not written", rule: "order_limits_missing", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.constitution = replace(f.constitution, "max_order_pct_nlv = 5.0\n", "")
			}, absent: []string{"cap_above_trading_max", "order_cap_vs_nlv"}, contains: "does not write order_limits.max_order_pct_nlv, so the trading gate refuses every order preview"},
		{name: "no order limits at all", rule: "order_limits_missing", severity: rpc.PolicyCheckError,
			edit: func(f *pcFiles, _ *PolicyCheckInput) { f.constitution = pcConstitutionHead }, contains: "order_limits.max_order_floor_base, order_limits.max_order_pct_nlv"},
		{name: "retired trading gate differs", rule: "retired_trading_gate", severity: rpc.PolicyCheckWarn,
			edit:     func(_ *pcFiles, in *PolicyCheckInput) { in.Trading.MaxNotional = new(12000.0) },
			contains: "[trading].max_notional = 12,000 EUR is retired and no longer read; [order_limits].max_order_floor_base = 10,000 EUR decides"},
		{name: "sweep numbers not written", rule: "compiled_default_in_force", severity: rpc.PolicyCheckInfo,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "reserve_floor_base = 10000.0\n", "")
			}, contains: "holds until these numbers are written"},
		{name: "sweep may buy while a currency is borrowed", rule: "sweep_buys_while_borrowed", severity: rpc.PolicyCheckWarn,
			edit: func(f *pcFiles, in *PolicyCheckInput) {
				f.protection = replace(f.protection, "no_buy_while_borrowed = true", "no_buy_while_borrowed = false")
				in.Book.Cash["USD"] = -20000
			}, contains: "The account is borrowing (−20,000 USD)"},
		{name: "exempt sweep cap above the trading cap", rule: "sweep_cap_exempt", severity: rpc.PolicyCheckInfo,
			edit: func(f *pcFiles, _ *PolicyCheckInput) {
				f.protection = replace(f.protection, "max_order_notional = 9000.0\nkeep_cash", "max_order_notional = 15000.0\nbills_exempt_from_trading_max_notional = true\nkeep_cash")
			}, absent: []string{"cap_above_trading_max", "cap_without_fx_headroom"}, contains: "the gap is intended"},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := clean
			in := pcInput(t, f)
			tc.edit(&f, &in)
			in.Files = pcWrite(t, f)
			r := CheckPolicy(in)
			got, ok := pcFinding(r, tc.rule)
			if !ok {
				t.Fatalf("rule %s did not fire; got %v", tc.rule, pcRules(r))
			}
			for _, f := range r.Findings {
				if f.Rule == tc.rule && strings.Contains(f.Message, tc.contains) {
					got = f
				}
			}
			if got.Severity != tc.severity {
				t.Fatalf("severity %s, want %s", got.Severity, tc.severity)
			}
			if tc.contains != "" && !strings.Contains(got.Message, tc.contains) {
				t.Fatalf("message %q lacks %q", got.Message, tc.contains)
			}
			if len(got.Keys) == 0 {
				t.Fatal("finding names no key")
			}
			for _, rule := range tc.absent {
				if _, bad := pcFinding(r, rule); bad {
					t.Fatalf("rule %s fired too: %v", rule, pcRules(r))
				}
			}
			if (r.Errors > 0) != (r.Status == "errors") {
				t.Fatalf("status %s with %d errors", r.Status, r.Errors)
			}
		})
		covered[tc.rule] = true
	}
	for _, id := range PolicyCheckRuleIDs() {
		if !covered[id] {
			t.Errorf("catalogue rule %s has no test case", id)
		}
	}
}

func TestPolicyCheckWithoutBookSkipsBookChecks(t *testing.T) {
	f := pcClean()
	f.constitution = strings.Replace(f.constitution, `base_currency = "EUR"`, `base_currency = "USD"`, 1)
	in := pcInput(t, f)
	in.Book, in.BookSkipped, in.FileStatus = nil, "no daemon", nil
	r := CheckPolicy(in)
	if r.Book != nil {
		t.Fatal("book reported without a book")
	}
	if _, ok := pcFinding(r, "base_currency_mismatch"); ok {
		t.Fatal("a book check ran without a book")
	}
	joined := strings.Join(r.Skipped, "\n")
	for _, want := range []string{"every check against the live book: no daemon", "version-bump drift"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("skipped %q lacks %q", joined, want)
		}
	}
}

func TestPolicyCheckEconomicsNamesItsAssumptions(t *testing.T) {
	r := CheckPolicy(pcInput(t, pcClean()))
	if !slices.ContainsFunc(r.Assumptions, func(a string) bool { return strings.Contains(a, "US Treasury bill: assumed yield") }) {
		t.Fatalf("assumptions %v lack the USD bill assumption", r.Assumptions)
	}
}

func TestPolicyCheckAbsentFilesRunDefaults(t *testing.T) {
	in := pcInput(t, pcFiles{})
	r := CheckPolicy(in)
	for _, f := range r.Files {
		if f.State != policyCheckFileAbsent {
			t.Fatalf("%s state %s", f.Policy, f.State)
		}
	}
	// Without a constitution there are no order limits, so the trading gate
	// refuses every order preview: that is the one error the defaults carry.
	if r.Errors != 1 || !slices.Equal(pcRules(r), []string{"order_limits_missing"}) {
		t.Fatalf("defaults produced errors: %v", pcRules(r))
	}
}

func TestPolicyCheckBookFromAccount(t *testing.T) {
	mv := 9000.0
	acct := &rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 100000,
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 7000, CashObserved: true},
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 2000, CashObserved: true, ExchangeRate: 0.9}}}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{SecType: "STOCK", Currency: "USD", Quantity: 10, MarketValueBase: &mv}},
		Options: []rpc.PositionView{{SecType: "OPTION", Currency: "USD", Quantity: 1, MarketValue: 1000}}}
	b := PolicyCheckBookFrom(acct, pos)
	if b == nil || b.Cash["EUR"] != 7000 || b.Cash["USD"] != 2000 || b.FXToBase["USD"] != 0.9 {
		t.Fatalf("book %+v", b)
	}
	if len(b.Positions) != 2 || b.Positions[1].MarketValueBase != 900 {
		t.Fatalf("positions %+v", b.Positions)
	}
	if PolicyCheckBookFrom(&rpc.AccountResult{BaseCurrency: "EUR"}, nil) != nil {
		t.Fatal("a zero-NLV account made a book")
	}
}

func TestPolicyCheckNumberFormatting(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 1000: "1,000", 1234567.5: "1,234,567.5", 2.676: "2.68", -9500: "-9,500", 0.999: "1"} {
		if got := policyCheckNumber(v); got != want {
			t.Errorf("policyCheckNumber(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestPolicyCheckDocsListEveryRule(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "docs", "understand", "policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range PolicyCheckRuleIDs() {
		if !strings.Contains(string(data), "`"+id+"`") {
			t.Errorf("docs/docs/understand/policy.md does not list rule %s", id)
		}
	}
}
