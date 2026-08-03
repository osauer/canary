package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func approvedConstitution() Constitution {
	return Constitution{
		Kind:          ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 1,
		Capital: ConstitutionCapital{
			BaseCurrency:        "EUR",
			ProtectedFloor:      new(200000.0),
			DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240),
			MaxUnreconciledDays: new(7),
		},
		Drawdown: ConstitutionDrawdown{
			WarnConsumedPct:  new(15.0),
			BlockConsumedPct: new(30.0),
			BlockEnforcement: EnforcementShadow,
		},
		Override: ConstitutionOverride{MaxDurationHours: new(24)},
		Recon: ConstitutionRecon{
			AmountTolerancePct:     new(0.5),
			AmountToleranceMin:     new(5.0),
			DateWindowBusinessDays: new(3),
			MaxReportAgeDays:       new(4),
		},
		Cadence: ConstitutionCadence{
			Morning: ConstitutionArtefact{Class: EnforcementAdvisory},
			EOD:     ConstitutionArtefact{Class: EnforcementAdvisory},
			Weekly:  ConstitutionArtefact{Class: EnforcementAdvisory},
		},
		Inventory: ConstitutionInventory{
			Rulebook: &ConstitutionPolicyPin{ID: "rulebook-v2", Version: "2"},
		},
	}
}

func approvedV3Constitution() Constitution {
	c := approvedConstitution()
	c.PolicyVersion = 3
	c.Recon.MaxEquityDivergencePct = new(1.25)
	return c
}

func approvedV4Constitution() Constitution {
	c := approvedV3Constitution()
	c.PolicyVersion = 4
	c.Cadence.Nudges = &ConstitutionNudgeCadence{
		Timezone:             new("Europe/Berlin"),
		ReconcileWarningDays: new(2),
	}
	c.Cadence.Monthly = &ConstitutionMonthlyCadence{
		Class:        new(EnforcementAdvisory),
		DayOfMonth:   new(1),
		NudgeAtLocal: new("09:00"),
	}
	return c
}

func TestConstitutionValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Constitution)
		wantErr string
	}{
		{"valid", func(c *Constitution) {}, ""},
		{"bad kind", func(c *Constitution) { c.Kind = "ibkr.other" }, "kind"},
		{"bad schema", func(c *Constitution) { c.SchemaVersion = 2 }, "schema_version"},
		{"empty id", func(c *Constitution) { c.PolicyID = " " }, "policy_id"},
		{"zero version", func(c *Constitution) { c.PolicyVersion = 0 }, "policy_version"},
		{"bad currency", func(c *Constitution) { c.Capital.BaseCurrency = "EURO" }, "base_currency"},
		{"negative floor", func(c *Constitution) { c.Capital.ProtectedFloor = new(-1.0) }, "protected_floor"},
		{"zero declared", func(c *Constitution) { c.Capital.DeclaredRiskCapital = new(0.0) }, "declared_risk_capital"},
		{"warn above block", func(c *Constitution) { c.Drawdown.WarnConsumedPct = new(40.0) }, "below block"},
		{"warn out of range", func(c *Constitution) { c.Drawdown.WarnConsumedPct = new(120.0) }, "(0, 100]"},
		{"hard rejected in v1", func(c *Constitution) { c.Drawdown.BlockEnforcement = "hard" }, "not promotable"},
		{"unknown enforcement", func(c *Constitution) { c.Drawdown.BlockEnforcement = "block-everything" }, "invalid"},
		{"bad cadence class", func(c *Constitution) { c.Cadence.Morning.Class = "mandatory" }, "only advisory"},
		{"pin missing version", func(c *Constitution) { c.Inventory.Rulebook = &ConstitutionPolicyPin{ID: "x"} }, "id and version"},
		{"negative recon tolerance", func(c *Constitution) { c.Recon.AmountToleranceMin = new(-1.0) }, "amount_tolerance_min"},
		{"zero recon window", func(c *Constitution) { c.Recon.DateWindowBusinessDays = new(0) }, "date_window_business_days"},
		{"zero report age", func(c *Constitution) { c.Recon.MaxReportAgeDays = new(0) }, "max_report_age_days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := approvedConstitution()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConstitutionV3EquityDivergenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   float64
		wantErr bool
	}{
		{"positive", 0.5, false},
		{"zero", 0, true},
		{"negative", -1, true},
		{"nan", math.NaN(), true},
		{"positive infinity", math.Inf(1), true},
		{"negative infinity", math.Inf(-1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := approvedV3Constitution()
			c.Recon.MaxEquityDivergencePct = &tc.value
			err := c.Validate()
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "positive and finite")) {
				t.Fatalf("Validate() = %v, want positive-and-finite error", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
	c := approvedConstitution()
	c.Recon.MaxEquityDivergencePct = new(0.5)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "requires policy_version >= 3") {
		t.Fatalf("v2 key error = %v, want targeted version error", err)
	}
}

func TestConstitutionV4CadenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Constitution)
		wantErr string
	}{
		{"valid", func(*Constitution) {}, ""},
		{"old version rejects v4 cadence", func(c *Constitution) { c.PolicyVersion = 3 }, "requires policy_version >= 4"},
		{"empty timezone", func(c *Constitution) { c.Cadence.Nudges.Timezone = new("") }, "timezone"},
		{"host local timezone", func(c *Constitution) { c.Cadence.Nudges.Timezone = new("Local") }, "timezone"},
		{"host local timezone case insensitive", func(c *Constitution) { c.Cadence.Nudges.Timezone = new("lOcAl") }, "timezone"},
		{"timezone leading whitespace", func(c *Constitution) { c.Cadence.Nudges.Timezone = new(" Europe/Berlin") }, "whitespace"},
		{"timezone trailing whitespace", func(c *Constitution) { c.Cadence.Nudges.Timezone = new("Europe/Berlin ") }, "whitespace"},
		{"unknown timezone", func(c *Constitution) { c.Cadence.Nudges.Timezone = new("Mars/Olympus") }, "timezone"},
		{"zero warning days", func(c *Constitution) { c.Cadence.Nudges.ReconcileWarningDays = new(0) }, "reconcile_warning_days"},
		{"bad monthly class", func(c *Constitution) { c.Cadence.Monthly.Class = new("mandatory") }, "only advisory"},
		{"zero monthly day", func(c *Constitution) { c.Cadence.Monthly.DayOfMonth = new(0) }, "day_of_month"},
		{"nonexistent monthly day", func(c *Constitution) { c.Cadence.Monthly.DayOfMonth = new(29) }, "day_of_month"},
		{"non-padded time", func(c *Constitution) { c.Cadence.Monthly.NudgeAtLocal = new("9:00") }, "nudge_at_local"},
		{"invalid time", func(c *Constitution) { c.Cadence.Monthly.NudgeAtLocal = new("24:00") }, "nudge_at_local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := approvedV4Constitution()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}

	for _, mutate := range []func(*Constitution){
		func(c *Constitution) { c.Cadence.Nudges = &ConstitutionNudgeCadence{Timezone: new("Europe/Berlin")} },
		func(c *Constitution) { c.Cadence.Nudges = &ConstitutionNudgeCadence{ReconcileWarningDays: new(2)} },
		func(c *Constitution) {
			c.Cadence.Monthly = &ConstitutionMonthlyCadence{Class: new(EnforcementAdvisory)}
		},
		func(c *Constitution) { c.Cadence.Monthly = &ConstitutionMonthlyCadence{DayOfMonth: new(1)} },
		func(c *Constitution) { c.Cadence.Monthly = &ConstitutionMonthlyCadence{NudgeAtLocal: new("09:00")} },
	} {
		c := approvedV3Constitution()
		mutate(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "requires policy_version >= 4") {
			t.Fatalf("old-version v4 key error = %v, want targeted version error", err)
		}
	}
}

func TestConstitutionV4CadenceTOMLIsStrictAndVersioned(t *testing.T) {
	decode := func(t *testing.T, version int, cadence string) Constitution {
		t.Helper()
		input := "kind = \"ibkr.risk_policy\"\nschema_version = 1\npolicy_id = \"risk-constitution\"\npolicy_version = " + strconv.Itoa(version) + "\n" + cadence
		var c Constitution
		metadata, err := toml.Decode(input, &c)
		if err != nil {
			t.Fatal(err)
		}
		if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
			t.Fatalf("typed cadence left undecoded keys: %v", undecoded)
		}
		return c
	}
	fullCadence := `[cadence.nudges]
timezone = "Europe/Berlin"
reconcile_warning_days = 2

[cadence.monthly]
class = "advisory"
day_of_month = 1
nudge_at_local = "09:00"
`
	v4 := decode(t, 4, fullCadence)
	if err := v4.Validate(); err != nil {
		t.Fatalf("v4 authored cadence = %v", err)
	}
	for _, key := range v4.UnapprovedKeys() {
		if strings.HasPrefix(key, "cadence.nudges.") || strings.HasPrefix(key, "cadence.monthly.") {
			t.Fatalf("authored v4 cadence stayed unapproved: %s", key)
		}
	}

	v3 := decode(t, 3, fullCadence)
	if err := v3.Validate(); err == nil || !strings.Contains(err.Error(), "requires policy_version >= 4") {
		t.Fatalf("v3 parsed v4 cadence without targeted rejection: %v", err)
	}

	// Absent cadence keys are defaults, never approval material.
	missing := decode(t, 4, `[cadence.nudges]
timezone = "Europe/Berlin"
`)
	for _, key := range missing.UnapprovedKeys() {
		if strings.HasPrefix(key, "cadence.") {
			t.Fatalf("absent v4 cadence key reported unapproved: %s", key)
		}
	}
}

func TestConstitutionInventoryRejectsRetiredCanaryTOMLKey(t *testing.T) {
	t.Parallel()
	const input = `kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1
[inventory.canary]
id = "active-v1"
version = "risk-policy-v1"
`
	var constitution Constitution
	metadata, err := toml.Decode(input, &constitution)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var unknown []string
	for _, key := range metadata.Undecoded() {
		unknown = append(unknown, key.String())
	}
	if got := strings.Join(unknown, ", "); !strings.Contains(got, "inventory.canary") {
		t.Fatalf("undecoded keys=%q, want retired inventory.canary", got)
	}
	if constitution.Inventory.Stress != nil {
		t.Fatalf("retired inventory.canary populated Stress: %+v", constitution.Inventory.Stress)
	}
}

// Material keys never backfill: an empty file section validates but stays
// unapproved, and a partially approved policy names exactly the gaps.
func TestConstitutionUnapprovedKeys(t *testing.T) {
	empty := Constitution{Kind: ConstitutionKind, SchemaVersion: 1, PolicyID: "c", PolicyVersion: 1}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty-but-well-formed constitution must validate, got %v", err)
	}
	got := empty.UnapprovedKeys()
	want := []string{
		"capital.base_currency", "capital.protected_floor", "capital.declared_risk_capital",
		"capital.max_equity_age_minutes", "capital.max_unreconciled_days",
		"drawdown.warn_consumed_pct", "drawdown.block_consumed_pct", "override.max_duration_hours",
		"recon.amount_tolerance_pct", "recon.amount_tolerance_min",
		"recon.date_window_business_days", "recon.max_report_age_days",
	}
	if len(got) != len(want) {
		t.Fatalf("UnapprovedKeys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("UnapprovedKeys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if keys := approvedConstitution().UnapprovedKeys(); len(keys) != 0 {
		t.Fatalf("fully approved constitution reports unapproved keys: %v", keys)
	}
	v3 := approvedV3Constitution()
	v3.Recon.MaxEquityDivergencePct = nil
	if got := v3.UnapprovedKeys(); len(got) == 0 || got[len(got)-1] != "recon.max_equity_divergence_pct" {
		t.Fatalf("v3 missing divergence key = %v, want unapproved", got)
	}
	v4 := approvedV4Constitution()
	v4.Cadence.Nudges.Timezone = nil
	v4.Cadence.Monthly.DayOfMonth = nil
	if got := v4.UnapprovedKeys(); len(got) != 0 {
		t.Fatalf("v4 absent cadence keys reported unapproved: %v — cadence defaults in code", got)
	}
}

// Every material and governance field must move the fingerprint: a
// threshold outside the fingerprint would be a silent policy change.
func TestConstitutionFingerprintCoversEveryField(t *testing.T) {
	base := approvedConstitution().FingerprintKey()
	v2Regression := approvedConstitution()
	v2Regression.PolicyVersion = 2
	for version, regression := range map[string]struct {
		got  string
		want string
	}{
		"v1": {base, "sha256:d7253847c81bab17eb56f0c053024ee47ebdf4a606f10b2231cb163eba24a716"},
		"v2": {v2Regression.FingerprintKey(), "sha256:2d6b4e068bdd8c636d46c33d2315be29102a3f455a3dd2268ae14176fa61dba9"},
		"v3": {approvedV3Constitution().FingerprintKey(), "sha256:eba0420989e74822f360d2db9e824ec74aa0fbea4e4f933f8a2aba2e2708b7cc"},
	} {
		if regression.got != regression.want {
			t.Errorf("%s fingerprint changed: got %s, want %s", version, regression.got, regression.want)
		}
	}
	if legacy := legacyConstitutionFingerprint(approvedConstitution()); base != legacy {
		t.Fatalf("pre-v3 fingerprint changed: got %s, legacy %s", base, legacy)
	}
	mutations := map[string]func(*Constitution){
		"policy_version":                     func(c *Constitution) { c.PolicyVersion = 2 },
		"policy_id":                          func(c *Constitution) { c.PolicyID = "other" },
		"capital.base_currency":              func(c *Constitution) { c.Capital.BaseCurrency = "USD" },
		"capital.protected_floor":            func(c *Constitution) { c.Capital.ProtectedFloor = new(200001.0) },
		"capital.protected_floor unapproved": func(c *Constitution) { c.Capital.ProtectedFloor = nil },
		"capital.declared_risk_capital":      func(c *Constitution) { c.Capital.DeclaredRiskCapital = new(60000.0) },
		"capital.max_equity_age_minutes":     func(c *Constitution) { c.Capital.MaxEquityAgeMinutes = new(60) },
		"capital.max_unreconciled_days":      func(c *Constitution) { c.Capital.MaxUnreconciledDays = new(14) },
		"drawdown.warn_consumed_pct":         func(c *Constitution) { c.Drawdown.WarnConsumedPct = new(10.0) },
		"drawdown.block_consumed_pct":        func(c *Constitution) { c.Drawdown.BlockConsumedPct = new(25.0) },
		"drawdown.block_enforcement":         func(c *Constitution) { c.Drawdown.BlockEnforcement = EnforcementAdvisory },
		"override.max_duration_hours":        func(c *Constitution) { c.Override.MaxDurationHours = new(8) },
		"recon.amount_tolerance_pct":         func(c *Constitution) { c.Recon.AmountTolerancePct = new(1.0) },
		"recon.amount_tolerance_min":         func(c *Constitution) { c.Recon.AmountToleranceMin = new(10.0) },
		"recon.date_window_business_days":    func(c *Constitution) { c.Recon.DateWindowBusinessDays = new(5) },
		"recon.max_report_age_days":          func(c *Constitution) { c.Recon.MaxReportAgeDays = new(7) },
		"cadence.morning.class":              func(c *Constitution) { c.Cadence.Morning.Class = "" },
		"inventory.rulebook":                 func(c *Constitution) { c.Inventory.Rulebook.Version = "3" },
		"inventory.protection added":         func(c *Constitution) { c.Inventory.Protection = &ConstitutionPolicyPin{ID: "p", Version: "1"} },
		"inventory.stress added":             func(c *Constitution) { c.Inventory.Stress = &ConstitutionPolicyPin{ID: "s", Version: "1"} },
	}
	for name, mutate := range mutations {
		c := approvedConstitution()
		mutate(&c)
		if c.FingerprintKey() == base {
			t.Errorf("mutation %q did not change the fingerprint", name)
		}
	}
	if approvedConstitution().FingerprintKey() != base {
		t.Fatal("fingerprint is not deterministic")
	}
	v3 := approvedV3Constitution()
	v3Base := v3.FingerprintKey()
	v3.Recon.MaxEquityDivergencePct = new(2.0)
	if v3.FingerprintKey() == v3Base {
		t.Fatal("recon.max_equity_divergence_pct did not change the v3 fingerprint")
	}
	v4 := approvedV4Constitution()
	v4Base := v4.FingerprintKey()
	if want := "sha256:3f390ab250fa37798cc8656b1372843c964004156fbc4d767e5da4ba11268550"; v4Base != want {
		t.Fatalf("v4 fingerprint changed: got %s, want %s", v4Base, want)
	}
	for name, mutate := range map[string]func(*Constitution){
		"cadence.nudges.timezone":               func(c *Constitution) { c.Cadence.Nudges.Timezone = new("Europe/Paris") },
		"cadence.nudges.reconcile_warning_days": func(c *Constitution) { c.Cadence.Nudges.ReconcileWarningDays = new(3) },
		"cadence.monthly.class":                 func(c *Constitution) { c.Cadence.Monthly.Class = nil },
		"cadence.monthly.day_of_month":          func(c *Constitution) { c.Cadence.Monthly.DayOfMonth = new(2) },
		"cadence.monthly.nudge_at_local":        func(c *Constitution) { c.Cadence.Monthly.NudgeAtLocal = new("10:00") },
	} {
		c := approvedV4Constitution()
		mutate(&c)
		if c.FingerprintKey() == v4Base {
			t.Errorf("v4 mutation %q did not change the fingerprint", name)
		}
	}
	missingTables := approvedV4Constitution()
	missingTables.Cadence.Nudges = nil
	missingTables.Cadence.Monthly = nil
	emptyTables := missingTables
	emptyTables.Cadence.Nudges = &ConstitutionNudgeCadence{}
	emptyTables.Cadence.Monthly = &ConstitutionMonthlyCadence{}
	if missingTables.FingerprintKey() != emptyTables.FingerprintKey() {
		t.Fatal("v4 fingerprint must project missing material keys as null, independent of empty table presence")
	}
}

func legacyConstitutionFingerprint(c Constitution) string {
	normalized := struct {
		Kind          string               `json:"kind"`
		SchemaVersion int                  `json:"schema_version"`
		PolicyID      string               `json:"policy_id"`
		PolicyVersion int                  `json:"policy_version"`
		Capital       ConstitutionCapital  `json:"capital"`
		Drawdown      ConstitutionDrawdown `json:"drawdown"`
		Override      ConstitutionOverride `json:"override"`
		Recon         struct {
			AmountTolerancePct     *float64 `json:"amount_tolerance_pct"`
			AmountToleranceMin     *float64 `json:"amount_tolerance_min"`
			DateWindowBusinessDays *int     `json:"date_window_business_days"`
			MaxReportAgeDays       *int     `json:"max_report_age_days"`
		} `json:"recon"`
		Cadence   ConstitutionCadence   `json:"cadence"`
		Inventory ConstitutionInventory `json:"inventory"`
	}{
		Kind: strings.TrimSpace(c.Kind), SchemaVersion: c.SchemaVersion,
		PolicyID: strings.TrimSpace(c.PolicyID), PolicyVersion: c.PolicyVersion,
		Capital: c.Capital, Drawdown: c.Drawdown, Override: c.Override,
		Recon: struct {
			AmountTolerancePct     *float64 `json:"amount_tolerance_pct"`
			AmountToleranceMin     *float64 `json:"amount_tolerance_min"`
			DateWindowBusinessDays *int     `json:"date_window_business_days"`
			MaxReportAgeDays       *int     `json:"max_report_age_days"`
		}{c.Recon.AmountTolerancePct, c.Recon.AmountToleranceMin, c.Recon.DateWindowBusinessDays, c.Recon.MaxReportAgeDays},
		Cadence: c.Cadence, Inventory: c.Inventory,
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestEvaluateCapitalTiers(t *testing.T) {
	c := approvedConstitution()
	now := time.Now()
	fresh := func(equity float64) *CapitalObservation {
		return &CapitalObservation{EquityBase: equity, AsOf: now.Add(-time.Minute)}
	}
	seeded := CapitalRuntime{AdjustedPeakBase: 260000, PeakAsOf: now.Add(-24 * time.Hour), Seeded: true, LastReconciledAt: now.Add(-time.Hour)}

	t.Run("nil policy is unapproved", func(t *testing.T) {
		v := EvaluateCapital(nil, seeded, fresh(260000), now)
		if v.Tier != CapitalTierUnapproved {
			t.Fatalf("tier = %s, want unapproved", v.Tier)
		}
	})
	t.Run("missing material key is unapproved, never ok", func(t *testing.T) {
		partial := c
		partial.Drawdown.BlockConsumedPct = nil
		v := EvaluateCapital(&partial, seeded, fresh(260000), now)
		if v.Tier != CapitalTierUnapproved {
			t.Fatalf("tier = %s, want unapproved", v.Tier)
		}
	})
	t.Run("no observation is unknown, never ok", func(t *testing.T) {
		v := EvaluateCapital(&c, seeded, nil, now)
		if v.Tier != CapitalTierUnknown {
			t.Fatalf("tier = %s, want unknown", v.Tier)
		}
	})
	t.Run("unseeded state is unknown", func(t *testing.T) {
		v := EvaluateCapital(&c, CapitalRuntime{LastReconciledAt: now}, fresh(260000), now)
		if v.Tier != CapitalTierUnknown {
			t.Fatalf("tier = %s, want unknown", v.Tier)
		}
	})
	t.Run("stale equity is unknown with disclosure", func(t *testing.T) {
		old := &CapitalObservation{EquityBase: 260000, AsOf: now.Add(-5 * time.Hour)}
		v := EvaluateCapital(&c, seeded, old, now)
		if v.Tier != CapitalTierUnknown || !v.EquityStale {
			t.Fatalf("tier = %s stale = %v, want unknown/true", v.Tier, v.EquityStale)
		}
	})
	t.Run("unreconciled past horizon is unknown", func(t *testing.T) {
		rt := seeded
		rt.LastReconciledAt = now.Add(-8 * 24 * time.Hour)
		v := EvaluateCapital(&c, rt, fresh(260000), now)
		if v.Tier != CapitalTierUnknown || !v.ReconcileStale {
			t.Fatalf("tier = %s reconcileStale = %v, want unknown/true", v.Tier, v.ReconcileStale)
		}
	})
	t.Run("only the active unreconciled-days override extends the horizon", func(t *testing.T) {
		rt := seeded
		rt.LastReconciledAt = now.Add(-8 * 24 * time.Hour)
		rt.UnreconciledOverrideUntil = now.Add(time.Hour)
		v := EvaluateCapital(&c, rt, fresh(260000), now)
		if v.ReconcileStale || v.Tier != CapitalTierOK {
			t.Fatalf("active override: tier=%s stale=%v, want ok/false", v.Tier, v.ReconcileStale)
		}
		rt.UnreconciledOverrideUntil = now.Add(-time.Minute)
		v = EvaluateCapital(&c, rt, fresh(260000), now)
		if !v.ReconcileStale || v.Tier != CapitalTierUnknown {
			t.Fatalf("expired override: tier=%s stale=%v, want unknown/true", v.Tier, v.ReconcileStale)
		}
	})
	t.Run("ok warn block ladder in declared-risk units", func(t *testing.T) {
		// peak 260k, declared 50k: warn at −7.5k (15%), block at −15k (30%).
		for _, tc := range []struct {
			equity float64
			tier   string
		}{
			{258000, CapitalTierOK},    // −2k = 4%
			{252000, CapitalTierWarn},  // −8k = 16%
			{244000, CapitalTierBlock}, // −16k = 32%
		} {
			v := EvaluateCapital(&c, seeded, fresh(tc.equity), now)
			if v.Tier != tc.tier {
				t.Fatalf("equity %.0f: tier = %s, want %s (consumed %v)", tc.equity, v.Tier, tc.tier, v.ConsumedPct)
			}
		}
	})
	t.Run("effective risk capital is min of declared and equity minus floor", func(t *testing.T) {
		v := EvaluateCapital(&c, seeded, fresh(230000), now)
		if v.EffectiveRiskCapitalBase == nil || *v.EffectiveRiskCapitalBase != 30000 {
			t.Fatalf("effective = %v, want 30000 (equity-floor binds)", v.EffectiveRiskCapitalBase)
		}
		v = EvaluateCapital(&c, seeded, fresh(300000), now)
		if v.EffectiveRiskCapitalBase == nil || *v.EffectiveRiskCapitalBase != 50000 {
			t.Fatalf("effective = %v, want 50000 (declared binds)", v.EffectiveRiskCapitalBase)
		}
	})
	t.Run("external flows do not move drawdown", func(t *testing.T) {
		rt := seeded
		rt.CumExternalFlowsBase = 20000 // a 20k deposit was declared
		v := EvaluateCapital(&c, rt, fresh(278000), now)
		// adjusted = 278k − 20k = 258k against peak 260k: −2k = 4% → ok.
		if v.Tier != CapitalTierOK {
			t.Fatalf("tier = %s, want ok (deposit must not read as recovery or drawdown)", v.Tier)
		}
	})
	t.Run("latch dominates recovery", func(t *testing.T) {
		rt := seeded
		rt.BlockLatched = true
		v := EvaluateCapital(&c, rt, fresh(261000), now) // above peak again
		if v.Tier != CapitalTierBlock {
			t.Fatalf("tier = %s, want block (latch persists until human reset)", v.Tier)
		}
	})
	t.Run("latch dominates stale data too", func(t *testing.T) {
		rt := seeded
		rt.BlockLatched = true
		v := EvaluateCapital(&c, rt, nil, now)
		if v.Tier != CapitalTierBlock {
			t.Fatalf("tier = %s, want block", v.Tier)
		}
	})
}

// The explain view must cover every material key: a limit the operator can
// approve but the view does not render would be an invisible control.
func TestConstitutionLimitsCoverAllMaterialKeys(t *testing.T) {
	rows := ConstitutionLimits(nil)
	byKey := map[string]ConstitutionLimit{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	for _, key := range (&Constitution{}).UnapprovedKeys() {
		row, ok := byKey[key]
		if !ok {
			t.Errorf("material key %s has no explain row", key)
			continue
		}
		if row.Source != "unapproved" || row.Value != "unapproved" {
			t.Errorf("nil policy: %s renders %q/%q, want unapproved/unapproved", key, row.Value, row.Source)
		}
		if row.Meaning == "" {
			t.Errorf("%s has no meaning text", key)
		}
	}
	c := approvedConstitution()
	for _, r := range ConstitutionLimits(&c) {
		if r.Key == "drawdown.block_enforcement" {
			continue // value is the class itself
		}
		if _, material := byKey[r.Key]; material && r.Source == "unapproved" {
			// every material key was approved above; none may render unapproved
			for _, k := range c.UnapprovedKeys() {
				if k == r.Key {
					t.Fatalf("approved key %s still renders unapproved", r.Key)
				}
			}
		}
	}
}

func TestConstitutionLimitsAreVersionAware(t *testing.T) {
	v2 := approvedConstitution()
	var v2Meaning string
	for _, row := range ConstitutionLimits(&v2) {
		if row.Key == "recon.max_equity_divergence_pct" {
			t.Fatal("v2 explain unexpectedly renders the v3-only divergence key")
		}
		if strings.HasPrefix(row.Key, "cadence.nudges.") || strings.HasPrefix(row.Key, "cadence.monthly.") {
			t.Fatalf("v2 explain unexpectedly renders v4-only key %s", row.Key)
		}
		if row.Key == "capital.max_unreconciled_days" {
			v2Meaning = row.Meaning
		}
	}
	if !strings.Contains(v2Meaning, "human reconcile attestation") || strings.Contains(v2Meaning, "automatic") {
		t.Fatalf("v2 meaning changed: %q", v2Meaning)
	}
	v3 := approvedV3Constitution()
	var v3Meaning string
	var divergence ConstitutionLimit
	for _, row := range ConstitutionLimits(&v3) {
		if strings.HasPrefix(row.Key, "cadence.nudges.") || strings.HasPrefix(row.Key, "cadence.monthly.") {
			t.Fatalf("v3 explain unexpectedly renders v4-only key %s", row.Key)
		}
		switch row.Key {
		case "capital.max_unreconciled_days":
			v3Meaning = row.Meaning
		case "recon.max_equity_divergence_pct":
			divergence = row
		}
	}
	if !strings.Contains(v3Meaning, "automatic clean-report extension") || !strings.Contains(v3Meaning, "human sign-off") {
		t.Fatalf("v3 meaning = %q", v3Meaning)
	}
	if divergence.Source != "file" || divergence.Value != "1.25%" {
		t.Fatalf("v3 divergence explain = %+v", divergence)
	}
	v4 := approvedV4Constitution()
	v4Rows := make(map[string]ConstitutionLimit)
	for _, row := range ConstitutionLimits(&v4) {
		v4Rows[row.Key] = row
	}
	for key, value := range map[string]string{
		"cadence.nudges.timezone":               "Europe/Berlin",
		"cadence.nudges.reconcile_warning_days": "2 days",
		"cadence.monthly.class":                 "advisory",
		"cadence.monthly.day_of_month":          "1 working day of month",
		"cadence.monthly.nudge_at_local":        "09:00",
	} {
		if row := v4Rows[key]; row.Source != "file" || row.Value != value {
			t.Errorf("v4 explain %s = %+v, want file/%s", key, row, value)
		}
	}
	// Un-authored cadence keys resolve to defaults with honest sources: the
	// clock is the machine's, everything else is the code default.
	bare := approvedV4Constitution()
	bare.Cadence.Nudges = nil
	bare.Cadence.Monthly = nil
	bareRows := make(map[string]ConstitutionLimit)
	for _, row := range ConstitutionLimits(&bare) {
		bareRows[row.Key] = row
	}
	if row := bareRows["cadence.nudges.timezone"]; row.Source != "machine" || row.Value == "" {
		t.Errorf("default timezone explain = %+v, want machine source with a value", row)
	}
	for key, value := range map[string]string{
		"cadence.nudges.reconcile_warning_days": "2 days",
		"cadence.monthly.class":                 "advisory",
		"cadence.monthly.day_of_month":          "1 working day of month",
		"cadence.monthly.nudge_at_local":        "09:00",
	} {
		if row := bareRows[key]; row.Source != "default" || row.Value != value {
			t.Errorf("default explain %s = %+v, want default/%s", key, row, value)
		}
	}
}
