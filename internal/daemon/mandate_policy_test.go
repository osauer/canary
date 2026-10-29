package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

const mandateTableTOML = `
[mandate]
max_loss_pct = 30.0
vol_target_pct = 15.0
tax_reserve_base = 70000.0
started_at = "2026-10-14"
gap_shock_pct = 25.0
gap_room_pct = 33.3

[mandate.sleeves]
owner_vol_pct = 7.0
core_vol_pct = 6.0
overlay_vol_pct = 4.0
`

// schema2ConstitutionTOML is a current-format constitution the mandate can
// be appended to.
func schema2ConstitutionTOML() string {
	s := strings.Replace(validRiskPolicyV3TOML(), "schema_version = 1", "schema_version = 2", 1)
	s = strings.Replace(s, `kind = "ibkr.risk_policy"`, `kind = "canary.risk_policy"`, 1)
	return strings.Replace(s, "policy_version = 3", "policy_version = 5", 1)
}

// A file written before [mandate] existed keeps both fingerprints; writing
// the table changes them, and the manager adopts it only with a higher
// policy_version.
func TestMandateTableFingerprintAndAdoption(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, schema2ConstitutionTOML())
	before := m.snapshot()
	if before.status != rpc.RiskPolicyStatusActive || before.policy.Mandate != nil {
		t.Fatalf("fixture: %+v", before)
	}
	withoutTable := before.policy.FingerprintKey()
	effectiveWithout := before.policy.EffectiveFingerprintKey()
	c := *before.policy
	c.Mandate = nil
	if c.FingerprintKey() != withoutTable {
		t.Fatal("a nil mandate table changed the fingerprint")
	}

	writePolicyTestFile(t, path, schema2ConstitutionTOML()+mandateTableTOML)
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusDrift || got.policy.Mandate != nil {
		t.Fatalf("the table was adopted without a version bump: %s %+v", got.status, got.policy.Mandate)
	}
	writePolicyTestFile(t, path, strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1)+mandateTableTOML)
	m.reload()
	got := m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive || got.policy.Mandate == nil || !got.policy.MandateActive() || *got.policy.Mandate.MaxLossPct != 30 || got.policy.Mandate.Sleeves == nil || *got.policy.Mandate.Sleeves.OwnerVolPct != 7 {
		t.Fatalf("the table was not adopted: %s %+v", got.status, got.policy.Mandate)
	}
	if got.policy.FingerprintKey() == withoutTable || got.policy.EffectiveFingerprintKey() == effectiveWithout {
		t.Fatal("writing the table left a fingerprint unchanged")
	}
	if keys := got.policy.UnapprovedKeys(); len(keys) != 0 {
		t.Fatalf("a complete table reads unapproved keys %v", keys)
	}
	if start, ok := got.policy.Mandate.MandateStart(); !ok || start != time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("start %v %v", start, ok)
	}
}

// A legacy revision cannot name the table, an unknown mandate key is
// refused, and a file carrying either keeps the last good policy in force.
func TestMandateTableRefusedByOlderSemanticsAndUnknownKeys(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, schema2ConstitutionTOML())
	good := m.snapshot().policy
	for name, body := range map[string]string{
		"legacy v3":        validRiskPolicyV3TOML() + mandateTableTOML,
		"unknown key":      strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + mandateTableTOML + "unknown_mandate_key = 1\n",
		"bad start":        strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + strings.Replace(mandateTableTOML, `"2026-10-14"`, `"14.10.2026"`, 1),
		"target above 100": strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + strings.Replace(mandateTableTOML, "vol_target_pct = 15.0", "vol_target_pct = 150.0", 1),
		// Allocations (2026-10-18): the machine's allocations sit inside the target.
		"machine above the target": strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + strings.Replace(mandateTableTOML, "core_vol_pct = 6.0", "core_vol_pct = 12.0", 1),
	} {
		t.Run(name, func(t *testing.T) {
			writePolicyTestFile(t, path, body)
			m.reload()
			got := m.snapshot()
			if got.status != rpc.RiskPolicyStatusError || got.policy != good {
				t.Fatalf("status %s message %q; last good policy kept: %v", got.status, got.message, got.policy == good)
			}
			switch name {
			case "legacy v3":
				if !strings.Contains(got.message, "schema 2 or legacy policy_version >= 4") {
					t.Fatalf("message %q", got.message)
				}
			case "unknown key":
				if !strings.Contains(got.message, "unknown risk policy key") || !strings.Contains(got.message, "unknown_mandate_key") {
					t.Fatalf("message %q", got.message)
				}
			case "bad start":
				if !strings.Contains(got.message, "started_at") {
					t.Fatalf("message %q", got.message)
				}
			case "target above 100":
				if !strings.Contains(got.message, "vol_target_pct") {
					t.Fatalf("message %q", got.message)
				}
			case "machine above the target":
				if !strings.Contains(got.message, "the machine's allocations sit inside the budget") {
					t.Fatalf("message %q", got.message)
				}
			}
		})
	}
}

// An incomplete table is active and names its unapproved keys; a schema-1
// file at revision 4 carries the same semantics and may name the table.
func TestMandateTableIncompleteAndLegacyV4(t *testing.T) {
	partial := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + "\n[mandate]\nmax_loss_pct = 30.0\n"
	m, _ := newTestRiskPolicyManager(t, partial)
	got := m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive || !got.policy.MandateActive() {
		t.Fatalf("partial table: %s %q", got.status, got.message)
	}
	if keys := got.policy.Mandate.UnapprovedKeys(); strings.Join(keys, " ") != "mandate.vol_target_pct mandate.tax_reserve_base mandate.started_at mandate.gap_shock_pct mandate.gap_room_pct" {
		t.Fatalf("unapproved %v", keys)
	}
	if all := got.policy.UnapprovedKeys(); !strings.Contains(strings.Join(all, " "), "mandate.started_at") {
		t.Fatalf("the constitution's unapproved list lacks the mandate keys: %v", all)
	}
	v4 := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 4", 1) + mandateTableTOML
	m, _ = newTestRiskPolicyManager(t, v4)
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusActive || got.policy.Mandate == nil {
		t.Fatalf("legacy v4 with the table: %s %q", got.status, got.message)
	}
}

// The ensure migration never writes the [mandate] table and leaves a
// written one byte for byte; the only mandate conversion is the two default
// tables of 2026-10-29 (mandate_defaults_policy_file_test.go).
func TestMandateTableSurvivesMigrationUnchanged(t *testing.T) {
	k := policyFileKind{name: PolicyFileConstitution, migrate: migrateConstitutionPolicyFileFrom(config.Trading{}, true)}
	complete := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + testOrderLimitsTOML + mandateTableTOML
	out, changes, _, err := previewPolicyFileMigration(k, []byte(complete), "test")
	if err != nil || !strings.Contains(string(out), mandateTableTOML) {
		t.Fatalf("a complete constitution with the table migrated: %v %v\n%s", changes, err, out)
	}
	for _, c := range changes {
		if !strings.HasPrefix(c, "write [mandate.iv_fallback].") && !strings.HasPrefix(c, "write [mandate.index_hedge].") && !strings.HasPrefix(c, "raise policy_version") {
			t.Fatalf("a complete constitution changed beyond the mandate defaults: %q", c)
		}
	}
	// A file that still lacks [order_limits] gains them and nothing else:
	// the mandate table is byte for byte what the owner wrote.
	lacking := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + mandateTableTOML
	out, changes, _, err = previewPolicyFileMigration(k, []byte(lacking), "test")
	if err != nil || len(changes) == 0 || !strings.Contains(string(out), mandateTableTOML) {
		t.Fatalf("migration touched the mandate table: %v %v\n%s", changes, err, out)
	}
	before, after := decodeTestConstitution(t, []byte(lacking)), decodeTestConstitution(t, out)
	if *before.Mandate.MaxLossPct != *after.Mandate.MaxLossPct || *before.Mandate.GapRoomPct != *after.Mandate.GapRoomPct || before.Mandate.StartedAt != after.Mandate.StartedAt {
		t.Fatalf("mandate numbers changed: %+v → %+v", before.Mandate, after.Mandate)
	}
	// The template shows every mandate key, commented out, and parses.
	template := string(ConstitutionPolicyTemplate("test"))
	for _, key := range []string{"# [mandate]", "# max_loss_pct = 0.0", "# gap_shock_pct = 25.0", "# gap_room_pct = 33.3", "# [mandate.sleeves]", "# overlay_vol_pct = 0.0"} {
		if !strings.Contains(template, key) {
			t.Fatalf("template lacks %q", key)
		}
	}
}

// index_gap_shock_pct (owner decision D4, 2026-10-14) is optional: a table
// without it keeps its fingerprint and every underlying on gap_shock_pct;
// written, it is adopted with a higher policy_version, changes the
// fingerprint, reaches the evaluator and is shown; out of range it is
// refused and the last good policy stays in force.
func TestMandateIndexGapShockKey(t *testing.T) {
	withTable := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + mandateTableTOML
	m, path := newTestRiskPolicyManager(t, withTable)
	without := m.snapshot().policy
	if without.Mandate == nil || without.Mandate.IndexGapShockPct != nil {
		t.Fatalf("fixture: %+v", without.Mandate)
	}
	if raw, _ := json.Marshal(without.Mandate); strings.Contains(string(raw), "index_gap_shock_pct") {
		t.Fatalf("an absent key enters the fingerprint projection: %s", raw)
	}
	if p := mandatePolicyNumbers(without); p.IndexGapShockPct != 0 || p.GapShockPctFor(true) != 25 {
		t.Fatalf("absent key: %+v", p)
	}
	written := strings.Replace(withTable, "policy_version = 6", "policy_version = 7", 1)
	written = strings.Replace(written, "gap_room_pct = 33.3\n", "gap_room_pct = 33.3\nindex_gap_shock_pct = 12.0\n", 1)
	writePolicyTestFile(t, path, written)
	m.reload()
	got := m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive || got.policy.Mandate.IndexGapShockPct == nil || *got.policy.Mandate.IndexGapShockPct != 12 {
		t.Fatalf("written key: %s %q %+v", got.status, got.message, got.policy.Mandate)
	}
	if got.policy.FingerprintKey() == without.FingerprintKey() {
		t.Fatal("writing index_gap_shock_pct left the fingerprint unchanged")
	}
	if p := mandatePolicyNumbers(got.policy); p.GapShockPctFor(true) != 12 || p.GapShockPctFor(false) != 25 {
		t.Fatalf("written key: %+v", p)
	}
	if v := mandatePolicyView(got.policy.Mandate); v.IndexGapShockPct == nil || *v.IndexGapShockPct != 12 || len(v.Unapproved) != 0 {
		t.Fatalf("view %+v", v)
	}
	good := got.policy
	writePolicyTestFile(t, path, strings.Replace(strings.Replace(written, "policy_version = 7", "policy_version = 8", 1), "index_gap_shock_pct = 12.0", "index_gap_shock_pct = 0.0", 1))
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusError || got.policy != good || !strings.Contains(got.message, "index_gap_shock_pct") {
		t.Fatalf("zero index gap: %s %q", got.status, got.message)
	}
	if !strings.Contains(string(ConstitutionPolicyTemplate("test")), "# index_gap_shock_pct = 12.0") {
		t.Fatal("the template lacks index_gap_shock_pct")
	}
}

const coreTableTOML = `
[mandate.core]
rule = "core-ibcf-v1"
isin = "IE00B3ZW0K18"
con_id = 9030
exchange = "IBIS"
currency = "EUR"
scale_pct = 25.0
band_pct = 10.0
min_order_base = 2500.0
max_value_pct = 40.0
`

// [mandate.core] is optional: a mandate without it keeps its fingerprint;
// written, it is adopted with a higher policy_version and changes the
// fingerprint; a value out of range or an unknown key refuses the file and
// keeps the last good policy; a half-written table names its keys to the
// core only.
func TestMandateCoreTable(t *testing.T) {
	withTable := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + mandateTableTOML
	m, path := newTestRiskPolicyManager(t, withTable)
	without := m.snapshot().policy
	if without.Mandate == nil || without.Mandate.Core != nil {
		t.Fatalf("fixture %+v", without.Mandate)
	}
	if raw, _ := json.Marshal(without.Mandate); strings.Contains(string(raw), `"core"`) {
		t.Fatalf("an absent table enters the fingerprint projection: %s", raw)
	}
	written := strings.Replace(withTable, "policy_version = 6", "policy_version = 7", 1) + coreTableTOML
	written = strings.Replace(written, "gap_room_pct = 33.3\n", "gap_room_pct = 33.3\nindex_gap_shock_pct = 12.0\n", 1)
	writePolicyTestFile(t, path, written)
	m.reload()
	got := m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive || got.policy.Mandate.Core == nil || *got.policy.Mandate.Core.ConID != 9030 || got.policy.Mandate.Core.Rule != "core-ibcf-v1" {
		t.Fatalf("written table: %s %q %+v", got.status, got.message, got.policy.Mandate)
	}
	if got.policy.FingerprintKey() == without.FingerprintKey() {
		t.Fatal("writing [mandate.core] left the fingerprint unchanged")
	}
	if keys := got.policy.Mandate.CoreUnapprovedKeys(); len(keys) != 0 {
		t.Fatalf("a complete table reads unwritten keys %v", keys)
	}
	good := got.policy
	version := 8
	for name, edit := range map[string][2]string{
		"scale above 100": {"scale_pct = 25.0", "scale_pct = 125.0"},
		"band zero":       {"band_pct = 10.0", "band_pct = 0.0"},
		"negative min":    {"min_order_base = 2500.0", "min_order_base = -1.0"},
		"cap above 100":   {"max_value_pct = 40.0", "max_value_pct = 140.0"},
		"bad contract":    {"con_id = 9030", "con_id = -4"},
		"bad isin":        {`isin = "IE00B3ZW0K18"`, `isin = "ie00b3zw0k18"`},
		"lower exchange":  {`exchange = "IBIS"`, `exchange = "ibis"`},
		"unknown key":     {"max_value_pct = 40.0", "max_value_pct = 40.0\nwindow_days = 5"},
	} {
		t.Run(name, func(t *testing.T) {
			version++
			body := strings.Replace(strings.Replace(written, "policy_version = 7", fmt.Sprintf("policy_version = %d", version), 1), edit[0], edit[1], 1)
			writePolicyTestFile(t, path, body)
			m.reload()
			if got := m.snapshot(); got.status != rpc.RiskPolicyStatusError || got.policy != good {
				t.Fatalf("status %s message %q; last good policy kept: %v", got.status, got.message, got.policy == good)
			}
		})
	}
	// con_id 0 lets the ISIN decide: accepted.
	version++
	writePolicyTestFile(t, path, strings.Replace(strings.Replace(written, "policy_version = 7", fmt.Sprintf("policy_version = %d", version), 1), "con_id = 9030", "con_id = 0", 1))
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusActive || *got.policy.Mandate.Core.ConID != 0 {
		t.Fatalf("con_id 0: %s %q", got.status, got.message)
	}
	// Scale 0 is the suspension the kill rule names, not an error.
	version++
	writePolicyTestFile(t, path, strings.Replace(strings.Replace(written, "policy_version = 7", fmt.Sprintf("policy_version = %d", version), 1), "scale_pct = 25.0", "scale_pct = 0.0", 1))
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusActive || *got.policy.Mandate.Core.ScalePct != 0 {
		t.Fatalf("scale 0: %s %q", got.status, got.message)
	}
	// A half-written table: the core names its keys; the constitution's
	// approval list does not carry them.
	version++
	writePolicyTestFile(t, path, strings.Replace(withTable, "policy_version = 6", fmt.Sprintf("policy_version = %d", version), 1)+"\n[mandate.core]\nrule = \"core-ibcf-v1\"\n")
	m.reload()
	got = m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive {
		t.Fatalf("half-written: %s %q", got.status, got.message)
	}
	if keys := strings.Join(got.policy.Mandate.CoreUnapprovedKeys(), " "); !strings.Contains(keys, "mandate.core.isin") || strings.Contains(keys, "mandate.core.rule") || !strings.Contains(keys, "mandate.index_gap_shock_pct") ||
		strings.Contains(keys, "mandate.core.con_id") || strings.Contains(keys, "mandate.core.exchange") {
		t.Fatalf("core keys %s", keys)
	}
	if all := strings.Join(got.policy.UnapprovedKeys(), " "); strings.Contains(all, "mandate.core") {
		t.Fatalf("core keys in the constitution's approval list: %s", all)
	}
	if !strings.Contains(string(ConstitutionPolicyTemplate("test")), "# [mandate.core]") {
		t.Fatal("the template lacks [mandate.core]")
	}
	// The table may name only the fund and its currency (owner decision
	// 2026-10-18): con_id and exchange are optional, and complete without.
	version++
	isinOnly := strings.Replace(strings.Replace(written, "con_id = 9030\n", "", 1), "exchange = \"IBIS\"\n", "", 1)
	writePolicyTestFile(t, path, strings.Replace(isinOnly, "policy_version = 7", fmt.Sprintf("policy_version = %d", version), 1))
	m.reload()
	got = m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive || got.policy.Mandate.Core.ConID != nil || got.policy.Mandate.Core.Exchange != "" || len(got.policy.Mandate.CoreUnapprovedKeys()) != 0 {
		t.Fatalf("isin and currency only: %s %q %+v", got.status, got.message, got.policy.Mandate.Core)
	}
}
