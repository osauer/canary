package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Cash policy settings: policy.cash.get, check and apply over a temporary
// protection policy file with synthetic values. No test reads an owner file:
// the server has no config, so the check reads the protection file alone.

const cashPolicyTestFile = `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-test"
policy_version = 14

[authority]
close_reduce_only = true
auto_submit = false
veto_window = "30m"

[buckets.trailing_stop]
enabled = true

# Cash management, as the owner keeps it.
[cash]
confirmation_window = "10m"

[cash.sweep]  # my sweep
enabled = true
mode = "shadow"
reserve_floor_base = 10000.0
reserve_pct_nlv = 10.0
min_order_notional = 20000.0
max_order_notional = 50000.0  # chosen after the review
max_order_pct_nlv = 10.0
order_step_base = 1000.0  # written by Canary v9.9.9; the sweep reads it from this file only
keep_cash = 5000.0
bills_exempt_from_trading_max_notional = true
no_buy_while_borrowed = true

[cash.sweep.currency.GBP]
keep_cash = 8000.0

# Currency leveling: repays a borrowed currency.
[cash.leveling]
enabled = false
trigger_base = 10000.0
cushion_base = 250.0
max_slippage_bp = 2.0
payback_days = 30
`

var cashPolicyTestNow = time.Date(2026, 10, 6, 12, 5, 0, 0, time.UTC)

// cashPolicyTestBook is a synthetic account: EUR base, NLV 200,000, EUR
// cash 60,000, USD borrowed 20,000 on trade-date cash at 0.855 EUR per USD,
// an order cap of 25,000 EUR, rates read to 3 Oct 2026.
func cashPolicyTestBook() cashPolicyBook {
	return cashPolicyBook{at: cashPolicyTestNow, base: "EUR", nlv: 200000,
		cash: map[string]float64{"EUR": 60000, "USD": -20000}, fx: map[string]float64{"EUR": 1, "USD": 0.855}, orderCap: 25000,
		sweep: cashSweepInput{BaseCurrency: "EUR", Ledger: map[string]cashSweepLedgerRow{"EUR": {Observed: true, TradeDate: 60000, ExchangeRate: 1}, "USD": {Observed: true, TradeDate: -20000, ExchangeRate: 0.855}}},
		rates: map[string]currencyLevelingRate{
			"EUR": {Cash: new(0.014), CashThrough: "2026-10-03"},
			"USD": {Loan: new(0.051), LoanThrough: "2026-10-03", Cash: new(0.033), CashThrough: "2026-09-12"},
		},
		ratesThrough: "2026-10-03",
		check: &PolicyCheckBook{BaseCurrency: "EUR", NetLiquidation: 200000, AsOf: cashPolicyTestNow,
			Cash: map[string]float64{"EUR": 60000, "USD": -20000}, FXToBase: map[string]float64{"EUR": 1, "USD": 0.855}}}
}

func cashPolicyServer(t *testing.T, file string) (*Server, string, *corestore.Store) {
	t.Helper()
	dir := privateTestDir(t)
	path := filepath.Join(dir, "protection-policy.toml")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	m := newProtectionPolicyManager(path, false, 0, func() time.Time { return cashPolicyTestNow })
	m.reload()
	s := &Server{protectionPolicies: m, coreStore: core, now: func() time.Time { return cashPolicyTestNow }}
	s.cashPolicyBookForTest = cashPolicyTestBook
	return s, path, core
}

func cashPolicyGet(t *testing.T, s *Server) *rpc.CashPolicySnapshot {
	t.Helper()
	out, err := s.handleCashPolicyGet(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cashPolicyCheck(t *testing.T, s *Server, revision string, changes map[string]any) *rpc.CashPolicyCheckResult {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range changes {
		raw[k], _ = json.Marshal(v)
	}
	params, _ := json.Marshal(rpc.CashPolicyCheckRequest{ExpectedRevision: revision, Changes: raw})
	out, err := s.handleCashPolicyCheck(t.Context(), &rpc.Request{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cashPolicyConfirmation is a fresh device confirmation.
func cashPolicyConfirmation() *rpc.CashPolicyConfirmation {
	return &rpc.CashPolicyConfirmation{DeskActionID: "7f3c2a90deadbeef", Credential: "companion:key-1", Envelope: `{"credential":"companion","signature":"synthetic"}`}
}

func cashPolicyApply(s *Server, terms, digest, id string, confirmation *rpc.CashPolicyConfirmation, origin string) (*rpc.CashPolicyApplyResult, error) {
	params, _ := json.Marshal(rpc.CashPolicyApplyRequest{Terms: terms, Digest: digest, RequestID: id, Confirmation: confirmation, Origin: origin})
	return s.handleCashPolicyApply(context.Background(), &rpc.Request{Params: params})
}

// cashPolicySave checks and applies changes, failing the test on any error.
func cashPolicySave(t *testing.T, s *Server, changes map[string]any, id string) *rpc.CashPolicyApplyResult {
	t.Helper()
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, changes)
	if check.Conflict || len(check.Errors) > 0 || check.Digest == "" {
		t.Fatalf("check %+v", check)
	}
	out, err := cashPolicyApply(s, check.Terms, check.Digest, id, cashPolicyConfirmation(), "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cashPolicySetting(snap *rpc.CashPolicySnapshot, key string) (rpc.CashPolicySetting, bool) {
	i := slices.IndexFunc(snap.Settings, func(s rpc.CashPolicySetting) bool { return s.Key == key })
	if i < 0 {
		return rpc.CashPolicySetting{}, false
	}
	return snap.Settings[i], true
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCashPolicyGetReportsEveryKeyWithSourceDefaultBoundsAndHelp(t *testing.T) {
	file := strings.Replace(cashPolicyTestFile, "order_step_base = 1000.0  # written by Canary v9.9.9; the sweep reads it from this file only\n", "", 1)
	file = strings.Replace(file, "bills_exempt_from_trading_max_notional = true\n", "", 1)
	s, path, _ := cashPolicyServer(t, file)
	snap := cashPolicyGet(t, s)
	if snap.FileState != rpc.CashPolicyFileOK || !snap.Writable || snap.PolicyVersion != 14 || snap.InForceVersion != 14 || snap.Path != path || !strings.HasPrefix(snap.Revision, "sha256:") {
		t.Fatalf("snapshot head %+v", snap)
	}
	if snap.BaseCurrency != "EUR" || !snap.Sections.Leveling.Present || !snap.Sections.Sweep.Present {
		t.Fatalf("sections %+v", snap.Sections)
	}
	// Every key in scope, for every listed currency: EUR (base), GBP (a
	// table) and USD (ledger).
	var want []string
	for _, sp := range cashPolicySpecs {
		if !sp.perCurrency {
			want = append(want, sp.key(""))
			continue
		}
		for _, ccy := range []string{"EUR", "GBP", "USD"} {
			want = append(want, sp.key(ccy))
		}
	}
	var got []string
	for _, row := range snap.Settings {
		got = append(got, row.Key)
		if row.Label == "" || row.Help == "" || row.Type == "" || row.Section == "" {
			t.Errorf("%s lacks its label, help, type or section: %+v", row.Key, row)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Fatalf("keys\n got %v\nwant %v", got, want)
	}
	for _, c := range []struct {
		key     string
		value   any
		source  string
		def     any
		min     *float64
		max     *float64
		exclude bool
		reset   bool
	}{
		{"cash.leveling.enabled", false, rpc.CashPolicySourceFile, false, nil, nil, false, false},
		{"cash.leveling.trigger_base", 10000.0, rpc.CashPolicySourceFile, 10000.0, new(0.0), nil, true, true},
		{"cash.leveling.payback_days", 30, rpc.CashPolicySourceFile, 30, new(1.0), new(365.0), false, true},
		{"cash.leveling.max_slippage_bp", 2.0, rpc.CashPolicySourceFile, 2.0, new(0.0), new(100.0), true, true},
		{"cash.leveling.currency.USD.deliberate_carry", false, rpc.CashPolicySourceCanaryDefault, false, nil, nil, false, false},
		{"cash.sweep.mode", rpc.CashSweepModeShadow, rpc.CashPolicySourceFile, rpc.CashSweepModeShadow, nil, nil, false, false},
		{"cash.sweep.max_order_notional", 50000.0, rpc.CashPolicySourceFile, 50000.0, new(0.0), nil, true, true},
		{"cash.sweep.reserve_pct_nlv", 10.0, rpc.CashPolicySourceFile, 10.0, new(0.0), new(100.0), false, true},
		// The builder note: order_step_base unwritten is not written, never a default.
		{"cash.sweep.order_step_base", nil, rpc.CashPolicySourceNotWritten, 1000.0, new(0.0), nil, false, true},
		// Absent reads off; Canary writes true.
		{"cash.sweep.bills_exempt_from_trading_max_notional", false, rpc.CashPolicySourceCanaryDefault, true, nil, nil, false, true},
		{"cash.sweep.currency.GBP.keep_cash", 8000.0, rpc.CashPolicySourceFile, nil, new(0.0), nil, false, false},
		{"cash.sweep.currency.USD.keep_cash", nil, rpc.CashPolicySourceCanaryDefault, nil, new(0.0), nil, false, false},
	} {
		row, ok := cashPolicySetting(snap, c.key)
		if !ok {
			t.Fatalf("%s missing", c.key)
		}
		if !cashPolicySame(row.Value, c.value) || row.Source != c.source || !cashPolicySame(row.Default, c.def) || row.MinExclusive != c.exclude || row.Reset != c.reset {
			t.Errorf("%s = %+v", c.key, row)
		}
		if (c.min == nil) != (row.Min == nil) || c.min != nil && *c.min != *row.Min || (c.max == nil) != (row.Max == nil) || c.max != nil && *c.max != *row.Max {
			t.Errorf("%s bounds min %v max %v", c.key, row.Min, row.Max)
		}
	}
	if row, _ := cashPolicySetting(snap, "cash.sweep.mode"); !slices.Equal(row.Choices, []string{"shadow", "active"}) || !slices.Equal(row.ChoiceLabels, []string{"Observe only", "Propose orders"}) {
		t.Fatalf("mode choices %+v", row)
	}
	if row, _ := cashPolicySetting(snap, "cash.sweep.currency.GBP.keep_cash"); !row.Removable || row.Unit != rpc.CashPolicyUnitOwn {
		t.Fatalf("per-currency float %+v", row)
	}
	// Facts from the synthetic book.
	for key, want := range map[string]string{
		"cash.leveling.trigger_base":                        "About 11,700 USD. USD is borrowed beyond it now.",
		"cash.leveling.cushion_base":                        "About 290 USD.",
		"cash.sweep.reserve_floor_base":                     "Now 20,000 EUR: 10% of NLV.",
		"cash.sweep.max_order_notional":                     "Now 20,000 to 50,000 EUR per order.",
		"cash.sweep.no_buy_while_borrowed":                  "Bill buys paused: USD is borrowed.",
		"cash.sweep.bills_exempt_from_trading_max_notional": "Order cap now: 25,000 EUR.",
	} {
		if row, _ := cashPolicySetting(snap, key); row.Fact != want {
			t.Errorf("%s fact %q, want %q", key, row.Fact, want)
		}
	}
	if snap.Sections.Leveling.Fact != "Interest rates from the broker's statements, to 3 Oct 2026." ||
		snap.Sections.Leveling.Cap != "One repayment at most 25,000 EUR: the order cap in force." ||
		snap.Sections.Sweep.Authority != "You approve each sweep order." || snap.Sections.Sweep.PreAuthorised {
		t.Fatalf("section facts %+v", snap.Sections)
	}
	usd := snap.Currencies[slices.IndexFunc(snap.Currencies, func(c rpc.CashPolicyCurrency) bool { return c.Currency == "USD" })]
	if !usd.Pair || usd.Cash == nil || *usd.Cash != -20000 || usd.LoanRate == nil || *usd.LoanRate != 0.051 || usd.Buys != "US Treasury bills" {
		t.Fatalf("USD row %+v", usd)
	}
	if gbp := snap.Currencies[slices.IndexFunc(snap.Currencies, func(c rpc.CashPolicyCurrency) bool { return c.Currency == "GBP" })]; gbp.Buys != "UK Treasury bills · No bills listed: add ISINs in the file" {
		t.Fatalf("GBP buys %q", gbp.Buys)
	}
	if snap.Currencies[0].Currency != "EUR" {
		t.Fatalf("base currency not first: %+v", snap.Currencies)
	}
	for _, f := range snap.Findings {
		if !slices.Contains(cashPolicyFindingRules, f.Rule) || len(f.Keys) == 0 || !strings.HasPrefix(f.Keys[0], "cash.") {
			t.Fatalf("finding %+v", f)
		}
	}
}

func TestCashPolicySaveWritesOnlyTheChangedLinesWithProvenanceAndBackup(t *testing.T) {
	s, path, core := cashPolicyServer(t, cashPolicyTestFile)
	before := readFile(t, path)
	head, _ := core.AuthorityHead(t.Context())
	out := cashPolicySave(t, s, map[string]any{
		"cash.leveling.trigger_base":    5000,
		"cash.sweep.max_order_notional": 80000,
		"cash.sweep.order_step_base":    500,
	}, "desk-cash-policy-1")
	if out.SavedVersion != 15 || !out.InForce || out.Replay || out.RequestID != "desk-cash-policy-1" || out.PolicyVersion != 15 || out.InForceVersion != 15 || out.FileState != rpc.CashPolicyFileOK {
		t.Fatalf("result %+v", out)
	}
	after := readFile(t, path)
	stamp := "2026-10-06 14:05 CEST"
	confirmed := "confirmed in the companion (action 7f3c2a90)"
	want := strings.NewReplacer(
		"policy_version = 14\n", "policy_version = 15  # raised in Desk "+stamp+"\n",
		"trigger_base = 10000.0\n", "trigger_base = 5000.0  # set in Desk "+stamp+", "+confirmed+"; was 10000.0\n",
		// The owner's comment stays; the provenance goes above the key.
		"max_order_notional = 50000.0  # chosen after the review\n", "# max_order_notional set in Desk "+stamp+", "+confirmed+"; was 50000.0\nmax_order_notional = 80000.0  # chosen after the review\n",
		// Canary's own comment is replaced.
		"order_step_base = 1000.0  # written by Canary v9.9.9; the sweep reads it from this file only\n", "order_step_base = 500.0  # set in Desk "+stamp+", "+confirmed+"; was 1000.0\n",
	).Replace(before)
	if after != want {
		t.Fatalf("file after save:\n%s\nwant:\n%s", after, want)
	}
	p, _, err := parseProtectionPolicy([]byte(after))
	if err != nil || *p.Cash.Leveling.TriggerBase != 5000 || p.Cash.Sweep.MaxOrderNotional != 80000 || *p.Cash.Sweep.OrderStepBase != 500 || p.PolicyVersion != 15 {
		t.Fatalf("saved file reads %+v %v", p.Cash, err)
	}
	if active, _ := s.protectionPolicies.Active(); active.PolicyVersion != 15 || *active.Cash.Leveling.TriggerBase != 5000 {
		t.Fatal("the reload in the same call did not adopt the save")
	}
	backups, _ := filepath.Glob(path + ".bak-desk-*")
	if len(backups) != 1 || readFile(t, backups[0]) != before {
		t.Fatalf("backup %v", backups)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", info.Mode())
	}
	event, found, err := core.GetEvent(t.Context(), daemonStateScope, cashPolicyEventKey("desk-cash-policy-1"))
	var receipt cashPolicyReceipt
	if err != nil || !found || event.Type != cashPolicyReceiptType || event.Origin != rpc.OrderOriginAgent || json.Unmarshal(event.PayloadJSON, &receipt) != nil {
		t.Fatalf("receipt %+v %v %v", event, found, err)
	}
	if receipt.SavedVersion != 15 || receipt.FromVersion != 14 || receipt.DeskActionID != "7f3c2a90deadbeef" || receipt.Credential != "companion:key-1" ||
		!cashPolicySame(receipt.Before["cash.leveling.trigger_base"], 10000.0) || receipt.Backup != backups[0] || receipt.Envelope == "" {
		t.Fatalf("receipt %+v", receipt)
	}
	if next, _ := core.AuthorityHead(t.Context()); next == head {
		t.Fatal("the save recorded no receipt")
	}
}

func TestCashPolicyResetWritesCanaryDefaultsAndLeavesSwitchesAndEntries(t *testing.T) {
	file := strings.NewReplacer("trigger_base = 10000.0", "trigger_base = 7000.0", "payback_days = 30", "payback_days = 60",
		"min_order_notional = 20000.0", "min_order_notional = 15000.0", "no_buy_while_borrowed = true", "no_buy_while_borrowed = false").Replace(cashPolicyTestFile)
	s, path, _ := cashPolicyServer(t, file)
	snap := cashPolicyGet(t, s)
	// What Desk's Reset fills: each section's keys flagged reset, at Canary's value.
	changes := map[string]any{}
	for _, row := range snap.Settings {
		if row.Reset {
			changes[row.Key] = row.Default
		}
	}
	out := cashPolicySave(t, s, changes, "desk-reset-1")
	if out.SavedVersion != 15 {
		t.Fatalf("result %+v", out)
	}
	p, _, err := parseProtectionPolicy([]byte(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	if *p.Cash.Leveling.TriggerBase != 10000 || *p.Cash.Leveling.PaybackDays != 30 || *p.Cash.Sweep.MinOrderNotional != 20000 || !*p.Cash.Sweep.NoBuyWhileBorrowed {
		t.Fatalf("reset did not write Canary's values: %+v %+v", p.Cash.Leveling, p.Cash.Sweep)
	}
	if p.Cash.Leveling.Enabled || !p.Cash.Sweep.Enabled || p.Cash.Sweep.Mode != "shadow" || *p.Cash.Sweep.Currency["GBP"].KeepCash != 8000 {
		t.Fatal("reset touched a switch, the mode or a per-currency entry")
	}
}

// Owner question 4: Reset restores Canary's written numbers and rules only.
func TestCashPolicyResetCoversNumbersAndRulesOnly(t *testing.T) {
	var got []string
	for _, sp := range cashPolicySpecs {
		if sp.reset() {
			got = append(got, sp.key(""))
		}
	}
	want := []string{"cash.leveling.trigger_base", "cash.leveling.cushion_base", "cash.leveling.max_slippage_bp", "cash.leveling.payback_days",
		"cash.sweep.reserve_floor_base", "cash.sweep.reserve_pct_nlv", "cash.sweep.min_order_notional", "cash.sweep.max_order_notional",
		"cash.sweep.max_order_pct_nlv", "cash.sweep.no_buy_while_borrowed", "cash.sweep.bills_exempt_from_trading_max_notional",
		"cash.sweep.order_step_base", "cash.sweep.keep_cash"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("reset covers %v, want %v", got, want)
	}
}

func TestCashPolicyValidationErrorsComePerFieldAndWriteNothing(t *testing.T) {
	s, path, core := cashPolicyServer(t, cashPolicyTestFile)
	before := readFile(t, path)
	head, _ := core.AuthorityHead(t.Context())
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{
		"cash.leveling.payback_days":        400,
		"cash.leveling.trigger_base":        -1,
		"cash.leveling.max_slippage_bp":     150,
		"cash.sweep.max_order_notional":     0,
		"cash.sweep.reserve_pct_nlv":        120,
		"cash.sweep.mode":                   "fast",
		"cash.sweep.enabled":                "yes",
		"cash.sweep.currency.GBP.keep_cash": -5,
	})
	want := map[string]string{
		"cash.leveling.payback_days":        "Must be from 1 to 365 days.",
		"cash.leveling.trigger_base":        "Must be finite and nonnegative.",
		"cash.leveling.max_slippage_bp":     "Must be at most 100 basis points.",
		"cash.sweep.max_order_notional":     "Must be more than 0: Canary reads 0 as not written.",
		"cash.sweep.reserve_pct_nlv":        "Must be a percentage from 0 to 100.",
		"cash.sweep.mode":                   "Must be Observe only or Propose orders.",
		"cash.sweep.enabled":                "Must be on or off.",
		"cash.sweep.currency.GBP.keep_cash": "Must not be negative.",
	}
	for key, msg := range want {
		if check.Errors[key] != msg {
			t.Errorf("%s error %q, want %q", key, check.Errors[key], msg)
		}
	}
	if check.Digest != "" || check.Terms != "" {
		t.Fatal("a draft with errors got terms")
	}
	if whole := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.payback_days": 2.5}); whole.Errors["cash.leveling.payback_days"] != "Must be a whole number of days." {
		t.Fatalf("whole days %v", whole.Errors)
	}
	// Terms Canary never issued for an invalid value are refused at apply too.
	terms, digest := cashPolicyTermsFor(snap.Revision, []cashPolicyEdit{{key: "cash.leveling.payback_days", raw: json.RawMessage("400")}})
	if _, err := cashPolicyApply(s, terms, digest, "desk-invalid", cashPolicyConfirmation(), ""); rpcCode(err) != rpc.CodePolicyInvalid {
		t.Fatalf("invalid apply %v", err)
	}
	if readFile(t, path) != before {
		t.Fatal("a refused draft wrote the file")
	}
	if after, _ := core.AuthorityHead(t.Context()); after != head {
		t.Fatal("a refused draft recorded a receipt")
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 0 {
		t.Fatalf("a refused draft wrote a backup %v", backups)
	}
}

func rpcCode(err error) string {
	if e, ok := err.(*rpc.Error); ok {
		return e.Code
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func TestCashPolicyStaleRevisionIsAConflictAndWritesNothing(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.trigger_base": 5000})
	// The owner edits the file by hand between the check and the save.
	edited := strings.Replace(cashPolicyTestFile, "cushion_base = 250.0", "cushion_base = 300.0", 1)
	edited = strings.Replace(edited, "policy_version = 14", "policy_version = 15", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if again := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.trigger_base": 5000}); !again.Conflict || len(again.Changes) != 0 {
		t.Fatalf("stale check %+v", again)
	}
	if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-stale", cashPolicyConfirmation(), ""); rpcCode(err) != rpc.CodeSettingsConflict {
		t.Fatalf("stale apply %v", err)
	}
	if readFile(t, path) != edited {
		t.Fatal("a stale save wrote the file")
	}
}

func TestCashPolicyRepeatedRequestIDIsIdempotent(t *testing.T) {
	s, path, core := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.cushion_base": 400})
	first, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-once", cashPolicyConfirmation(), "")
	if err != nil || first.Replay || first.SavedVersion != 15 {
		t.Fatalf("first %+v %v", first, err)
	}
	saved := readFile(t, path)
	head, _ := core.AuthorityHead(t.Context())
	again, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-once", cashPolicyConfirmation(), "")
	if err != nil || !again.Replay || again.SavedVersion != 15 || !again.InForce {
		t.Fatalf("retry %+v %v", again, err)
	}
	if readFile(t, path) != saved {
		t.Fatal("a retried request wrote the file again")
	}
	if after, _ := core.AuthorityHead(t.Context()); after != head {
		t.Fatal("a retried request recorded another receipt")
	}
	if backups, _ := filepath.Glob(path + ".bak-desk-*"); len(backups) != 1 {
		t.Fatalf("a retried request wrote another backup %v", backups)
	}
	// The same id with other terms is refused.
	other := cashPolicyCheck(t, s, again.Revision, map[string]any{"cash.leveling.cushion_base": 500})
	if _, err := cashPolicyApply(s, other.Terms, other.Digest, "desk-once", cashPolicyConfirmation(), ""); rpcCode(err) != rpc.CodeRequestReused {
		t.Fatalf("reused id %v", err)
	}
	if readFile(t, path) != saved {
		t.Fatal("a reused request id wrote the file")
	}
}

// The legacy layout reads, and a save edits the sweep where the file keeps
// it, so the sweep is never written twice.
func TestCashPolicyLegacyLayoutIsReadAndWrittenInPlace(t *testing.T) {
	s, path, _ := cashPolicyServer(t, strings.Replace(legacyCashFile, "policy_id = \"protection-test\"", "policy_id = \"protection-test\"", 1))
	snap := cashPolicyGet(t, s)
	if !snap.LegacyLayout || snap.FileState != rpc.CashPolicyFileOK || !snap.Writable || !snap.Sections.Sweep.PreAuthorised {
		t.Fatalf("legacy snapshot %+v", snap)
	}
	if !strings.HasPrefix(snap.Sections.Sweep.Authority, "The daemon sends sweep orders itself ") ||
		!strings.HasSuffix(snap.Sections.Sweep.Authority, " after announcing them, because Canary's protection policy file pre-authorises the sweep. You can change this there (pre_authorised in [cash]).") {
		t.Fatalf("sweep authority %q", snap.Sections.Sweep.Authority)
	}
	if row, _ := cashPolicySetting(snap, "cash.sweep.max_order_notional"); !cashPolicySame(row.Value, 12000.0) || row.Source != rpc.CashPolicySourceFile {
		t.Fatalf("legacy value %+v", row)
	}
	if row, _ := cashPolicySetting(snap, "cash.sweep.currency.EUR.keep_cash"); !cashPolicySame(row.Value, 6000.0) || row.Source != rpc.CashPolicySourceFile {
		t.Fatalf("legacy currency value %+v", row)
	}
	cashPolicySave(t, s, map[string]any{"cash.sweep.max_order_notional": 15000, "cash.sweep.currency.USD.keep_cash": 3000, "cash.sweep.enabled": false}, "desk-legacy")
	text := readFile(t, path)
	if strings.Contains(text, "[cash.sweep") {
		t.Fatalf("a save wrote the sweep a second time:\n%s", text)
	}
	for _, want := range []string{"max_order_notional = 15000.0  # set in Desk", "[buckets.cash_sweep.currency.USD]\nkeep_cash = 3000.0  # set in Desk", "enabled = false  # set in Desk"} {
		if !strings.Contains(text, want) {
			t.Fatalf("legacy save lacks %q:\n%s", want, text)
		}
	}
	p, _, err := parseProtectionPolicy([]byte(text))
	if err != nil || p.Cash.Sweep.MaxOrderNotional != 15000 || *p.Cash.Sweep.Currency["USD"].KeepCash != 3000 || p.Cash.Sweep.Enabled {
		t.Fatalf("legacy save reads %+v %v", p.Cash.Sweep, err)
	}
}

func TestCashPolicyRefusesAnOriginThatIsNotAllowed(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	before := readFile(t, path)
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.trigger_base": 12000})
	for _, origin := range []string{rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginDaemonOwnerQueued, "mcp"} {
		_, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-origin-1", cashPolicyConfirmation(), origin)
		if err == nil || !strings.Contains(err.Error(), "only from Desk's console") {
			t.Fatalf("origin %q: %v", origin, err)
		}
	}
	if readFile(t, path) != before {
		t.Fatal("a refused origin wrote the file")
	}
	if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-agent", cashPolicyConfirmation(), rpc.OrderOriginAgent); err != nil {
		t.Fatalf("agent origin with confirmation: %v", err)
	}
}

// Owner question 1 (2026-10-06 15:31 CEST): every save carries a
// confirmation reference; nothing is written without one.
func TestCashPolicyApplyNeedsAConfirmationReferenceOnEverySave(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	before := readFile(t, path)
	snap := cashPolicyGet(t, s)
	narrow := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.trigger_base": 20000, "cash.sweep.enabled": false})
	if len(narrow.Consequences) != 0 {
		t.Fatalf("a narrowing draft stated consequences %v", narrow.Consequences)
	}
	for _, c := range []*rpc.CashPolicyConfirmation{nil, {DeskActionID: "a1"}, {DeskActionID: "a1", Credential: "passkey:x"}} {
		if _, err := cashPolicyApply(s, narrow.Terms, narrow.Digest, "desk-narrow", c, ""); rpcCode(err) != rpc.CodeConfirmationRequired {
			t.Fatalf("confirmation %+v: %v", c, err)
		}
	}
	if readFile(t, path) != before {
		t.Fatal("a save without a confirmation reference wrote the file")
	}
}

// Owner question 1 (2026-10-06 15:31 CEST: "Cache the decision for 5 or 10
// minutes, if not serious concerns"), with the window in the file (review
// finding C2, 2026-10-06 16:56 CEST): a save may rely on an earlier save the
// device confirmed, unless it lets more reach the broker, inside the window
// Canary works out itself from its receipt and [cash] confirmation_window in
// force at the relying save.
func TestCashPolicySaveMayRelyOnAnEarlierDeviceConfirmation(t *testing.T) {
	s, path, core := cashPolicyServer(t, cashPolicyTestFile)
	first := cashPolicySave(t, s, map[string]any{"cash.leveling.cushion_base": 300}, "desk-cash-policy-FIRST")
	until := cashPolicyTestNow.Add(10 * time.Minute)
	if !first.ConfirmedUntil.Equal(until) {
		t.Fatalf("the device-confirmed save opens a window to %v, want %v", first.ConfirmedUntil, until)
	}
	relying := func(id string) *rpc.CashPolicyConfirmation {
		c := cashPolicyConfirmation()
		c.DeskActionID, c.Envelope, c.ConfirmedBy = id, `{"credential":"relied","confirmed_by":"desk-cash-policy-FIRST"}`, "desk-cash-policy-FIRST"
		return c
	}
	at := func(d time.Duration) { s.now = func() time.Time { return cashPolicyTestNow.Add(d) } }
	at(2 * time.Minute)
	narrow := cashPolicyCheck(t, s, first.Revision, map[string]any{"cash.leveling.trigger_base": 12000})
	if len(narrow.Consequences) != 0 {
		t.Fatalf("narrowing draft has consequences %v", narrow.Consequences)
	}
	saved, err := cashPolicyApply(s, narrow.Terms, narrow.Digest, "desk-cash-policy-SECOND", relying("8a1b2c3d4e5f"), "")
	if err != nil || saved.SavedVersion != 16 || !saved.ConfirmedUntil.Equal(until) {
		t.Fatalf("relying save %+v %v", saved, err)
	}
	if !strings.Contains(readFile(t, path), "trigger_base = 12000.0  # set in Desk 2026-10-06 14:07 CEST, relying on the companion's confirmation of action 7f3c2a90 until 14:15 CEST (action 8a1b2c3d); was 10000.0") {
		t.Fatalf("relying provenance:\n%s", readFile(t, path))
	}
	event, _, _ := core.GetEvent(t.Context(), daemonStateScope, cashPolicyEventKey("desk-cash-policy-SECOND"))
	var receipt cashPolicyReceipt
	if json.Unmarshal(event.PayloadJSON, &receipt) != nil || receipt.ConfirmedBy != "desk-cash-policy-FIRST" || !receipt.ConfirmedUntil.Equal(until) {
		t.Fatalf("relying receipt %+v", receipt)
	}
	// A retry of the relying save answers with the same receipt and end.
	if again, err := cashPolicyApply(s, narrow.Terms, narrow.Digest, "desk-cash-policy-SECOND", relying("8a1b2c3d4e5f"), ""); err != nil || !again.Replay || !again.ConfirmedUntil.Equal(until) {
		t.Fatalf("replay %+v %v", again, err)
	}
	refused := func(name string, changes map[string]any, c *rpc.CashPolicyConfirmation, words string) {
		t.Helper()
		written := readFile(t, path)
		snap := cashPolicyGet(t, s)
		check := cashPolicyCheck(t, s, snap.Revision, changes)
		_, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-cash-policy-"+strings.ReplaceAll(name, " ", "-"), c, "")
		if rpcCode(err) != rpc.CodeConfirmationRequired || !strings.Contains(err.Error(), words) {
			t.Fatalf("%s: %v", name, err)
		}
		if readFile(t, path) != written {
			t.Fatalf("%s wrote the file", name)
		}
	}
	// The serious concern: a save that lets more reach the broker needs the device.
	refused("widening", map[string]any{"cash.leveling.enabled": true}, relying("w1"), "lets more reach the broker")
	unknown := relying("u1")
	unknown.ConfirmedBy = "desk-cash-policy-NONE"
	refused("unknown save", map[string]any{"cash.leveling.trigger_base": 13000}, unknown, "holds no save")
	chained := relying("c1")
	chained.ConfirmedBy = "desk-cash-policy-SECOND"
	refused("chained", map[string]any{"cash.leveling.trigger_base": 13000}, chained, "not confirmed on your device")
	other := relying("o1")
	other.Credential = "passkey:other"
	refused("another credential", map[string]any{"cash.leveling.trigger_base": 13000}, other, "not confirmed on your device")
	// The window ends ten minutes after the device-confirmed save's receipt.
	at(10 * time.Minute)
	refused("expired", map[string]any{"cash.leveling.trigger_base": 13000}, relying("e1"), "ended at 14:15 CEST")
	// The window in force at the relying save decides: a shorter one in the
	// file ends the reliance sooner, and 0s ends it.
	for _, window := range []struct{ value, words string }{{`"1m"`, "ended at 14:06 CEST"}, {`"0s"`, "confirmation_window is 0s"}} {
		at(2 * time.Minute)
		text := readFile(t, path)
		text = strings.Replace(text, `confirmation_window = "10m"`, "confirmation_window = "+window.value, 1)
		text = strings.Replace(text, "confirmation_window = \"1m\"", "confirmation_window = "+window.value, 1)
		version := cashPolicyGet(t, s).PolicyVersion
		text = strings.Replace(text, fmt.Sprintf("policy_version = %d", version), fmt.Sprintf("policy_version = %d", version+1), 1)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		s.protectionPolicies.reload()
		refused("window "+strings.Trim(window.value, `"`), map[string]any{"cash.leveling.trigger_base": 13000}, relying("x1"), window.words)
	}
}

// Owner questions 2 and 6: pre-authorisation, bill lists, the ETF fallback
// and tax_reviewed_at stay in the file.
func TestCashPolicyScopeLeavesPreAuthorisationAndBillListsInTheFile(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	keys := []string{"cash.pre_authorised", "cash.sweep.currency.EUR.isins", "cash.sweep.currency.EUR.fallback", "cash.sweep.currency.EUR.etf_symbol",
		"cash.sweep.tax_reviewed_at", "cash.sweep.currency_priority", "cash.sweep.reserve_cushion_eur", "cash.sweep.min_net_gain", "authority.veto_window"}
	changes := map[string]any{}
	for _, key := range keys {
		if _, ok := cashPolicySetting(snap, key); ok {
			t.Fatalf("%s is on the screen", key)
		}
		changes[key] = "x"
	}
	check := cashPolicyCheck(t, s, snap.Revision, changes)
	for _, key := range keys {
		if check.Errors[key] != "Desk cannot change this setting; change it in the file." {
			t.Fatalf("%s: %q", key, check.Errors[key])
		}
	}
	// Terms naming a key out of scope are refused at apply too.
	terms, digest := cashPolicyTermsFor(snap.Revision, []cashPolicyEdit{{key: "cash.pre_authorised", raw: json.RawMessage(`["cash_sweep"]`)}})
	if _, err := cashPolicyApply(s, terms, digest, "desk-scope", cashPolicyConfirmation(), ""); rpcCode(err) != rpc.CodePolicyInvalid {
		t.Fatalf("out-of-scope apply %v", err)
	}
}

// Owner question 3 (2026-10-06 15:31 CEST): while the file pre-authorises
// the sweep, a save may switch it on and move it to active; the first
// consequence says that the daemon then sends the orders itself, after the
// veto window, and the cap in force.
func TestCashPolicyRaisingAPreAuthorisedSweepSaysTheDaemonSendsFirst(t *testing.T) {
	file := strings.Replace(cashPolicyTestFile, "[cash]\n", "[cash]\npre_authorised = [\"cash_sweep\"]\n", 1)
	file = strings.Replace(file, "enabled = true\nmode = \"shadow\"", "enabled = false\nmode = \"shadow\"", 1)
	s, _, _ := cashPolicyServer(t, file)
	snap := cashPolicyGet(t, s)
	if !snap.Sections.Sweep.PreAuthorised || !strings.Contains(snap.Sections.Sweep.Authority, "The daemon sends sweep orders itself 30 minutes after announcing them") {
		t.Fatalf("authority %+v", snap.Sections.Sweep)
	}
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.sweep.enabled": true, "cash.sweep.mode": "active", "cash.sweep.max_order_notional": 60000})
	// The synthetic NLV is 200,000: 10% is 20,000, below 60,000; bills are exempt from the 25,000 order cap.
	if len(check.Errors) != 0 || check.Digest == "" || len(check.Consequences) == 0 ||
		check.Consequences[0] != "The daemon will send sweep orders itself, 30 minutes after announcing them, up to 60,000 EUR each at today's NLV." {
		t.Fatalf("raise %+v", check)
	}
	for _, c := range check.Consequences[1:] {
		if strings.Contains(c, "proposals you can approve") || strings.Contains(c, "Observe only") {
			t.Fatalf("a sentence says the owner approves what the daemon sends: %q", c)
		}
	}
	// Without the bill exemption each order is held to the order cap in force.
	noExempt := strings.Replace(file, "bills_exempt_from_trading_max_notional = true", "bills_exempt_from_trading_max_notional = false", 1)
	s2, _, _ := cashPolicyServer(t, noExempt)
	snap2 := cashPolicyGet(t, s2)
	if c := cashPolicyCheck(t, s2, snap2.Revision, map[string]any{"cash.sweep.enabled": true, "cash.sweep.mode": "active"}); len(c.Consequences) == 0 ||
		c.Consequences[0] != "The daemon will send sweep orders itself, 30 minutes after announcing them, up to 25,000 EUR each at today's NLV." {
		t.Fatalf("held to the order cap %+v", c.Consequences)
	}
	// An active, pre-authorised sweep may be switched off or back to shadow,
	// and a larger order there says that the daemon sends it.
	active := strings.Replace(file, "enabled = false\nmode = \"shadow\"", "enabled = true\nmode = \"active\"", 1)
	s3, _, _ := cashPolicyServer(t, active)
	snap3 := cashPolicyGet(t, s3)
	for _, change := range []map[string]any{{"cash.sweep.enabled": false}, {"cash.sweep.mode": "shadow"}} {
		if c := cashPolicyCheck(t, s3, snap3.Revision, change); len(c.Errors) != 0 || c.Digest == "" || len(c.Consequences) != 0 {
			t.Fatalf("%v: %+v", change, c)
		}
	}
	raise := cashPolicyCheck(t, s3, snap3.Revision, map[string]any{"cash.sweep.max_order_notional": 60000})
	if len(raise.Consequences) != 1 || !strings.Contains(raise.Consequences[0], "The daemon sends sweep orders itself, 30 minutes after announcing them.") {
		t.Fatalf("pre-authorised consequence %v", raise.Consequences)
	}
}

func TestCashPolicyRefusesAFileCanaryDoesNotRunAsWritten(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	// Edited by hand without raising policy_version: drift.
	drift := strings.Replace(cashPolicyTestFile, "cushion_base = 250.0", "cushion_base = 300.0", 1)
	if err := os.WriteFile(path, []byte(drift), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := cashPolicyGet(t, s)
	if snap.FileState != rpc.CashPolicyFileDrift || snap.Writable || !strings.Contains(snap.Message, "without raising policy_version") {
		t.Fatalf("drift snapshot %+v", snap)
	}
	if row, _ := cashPolicySetting(snap, "cash.leveling.cushion_base"); !cashPolicySame(row.Value, 250.0) {
		t.Fatalf("a drifted file shows %v, not the value Canary runs", row.Value)
	}
	params, _ := json.Marshal(rpc.CashPolicyCheckRequest{ExpectedRevision: snap.Revision, Changes: map[string]json.RawMessage{"cash.leveling.trigger_base": json.RawMessage("5000")}})
	if _, err := s.handleCashPolicyCheck(t.Context(), &rpc.Request{Params: params}); rpcCode(err) != rpc.CodePolicyUnwritable {
		t.Fatalf("drift check %v", err)
	}
	terms, digest := cashPolicyTermsFor(snap.Revision, []cashPolicyEdit{{key: "cash.leveling.trigger_base", raw: json.RawMessage("5000")}})
	if _, err := cashPolicyApply(s, terms, digest, "desk-drift", cashPolicyConfirmation(), ""); rpcCode(err) != rpc.CodePolicyUnwritable {
		t.Fatalf("drift apply %v", err)
	}
	if readFile(t, path) != drift {
		t.Fatal("a drifted file was written")
	}
	// Refused and missing files are read-only too.
	if err := os.WriteFile(path, []byte("kind = \"canary.protection_policy\"\nunknown_key = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snap := cashPolicyGet(t, s); snap.FileState != rpc.CashPolicyFileRefused || snap.Writable || snap.Message == "" {
		t.Fatalf("refused snapshot %+v", snap)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if snap := cashPolicyGet(t, s); snap.FileState != rpc.CashPolicyFileMissing || snap.Writable {
		t.Fatalf("missing snapshot %+v", snap)
	}
}

func TestCashPolicyNewTablesRemovalsAndDeliberateCarry(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	cashPolicySave(t, s, map[string]any{"cash.leveling.currency.USD.deliberate_carry": true, "cash.sweep.currency.GBP.keep_cash": nil}, "desk-tables-1")
	text := readFile(t, path)
	stamp := "2026-10-06 14:05 CEST, confirmed in the companion (action 7f3c2a90)"
	for _, want := range []string{
		// A new table goes after its parent's block.
		"payback_days = 30\n\n[cash.leveling.currency.USD]\ndeliberate_carry = true  # set in Desk " + stamp + "; was not in the file\n",
		// A cleared float leaves a note in its place.
		"[cash.sweep.currency.GBP]\n# keep_cash removed in Desk " + stamp + "; was 8000.0\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("file lacks %q:\n%s", want, text)
		}
	}
	snap := cashPolicyGet(t, s)
	if row, _ := cashPolicySetting(snap, "cash.sweep.currency.GBP.keep_cash"); row.Value != nil || row.Source != rpc.CashPolicySourceCanaryDefault {
		t.Fatalf("cleared float reads %+v", row)
	}
	// Unticking writes false, so the change stays on record.
	cashPolicySave(t, s, map[string]any{"cash.leveling.currency.USD.deliberate_carry": false}, "desk-tables-2")
	text = readFile(t, path)
	if !strings.Contains(text, "deliberate_carry = false  # set in Desk") || strings.Count(text, "[cash.leveling.currency.USD]") != 1 {
		t.Fatalf("untick:\n%s", text)
	}
	if p, _, err := parseProtectionPolicy([]byte(text)); err != nil || p.PolicyVersion != 16 {
		t.Fatalf("after two saves %v %v", p.PolicyVersion, err)
	}
}

// Switching a feature on from a file without its table writes the whole
// table from the draft.
func TestCashPolicySwitchingTheSweepOnWritesItsTable(t *testing.T) {
	start := cashPolicyTestFile[:strings.Index(cashPolicyTestFile, "# Cash management")] + cashPolicyTestFile[strings.Index(cashPolicyTestFile, "# Currency leveling"):]
	s, path, _ := cashPolicyServer(t, start)
	snap := cashPolicyGet(t, s)
	if snap.Sections.Sweep.Present {
		t.Fatal("no sweep table, but present")
	}
	changes := map[string]any{"cash.sweep.enabled": true, "cash.sweep.mode": "shadow"}
	for _, row := range snap.Settings {
		if row.Section == rpc.CashPolicySectionSweep && row.Currency == "" && row.Source != rpc.CashPolicySourceFile && row.Default != nil && row.Key != "cash.sweep.enabled" && row.Key != "cash.sweep.mode" {
			changes[row.Key] = row.Default
		}
	}
	check := cashPolicyCheck(t, s, snap.Revision, changes)
	if len(check.Errors) != 0 || len(check.Consequences) == 0 || !strings.Contains(check.Consequences[0], "Observe only") {
		t.Fatalf("switch-on check %+v", check)
	}
	if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-sweep-on", cashPolicyConfirmation(), ""); err != nil {
		t.Fatal(err)
	}
	p, _, err := parseProtectionPolicy([]byte(readFile(t, path)))
	if err != nil || !p.Cash.Sweep.enabled() || len(p.Cash.Sweep.missingNumbers()) != 0 || p.Cash.Sweep.effectiveMode() != rpc.CashSweepModeShadow {
		t.Fatalf("switched-on sweep %+v %v", p.Cash.Sweep, err)
	}
	if !strings.Contains(readFile(t, path), "payback_days = 30\n\n[cash.sweep]\nenabled = true  # set in Desk") {
		t.Fatalf("new sweep table placement:\n%s", readFile(t, path))
	}
}

func TestCashPolicyConsequencesNameWhatLetsMoreReachTheBroker(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{
		"cash.leveling.enabled":                             true,
		"cash.leveling.trigger_base":                        5000,
		"cash.sweep.mode":                                   "active",
		"cash.sweep.no_buy_while_borrowed":                  false,
		"cash.sweep.max_order_notional":                     40000,
		"cash.sweep.currency.GBP.keep_cash":                 2000,
		"cash.sweep.reserve_floor_base":                     5000,
		"cash.sweep.bills_exempt_from_trading_max_notional": false,
	})
	want := []string{
		"Currency leveling starts proposing conversions for loans beyond 5,000 EUR. Each repayment waits for your approval.",
		"Leveling repays loans from 5,000 EUR instead of 10,000 EUR.",
		"Sweep rows become proposals you can approve.",
		// The share of NLV sets the reserve today (review finding C3).
		"At today's NLV the reserve kept as cash stays 20,000 EUR: 10% of NLV sets it, so the change matters only if NLV falls.",
		"Bill buys may go ahead while a currency is borrowed.",
		"GBP keeps 2,000 GBP as settlement float instead of 8,000 GBP.",
	}
	if !slices.Equal(check.Consequences, want) {
		t.Fatalf("consequences\n got %q\nwant %q", check.Consequences, want)
	}
	if check.SavedVersion != 15 || check.PolicyVersion != 14 || len(check.Changes) != 8 || check.Facts["cash.leveling.trigger_base"] != "About 5,800 USD. USD is borrowed beyond it now." {
		t.Fatalf("check %+v", check)
	}
	texts := map[string][2]string{}
	for _, c := range check.Changes {
		texts[c.Key] = [2]string{c.FromText, c.ToText}
		if c.Key == "cash.sweep.currency.GBP.keep_cash" && (!cashPolicySame(c.From, 8000.0) || c.FromSource != rpc.CashPolicySourceFile || c.Label != "Settlement float" || c.Currency != "GBP") {
			t.Fatalf("change %+v", c)
		}
	}
	for key, want := range map[string][2]string{
		"cash.leveling.enabled":                             {"Off", "On"},
		"cash.leveling.trigger_base":                        {"10,000 EUR", "5,000 EUR"},
		"cash.sweep.mode":                                   {"Observe only", "Propose orders"},
		"cash.sweep.currency.GBP.keep_cash":                 {"8,000 GBP", "2,000 GBP"},
		"cash.sweep.bills_exempt_from_trading_max_notional": {"On", "Off"},
	} {
		if texts[key] != want {
			t.Errorf("%s reads %q, want %q", key, texts[key], want)
		}
	}
	// A change that changes nothing is dropped; writing a built-in value the
	// file leaves out changes nothing either.
	if none := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.leveling.trigger_base": 10000, "cash.leveling.currency.USD.deliberate_carry": false}); len(none.Changes) != 0 || none.Digest != "" {
		t.Fatalf("no-op draft %+v", none)
	}
}

func TestCashPolicyTermsAreCanonicalAndBoundToTheirDigest(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	before := readFile(t, path)
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.sweep.keep_cash": 4000, "cash.leveling.payback_days": 45})
	want := `{"changes":{"cash.leveling.payback_days":45,"cash.sweep.keep_cash":4000},"expected_revision":"` + snap.Revision + `","kind":"canary.cash_policy_change","version":1}`
	if check.Terms != want || check.Digest != cashPolicyDigest([]byte(want)) {
		t.Fatalf("terms %s", check.Terms)
	}
	for _, c := range []struct{ terms, digest string }{
		{check.Terms, cashPolicyDigest([]byte("other"))},
		{strings.Replace(check.Terms, `"version":1`, `"version": 1`, 1), cashPolicyDigest([]byte(strings.Replace(check.Terms, `"version":1`, `"version": 1`, 1)))},
		{strings.Replace(check.Terms, `4000`, `3000`, 1), check.Digest},
	} {
		if _, err := cashPolicyApply(s, c.terms, c.digest, "desk-terms", cashPolicyConfirmation(), ""); rpcCode(err) == "" || !strings.HasPrefix(rpcCode(err), "other: ") {
			t.Fatalf("altered terms %q: %v", c.terms, err)
		}
	}
	if readFile(t, path) != before {
		t.Fatal("altered terms wrote the file")
	}
}

func TestCashPolicyMethodsKeepTheirCodesAtTheDaemonWire(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	dispatch := func(method, params string) rpc.Response {
		t.Helper()
		var out bytes.Buffer
		s.dispatch(t.Context(), &rpc.Request{ID: "wire-cash-policy", Method: method, Params: json.RawMessage(params)}, json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
		var response rpc.Response
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatalf("dispatch %s: %s: %v", method, out.Bytes(), err)
		}
		return response
	}
	get := dispatch(rpc.MethodCashPolicyGet, `{}`)
	var snap rpc.CashPolicySnapshot
	if !get.Ok || json.Unmarshal(get.Result, &snap) != nil || snap.Revision == "" {
		t.Fatalf("get %+v", get)
	}
	check := dispatch(rpc.MethodCashPolicyCheck, `{"expected_revision":"`+snap.Revision+`","changes":{"cash.leveling.trigger_base":5000}}`)
	var result rpc.CashPolicyCheckResult
	if !check.Ok || json.Unmarshal(check.Result, &result) != nil || result.Digest == "" {
		t.Fatalf("check %+v", check)
	}
	termsJSON, _ := json.Marshal(result.Terms)
	noConfirmation := dispatch(rpc.MethodCashPolicyApply, `{"terms":`+string(termsJSON)+`,"digest":"`+result.Digest+`","request_id":"wire-1"}`)
	if noConfirmation.Ok || noConfirmation.Error == nil || noConfirmation.Error.Code != rpc.CodeConfirmationRequired {
		t.Fatalf("no confirmation %+v", noConfirmation)
	}
	unknown := dispatch(rpc.MethodCashPolicyCheck, `{"expected_revision":"x","changes":{},"extra":1}`)
	if unknown.Ok || unknown.Error.Code != rpc.CodeBadRequest {
		t.Fatalf("unknown field %+v", unknown)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(cashPolicyTestFile, "policy_version = 14", "policy_version = 15", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := dispatch(rpc.MethodCashPolicyApply, `{"terms":`+string(termsJSON)+`,"digest":"`+result.Digest+`","request_id":"wire-2","confirmation":{"desk_action_id":"a1","credential":"passkey:x","envelope":"{}"}}`)
	if stale.Ok || stale.Error.Code != rpc.CodeSettingsConflict {
		t.Fatalf("stale %+v", stale)
	}
}

// A file that is ahead of the policy in force is written at its own version
// plus one, and the reload adopts it.
func TestCashPolicyWritesAFileAheadOfThePolicyInForce(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	if err := os.WriteFile(path, []byte(strings.Replace(cashPolicyTestFile, "policy_version = 14", "policy_version = 15", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := cashPolicyGet(t, s)
	if snap.FileState != rpc.CashPolicyFileAhead || !snap.Writable || snap.PolicyVersion != 15 || snap.InForceVersion != 14 {
		t.Fatalf("ahead %+v", snap)
	}
	out := cashPolicySave(t, s, map[string]any{"cash.leveling.trigger_base": 9000}, "desk-ahead")
	if out.SavedVersion != 16 || !out.InForce || out.InForceVersion != 16 {
		t.Fatalf("ahead save %+v", out)
	}
}

func TestCashPolicyTOMLDocAddsTablesAfterTheirFamily(t *testing.T) {
	doc := parseTOMLDoc([]byte("a = 1\n\n[cash]\npre_authorised = []\n\n# Leveling\n[cash.leveling]\nenabled = false\n# a trailing comment\n\n[other]\nx = 1\n"))
	doc.addTable("cash.sweep")
	doc.insert("cash.sweep", []string{"enabled = true"})
	want := "a = 1\n\n[cash]\npre_authorised = []\n\n# Leveling\n[cash.leveling]\nenabled = false\n\n[cash.sweep]\nenabled = true\n\n# a trailing comment\n\n[other]\nx = 1\n"
	if string(doc.bytes()) != want {
		t.Fatalf("got\n%s\nwant\n%s", doc.bytes(), want)
	}
	if !canaryWrote("# written by Canary v3; read from this file only") || !canaryWrote("# set in Desk 2026-10-06 14:05 CEST") || canaryWrote("# my own note") {
		t.Fatal("canaryWrote")
	}
}

// The findings are Canary's policy check over the file, or over the draft,
// kept for the rules that name a cash key.
func TestCashPolicyFindingsAreThePolicyCheckOnTheFileAndTheDraft(t *testing.T) {
	s, _, _ := cashPolicyServer(t, strings.Replace(cashPolicyTestFile, "no_buy_while_borrowed = true", "no_buy_while_borrowed = false", 1))
	snap := cashPolicyGet(t, s)
	i := slices.IndexFunc(snap.Findings, func(f rpc.CashPolicyFinding) bool { return f.Rule == "sweep_buys_while_borrowed" })
	if i < 0 || snap.Findings[i].Severity != rpc.PolicyCheckWarn || !slices.Contains(snap.Findings[i].Keys, "cash.sweep.no_buy_while_borrowed") || !strings.Contains(snap.Findings[i].Text, "USD") {
		t.Fatalf("findings %+v", snap.Findings)
	}
	// The draft that restores the rule clears the finding.
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.sweep.no_buy_while_borrowed": true})
	if slices.ContainsFunc(check.Findings, func(f rpc.CashPolicyFinding) bool { return f.Rule == "sweep_buys_while_borrowed" }) {
		t.Fatalf("draft findings %+v", check.Findings)
	}
	for _, f := range append(snap.Findings, check.Findings...) {
		if !slices.Contains(cashPolicyFindingRules, f.Rule) || len(f.Keys) == 0 || !strings.HasPrefix(f.Keys[0], "cash.") {
			t.Fatalf("finding outside the cash rules %+v", f)
		}
	}
}

// With order entry off while the sweep proposes orders and leveling is on,
// the order_entry_off_for_active_bucket error names the Orders row and the
// leveling switch, so the screen shows why nothing can be placed.
func TestCashPolicyFindingsReachTheOrderEntryOffRows(t *testing.T) {
	in := pcInput(t, pcClean())
	in.Trading.Mode = config.TradingModeDisabled
	got := cashPolicyFindings(CheckPolicy(in))
	i := slices.IndexFunc(got, func(f rpc.CashPolicyFinding) bool { return f.Rule == "order_entry_off_for_active_bucket" })
	if i < 0 {
		t.Fatalf("the order-entry-off error reaches no cash row: %+v", got)
	}
	if got[i].Severity != rpc.PolicyCheckError {
		t.Fatalf("severity %q, want error", got[i].Severity)
	}
	for _, key := range []string{"cash.sweep.mode", "cash.leveling.enabled"} {
		if !slices.Contains(got[i].Keys, key) {
			t.Errorf("finding lacks %s: keys %v", key, got[i].Keys)
		}
	}
}

// Settings review item 4 (2026-10-07): the no_buy_while_borrowed fact is the
// sweep's own borrowing test (cashSweepBorrowingFor), not a copy of it: the
// lower of trade-date and settled cash counts, unknown cash holds, and the
// words say what pauses the buys. The sentences for the rule switched off and
// for an unknown ledger stay.
func TestCashPolicyBorrowedFactIsTheSweepsOwnTest(t *testing.T) {
	for _, c := range []struct {
		name string
		file string
		book func(*cashPolicyBook)
		want string
	}{
		{"nothing borrowed", cashPolicyTestFile, func(b *cashPolicyBook) {
			b.sweep.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 6000, ExchangeRate: 0.855}
			b.cash["USD"] = 6000
		}, "No currency is borrowed now."},
		{"borrowed on trade-date cash", cashPolicyTestFile, nil, "Bill buys paused: USD is borrowed."},
		{"borrowed on settled cash only", cashPolicyTestFile, func(b *cashPolicyBook) {
			b.sweep.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 500, Settled: new(-12000.0), ExchangeRate: 0.855}
			b.cash["USD"] = 500
		}, "Bill buys paused: USD is borrowed on settled cash."},
		{"cash unknown", cashPolicyTestFile, func(b *cashPolicyBook) {
			b.sweep.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 6000, ExchangeRate: 0.855}
			b.cash["USD"] = 6000
			b.sweep.Ledger["GBP"] = cashSweepLedgerRow{ExchangeRate: 1.15}
		}, "Bill buys paused: GBP cash can't be read."},
		// A currency holding a classified equivalent without a ledger row is
		// unknown to the sweep (review finding 7, 2026-10-07): the fact lists
		// the planner's currencies, holdings included, not the ledger's alone.
		{"equivalents held in a currency without a ledger row", cashPolicyTestFile, func(b *cashPolicyBook) {
			b.sweep.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 6000, ExchangeRate: 0.855}
			b.cash["USD"] = 6000
			b.sweep.Holdings = map[string][]cashSweepHolding{"CAD": {cashSweepTestBill(801, "CAD", cashSweepInstrumentCATBill, 40, 20)}}
		}, "Bill buys paused: CAD cash can't be read."},
		{"rule off", strings.Replace(cashPolicyTestFile, "no_buy_while_borrowed = true", "no_buy_while_borrowed = false", 1), nil,
			"USD is borrowed now; bill buys go ahead."},
		{"ledger unknown", cashPolicyTestFile, func(b *cashPolicyBook) {
			b.sweep.Ledger, b.cash, b.ledgerReason = nil, map[string]float64{}, "the account could not be read"
		}, "Whether a currency is borrowed is unknown now: the account could not be read."},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := cashPolicyServer(t, c.file)
			if c.book != nil {
				s.cashPolicyBookForTest = func() cashPolicyBook { b := cashPolicyTestBook(); c.book(&b); return b }
			}
			row, _ := cashPolicySetting(cashPolicyGet(t, s), "cash.sweep.no_buy_while_borrowed")
			if row.Fact != c.want {
				t.Fatalf("fact %q, want %q", row.Fact, c.want)
			}
		})
	}
}

// The payback fact comes from the running plan, which does not follow a
// draft, so it is labelled as today's plan.
func TestCashPolicyPaybackFactIsTodaysPlan(t *testing.T) {
	s, _, _ := cashPolicyServer(t, strings.Replace(cashPolicyTestFile, "[cash.leveling]\nenabled = false", "[cash.leveling]\nenabled = true", 1))
	s.cashPolicyBookForTest = func() cashPolicyBook {
		b := cashPolicyTestBook()
		b.leveling = &rpc.TradeProposalCurrencyLevelingStatus{Bundles: []rpc.TradeProposalCurrencyLevelingBundle{{Currency: "USD", SavingBase: 52.4, CostBase: 4.2, PaybackDays: 30}}}
		return b
	}
	row, _ := cashPolicySetting(cashPolicyGet(t, s), "cash.leveling.payback_days")
	if want := "Today's plan: USD saves about 52 EUR within 30 days, costs at most 5 EUR."; row.Fact != want {
		t.Fatalf("fact %q, want %q", row.Fact, want)
	}
}

// Settings review item 1 (2026-10-07): the order-cap rows speak of the order
// cap and the switch in the screen's words, the cap in whole units.
func TestCashPolicyOrderCapRowsSayWhatTheSwitchDoes(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	exempt, _ := cashPolicySetting(snap, "cash.sweep.bills_exempt_from_trading_max_notional")
	if exempt.Label != "Let bill orders exceed the order cap" ||
		exempt.Help != "A bill order in the same currency may exceed the order cap, up to the sweep's largest order." ||
		exempt.Fact != "Order cap now: 25,000 EUR." {
		t.Fatalf("exemption row %+v", exempt)
	}
	if largest, _ := cashPolicySetting(snap, "cash.sweep.max_order_notional"); largest.Help != "Each sweep order, buy or sell, is capped at this amount or the share of NLV below, whichever is larger. It must also fit the order cap; only bill orders may exceed it, and only while \"Let bill orders exceed the order cap\" is on." {
		t.Fatalf("largest order help %q", largest.Help)
	}
	s.cashPolicyBookForTest = func() cashPolicyBook {
		b := cashPolicyTestBook()
		b.orderCap, b.orderCapReason = 0, "[order_limits] is incomplete"
		return b
	}
	if row, _ := cashPolicySetting(cashPolicyGet(t, s), "cash.sweep.bills_exempt_from_trading_max_notional"); row.Fact != "The order cap can't be read now." {
		t.Fatalf("unreadable cap fact %q", row.Fact)
	}
}

// A file ahead of the policy in force is adopted at the manager's next
// reread: within its reload interval while hot reload is on; otherwise the
// next restart or save rereads it and no interval is promised.
func TestCashPolicyAheadMessageNamesTheNextReread(t *testing.T) {
	s, path, _ := cashPolicyServer(t, cashPolicyTestFile)
	if err := os.WriteFile(path, []byte(strings.Replace(cashPolicyTestFile, "policy_version = 14", "policy_version = 15", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "the file holds version 15; Canary runs version 14 and adopts the file at its next reread"
	if snap := cashPolicyGet(t, s); snap.FileState != rpc.CashPolicyFileAhead || snap.Message != want {
		t.Fatalf("without hot reload: %s %q", snap.FileState, snap.Message)
	}
	s.protectionPolicies.hotReload, s.protectionPolicies.reloadInterval = true, 45*time.Second
	if snap := cashPolicyGet(t, s); snap.Message != want+", within 45 seconds" {
		t.Fatalf("with hot reload: %q", snap.Message)
	}
}

// Settings review item 3 (2026-10-07): a sweep currency with no bills listed
// and no usable ETF has nothing to buy; the finding reaches the sweep's
// switch in the screen's words and names every such currency.
func TestCashPolicyNothingToBuyReachesTheSweepSwitch(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	i := slices.IndexFunc(snap.Findings, func(f rpc.CashPolicyFinding) bool { return f.Rule == "sweep_nothing_to_buy" })
	if i < 0 {
		t.Fatalf("no nothing-to-buy finding: %+v", snap.Findings)
	}
	f := snap.Findings[i]
	if f.Severity != rpc.PolicyCheckWarn || !slices.Equal(f.Keys, []string{"cash.sweep.enabled", "cash.sweep.currency.EUR.isins", "cash.sweep.currency.GBP.isins"}) ||
		f.Text != "Nothing to buy in EUR or GBP: the policy file lists no bills or fallback ETF for them, so they stay in cash." {
		t.Fatalf("finding %+v", f)
	}
}

// Review finding C1 (2026-10-06 16:56 CEST): a number written where the file
// had none is always a consequence, worded as what then happens. A sweep that
// is pre-authorised, on and active but lacks a number holds; writing it lets
// the daemon send orders, so a relying save is refused.
func TestCashPolicyWritingANumberTheFileLackedIsAConsequence(t *testing.T) {
	preAuthorised := strings.Replace(cashPolicyTestFile, "[cash]\n", "[cash]\npre_authorised = [\"cash_sweep\"]\n", 1)
	preAuthorised = strings.Replace(preAuthorised, "enabled = true\nmode = \"shadow\"", "enabled = true\nmode = \"active\"", 1)
	for _, c := range []struct {
		line, key string
		value     any
		first     string
	}{
		{"max_order_notional = 50000.0  # chosen after the review\n", "cash.sweep.max_order_notional", 60000,
			"The daemon will send sweep orders itself, 30 minutes after announcing them, up to 60,000 EUR each at today's NLV."},
		{"min_order_notional = 20000.0\n", "cash.sweep.min_order_notional", 20000,
			"The daemon will send sweep orders itself, 30 minutes after announcing them, up to 50,000 EUR each at today's NLV."},
		{"reserve_floor_base = 10000.0\n", "cash.sweep.reserve_floor_base", 10000,
			"The daemon will send sweep orders itself, 30 minutes after announcing them, up to 50,000 EUR each at today's NLV."},
	} {
		t.Run(c.key, func(t *testing.T) {
			file := strings.Replace(preAuthorised, c.line, "", 1)
			s, path, _ := cashPolicyServer(t, file)
			first := cashPolicySave(t, s, map[string]any{"cash.leveling.cushion_base": 300}, "desk-cash-policy-FIRST")
			check := cashPolicyCheck(t, s, first.Revision, map[string]any{c.key: c.value})
			if len(check.Errors) != 0 || check.Digest == "" || len(check.Consequences) == 0 || check.Consequences[0] != c.first {
				t.Fatalf("writing %s: %+v", c.key, check)
			}
			written := readFile(t, path)
			relying := cashPolicyConfirmation()
			relying.DeskActionID, relying.ConfirmedBy = "8a1b2c3d", "desk-cash-policy-FIRST"
			if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-cash-policy-SECOND", relying, ""); rpcCode(err) != rpc.CodeConfirmationRequired ||
				!strings.Contains(err.Error(), "lets more reach the broker") {
				t.Fatalf("a relying save unblocked the sweep: %v", err)
			}
			if readFile(t, path) != written {
				t.Fatal("the refused save wrote the file")
			}
		})
	}
	// Without pre-authorisation the sweep proposes; with more missing it still
	// waits; leveling that lacks its band can then propose.
	active := strings.Replace(cashPolicyTestFile, "enabled = true\nmode = \"shadow\"", "enabled = true\nmode = \"active\"", 1)
	lacking := strings.Replace(active, "max_order_notional = 50000.0  # chosen after the review\n", "", 1)
	levelingOn := strings.Replace(cashPolicyTestFile, "[cash.leveling]\nenabled = false\ntrigger_base = 10000.0\n", "[cash.leveling]\nenabled = true\n", 1)
	for _, c := range []struct {
		name, file string
		changes    map[string]any
		want       string
	}{
		{"sweep proposes", lacking, map[string]any{"cash.sweep.max_order_notional": 60000},
			"The sweep can now propose bill orders for you to approve: the file had no largest order."},
		{"sweep still waits", strings.Replace(lacking, "min_order_notional = 20000.0\n", "", 1), map[string]any{"cash.sweep.max_order_notional": 60000},
			"The file gets the sweep's largest order; the sweep still waits for its smallest buy."},
		{"sweep off", strings.Replace(lacking, "enabled = true\nmode = \"active\"", "enabled = false\nmode = \"active\"", 1), map[string]any{"cash.sweep.max_order_notional": 60000},
			"The sweep can now work once you switch it on: the file had no largest order."},
		{"leveling proposes", levelingOn, map[string]any{"cash.leveling.trigger_base": 10000},
			"Leveling can now propose conversions for you to approve: the file had no band."},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := cashPolicyServer(t, c.file)
			check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, c.changes)
			if len(check.Errors) != 0 || !slices.Contains(check.Consequences, c.want) {
				t.Fatalf("%s: %+v", c.name, check)
			}
		})
	}
}

// Review finding C3 (2026-10-06 16:56 CEST): a size that depends on NLV or
// the order cap in force says what it comes to at today's NLV, and why when
// that does not change today; the save stays a widening either way.
func TestCashPolicySizesSayWhatChangesAtTodaysNLV(t *testing.T) {
	// The synthetic NLV is 200,000 and the order cap in force 25,000.
	for _, c := range []struct {
		name    string
		replace [2]string
		book    func(*cashPolicyBook)
		changes map[string]any
		want    string
	}{
		{"fixed amount sets it", [2]string{}, nil, map[string]any{"cash.sweep.max_order_notional": 80000},
			"At today's NLV the largest sweep order rises from 50,000 EUR to 80,000 EUR."},
		{"share sets it", [2]string{"max_order_pct_nlv = 10.0", "max_order_pct_nlv = 50.0"}, nil, map[string]any{"cash.sweep.max_order_notional": 80000},
			"At today's NLV the largest sweep order stays 100,000 EUR: 50% of NLV sets it, so the change matters only if NLV falls."},
		{"cap holds it", [2]string{"bills_exempt_from_trading_max_notional = true", "bills_exempt_from_trading_max_notional = false"}, nil, map[string]any{"cash.sweep.max_order_notional": 80000},
			"At today's NLV the largest sweep order stays 25,000 EUR: the order cap in force holds it, so the change matters only if that cap rises or bills may pass it."},
		{"larger share", [2]string{}, nil, map[string]any{"cash.sweep.max_order_pct_nlv": 40},
			"At today's NLV the largest sweep order rises from 50,000 EUR to 80,000 EUR."},
		{"smaller reserve share", [2]string{}, nil, map[string]any{"cash.sweep.reserve_pct_nlv": 2},
			"At today's NLV the reserve kept as cash falls from 20,000 EUR to 10,000 EUR."},
		{"bills pass the cap", [2]string{"bills_exempt_from_trading_max_notional = true", "bills_exempt_from_trading_max_notional = false"}, nil,
			map[string]any{"cash.sweep.bills_exempt_from_trading_max_notional": true},
			"Bill orders may pass the order cap in force of 25,000 EUR: at today's NLV the largest sweep order rises from 25,000 EUR to 50,000 EUR."},
		{"NLV unknown", [2]string{}, func(b *cashPolicyBook) { b.nlv = 0 }, map[string]any{"cash.sweep.max_order_notional": 80000},
			"The largest sweep order's fixed amount rises from 50,000 EUR to 80,000 EUR; what that comes to today is unknown, because NLV cannot be read now."},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := cashPolicyTestFile
			if c.replace[0] != "" {
				file = strings.Replace(file, c.replace[0], c.replace[1], 1)
			}
			s, _, _ := cashPolicyServer(t, file)
			if c.book != nil {
				s.cashPolicyBookForTest = func() cashPolicyBook { b := cashPolicyTestBook(); c.book(&b); return b }
			}
			check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, c.changes)
			if len(check.Errors) != 0 || !slices.Equal(check.Consequences, []string{c.want}) {
				t.Fatalf("%s:\n got %q\nwant %q", c.name, check.Consequences, c.want)
			}
		})
	}
}

// Review finding C2 (2026-10-06 16:56 CEST): the window is [cash]
// confirmation_window, read from the file only and shown read-only.
func TestCashPolicyConfirmationWindowComesFromTheFile(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	snap := cashPolicyGet(t, s)
	if snap.ConfirmationWindowSeconds != 600 || snap.Confirmation != "For 10 minutes after your passkey or companion confirms a save, further saves from the same Desk console that let no more reach the broker need no new confirmation. You can change the 10 minutes in Canary's protection policy file (confirmation_window in [cash])." {
		t.Fatalf("window %d %q", snap.ConfirmationWindowSeconds, snap.Confirmation)
	}
	if _, ok := cashPolicySetting(snap, "cash.confirmation_window"); ok {
		t.Fatal("the window is on the screen as a setting Desk could change")
	}
	check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.confirmation_window": "1h"})
	if check.Errors["cash.confirmation_window"] != "Desk cannot change this setting; change it in the file." {
		t.Fatalf("window change %+v", check.Errors)
	}
	none, _, _ := cashPolicyServer(t, strings.Replace(cashPolicyTestFile, "[cash]\nconfirmation_window = \"10m\"\n", "[cash]\n", 1))
	if snap := cashPolicyGet(t, none); snap.ConfirmationWindowSeconds != 0 || snap.Confirmation != "Every save asks your passkey or companion. You can allow a few minutes without asking in Canary's protection policy file (confirmation_window in [cash])." {
		t.Fatalf("no window %d %q", snap.ConfirmationWindowSeconds, snap.Confirmation)
	}
	if saved := cashPolicySave(t, none, map[string]any{"cash.leveling.cushion_base": 300}, "desk-cash-policy-NONE"); !saved.ConfirmedUntil.IsZero() {
		t.Fatalf("a save opened a window of 0s: %v", saved.ConfirmedUntil)
	}
	for value, ok := range map[string]bool{`"10m"`: true, `"0s"`: true, `"90s"`: true, `"-1m"`: false, `"soon"`: false, `10`: false} {
		_, _, err := parseProtectionPolicy([]byte(strings.Replace(cashPolicyTestFile, `confirmation_window = "10m"`, "confirmation_window = "+value, 1)))
		if (err == nil) != ok {
			t.Errorf("confirmation_window = %s: %v", value, err)
		}
	}
}

// The startup migration writes the owner's window where a file has none,
// with the decision beside it, and raises policy_version for it.
func TestProtectionMigrationWritesTheConfirmationWindow(t *testing.T) {
	for name, file := range map[string]string{
		"into [cash]":     strings.Replace(cashPolicyTestFile, "[cash]\nconfirmation_window = \"10m\"\n", "[cash]\npre_authorised = []\n", 1),
		"a new [cash]":    strings.Replace(cashPolicyTestFile, "[cash]\nconfirmation_window = \"10m\"\n\n", "", 1),
		"kept as written": strings.Replace(cashPolicyTestFile, `confirmation_window = "10m"`, `confirmation_window = "0s"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			before, _, err := parseProtectionPolicy([]byte(file))
			if err != nil {
				t.Fatal(err)
			}
			out, changes, notes, err := migrateProtectionPolicyFile([]byte(file), "v9.9.9")
			if err != nil || len(notes) != 0 {
				t.Fatalf("migration %v %v", err, notes)
			}
			after, _, err := parseProtectionPolicy(out)
			if err != nil {
				t.Fatal(err)
			}
			if name == "kept as written" {
				if len(changes) != 0 || after.Cash.ConfirmationWindow != "0s" {
					t.Fatalf("an owner's window was rewritten: %v", changes)
				}
				return
			}
			text := string(out)
			if after.Cash.ConfirmationWindow != "10m" || after.PolicyVersion != before.PolicyVersion+1 ||
				!strings.Contains(text, "Owner decision 2026-10-06 15:31\n# CEST: \"Cache the decision for 5 or 10 minutes, if not serious concerns\".\nconfirmation_window = \"10m\"  # written by Canary v9.9.9") {
				t.Fatalf("%s: %v\n%s", name, changes, text)
			}
			if name == "a new [cash]" && strings.Index(text, "[cash]\n") > strings.Index(text, "# Cash management, as the owner keeps it.") {
				t.Fatalf("the new [cash] is not above the cash tables and their comments:\n%s", text)
			}
			if again, changes, _, err := migrateProtectionPolicyFile(out, "v9.9.9"); err != nil || len(changes) != 0 || string(again) != text {
				t.Fatalf("a second ensure changed %v (%v)", changes, err)
			}
		})
	}
}
