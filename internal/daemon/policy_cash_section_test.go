package daemon

import (
	"slices"
	"strings"
	"testing"
)

// Cash management moved out of the protection buckets into its own [cash]
// section with its own authority (owner decision 2026-10-06 06:08 CEST, "Now,
// on this branch"). A file written before the move still reads the same; policy
// ensure moves it once and changes no setting.

// legacyCashFile is a synthetic owner file in the old layout: the sweep under
// [buckets.cash_sweep] with a currency table and comments, its
// pre-authorisation in [authority].
const legacyCashFile = pcProtectionHead + `pre_authorised = ["trailing_stop", "cash_sweep"]
veto_window = "45m"

[buckets.trailing_stop]
enabled = true

[buckets.cash_sweep]  # my sweep
enabled = true
mode = "shadow"
max_order_notional = 12000.0
max_order_pct_nlv = 10.0
min_order_notional = 20000.0
reserve_floor_base = 10000.0
reserve_pct_nlv = 10.0
order_step_base = 1000.0
keep_cash = 5000.0  # float per currency
bills_exempt_from_trading_max_notional = true
no_buy_while_borrowed = true

[buckets.cash_sweep.currency.EUR]
fallback = "none"
keep_cash = 6000.0

[cash.leveling]
enabled = false
trigger_base = 10000.0
cushion_base = 250.0
max_slippage_bp = 2.0
payback_days = 30
`

func TestCashSectionReadsTheLegacyLayout(t *testing.T) {
	legacy, _, err := parseProtectionPolicy([]byte(legacyCashFile))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Cash.Sweep == nil || legacy.Cash.Sweep.MaxOrderNotional != 12000 || legacy.Buckets.LegacyCashSweep != nil ||
		legacy.Cash.Sweep.currency("EUR").Fallback != "none" {
		t.Fatalf("legacy sweep = %+v", legacy.Cash.Sweep)
	}
	if !slices.Equal(legacy.Authority.PreAuthorised, []string{"trailing_stop"}) || !slices.Equal(legacy.Cash.PreAuthorised, []string{"cash_sweep"}) ||
		!legacy.preAuthorised("cash_sweep") || !legacy.preAuthorised("trailing_stop") {
		t.Fatalf("legacy authority %v cash %v", legacy.Authority.PreAuthorised, legacy.Cash.PreAuthorised)
	}
	moved := strings.NewReplacer(`pre_authorised = ["trailing_stop", "cash_sweep"]`, `pre_authorised = ["trailing_stop"]`,
		"[buckets.cash_sweep", "[cash.sweep").Replace(legacyCashFile) + "\n[cash]\npre_authorised = [\"cash_sweep\"]\n"
	current, _, err := parseProtectionPolicy([]byte(moved))
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintProtectionPolicy(legacy) != fingerprintProtectionPolicy(current) {
		t.Fatal("the legacy layout reads as another policy than the [cash] layout")
	}
	both := strings.Replace(legacyCashFile, "[cash.leveling]", "[cash.sweep]\nenabled = false\n\n[cash.leveling]", 1)
	if _, _, err := parseProtectionPolicy([]byte(both)); err == nil || !strings.Contains(err.Error(), "written twice") {
		t.Fatalf("a sweep in both places = %v, want refused", err)
	}
}

func TestCashSectionEnsureMovesTheLegacyLayoutOnce(t *testing.T) {
	before, _, err := parseProtectionPolicy([]byte(legacyCashFile))
	if err != nil {
		t.Fatal(err)
	}
	out, changes, notes, err := migrateProtectionPolicyFile([]byte(legacyCashFile), "v9.9.9")
	if err != nil || len(notes) != 0 {
		t.Fatalf("migration: %v notes %v", err, notes)
	}
	text := string(out)
	for _, want := range []string{
		"[cash.sweep]  # my sweep", "[cash.sweep.currency.EUR]", "keep_cash = 5000.0  # float per currency",
		`pre_authorised = ["trailing_stop"]`, "[cash]", `pre_authorised = ["cash_sweep"]`, "moved from [authority] by Canary v9.9.9",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("migrated file lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[buckets.cash_sweep") {
		t.Fatalf("the legacy header survived:\n%s", text)
	}
	for _, want := range []string{"moved [buckets.cash_sweep] to [cash.sweep]: cash management has its own section",
		"moved cash_sweep from [authority] pre_authorised to [cash] pre_authorised: the cash sweep has its own authority"} {
		if !slices.Contains(changes, want) {
			t.Fatalf("changes %v lack %q", changes, want)
		}
	}
	after, _, err := parseProtectionPolicy(out)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintProtectionPolicy(before) != fingerprintProtectionPolicy(after) || after.PolicyVersion != before.PolicyVersion {
		t.Fatal("moving the cash tables changed the policy in force or its version")
	}
	again, changes, _, err := migrateProtectionPolicyFile(out, "v9.9.9")
	if err != nil || len(changes) != 0 || string(again) != text {
		t.Fatalf("a second ensure changed %v (%v)", changes, err)
	}
	// Defined keys at the old places count as written at the new ones.
	defined := definedTOMLKeys([]byte(legacyCashFile))
	for _, key := range []string{"cash", "cash.sweep", "cash.sweep.max_order_notional", "cash.sweep.currency.EUR.fallback", "cash.pre_authorised"} {
		if !defined[key] {
			t.Fatalf("legacy file does not define %s for policy show", key)
		}
	}
}

// A sweep written as dotted keys is not a plain section: ensure leaves it
// where it is, with a note, and it still reads.
func TestCashSectionEnsureLeavesADottedLegacyTable(t *testing.T) {
	file := pcProtectionHead + "\n[buckets]\ncash_sweep.enabled = false\n"
	if p, _, err := parseProtectionPolicy([]byte(file)); err != nil || p.Cash.Sweep == nil {
		t.Fatalf("dotted legacy sweep = %+v, %v", p.Cash.Sweep, err)
	}
	_, _, notes, err := migrateProtectionPolicyFile([]byte(file), "v9.9.9")
	if err != nil || !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "not moved to [cash.sweep]") }) {
		t.Fatalf("notes %v err %v", notes, err)
	}
}
