package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// syntheticProtectionPolicy is a protection file with a few owner keys, an
// enabled cash sweep with one currency table, and a budget governor on the
// Rulebook basis. Values are synthetic.
const syntheticProtectionPolicy = `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-test"
policy_version = 3

[authority]
close_reduce_only = true
auto_submit = false

[buckets.theta_hygiene]
enabled = true
max_dte = 14
min_abs_theta_per_day = 5.0
min_extrinsic_pct_of_mark = 40.0
max_spread_pct_of_mid = 25.0

[buckets.trailing_stop.stock_etf]
default_pct = 9.0

[buckets.budget_reduction]
enabled = true
basis = "rulebook"
max_order_notional = 2500.0

[buckets.cash_sweep]
enabled = true
max_order_notional = 12000.0

[buckets.cash_sweep.currency.EUR]
instruments = ["de_bubill"]
fallback = "etf"
`

func syntheticEffectiveView(t *testing.T) *rpc.PolicyEffectiveView {
	t.Helper()
	dir := t.TempDir()
	protectionPath := filepath.Join(dir, "protection-policy.toml")
	if err := os.WriteFile(protectionPath, []byte(syntheticProtectionPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	protection, _, err := parseProtectionPolicy([]byte(syntheticProtectionPolicy))
	if err != nil {
		t.Fatal(err)
	}
	opportunity := defaultOpportunityPolicy()
	files := []rpc.PolicyFileStatus{
		{Policy: PolicyFileProtection, Path: protectionPath, Status: "active", PolicyID: "protection-test", PolicyVersion: "3"},
		{Policy: PolicyFileRulebook, Path: filepath.Join(dir, "absent-rulebook.toml"), Status: "default"},
	}
	settings := &rpc.PlatformSettings{}
	settings.Trading.Limits.MaxNotional = rpc.SettingsFloat{Value: 5000, Source: rpc.PolicySourceRuntime, Access: "write"}
	settings.Trading.Mode = rpc.SettingsString{Value: "paper", Source: "config", Access: "read"}
	settings.Trading.Account = rpc.SettingsString{Value: "DU0000000", Source: "config", Access: "read"}
	in := policyEffectiveInputs{
		origin: "daemon", files: files, limits: risk.ConstitutionLimits(nil),
		rulebook: risk.DefaultRulebookPolicy(), protection: &protection, opportunity: &opportunity,
		trading: config.Trading{Mode: "paper", MaxNotional: 8000}, settings: settings,
		definedByKey: definedKeysForFiles(files),
	}
	return buildPolicyEffectiveView(in)
}

func effectiveRows(view *rpc.PolicyEffectiveView) map[string]rpc.PolicyEffectiveRow {
	rows := map[string]rpc.PolicyEffectiveRow{}
	for _, sec := range view.Sections {
		for _, g := range sec.Groups {
			for _, r := range g.Rows {
				rows[sec.ID+":"+r.Key] = r
			}
		}
	}
	return rows
}

// Every key of every policy struct (by the generated help tables, which the
// config reference shares) appears in the view: a key that moves behaviour
// but never prints reads as fixed.
func TestPolicyEffectiveViewCoversEveryPolicyKey(t *testing.T) {
	rows := effectiveRows(syntheticEffectiveView(t))
	has := func(section, key string) bool {
		if _, ok := rows[section+":"+key]; ok {
			return true
		}
		for k := range rows {
			if strings.HasPrefix(k, section+":"+key+".") {
				return true // a map key prints one row per entry
			}
		}
		return false
	}
	tables := []struct {
		section string
		help    map[string]string
	}{
		{rpc.PolicySectionProtection, protectionPolicyHelp},
		{rpc.PolicySectionRulebook, rulebookPolicyHelp},
		{rpc.PolicySectionOpportunity, opportunityPolicyHelp},
		{rpc.PolicySectionTrading, tradingConfigHelp},
	}
	for _, table := range tables {
		if len(table.help) == 0 {
			t.Fatalf("%s: empty help table", table.section)
		}
		for key := range table.help {
			want := strings.ReplaceAll(key, "<name>", "EUR")
			for _, set := range regimeSetColumns {
				if rest, ok := strings.CutPrefix(want, set.field+"."); ok {
					want = "regime_*." + rest
				}
			}
			if !has(table.section, want) {
				t.Errorf("%s key %s is missing from the view", table.section, key)
			}
		}
	}
	for _, l := range risk.ConstitutionLimits(nil) {
		if !has(rpc.PolicySectionConstitution, l.Key) {
			t.Errorf("constitution key %s is missing from the view", l.Key)
		}
	}
	for _, spec := range rpc.SettingsKeys() {
		if strings.HasPrefix(spec.Key, "trading.") {
			continue // printed in the trading section
		}
		if !has(rpc.PolicySectionRuntime, spec.Key) {
			t.Errorf("runtime setting %s is missing from the view", spec.Key)
		}
	}
}

// Each row says where its value comes from: the file, Canary's default, a
// value Canary maintains, or a number only the owner can write.
func TestPolicyEffectiveViewAttributesSources(t *testing.T) {
	rows := effectiveRows(syntheticEffectiveView(t))
	for key, want := range map[string]struct{ value, source string }{
		"protection:buckets.theta_hygiene.max_dte":                         {"14 days", rpc.PolicySourceFile},
		"protection:buckets.theta_hygiene.max_spread_pct_of_mid":           {"25%", rpc.PolicySourceFile},
		"protection:buckets.trailing_stop.stock_etf.default_pct":           {"9%", rpc.PolicySourceFile},
		"protection:authority.veto_window":                                 {"30m", rpc.PolicySourceDefault},
		"protection:authority.pre_authorised":                              {"none", rpc.PolicySourceNeedsYourNumber},
		"protection:buckets.budget_reduction.mode":                         {"shadow", rpc.PolicySourceDefault},
		"protection:buckets.budget_reduction.per_line_pct_of_risk_capital": {"10% NLV (Rulebook)", rpc.PolicySourceMachine},
		"protection:buckets.cash_sweep.max_order_notional":                 {"12,000", rpc.PolicySourceFile},
		"protection:buckets.cash_sweep.tax_reviewed_at":                    {"—", rpc.PolicySourceNeedsYourNumber},
		"protection:buckets.cash_sweep.currency.EUR.instruments":           {"de_bubill", rpc.PolicySourceFile},
		"protection:buckets.cash_sweep.currency.EUR.etf_symbol":            {"—", rpc.PolicySourceNeedsYourNumber},
		"protection:buckets.cash_sweep.currency.EUR.max_maturity_days":     {"182 days", rpc.PolicySourceDefault},
		"protection:buckets.cash_sweep.currency.EUR.settlement_days":       {"Canary's maintained route", rpc.PolicySourceMachine},
		"protection:buckets.cash_sweep.currency.USD.instruments":           {"us_tbill", rpc.PolicySourceDefault},
		"rulebook:regime_*.premium_budget_watch_pct":                       {"25% / 20% / 15%", rpc.PolicySourceDefault},
		"trading:trading.mode":                                             {"paper", "config"},
	} {
		got, ok := rows[key]
		if !ok {
			t.Errorf("%s: no row", key)
			continue
		}
		if got.Value != want.value || got.Source != want.source {
			t.Errorf("%s = %q (%s), want %q (%s)", key, got.Value, got.Source, want.value, want.source)
		}
	}
	notional := rows["trading:trading.max_notional"]
	if notional.Value != "5000" || notional.Source != rpc.PolicySourceRuntime || notional.FileValue != "8000" {
		t.Errorf("runtime override row = %+v, want 5000 over config.toml 8000", notional)
	}
	for key := range rows {
		if strings.Contains(key, "account") || strings.Contains(key, "endpoint") || strings.Contains(key, "client_id") {
			t.Errorf("%s: account identity is not policy and must not print", key)
		}
	}
	if m := rows["protection:buckets.theta_hygiene.max_dte"].Meaning; !strings.HasPrefix(m, "Only considers options") {
		t.Errorf("meaning not taken from the shared help table: %q", m)
	}
}

func TestDefinedTOMLKeysFlattensTables(t *testing.T) {
	got := definedTOMLKeys([]byte(syntheticProtectionPolicy))
	for _, key := range []string{"kind", "authority", "authority.auto_submit", "buckets.cash_sweep.currency.EUR.fallback"} {
		if !got[key] {
			t.Errorf("%s not defined in %v", key, got)
		}
	}
	if got["buckets.cash_sweep.currency.EUR.keep_cash"] {
		t.Error("an absent key reads as defined")
	}
}
