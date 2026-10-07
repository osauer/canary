package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Risk presets over two files (owner decisions 2026-10-07 08:30 and 08:33
// CEST): the derivation, the receipts and the restore, the consequences, the
// device verification and the two-file write. Every number is synthetic.

// cashPolicyTestConstitution is a constitution at Balanced's order limits.
const cashPolicyTestConstitution = `kind = "canary.risk_policy"
schema_version = 2
policy_id = "constitution-test"
policy_version = 7

[capital]
base_currency = "EUR"

[order_limits]
max_order_floor_base = 10000.0
max_order_pct_nlv = 10.0  # my share
max_order_ceiling_base = 100000.0
max_option_contracts = 10
allow_stock_short = false
allow_option_sell_to_open = false
max_bond_maturity_years = 30
`

// testDevice is a synthetic owner credential: a P-256 key pair, its id as
// Desk derives it and the [desk_device] line the owner would pin.
type testDevice struct {
	class string
	key   *ecdsa.PrivateKey
	id    string
	point []byte
}

func newTestDevice(t *testing.T, class string) testDevice {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	d := testDevice{class: class, key: key, point: point}
	switch class {
	case "companion":
		// As Desk derives it (execution_companion.go companionKeyID).
		sum := sha256.Sum256(point)
		d.id = base64.RawURLEncoding.EncodeToString(sum[:16])
	default:
		d.id = base64.RawURLEncoding.EncodeToString([]byte("passkey-credential-" + hex.EncodeToString(point[1:5])))
	}
	return d
}

func (d testDevice) line() string {
	return d.class + ` = "` + d.id + ":" + base64.RawURLEncoding.EncodeToString(d.point) + `"`
}

func (d testDevice) credential() string { return d.class + ":" + d.id }

// pinned appends [desk_device] with the devices to a constitution.
func pinned(constitution string, devices ...testDevice) string {
	var b strings.Builder
	b.WriteString(constitution + "\n[desk_device]\n")
	for _, d := range devices {
		b.WriteString(d.line() + "\n")
	}
	return b.String()
}

// review is Desk's review of a check, the bytes the owner's device signs.
func cashPolicyTestReview(check *rpc.CashPolicyCheckResult) string {
	raw, _ := json.Marshal(map[string]any{"kind": "cash_policy", "changes": check.Changes, "consequences": check.Consequences, "findings": check.Findings,
		"policy_version": check.PolicyVersion, "saved_version": check.SavedVersion, "base_currency": check.BaseCurrency})
	return string(raw)
}

// confirm signs a check's terms the way Desk's device does and returns the
// confirmation Desk sends with the save. review may differ from the terms'
// own review to test a broken chain.
func (d testDevice) confirm(t *testing.T, actionID, terms, review string) *rpc.CashPolicyConfirmation {
	t.Helper()
	digest := deskPolicyDigest(actionID, terms, review)
	fields := map[string]string{"credential": d.class, "review_json": review}
	switch d.class {
	case "companion":
		nonce := make([]byte, 32)
		rand.Read(nonce)
		challenge := base64.RawURLEncoding.EncodeToString(nonce)
		hash := sha256.Sum256([]byte(deskCompanionPolicyPrefix + "\n" + actionID + "\n" + digest + "\n" + challenge))
		r, s, err := ecdsa.Sign(rand.Reader, d.key, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		fields["key_id"], fields["challenge"], fields["signature"] = d.id, challenge, base64.RawURLEncoding.EncodeToString(sig)
	default:
		rp := sha256.Sum256([]byte(deskPasskeyRelyingParty))
		auth := append(append([]byte{}, rp[:]...), 0x05, 0, 0, 0, 7)
		nonce := make([]byte, 32)
		rand.Read(nonce)
		binding := sha256.Sum256(append([]byte(deskPasskeyPolicyPrefix+"\n"+actionID+"\n"+digest+"\n"), nonce...))
		client, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(append(nonce, binding[:]...)), "origin": "http://localhost:8791"})
		clientHash := sha256.Sum256(client)
		signed := sha256.Sum256(append(append([]byte{}, auth...), clientHash[:]...))
		sig, err := ecdsa.SignASN1(rand.Reader, d.key, signed[:])
		if err != nil {
			t.Fatal(err)
		}
		fields["credential_id"], fields["authenticator_data"], fields["client_data_json"], fields["signature"] = d.id,
			base64.RawURLEncoding.EncodeToString(auth), base64.RawURLEncoding.EncodeToString(client), base64.RawURLEncoding.EncodeToString(sig)
	}
	raw, _ := json.Marshal(fields)
	return &rpc.CashPolicyConfirmation{DeskActionID: actionID, Credential: d.credential(), Envelope: string(raw)}
}

// cashPolicyServerWithConstitution is cashPolicyServer with a risk policy
// manager over a temporary constitution.
func cashPolicyServerWithConstitution(t *testing.T, file, constitution string) (*Server, string, string, *corestore.Store) {
	t.Helper()
	s, path, core := cashPolicyServer(t, file)
	conPath := filepath.Join(filepath.Dir(path), "risk-policy.toml")
	if err := os.WriteFile(conPath, []byte(constitution), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newRiskPolicyManager(conPath, 30*time.Second, func() time.Time { return cashPolicyTestNow })
	m.reload()
	if snap := m.snapshot(); snap.status != rpc.RiskPolicyStatusActive {
		t.Fatalf("constitution did not load: %s %s", snap.status, snap.message)
	}
	s.riskPolicies = m
	return s, path, conPath, core
}

// cashPolicySaveAs checks and applies changes with a device's signature.
func cashPolicySaveAs(t *testing.T, s *Server, d testDevice, changes map[string]any, id string) (*rpc.CashPolicyApplyResult, error) {
	t.Helper()
	snap := cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, changes)
	if check.Conflict || len(check.Errors) > 0 || check.Digest == "" {
		t.Fatalf("check %+v", check)
	}
	return cashPolicyApply(s, check.Terms, check.Digest, id, d.confirm(t, "action-"+id, check.Terms, cashPolicyTestReview(check)), "")
}

func presetChanges(id string) map[string]any {
	p, ok := cashPolicyPresetByID(id)
	if !ok {
		panic(id)
	}
	out := map[string]any{}
	maps.Copy(out, p.values)
	return out
}

// Acceptance 1: Balanced is Canary's defaults by construction, and the table
// carries the owner's numbers of 2026-10-07 08:30 CEST. The ceiling is
// covered by no preset.
func TestCashPolicyBalancedIsCanarysDefaultsAndTheTableIsTheOwners(t *testing.T) {
	tables := cashPolicyPresetTables()
	if len(tables) != 1 || tables[0].revision != "2026-10-07" {
		t.Fatalf("tables %+v", tables)
	}
	balanced, _ := cashPolicyPresetByID(rpc.CashPolicyPresetBalanced)
	for key, want := range map[string]any{
		"cash.sweep.reserve_floor_base": cashPolicyWritten["cash.sweep.reserve_floor_base"], "cash.sweep.reserve_pct_nlv": cashPolicyWritten["cash.sweep.reserve_pct_nlv"],
		"order_limits.max_order_floor_base": orderLimitsWriteFloorBase, "order_limits.max_order_pct_nlv": orderLimitsWritePctNLV, "order_limits.max_option_contracts": orderLimitsWriteOptionQty,
	} {
		if !cashPolicySame(balanced.values[key], want) {
			t.Fatalf("Balanced %s = %v, Canary writes %v", key, balanced.values[key], want)
		}
	}
	want := map[string][5]float64{ // reserve floor, reserve pct, cap floor, cap pct, contracts
		rpc.CashPolicyPresetCautious:   {15000, 15, 5000, 5, 5},
		rpc.CashPolicyPresetBalanced:   {10000, 10, 10000, 10, 10},
		rpc.CashPolicyPresetAggressive: {10000, 5, 15000, 20, 100},
	}
	for id, w := range want {
		p, _ := cashPolicyPresetByID(id)
		for i, key := range cashPolicyCoveredKeys {
			if got, _ := cashPolicyNumber(p.values[key]); got != w[i] {
				t.Fatalf("%s %s = %v, want %v", id, key, got, w[i])
			}
		}
		if _, covered := p.values["order_limits.max_order_ceiling_base"]; covered || len(p.values) != len(cashPolicyCoveredKeys) {
			t.Fatalf("%s covers %v", id, p.values)
		}
	}
	// The written default rows show Balanced as Canary's value.
	s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
	snap := cashPolicyGet(t, s)
	for _, key := range []string{"order_limits.max_order_floor_base", "order_limits.max_order_pct_nlv", "order_limits.max_option_contracts"} {
		row, ok := cashPolicySetting(snap, key)
		if !ok || row.Section != rpc.CashPolicySectionOrderLimits || !cashPolicySame(row.Default, balanced.values[key]) || row.Source != rpc.CashPolicySourceFile || !row.Reset || row.Help == "" {
			t.Fatalf("%s row %+v", key, row)
		}
	}
	if snap.Constitution == nil || snap.Constitution.FileState != rpc.CashPolicyFileOK || snap.Constitution.PolicyVersion != 7 || !snap.Constitution.Writable {
		t.Fatalf("constitution %+v", snap.Constitution)
	}
	if !strings.Contains(snap.Revision, "+sha256:") {
		t.Fatalf("revision names one file: %s", snap.Revision)
	}
}

// Acceptance 2 and 6: every spec's `more` transition yields a consequence
// sentence and refuses a reliance; Balanced → Cautious has none, Balanced →
// Aggressive has one per quantity (three), and a Custom → preset with one
// looser key has at least one.
func TestCashPolicyEveryMoreTransitionHasASentenceAndRefusesReliance(t *testing.T) {
	// Fixture changes so a transition can be "more": leveling carries USD on
	// purpose, the sweep is off, bills do not pass the cap.
	file := strings.Replace(cashPolicyTestFile, "[cash.sweep]  # my sweep\nenabled = true", "[cash.sweep]  # my sweep\nenabled = false", 1)
	file = strings.Replace(file, "bills_exempt_from_trading_max_notional = true", "bills_exempt_from_trading_max_notional = false", 1)
	file += "\n[cash.leveling.currency.USD]\ndeliberate_carry = true\n"
	more := map[string]any{
		"cash.leveling.enabled": true, "cash.leveling.trigger_base": 5000, "cash.leveling.cushion_base": 500, "cash.leveling.max_slippage_bp": 5, "cash.leveling.payback_days": 60,
		"cash.leveling.currency.USD.deliberate_carry": false,
		"cash.sweep.enabled":                          true, "cash.sweep.mode": "active", "cash.sweep.reserve_floor_base": 5000, "cash.sweep.reserve_pct_nlv": 5, "cash.sweep.min_order_notional": 10000,
		"cash.sweep.max_order_notional": 80000, "cash.sweep.max_order_pct_nlv": 20, "cash.sweep.no_buy_while_borrowed": false, "cash.sweep.bills_exempt_from_trading_max_notional": true,
		"cash.sweep.keep_cash": 1000, "cash.sweep.currency.GBP.keep_cash": 1000,
		"order_limits.max_order_floor_base": 15000, "order_limits.max_order_pct_nlv": 20, "order_limits.max_option_contracts": 100,
	}
	for _, sp := range cashPolicySpecs {
		if sp.more == nil {
			continue
		}
		key := sp.key("")
		if sp.perCurrency {
			key = sp.key(map[string]string{rpc.CashPolicySectionLeveling: "USD", rpc.CashPolicySectionSweep: "GBP"}[sp.section])
		}
		if _, ok := more[key]; !ok {
			t.Fatalf("the table lacks a more-transition for %s", key)
		}
	}
	device := newTestDevice(t, "companion")
	for key, to := range more {
		t.Run(key, func(t *testing.T) {
			s, path, conPath, _ := cashPolicyServerWithConstitution(t, file, pinned(cashPolicyTestConstitution, device))
			before, conBefore := readFile(t, path), readFile(t, conPath)
			snap := cashPolicyGet(t, s)
			check := cashPolicyCheck(t, s, snap.Revision, map[string]any{key: to})
			if len(check.Errors) > 0 || len(check.Consequences) == 0 || !check.DeviceRequired {
				t.Fatalf("%s → %v: errors %v, consequences %v, device %v", key, to, check.Errors, check.Consequences, check.DeviceRequired)
			}
			relied := &rpc.CashPolicyConfirmation{DeskActionID: "later", Credential: device.credential(), Envelope: `{"credential":"relied"}`, ConfirmedBy: "desk-earlier"}
			if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-more", relied, ""); rpcCode(err) != rpc.CodeConfirmationRequired {
				t.Fatalf("reliance accepted for %s: %v", key, err)
			}
			if readFile(t, path) != before || readFile(t, conPath) != conBefore {
				t.Fatal("a refused reliance wrote a file")
			}
		})
	}

	s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
	snap := cashPolicyGet(t, s)
	if snap.Preset == nil || snap.Preset.ID != rpc.CashPolicyPresetBalanced {
		t.Fatalf("stance %+v", snap.Preset)
	}
	cautious := cashPolicyCheck(t, s, snap.Revision, presetChanges(rpc.CashPolicyPresetCautious))
	if len(cautious.Consequences) != 0 || cautious.PresetFrom != rpc.CashPolicyPresetBalanced || cautious.PresetTo != rpc.CashPolicyPresetCautious || !cautious.DeviceRequired {
		t.Fatalf("Balanced → Cautious: %+v", cautious)
	}
	aggressive := cashPolicyCheck(t, s, snap.Revision, presetChanges(rpc.CashPolicyPresetAggressive))
	want := []string{
		"At today's NLV the reserve kept as cash falls from 20,000 EUR to 10,000 EUR.",
		"At today's NLV the order cap on new orders rises from 20,000 EUR to 40,000 EUR.",
		"An opening option order may hold up to 100 contracts instead of 10 contracts; a delta-reducing exit is not held to it.",
	}
	if !slices.Equal(aggressive.Consequences, want) || aggressive.PresetTo != rpc.CashPolicyPresetAggressive || aggressive.ConstitutionSavedVersion != 8 {
		t.Fatalf("Balanced → Aggressive:\n got %q\nwant %q (%+v)", aggressive.Consequences, want, aggressive)
	}
	// Custom with one key looser than Cautious: Cautious tightens four keys
	// and loosens the fifth, so the review needs the device for that one.
	mixed := strings.Replace(cashPolicyTestConstitution, "max_option_contracts = 10", "max_option_contracts = 3", 1)
	s, _, _, _ = cashPolicyServerWithConstitution(t, cashPolicyTestFile, mixed)
	snap = cashPolicyGet(t, s)
	check := cashPolicyCheck(t, s, snap.Revision, presetChanges(rpc.CashPolicyPresetCautious))
	if snap.Preset.ID != rpc.CashPolicyPresetCustom || len(check.Consequences) != 1 || !strings.Contains(check.Consequences[0], "up to 5 contracts instead of 3") {
		t.Fatalf("Custom → Cautious: stance %s, consequences %q", snap.Preset.ID, check.Consequences)
	}
}

// Acceptance 3: every preset runs every check rule on the template-like
// files and a synthetic book with positions at 150,000, 240,000 and
// 2,500,000. The cap and the reserve sit inside the 2–50% bands; an option
// line worth 13,000 a contract is unexitable only where the cap in force is
// smaller and the exit's delta cannot be measured.
func TestCashPolicyPresetsPassTheCheckRules(t *testing.T) {
	presets := cashPolicyPresetTables()[0].presets
	for _, nlv := range []float64{150000, 240000, 2500000} {
		for _, p := range presets {
			num := func(key string) string { f, _ := cashPolicyNumber(p.values[key]); return tomlFloat(f) }
			protection := pcProtectionHead + `
[buckets.risk_reduction]
enabled = true
max_order_notional = 10000.0

[cash.sweep]
enabled = true
mode = "active"
reserve_floor_base = ` + num("cash.sweep.reserve_floor_base") + `
reserve_pct_nlv = ` + num("cash.sweep.reserve_pct_nlv") + `
min_order_notional = 20000.0
max_order_notional = 50000.0
max_order_pct_nlv = 10.0
order_step_base = 1000.0
keep_cash = 5000.0
bills_exempt_from_trading_max_notional = true
no_buy_while_borrowed = true

[cash.sweep.currency.EUR]
instruments = ["de_bubill"]
fallback = "none"
isins = ["` + synthDEBill + `"]
`
			contracts, _ := cashPolicyNumber(p.values["order_limits.max_option_contracts"])
			constitution := pcConstitutionHead + "\n[order_limits]\nmax_order_floor_base = " + num("order_limits.max_order_floor_base") + "\nmax_order_pct_nlv = " + num("order_limits.max_order_pct_nlv") +
				"\nmax_order_ceiling_base = 100000.0\nmax_option_contracts = " + tomlInt(int(contracts)) + "\nallow_stock_short = false\nallow_option_sell_to_open = false\nmax_bond_maturity_years = 30\n"
			capFloor, _ := cashPolicyNumber(p.values["order_limits.max_order_floor_base"])
			capPct, _ := cashPolicyNumber(p.values["order_limits.max_order_pct_nlv"])
			cap := min(100000, max(capFloor, capPct/100*nlv))
			for _, measured := range []bool{true, false} {
				book := &PolicyCheckBook{BaseCurrency: "EUR", NetLiquidation: nlv, AsOf: pcNow, Cash: map[string]float64{"EUR": nlv / 4}, FXToBase: map[string]float64{"EUR": 1, "USD": 0.9}, PositionsKnown: true,
					Positions: []PolicyCheckPosition{
						{Kind: policyCheckKindStock, Currency: "USD", Quantity: 100, MarketValueBase: 30000, ConID: 5001, Underlying: "SYNA", DollarDeltaBase: new(30000.0)},
						{Kind: policyCheckKindOption, Currency: "USD", Quantity: 2, MarketValueBase: 26000, ConID: 5101, Underlying: "SYNA", Right: "C", Expiry: "2027-01-15", Multiplier: 100},
					}}
				if measured {
					book.Positions[1].DollarDeltaBase = new(9000.0)
				}
				in := PolicyCheckInput{Now: pcNow, Files: pcWrite(t, pcFiles{rulebook: pcRulebookHead, protection: protection, constitution: constitution}), ConfigPath: "/x/config.toml",
					Trading: config.Trading{Mode: config.TradingModeLive}.WithDefaults(), Book: book, FileStatus: map[string]string{}}
				report := CheckPolicy(in)
				rules := pcRules(report)
				for _, f := range report.Findings {
					aboutCap := slices.ContainsFunc(f.Keys, func(k rpc.PolicyCheckKey) bool { return strings.Contains(k.Key, "[order_limits]") })
					switch f.Rule {
					case "cash_reserve_vs_nlv", "order_limits_missing", "file_refused":
						t.Fatalf("%s at NLV %.0f: %s: %s", p.id, nlv, f.Rule, f.Message)
					case "order_cap_vs_nlv":
						// The synthetic reduction bucket's own cap is tiny against
						// 2,500,000; the order cap itself must sit inside the band.
						if aboutCap {
							t.Fatalf("%s at NLV %.0f: %s: %s", p.id, nlv, f.Rule, f.Message)
						}
					}
				}
				wantLot := !measured && 13000 > cap
				if got := slices.Contains(rules, "lot_above_trading_max"); got != wantLot {
					t.Fatalf("%s at NLV %.0f (cap %.0f, measured %v): lot_above_trading_max = %v, want %v; rules %v", p.id, nlv, cap, measured, got, wantLot, rules)
				}
			}
		}
	}
}

func tomlInt(v int) string { return strings.TrimSuffix(tomlFloat(float64(v)), ".0") }

// Acceptance 4: the stance derives from the files' values.
func TestCashPolicyStanceDerivesFromTheFiles(t *testing.T) {
	stance := func(t *testing.T, file, constitution string) *rpc.CashPolicyPreset {
		t.Helper()
		s, _, _, _ := cashPolicyServerWithConstitution(t, file, constitution)
		return cashPolicyGet(t, s).Preset
	}
	if p := stance(t, cashPolicyTestFile, cashPolicyTestConstitution); p.ID != rpc.CashPolicyPresetBalanced || p.Label != "Balanced" || p.Revision != "2026-10-07" || p.Note != "" || p.Explainer != "Canary's defaults for these sizes." {
		t.Fatalf("defaults read %+v", p)
	}
	if p := stance(t, strings.Replace(cashPolicyTestFile, "reserve_pct_nlv = 10.0", "reserve_pct_nlv = 12.0", 1), cashPolicyTestConstitution); p.ID != rpc.CashPolicyPresetCustom || p.Label != "Custom" || p.Explainer != "Your own values." {
		t.Fatalf("one key changed reads %+v", p)
	}
	if p := stance(t, strings.Replace(cashPolicyTestFile, "reserve_pct_nlv = 10.0", "reserve_pct_nlv = 10", 1), cashPolicyTestConstitution); p.ID != rpc.CashPolicyPresetBalanced {
		t.Fatalf("10 and 10.0 read %+v", p)
	}
	cautious := strings.NewReplacer("reserve_floor_base = 10000.0", "reserve_floor_base = 15000.0", "reserve_pct_nlv = 10.0", "reserve_pct_nlv = 15.0").Replace(cashPolicyTestFile)
	cautiousCon := strings.NewReplacer("max_order_floor_base = 10000.0", "max_order_floor_base = 5000.0", "max_order_pct_nlv = 10.0", "max_order_pct_nlv = 5.0", "max_option_contracts = 10", "max_option_contracts = 5").Replace(cashPolicyTestConstitution)
	if p := stance(t, cautious, cautiousCon); p.ID != rpc.CashPolicyPresetCautious {
		t.Fatalf("Cautious values read %+v", p)
	}
	// No sweep table: the stance follows the caps and says so (a fresh start
	// reads Balanced).
	before, _, _ := strings.Cut(cashPolicyTestFile, "[cash.sweep]")
	end := strings.Index(cashPolicyTestFile, "# Currency leveling")
	noSweep := before + cashPolicyTestFile[end:]
	if p := stance(t, noSweep, cashPolicyTestConstitution); p.ID != rpc.CashPolicyPresetBalanced || p.Note != "Cash sweep not set up yet." {
		t.Fatalf("no sweep reads %+v", p)
	}
	// A drifted constitution compares from the policy in force, with a note.
	s, _, conPath, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
	if err := os.WriteFile(conPath, []byte(strings.Replace(cashPolicyTestConstitution, "max_order_pct_nlv = 10.0", "max_order_pct_nlv = 20.0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := cashPolicyGet(t, s)
	if snap.Constitution.FileState != rpc.CashPolicyFileDrift || snap.Constitution.Writable || snap.Preset.ID != rpc.CashPolicyPresetBalanced ||
		!strings.HasPrefix(snap.Preset.Note, "The order caps are compared from the policy in force (the file was edited without raising policy_version") {
		t.Fatalf("drifted constitution reads %+v / %+v", snap.Constitution, snap.Preset)
	}
	if check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"cash.sweep.keep_cash": 4000}); len(check.Errors) != 0 || check.PresetTo != rpc.CashPolicyPresetBalanced {
		t.Fatalf("a cash change beside a drifted constitution: %+v", check)
	}
	_, err := s.handleCashPolicyCheck(t.Context(), &rpc.Request{Params: mustJSON(rpc.CashPolicyCheckRequest{ExpectedRevision: snap.Revision, Changes: map[string]json.RawMessage{"order_limits.max_order_pct_nlv": json.RawMessage("5")}})})
	if rpcCode(err) != rpc.CodePolicyUnwritable {
		t.Fatalf("a cap change over a drifted constitution: %v", err)
	}
	// An earlier table keeps its name.
	earlier := cashPolicyPresetTable{revision: "2026-09-01", presets: []cashPolicyPresetDef{{id: rpc.CashPolicyPresetBalanced, label: "Balanced", explainer: "Canary's defaults for these sizes.",
		values: map[string]any{"cash.sweep.reserve_floor_base": 10000.0, "cash.sweep.reserve_pct_nlv": 10.0, "order_limits.max_order_floor_base": 10000.0, "order_limits.max_order_pct_nlv": 5.0, "order_limits.max_option_contracts": 5}}}}
	old := cashPolicyStance(append(cashPolicyPresetTables(), earlier), earlier.presets[0].values)
	if old.ID != rpc.CashPolicyPresetBalanced || old.Label != "Balanced (values of 2026-09-01)" || old.Revision != "2026-09-01" {
		t.Fatalf("earlier table reads %+v", old)
	}
	if p := cashPolicyStance(cashPolicyPresetTables(), earlier.presets[0].values); p.ID != rpc.CashPolicyPresetCustom {
		t.Fatalf("without the earlier table the old values read %+v", p)
	}
	// Presets and their facts at the synthetic NLV of 200,000.
	snap = cashPolicyGet(t, cashPolicyServerWithConstitutionOnly(t))
	facts := map[string][3]float64{}
	for _, p := range snap.Presets {
		facts[p.ID] = [3]float64{p.Facts.ReserveBase, p.Facts.OrderCapBase, float64(p.Facts.Contracts)}
	}
	want := map[string][3]float64{rpc.CashPolicyPresetCautious: {30000, 10000, 5}, rpc.CashPolicyPresetBalanced: {20000, 20000, 10}, rpc.CashPolicyPresetAggressive: {10000, 40000, 100}}
	for id, w := range want {
		if facts[id] != w {
			t.Fatalf("%s facts %v, want %v", id, facts[id], w)
		}
	}
	if snap.Presets[0].Facts.ReserveText != "30,000 EUR: 15% of NLV" || snap.Presets[0].Facts.OrderCapText != "10,000 EUR: 5% of NLV" || snap.Presets[2].Facts.ReserveText != "10,000 EUR: the amount, more than 5% of NLV" {
		t.Fatalf("facts text %+v", snap.Presets)
	}
}

func cashPolicyServerWithConstitutionOnly(t *testing.T) *Server {
	t.Helper()
	s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
	return s
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// Acceptance 5 and 7: a device-verified save writes the constitution first
// and the protection file second, each with provenance naming the preset;
// receipts carry the stance before and after; restore serves the owner's
// values after Custom → Cautious → Balanced and a hand edit withdraws it; a
// governance event records each constitution revision.
func TestCashPolicyTwoFileSaveReceiptsAndRestore(t *testing.T) {
	for _, class := range []string{"companion", "passkey"} {
		t.Run(class, func(t *testing.T) {
			device := newTestDevice(t, class)
			custom := strings.NewReplacer("reserve_floor_base = 10000.0", "reserve_floor_base = 12000.0", "reserve_pct_nlv = 10.0", "reserve_pct_nlv = 12.0").Replace(cashPolicyTestFile)
			customCon := strings.NewReplacer("max_order_pct_nlv = 10.0", "max_order_pct_nlv = 4.0", "max_option_contracts = 10", "max_option_contracts = 4").Replace(cashPolicyTestConstitution)
			s, path, conPath, core := cashPolicyServerWithConstitution(t, custom, pinned(customCon, device))
			snap := cashPolicyGet(t, s)
			if snap.Preset.ID != rpc.CashPolicyPresetCustom || snap.Restore != nil || !slices.Equal(snap.Device.Verifiable, []string{device.credential()}) || snap.Device.Message != "" {
				t.Fatalf("custom desk %+v restore %+v device %+v", snap.Preset, snap.Restore, snap.Device)
			}
			out, err := cashPolicySaveAs(t, s, device, presetChanges(rpc.CashPolicyPresetCautious), "desk-cautious")
			if err != nil {
				t.Fatal(err)
			}
			if out.Verified != device.credential() || out.SavedVersion != 15 || out.ConstitutionSavedVersion != 8 || !out.InForce || out.Partial != nil || out.Preset.ID != rpc.CashPolicyPresetCautious {
				t.Fatalf("Cautious save %+v", out)
			}
			con := readFile(t, conPath)
			// The owner's own comment on max_order_pct_nlv stays on its line; the
			// provenance goes above it, as editCashPolicyFile does.
			for _, want := range []string{"policy_version = 8  # raised in Desk 2026-10-06 14:05 CEST", "# max_order_pct_nlv set in Desk 2026-10-06 14:05 CEST from the Cautious preset, confirmed", "; was 4.0\nmax_order_pct_nlv = 5.0  # my share",
				"max_option_contracts = 5  # set in Desk", "max_order_floor_base = 5000.0  # set in Desk"} {
				if !strings.Contains(con, want) {
					t.Fatalf("constitution lacks %q:\n%s", want, con)
				}
			}
			prot := readFile(t, path)
			if !strings.Contains(prot, "reserve_pct_nlv = 15.0  # set in Desk 2026-10-06 14:05 CEST from the Cautious preset, confirmed") || !strings.Contains(prot, "policy_version = 15") {
				t.Fatalf("protection file:\n%s", prot)
			}
			if snap := s.riskPolicies.snapshot(); snap.policy == nil || snap.policy.PolicyVersion != 8 || *snap.policy.OrderLimits.MaxOptionContracts != 5 || snap.status != rpc.RiskPolicyStatusActive {
				t.Fatalf("the risk manager did not adopt the save: %+v", snap)
			}
			events, err := core.LoadEvents(t.Context(), corestore.EventQuery{ScopeKey: daemonStateScope, Type: coreEventRiskPolicy})
			if err != nil || len(events) == 0 {
				t.Fatalf("governance events %v %d", err, len(events))
			}
			var governance map[string]any
			_ = json.Unmarshal(events[len(events)-1].PayloadJSON, &governance)
			if governance["kind"] != orderLimitsRevisionEventKind || governance["request_id"] != "desk-cautious" || governance["verified"] != device.credential() ||
				governance["preset_before"] != rpc.CashPolicyPresetCustom || governance["preset_after"] != rpc.CashPolicyPresetCautious || governance["policy_version"] != 8.0 {
				t.Fatalf("governance event %v", governance)
			}
			// The restore offers the owner's values while the files read a preset.
			snap = cashPolicyGet(t, s)
			owner := map[string]any{"cash.sweep.reserve_floor_base": 12000.0, "cash.sweep.reserve_pct_nlv": 12.0, "order_limits.max_order_floor_base": 10000.0, "order_limits.max_order_pct_nlv": 4.0, "order_limits.max_option_contracts": 4}
			sameValues := func(got, want map[string]any) bool {
				if len(got) != len(want) {
					return false
				}
				for k, v := range want {
					if !cashPolicySame(got[k], v) {
						return false
					}
				}
				return true
			}
			if snap.Restore == nil || snap.Restore.Preset != rpc.CashPolicyPresetCautious || !snap.Restore.SavedAt.Equal(cashPolicyTestNow) || !sameValues(snap.Restore.Values, owner) {
				t.Fatalf("restore after Cautious %+v", snap.Restore)
			}
			if _, err := cashPolicySaveAs(t, s, device, presetChanges(rpc.CashPolicyPresetBalanced), "desk-balanced"); err != nil {
				t.Fatal(err)
			}
			snap = cashPolicyGet(t, s)
			if snap.Preset.ID != rpc.CashPolicyPresetBalanced || snap.Restore == nil || !sameValues(snap.Restore.Values, owner) {
				t.Fatalf("restore after Cautious → Balanced %+v %+v", snap.Preset, snap.Restore)
			}
			// The receipts carry the stance before and after.
			ev, found, err := core.GetEvent(t.Context(), daemonStateScope, cashPolicyEventKey("desk-balanced"))
			var receipt cashPolicyReceipt
			if err != nil || !found || json.Unmarshal(ev.PayloadJSON, &receipt) != nil || receipt.Version != 2 || receipt.PresetBefore != rpc.CashPolicyPresetCautious || receipt.PresetAfter != rpc.CashPolicyPresetBalanced ||
				receipt.Verified != device.credential() || receipt.Constitution == nil || receipt.Constitution.SavedVersion != 9 || receipt.Constitution.GovernanceEvent != "recorded" || !strings.Contains(receipt.WrittenRevision, "+sha256:") {
				t.Fatalf("receipt %+v (%v %v)", receipt, found, err)
			}
			// A hand edit withdraws the offer.
			data := readFile(t, path)
			if err := os.WriteFile(path, []byte(strings.Replace(data, "policy_version = 16", "policy_version = 17", 1)+"# edited by hand\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if snap := cashPolicyGet(t, s); snap.Restore != nil {
				t.Fatalf("restore survives a hand edit %+v", snap.Restore)
			}
			// A cash-only save with a signed envelope is verified too; one with
			// the audit-only envelope still saves, unverified.
			s.protectionPolicies.reload()
			out, err = cashPolicySaveAs(t, s, device, map[string]any{"cash.sweep.keep_cash": 4000}, "desk-cash-signed")
			if err != nil || out.Verified != device.credential() || out.ConstitutionSavedVersion != 0 {
				t.Fatalf("cash-only signed save %+v %v", out, err)
			}
			out = cashPolicySave(t, s, map[string]any{"cash.sweep.keep_cash": 4500}, "desk-cash-audit")
			if out.Verified != "" || out.SavedVersion != 19 {
				t.Fatalf("cash-only audit save %+v", out)
			}
		})
	}
}

// Acceptance 7: what the two-file write refuses and how it fails.
func TestCashPolicyOrderCapChangesNeedAVerifiedDevice(t *testing.T) {
	device, other := newTestDevice(t, "companion"), newTestDevice(t, "companion")
	changes := map[string]any{"order_limits.max_order_pct_nlv": 20}
	refused := func(t *testing.T, constitution string, confirm func(check *rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation, wantCode, wantText string) {
		t.Helper()
		s, path, conPath, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, constitution)
		before, conBefore := readFile(t, path), readFile(t, conPath)
		check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, changes)
		_, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-cap", confirm(check), "")
		if rpcCode(err) != wantCode || !strings.Contains(err.Error(), wantText) {
			t.Fatalf("got %v, want %s %q", err, wantCode, wantText)
		}
		if readFile(t, path) != before || readFile(t, conPath) != conBefore {
			t.Fatal("a refused save wrote a file")
		}
	}
	t.Run("no key pinned", func(t *testing.T) {
		refused(t, cashPolicyTestConstitution, func(c *rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			return device.confirm(t, "a1", c.Terms, cashPolicyTestReview(c))
		}, rpc.CodeConfirmationUnverifiable, "Add the key Desk shows for it to risk-policy.toml under [desk_device]")
		s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
		if snap := cashPolicyGet(t, s); len(snap.Device.Verifiable) != 0 || !strings.Contains(snap.Device.Message, "cannot verify your device yet") {
			t.Fatalf("device %+v", snap.Device)
		}
	})
	t.Run("audit-only envelope", func(t *testing.T) {
		refused(t, pinned(cashPolicyTestConstitution, device), func(*rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			c := cashPolicyConfirmation()
			c.Credential = device.credential()
			return c
		}, rpc.CodeConfirmationUnverifiable, "carries no signature over the save")
	})
	t.Run("another key", func(t *testing.T) {
		refused(t, pinned(cashPolicyTestConstitution, device), func(c *rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			return other.confirm(t, "a2", c.Terms, cashPolicyTestReview(c))
		}, rpc.CodeConfirmationUnverifiable, "pins no key for companion:"+other.id)
	})
	t.Run("signature by another key under the pinned id", func(t *testing.T) {
		refused(t, pinned(cashPolicyTestConstitution, device), func(c *rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			forged := other.confirm(t, "a3", c.Terms, cashPolicyTestReview(c))
			forged.Credential = device.credential()
			forged.Envelope = strings.Replace(forged.Envelope, other.id, device.id, 1)
			return forged
		}, rpc.CodeConfirmationUnverifiable, "does not verify against the key pinned for it")
	})
	t.Run("signed for other terms", func(t *testing.T) {
		refused(t, pinned(cashPolicyTestConstitution, device), func(c *rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			otherTerms := strings.Replace(c.Terms, "20", "30", 1)
			return device.confirm(t, "a4", otherTerms, cashPolicyTestReview(c))
		}, rpc.CodeConfirmationUnverifiable, "does not verify")
	})
	t.Run("review of other changes", func(t *testing.T) {
		refused(t, pinned(cashPolicyTestConstitution, device), func(c *rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			review := strings.Replace(cashPolicyTestReview(c), `"to":20`, `"to":30`, 1)
			return device.confirm(t, "a5", c.Terms, review)
		}, rpc.CodeConfirmationUnverifiable, "shows a value other than the one Canary would write")
	})
	t.Run("reliance", func(t *testing.T) {
		refused(t, pinned(cashPolicyTestConstitution, device), func(*rpc.CashPolicyCheckResult) *rpc.CashPolicyConfirmation {
			return &rpc.CashPolicyConfirmation{DeskActionID: "a6", Credential: device.credential(), Envelope: `{"credential":"relied"}`, ConfirmedBy: "desk-earlier"}
		}, rpc.CodeConfirmationRequired, "needs your device every time")
	})
	t.Run("tightening needs the device too", func(t *testing.T) {
		s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, pinned(cashPolicyTestConstitution, device))
		check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, map[string]any{"order_limits.max_order_pct_nlv": 5})
		if len(check.Consequences) != 0 || !check.DeviceRequired {
			t.Fatalf("tightening %+v", check)
		}
		c := cashPolicyConfirmation()
		c.Credential = device.credential()
		if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-tighten", c, ""); rpcCode(err) != rpc.CodeConfirmationUnverifiable {
			t.Fatalf("tightening without a verifiable device: %v", err)
		}
	})
	t.Run("a bad signature refuses a cash-only save too", func(t *testing.T) {
		s, path, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, pinned(cashPolicyTestConstitution, device))
		before := readFile(t, path)
		check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, map[string]any{"cash.sweep.keep_cash": 4000})
		forged := other.confirm(t, "a7", check.Terms, cashPolicyTestReview(check))
		forged.Credential = device.credential()
		forged.Envelope = strings.Replace(forged.Envelope, other.id, device.id, 1)
		if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-cash-forged", forged, ""); rpcCode(err) != rpc.CodeConfirmationUnverifiable || readFile(t, path) != before {
			t.Fatalf("forged cash-only save: %v", err)
		}
	})
}

// Acceptance 7: the failure cases of the two-file write. A failure after the
// constitution was written is a partial save reported as such, and the
// stance reads Custom by value; a constitution that moved between the check
// and the save is a conflict that writes nothing; a reread waits for a save.
func TestCashPolicyTwoFileWriteFailures(t *testing.T) {
	device := newTestDevice(t, "companion")
	t.Run("partial save", func(t *testing.T) {
		s, path, conPath, core := cashPolicyServerWithConstitution(t, cashPolicyTestFile, pinned(cashPolicyTestConstitution, device))
		before := readFile(t, path)
		s.cashPolicyWriteFault = func(string) error { return os.ErrPermission }
		out, err := cashPolicySaveAs(t, s, device, presetChanges(rpc.CashPolicyPresetCautious), "desk-partial")
		if err != nil {
			t.Fatal(err)
		}
		if out.Partial == nil || out.Partial.Saved != rpc.CashPolicySectionOrderLimits || out.Partial.NotSaved != "cash" || !strings.Contains(out.Partial.Reason, "ermission denied") ||
			out.InForce || out.ConstitutionSavedVersion != 8 || out.SavedVersion != 14 || out.Preset.ID != rpc.CashPolicyPresetCustom {
			t.Fatalf("partial save %+v", out)
		}
		if readFile(t, path) != before || !strings.Contains(readFile(t, conPath), "max_option_contracts = 5  # set in Desk") {
			t.Fatal("the partial save did not stop after the constitution")
		}
		ev, found, _ := core.GetEvent(t.Context(), daemonStateScope, cashPolicyEventKey("desk-partial"))
		var receipt cashPolicyReceipt
		if !found || json.Unmarshal(ev.PayloadJSON, &receipt) != nil || receipt.Partial == nil || receipt.PresetAfter != rpc.CashPolicyPresetCustom || receipt.Constitution == nil || receipt.Backup != "" {
			t.Fatalf("partial receipt %+v", receipt)
		}
		// The retry answers with the receipt; a fresh save of the cash half
		// completes the preset.
		s.cashPolicyWriteFault = nil
		snap := cashPolicyGet(t, s)
		if snap.Restore != nil {
			t.Fatalf("restore offered while the desk reads Custom: %+v", snap.Restore)
		}
		if out, err := cashPolicySaveAs(t, s, device, map[string]any{"cash.sweep.reserve_floor_base": 15000, "cash.sweep.reserve_pct_nlv": 15}, "desk-second-half"); err != nil || out.Preset.ID != rpc.CashPolicyPresetCautious || out.Partial != nil {
			t.Fatalf("second half %+v %v", out, err)
		}
	})
	t.Run("constitution moved between check and save", func(t *testing.T) {
		s, path, conPath, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, pinned(cashPolicyTestConstitution, device))
		before := readFile(t, path)
		check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, presetChanges(rpc.CashPolicyPresetCautious))
		moved := strings.Replace(readFile(t, conPath), "policy_version = 7", "policy_version = 8", 1)
		if err := os.WriteFile(conPath, []byte(moved), 0o600); err != nil {
			t.Fatal(err)
		}
		s.riskPolicies.reload()
		if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-moved", device.confirm(t, "m1", check.Terms, cashPolicyTestReview(check)), ""); rpcCode(err) != rpc.CodeSettingsConflict || readFile(t, path) != before || readFile(t, conPath) != moved {
			t.Fatalf("moved constitution: %v", err)
		}
	})
	t.Run("the reread honours the file lock", func(t *testing.T) {
		s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
		s.riskPolicies.fileMu.Lock()
		done := make(chan struct{})
		go func() { s.riskPolicies.reload(); close(done) }()
		select {
		case <-done:
			t.Fatal("reload ran while the write held the lock")
		case <-time.After(100 * time.Millisecond):
		}
		s.riskPolicies.fileMu.Unlock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("reload never ran")
		}
	})
	t.Run("constitution without the table", func(t *testing.T) {
		s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, pcConstitutionHead+"\n[capital]\nbase_currency = \"EUR\"\n")
		snap := cashPolicyGet(t, s)
		if snap.Preset.ID != rpc.CashPolicyPresetCustom || !strings.Contains(snap.Preset.Note, "does not write order_limits.max_order_floor_base") || snap.Sections.OrderLimits.Present {
			t.Fatalf("no table %+v %+v", snap.Preset, snap.Sections.OrderLimits)
		}
		check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"order_limits.max_order_pct_nlv": 20})
		if !strings.Contains(check.Errors["order_limits.max_order_pct_nlv"], "no [order_limits] table yet") {
			t.Fatalf("errors %+v", check.Errors)
		}
	})
}

// A malformed [desk_device] line never refuses the constitution (a refused
// constitution would leave no policy and refuse every preview after a
// restart): the loader keeps the policy, the snapshot names the line's
// error and a cap change is refused with it. An unknown key under the table
// is refused like an unknown key anywhere else in the file. A companion id
// is the first 16 bytes of the point's SHA-256 in base64url, as Desk derives
// it, so a pasted line and its key cannot disagree.
func TestConstitutionDeskDeviceLinesAreValidated(t *testing.T) {
	device := newTestDevice(t, "companion")
	if _, err := parseConstitutionFile([]byte(pinned(cashPolicyTestConstitution, device))); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConstitutionFile([]byte(cashPolicyTestConstitution + "\n[desk_device]\nwatch = \"x\"\n")); err == nil {
		t.Fatal("an unknown key under [desk_device] was accepted")
	}
	hexID := hex.EncodeToString(func() []byte { s := sha256.Sum256(device.point); return s[:16] }())
	for name, line := range map[string]string{
		"no id":        `companion = "` + base64.RawURLEncoding.EncodeToString(device.point) + `"`,
		"hex id":       `companion = "` + hexID + `:` + base64.RawURLEncoding.EncodeToString(device.point) + `"`,
		"not a point":  `companion = "` + device.id + `:AAAA"`,
		"passkey junk": `passkey = "cred:not-base64!!"`,
	} {
		constitution := cashPolicyTestConstitution + "\n[desk_device]\n" + line + "\n"
		c, err := parseConstitutionFile([]byte(constitution))
		if err != nil {
			t.Fatalf("%s refused the constitution: %v", name, err)
		}
		if _, err := c.DeskDevice.Keys(); err == nil {
			t.Fatalf("%s parsed as a key", name)
		}
		s, path, conPath, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, constitution)
		before, conBefore := readFile(t, path), readFile(t, conPath)
		snap := cashPolicyGet(t, s)
		if len(snap.Device.Verifiable) != 0 || !strings.HasPrefix(snap.Device.Message, "Canary cannot verify your device: desk_device.") {
			t.Fatalf("%s: device %+v", name, snap.Device)
		}
		check := cashPolicyCheck(t, s, snap.Revision, map[string]any{"order_limits.max_order_pct_nlv": 20})
		if _, err := cashPolicyApply(s, check.Terms, check.Digest, "desk-badline", device.confirm(t, "b1", check.Terms, cashPolicyTestReview(check)), ""); rpcCode(err) != rpc.CodeConfirmationUnverifiable || !strings.Contains(err.Error(), "desk_device.") {
			t.Fatalf("%s: cap change %v", name, err)
		}
		if readFile(t, path) != before || readFile(t, conPath) != conBefore {
			t.Fatalf("%s: a refused save wrote a file", name)
		}
	}
	if keys, err := (&risk.ConstitutionDeskDevice{Passkey: newTestDevice(t, "passkey").line()[len(`passkey = "`) : len(newTestDevice(t, "passkey").line())-1]}).Keys(); err != nil || len(keys) != 1 || keys[0].Class != "passkey" {
		t.Fatalf("passkey line %v %+v", err, keys)
	}
}

// Review fields the companion knows (Desk's Policy.swift) are unchanged: a
// check's changes carry no new field, so the companion signs them as before.
func TestCashPolicyChangesKeepTheCompanionsFields(t *testing.T) {
	s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution)
	check := cashPolicyCheck(t, s, cashPolicyGet(t, s).Revision, presetChanges(rpc.CashPolicyPresetAggressive))
	raw, _ := json.Marshal(check.Changes)
	var changes []map[string]any
	_ = json.Unmarshal(raw, &changes)
	known := []string{"key", "label", "unit", "currency", "from", "from_source", "to", "from_text", "to_text"}
	for _, c := range changes {
		for field := range c {
			if !slices.Contains(known, field) {
				t.Fatalf("change carries %q, which the companion does not show", field)
			}
		}
	}
	// Aggressive keeps Balanced's reserve floor, so four keys change.
	if len(changes) != 4 {
		t.Fatalf("changes %d", len(changes))
	}
	var terms map[string]any
	_ = json.Unmarshal([]byte(check.Terms), &terms)
	if len(terms) != 4 || terms["version"] != 1.0 {
		t.Fatalf("terms %v", terms)
	}
}

// A fixed vector from Desk's own code (desk eb4383d, the real companion
// path: enrolment, prepare, challenge, companionSubmit, policyDeviceLine;
// the signing key was a test key): the [desk_device] line Desk shows and
// the envelope Desk sent verify here, so the two readings of the key id and
// the digest chain cannot drift apart again without this test saying so.
func TestCashPolicyVerifiesDesksOwnCompanionEnvelope(t *testing.T) {
	const (
		line     = `Yib7YatlNMp7PBkgkDcaYg:BHA_5_1h8QKDK8LBlTnh1KRZp9xEhEBMiA4sfIH46M39z_hR1ij6Od1hpx5SThcTuLHg_LRK5BCJNdwn-h5tvqI`
		actionID = "KR5KS66WR3JYM23EEIZOEA3QS3"
		terms    = `{"changes":{"cash.leveling.trigger_base":5000},"expected_revision":"sha256:r1","kind":"canary.cash_policy_change","version":1}`
		envelope = `{"challenge":"MiCy8VxP7unpzdZDIw3FE1FGAWAwLzjsUDvRvw6PEms","credential":"companion","key_id":"Yib7YatlNMp7PBkgkDcaYg","review_json":"{\"base_currency\":\"\",\"changes\":[{\"key\":\"cash.leveling.trigger_base\",\"label\":\"Band\",\"unit\":\"base\",\"from\":10000,\"from_source\":\"file\",\"to\":5000,\"from_text\":\"\",\"to_text\":\"\"}],\"consequences\":[\"Leveling repays loans from 5,000 EUR instead of 10,000 EUR.\"],\"findings\":null,\"kind\":\"cash_policy\",\"policy_version\":14,\"saved_version\":15}","signature":"1fCJWfFL2I5cJEnIBpPK2za2Ve1L77nzwHYd-TIUzyKDQIWSqDRhoNXn88sE3iCPaKVKzWehZeDomI8DLWxrKw"}`
	)
	key, err := risk.ParseDeskDeviceKey("companion", line)
	if err != nil {
		t.Fatal(err)
	}
	c := &rpc.CashPolicyConfirmation{DeskActionID: actionID, Credential: "companion:Yib7YatlNMp7PBkgkDcaYg", Envelope: envelope}
	verified, err := verifyCashPolicyDevice([]risk.DeskDeviceKey{key}, c, terms)
	if err != nil || verified != "companion:Yib7YatlNMp7PBkgkDcaYg" {
		t.Fatalf("Desk's real envelope: %q %v", verified, err)
	}
	for name, bad := range map[string]*rpc.CashPolicyConfirmation{
		"other terms":  {DeskActionID: actionID, Credential: c.Credential, Envelope: envelope},
		"other action": {DeskActionID: "KR5KS66WR3JYM23EEIZOEA3QS4", Credential: c.Credential, Envelope: envelope},
		"other review": {DeskActionID: actionID, Credential: c.Credential, Envelope: strings.Replace(envelope, "10000", "10001", 1)},
		"other sig":    {DeskActionID: actionID, Credential: c.Credential, Envelope: strings.Replace(envelope, "1fCJWfFL", "1fCJWfFM", 1)},
	} {
		useTerms := terms
		if name == "other terms" {
			useTerms = strings.Replace(terms, "5000", "4000", 1)
		}
		if _, err := verifyCashPolicyDevice([]risk.DeskDeviceKey{key}, bad, useTerms); err == nil {
			t.Fatalf("%s verified", name)
		}
	}
	// The loader keeps a constitution that pins this line, and the snapshot
	// lists the credential.
	s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, cashPolicyTestConstitution+"\n[desk_device]\ncompanion = \""+line+"\"\n")
	if snap := cashPolicyGet(t, s); !slices.Equal(snap.Device.Verifiable, []string{"companion:Yib7YatlNMp7PBkgkDcaYg"}) {
		t.Fatalf("device %+v", snap.Device)
	}
}
