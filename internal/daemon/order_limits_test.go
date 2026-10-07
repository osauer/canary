package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Synthetic numbers throughout; none is an account's.

func orderLimitsTestStockBuy(notional float64) (rpc.OrderDraft, rpc.OrderPositionImpact, orderNotionalAuthority) {
	draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
	return draft, rpc.OrderPositionImpact{Before: 0, After: 100, Effect: rpc.OrderPositionEffectOpen}, protectiveExitTestNotional(notional)
}

// The gate reads [order_limits] only. With no policy, or a key missing, every
// order is refused naming why; the retired config.toml gates and their
// compiled defaults never stand in.
func TestOrderLimitsFailClosedAndIgnoreRetiredConfig(t *testing.T) {
	now := time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	srv.cfg.Trading.MaxNotional, srv.cfg.Trading.AllowStockShort = new(1e9), new(true)
	srv.riskPolicies = nil
	draft, open, notional := orderLimitsTestStockBuy(5000)

	limits := srv.orderLimitsInForce("EUR")
	err := validateOrderRiskAuthority(limits, draft, open, notional, "EUR", protectiveExitInventory{}, deltaReductionEvidence{})
	if limits.Complete || err == nil || !strings.Contains(err.Error(), "no risk policy is loaded") {
		t.Fatalf("no policy: limits %+v err %v, want every order refused", limits, err)
	}

	table := testOrderLimitsTable(10000)
	table.MaxOrderFloorBase = nil
	installTestOrderLimits(srv, table)
	limits = srv.orderLimitsInForce("EUR")
	err = validateOrderRiskAuthority(limits, draft, open, notional, "EUR", protectiveExitInventory{}, deltaReductionEvidence{})
	if err == nil || !strings.Contains(err.Error(), "does not write order_limits.max_order_floor_base") {
		t.Fatalf("missing floor: err %v, want a refusal naming the key", err)
	}
	blocker := orderRiskLimitBlocker(limits, err)
	if blocker.Code != "order_risk_limit" || !strings.Contains(blocker.Message, "order_limits.max_order_floor_base") || blocker.Action == "" {
		t.Fatalf("blocker = %+v, want order_risk_limit naming the key", blocker)
	}

	installTestOrderLimits(srv, testOrderLimitsTable(10000))
	limits = srv.orderLimitsInForce("EUR")
	draft, open, notional = orderLimitsTestStockBuy(20000)
	if err := validateOrderRiskAuthority(limits, draft, open, notional, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil ||
		!strings.Contains(err.Error(), "order notional 20,000 EUR exceeds the order cap in force 10,000 EUR (the floor") {
		t.Fatalf("retired max_notional = 1e9 must not lift the cap: %v", err)
	}
	short := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 10, Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "EUR"}}
	if err := validateOrderRiskAuthority(limits, short, rpc.OrderPositionImpact{Before: 0, After: -10, Effect: rpc.OrderPositionEffectOpenShort}, protectiveExitTestNotional(800), "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil ||
		!strings.Contains(err.Error(), "[order_limits].allow_stock_short") {
		t.Fatalf("retired allow_stock_short = true must not permit a short: %v", err)
	}
}

func orderLimitsTestAccount(account string, nlv float64, base string, asOf time.Time) *rpc.AccountResult {
	return &rpc.AccountResult{AccountID: account, BaseCurrency: base, NetLiquidation: nlv, AsOf: asOf,
		Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Scope:  rpc.AccountDataScope{AccountID: account, AccountMode: "paper"},
			Fields: &rpc.AccountFieldAvailability{NetLiquidation: true, BaseCurrency: true}}}
}

// The cap scales with a current account reading and falls back to the floor,
// saying so, when the reading is old, missing or in another currency.
func TestOrderLimitsInForceScalesWithTheAccountReading(t *testing.T) {
	now := time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	if l := srv.orderLimitsInForce("EUR"); l.CapBase != 10000 || l.CapBound != risk.OrderCapBoundFloor || !strings.Contains(l.Summary, "no current account reading") {
		t.Fatalf("before any reading = %+v, want the floor flagged", l)
	}
	account := srv.currentBrokerStateScope().Account
	srv.recordOrderLimitsNLV(orderLimitsTestAccount(account, 240000, "EUR", now))
	if l := srv.orderLimitsInForce("EUR"); l.CapBase != 12000 || l.CapBound != risk.OrderCapBoundPctNLV {
		t.Fatalf("NLV 240,000 = %+v, want 12,000 bound by 5%% of NLV", l)
	}
	if l := srv.orderLimitsInForce("USD"); l.CapBase != 10000 || !strings.Contains(l.NLVUnavailable, "not the base currency USD") {
		t.Fatalf("another base currency = %+v, want the floor", l)
	}
	srv.recordOrderLimitsNLV(orderLimitsTestAccount("DU9999999", 2500000, "EUR", now))
	if l := srv.orderLimitsInForce("EUR"); l.CapBase != 12000 {
		t.Fatalf("another account's reading changed the cap: %+v", l)
	}
	stale := now.Add(orderLimitsNLVMaxAge + time.Minute)
	srv.now = func() time.Time { return stale }
	if l := srv.orderLimitsInForce("EUR"); l.CapBase != 10000 || l.CapBound != risk.OrderCapBoundFloor || !strings.Contains(l.Summary, "minutes ago") {
		t.Fatalf("stale reading = %+v, want the floor flagged", l)
	}
	srv.now = func() time.Time { return now }
	srv.recordOrderLimitsNLV(orderLimitsTestAccount(account, 2500000, "EUR", now.Add(time.Second)))
	if l := srv.orderLimitsInForce("EUR"); l.CapBase != 100000 || l.CapBound != risk.OrderCapBoundCeiling {
		t.Fatalf("NLV 2.5M = %+v, want the 100,000 ceiling", l)
	}
	// Amounts stated in another currency than the account's refuse orders.
	installTestOrderLimits(srv, testOrderLimitsTable(10000))
	snap := srv.riskPolicies.snapshot()
	snap.policy.Capital.BaseCurrency = "USD"
	if l := srv.orderLimitsInForce("EUR"); l.Complete || !strings.Contains(l.Summary, "states its amounts in USD") {
		t.Fatalf("currency mismatch = %+v, want incomplete", l)
	}
}

// The runtime override of max_notional is replaced by the one-shot policy
// override of the floor: it lifts the cap to the ceiling until it expires,
// and no other order limit takes an override.
func TestOrderLimitsFloorOverride(t *testing.T) {
	now := time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	srv.riskCapital = newTestRiskCapitalStore(t)
	clock := now
	srv.riskCapital.now = func() time.Time { return clock }
	srv.now = func() time.Time { return clock }
	installTestOrderLimits(srv, testOrderLimitsTable(10000))
	policy := srv.riskPolicies.snapshot().policy
	policy.Override.MaxDurationHours = new(24)

	if _, err := srv.riskCapital.GrantOverride(rpc.OverrideParams{Control: "order_limits.allow_stock_short", Reason: "test", Hours: 2}, policy); err == nil ||
		!strings.Contains(err.Error(), "only order_limits.max_order_floor_base takes a one-shot override") {
		t.Fatalf("an override of allow_stock_short: %v, want refused", err)
	}
	rec, err := srv.riskCapital.GrantOverride(rpc.OverrideParams{Control: risk.OrderLimitFloorOverrideControl, Reason: "one larger trim", Hours: 2}, policy)
	if err != nil {
		t.Fatal(err)
	}
	l := srv.orderLimitsInForce("EUR")
	if l.CapBase != 100000 || l.CapBound != risk.OrderCapBoundOverride || l.OverrideID != rec.ID {
		t.Fatalf("with the override = %+v, want the ceiling", l)
	}
	draft, open, notional := orderLimitsTestStockBuy(60000)
	if err := validateOrderRiskAuthority(l, draft, open, notional, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err != nil {
		t.Fatalf("an order within the lifted cap: %v", err)
	}
	draft, open, notional = orderLimitsTestStockBuy(100001)
	if err := validateOrderRiskAuthority(l, draft, open, notional, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil {
		t.Fatal("the override lifted the cap beyond the ceiling")
	}
	clock = rec.ExpiresAt.Add(time.Second)
	if l := srv.orderLimitsInForce("EUR"); l.CapBase != 10000 || l.CapBound != risk.OrderCapBoundFloor {
		t.Fatalf("after expiry = %+v, want the floor again", l)
	}
}

// The runtime limit settings are retired and name their replacement.
func TestRetiredRuntimeLimitSettingsAreRefused(t *testing.T) {
	for _, key := range []string{"max_notional", "max_option_contracts", "allow_stock_short", "allow_option_sell_to_open"} {
		raw, _ := json.Marshal(map[string]any{"limits": map[string]any{key: 1}})
		_, err := flattenSettingsPatch(map[string]json.RawMessage{"trading": raw})
		if err == nil || !strings.Contains(err.Error(), "trading.limits."+key+" is retired") || !strings.Contains(err.Error(), "--control order_limits.max_order_floor_base") {
			t.Errorf("%s: err %v, want the retirement naming the policy override", key, err)
		}
	}
	for _, spec := range rpc.SettingsKeys() {
		if strings.HasPrefix(spec.Key, "trading.limits.") {
			t.Errorf("registry still advertises %s", spec.Key)
		}
	}
	settings := tradingLimitSettingsFrom(testOrderLimits(10000))
	if settings.MaxNotional.Value != 10000 || settings.MaxNotional.Access != rpc.SettingsAccessRead || settings.MaxNotional.Source != rpc.SettingsSourcePolicy ||
		!strings.Contains(settings.MaxNotional.Reason, "[order_limits]") {
		t.Fatalf("settings view = %+v, want the cap in force, read-only from the policy", settings.MaxNotional)
	}
}

const orderLimitsMigrationConstitution = `kind = "canary.risk_policy"
schema_version = 2
policy_id = "risk-constitution"
policy_version = 5

[capital]
base_currency = "EUR"
protected_floor = 1000.0

[override]
max_duration_hours = 24
`

func decodeTestConstitution(t *testing.T, data []byte) risk.Constitution {
	t.Helper()
	if err := parseConstitutionPolicy(data); err != nil {
		t.Fatalf("migrated constitution does not load: %v\n%s", err, data)
	}
	var c risk.Constitution
	if _, err := toml.Decode(string(data), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// policy ensure writes [order_limits] from config.toml [trading] as written,
// the compiled value where a key is absent, plus the scaled-cap keys, and
// raises policy_version; a written key is never changed.
func TestConstitutionMigrationWritesOrderLimitsFromConfig(t *testing.T) {
	explicit := config.Trading{MaxNotional: new(12000.0), MaxOptionContracts: new(3), AllowStockShort: new(true)}
	out, changes, _, err := migrateConstitutionPolicyFileFrom(explicit, true)([]byte(orderLimitsMigrationConstitution), "test")
	if err != nil {
		t.Fatal(err)
	}
	c := decodeTestConstitution(t, out)
	o := c.OrderLimits
	if c.PolicyVersion != 6 || o == nil || *o.MaxOrderFloorBase != 12000 || *o.MaxOrderPctNLV != 5 || *o.MaxOrderCeilingBase != 100000 ||
		*o.MaxOptionContracts != 3 || !*o.AllowStockShort || *o.AllowOptionSellToOpen {
		t.Fatalf("explicit source migrated to v%d %+v", c.PolicyVersion, o)
	}
	joined := strings.Join(changes, "\n")
	for _, want := range []string{"max_order_floor_base = 12000.0 (config.toml [trading].max_notional)", "raise policy_version 5 -> 6",
		"allow_option_sell_to_open = false (Canary's compiled default; config.toml [trading] does not set allow_option_sell_to_open)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("changes lack %q:\n%s", want, joined)
		}
	}

	out, changes, _, err = migrateConstitutionPolicyFileFrom(config.Trading{}, true)([]byte(orderLimitsMigrationConstitution), "test")
	if err != nil {
		t.Fatal(err)
	}
	o = decodeTestConstitution(t, out).OrderLimits
	if *o.MaxOrderFloorBase != 10000 || *o.MaxOptionContracts != 5 || *o.AllowStockShort || *o.AllowOptionSellToOpen ||
		!strings.Contains(strings.Join(changes, "\n"), "max_order_floor_base = 10000.0 (Canary's compiled default; config.toml [trading] does not set max_notional)") {
		t.Fatalf("compiled-default source migrated to %+v, changes %v", o, changes)
	}

	partial := orderLimitsMigrationConstitution + "\n[order_limits]\nmax_order_floor_base = 20000.0\n"
	out, changes, _, err = migrateConstitutionPolicyFileFrom(explicit, true)([]byte(partial), "test")
	if err != nil {
		t.Fatal(err)
	}
	if o := decodeTestConstitution(t, out).OrderLimits; *o.MaxOrderFloorBase != 20000 || len(o.MissingKeys()) != 0 || strings.Contains(strings.Join(changes, "\n"), "max_order_floor_base") {
		t.Fatalf("a written floor changed or a key stayed missing: %+v %v", o, changes)
	}

	complete := orderLimitsMigrationConstitution + testOrderLimitsTOML
	if out, changes, _, err := migrateConstitutionPolicyFileFrom(explicit, true)([]byte(complete), "test"); err != nil || len(changes) != 0 || string(out) != complete {
		t.Fatalf("a complete table migrated again: %v %v", changes, err)
	}
}

// End to end: the dry run proposes the migration, applying the reviewed plan
// backs the file up and the manager adopts the higher version.
func TestEnsureMigratesOrderLimitsWithBackupAndVersionBump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "risk-policy.toml")
	if err := os.WriteFile(path, []byte(orderLimitsMigrationConstitution), 0o600); err != nil {
		t.Fatal(err)
	}
	set := PolicyFileSet{Constitution: path, OrderGateSource: config.Trading{MaxNotional: new(10000.0)}, OrderGateSourceRead: true}
	before := newRiskPolicyManager(path, time.Second, nil)
	before.reload()
	if l := risk.EvaluateOrderLimits(before.snapshot().policy.OrderLimits, "EUR", risk.OrderLimitsNLV{}, nil, ""); l.Complete {
		t.Fatal("fixture already has order limits")
	}
	dry := EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true})
	if len(dry) != 1 || dry[0].Action != PolicyFileWouldMigrate || !strings.Contains(dry[0].Diff, "+[order_limits]") {
		t.Fatalf("dry run = %+v", dry)
	}
	if data, _ := os.ReadFile(path); string(data) != orderLimitsMigrationConstitution {
		t.Fatal("the dry run wrote the file")
	}
	applied := applyReviewedTestConversions(t, set, EnsureOptions{Release: "test", Now: time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)})
	if len(applied) != 1 || applied[0].Action != PolicyFileMigrated || applied[0].Backup == "" {
		t.Fatalf("apply = %+v", applied)
	}
	if backup, err := os.ReadFile(applied[0].Backup); err != nil || string(backup) != orderLimitsMigrationConstitution {
		t.Fatalf("backup = %q %v", backup, err)
	}
	before.reload()
	snap := before.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive || snap.policy.PolicyVersion != 6 || len(snap.policy.OrderLimits.MissingKeys()) != 0 {
		t.Fatalf("after migration status %s version %d limits %+v", snap.status, snap.policy.PolicyVersion, snap.policy.OrderLimits)
	}
	if again := EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}); again[0].Action != PolicyFileUnchanged {
		t.Fatalf("second dry run = %+v", again)
	}
}

// Daemon start applies each file's migration itself, after a backup: an
// upgraded install gains [order_limits] and an enabled sweep's new keys
// without anyone running a plan, and the second start changes nothing
// (owner instruction 2026-10-06 06:33 CEST).
func TestDaemonStartWritesTheDefaultsAnUpgradeLacks(t *testing.T) {
	dir := t.TempDir()
	constitution := filepath.Join(dir, "risk-policy.toml")
	protection := filepath.Join(dir, "protection-policy.toml")
	sweep := string(ProtectionPolicyTemplate("test")) + "\n[buckets.cash_sweep]\nenabled = true\nmode = \"shadow\"\nmax_order_notional = 20000.0\nmin_order_notional = 10000.0\n"
	for path, data := range map[string]string{constitution: orderLimitsMigrationConstitution, protection: sweep} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set := PolicyFileSet{Constitution: constitution, Protection: protection, OrderGateSource: config.Trading{MaxNotional: new(10000.0)}, OrderGateSourceRead: true}
	start := EnsureOptions{Release: "test", Now: time.Date(2026, 10, 6, 6, 0, 0, 0, time.UTC), ApplyMigrations: true}

	if dry := EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true, ApplyMigrations: true}); len(dry) != 2 || dry[0].Action != PolicyFileWouldMigrate || dry[1].Action != PolicyFileWouldMigrate {
		t.Fatalf("a dry run must write nothing: %+v", dry)
	}
	for _, a := range EnsurePolicyFiles(set, start) {
		if a.Action != PolicyFileMigrated || a.Backup == "" {
			t.Fatalf("start = %+v", a)
		}
	}
	m := newRiskPolicyManager(constitution, time.Second, nil)
	m.reload()
	if snap := m.snapshot(); snap.status != rpc.RiskPolicyStatusActive || len(snap.policy.OrderLimits.MissingKeys()) != 0 {
		t.Fatalf("after start: status %s, order limits %+v", snap.status, snap.policy.OrderLimits)
	}
	if data, _ := os.ReadFile(protection); !strings.Contains(string(data), "no_buy_while_borrowed = true") {
		t.Fatal("the enabled sweep did not gain its new keys")
	}
	for _, a := range EnsurePolicyFiles(set, start) {
		if a.Action != PolicyFileUnchanged {
			t.Fatalf("second start = %+v", a)
		}
	}
}

// A missing constitution is written with the order limits filled in from
// config.toml, so a fresh install trades within today's gates.
func TestConstitutionTemplateCarriesOrderLimits(t *testing.T) {
	c := decodeTestConstitution(t, constitutionPolicyTemplateFrom("test", config.Trading{MaxNotional: new(7000.0)}, true))
	if o := c.OrderLimits; o == nil || len(o.MissingKeys()) != 0 || *o.MaxOrderFloorBase != 7000 || *o.MaxOrderPctNLV != 5 || *o.MaxOrderCeilingBase != 100000 {
		t.Fatalf("template order limits = %+v", c.OrderLimits)
	}
	if len(c.UnapprovedKeys()) == 0 {
		t.Fatal("the template must keep the capital numbers as placeholders")
	}
}

// A preview under incomplete order limits is refused with the typed
// order_risk_limit blocker naming the missing key, even for a small order.
func TestOrderPreviewRefusesWithTheMissingOrderLimitNamed(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	table := testOrderLimitsTable(10000)
	table.MaxOrderPctNLV = nil
	installTestOrderLimits(srv, table)
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	limit := 100.0
	_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "buy", Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK"}, Quantity: 1, LimitPrice: &limit})
	blockers := previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != previewRiskLimitCode || !strings.Contains(blockers[0].Message, "order_limits.max_order_pct_nlv") {
		t.Fatalf("preview err %v blockers %+v, want order_risk_limit naming the key", err, blockers)
	}
}
