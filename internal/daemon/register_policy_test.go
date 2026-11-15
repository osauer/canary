package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/register"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The strategy register (internal-docs/design/strategy-register.md),
// witnessed on synthetic policy files.

const registerCoreTOML = `
[mandate.core]
rule = "core-ibcf-v1"
isin = "IE00B3ZW0K18"
currency = "EUR"
scale_pct = 25.0
band_pct = 10.0
min_order_base = 2500.0
max_value_pct = 40.0
`

// registerFile is a schema-2 constitution with [mandate], the core's first
// form and extra, at the given policy version.
func registerFile(version, extra string) string {
	return strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = "+version, 1) + strings.Replace(mandateTableTOML, "gap_room_pct = 33.3\n", "gap_room_pct = 33.3\nindex_gap_shock_pct = 12.0\n", 1) + registerCoreTOML + extra
}

// A file written before the register keeps both fingerprints byte for byte:
// the core's new keys and the [mandate.strategies] map are omitted while
// absent. The core's first form reads as one entry in shadow, as before the
// register; writing a stage or a spec hash is a revision.
func TestRegisterKeepsOldFilesAndTheirFingerprints(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, registerFile("6", ""))
	got := m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive {
		t.Fatalf("status %s %q", got.status, got.message)
	}
	// Pinned from this file on main e33ebeb1, before the register existed.
	const registerOldFileFingerprint = "sha256:1cfd5edfdd7cb162df58fe12e29540f1818dbf27618707808667677ff84183d9"
	const registerOldFileEffectiveFingerprint = "sha256:982f6021f3d12ef9ac23373f896679e69a53d2f4821f0fc5442dd2d3a2b7ff7b"
	fp := got.policy.FingerprintKey()
	if fp != registerOldFileFingerprint {
		t.Fatalf("fingerprint %s, want the pre-register %s", fp, registerOldFileFingerprint)
	}
	if efp := got.policy.EffectiveFingerprintKey(); efp != registerOldFileEffectiveFingerprint {
		t.Fatalf("effective fingerprint %s, want the pre-register %s", efp, registerOldFileEffectiveFingerprint)
	}
	entries := got.policy.Mandate.Register()
	if len(entries) != 1 || entries[0].ID != risk.CoreStrategyID || entries[0].Stage != "" || entries[0].SpecHash != "" || len(entries[0].Unwritten) != 0 {
		t.Fatalf("register %+v", entries)
	}
	resolved := register.Resolve(entries, register.Catalogue())
	if resolved[0].State != register.StateShadow || resolved[0].Reserves() || len(resolved[0].Reasons) != 0 {
		t.Fatalf("the core's first form resolves to %+v, want shadow reserving nothing", resolved[0])
	}
	writePolicyTestFile(t, path, registerFile("7", "spec_hash = \""+core.SpecHash+"\"\nstage = \"shadow\"\n"))
	m.reload()
	cited := m.snapshot()
	if cited.status != rpc.RiskPolicyStatusActive || cited.policy.FingerprintKey() == fp || cited.policy.Mandate.Core.Stage != risk.StageShadow {
		t.Fatalf("writing the core's stage and hash: %s %q, fingerprint moved %v", cited.status, cited.message, cited.policy.FingerprintKey() != fp)
	}
}

// What contradicts itself refuses the whole file and keeps the last good
// policy; a missing key never does.
func TestRegisterRefusesContradictions(t *testing.T) {
	entry := func(id, body string) string { return "\n[mandate.strategies." + id + "]\n" + body }
	hash := strings.Repeat("ab", 32)
	full := func(sleeve string) string {
		return `rule = "synthetic-v1"` + "\nspec_hash = \"" + hash + "\"\nsleeve = \"" + sleeve + "\"\nrisk_allocation_pct = 2.0\nstage = \"shadow\"\nscale_pct = 25.0\n"
	}
	for name, tc := range map[string]struct{ extra, want string }{
		"core in the strategies map":                  {entry("core", full("s9")), "the core has one form"},
		"two entries, one sleeve":                     {entry("s8", full("s9")) + entry("s9", strings.Replace(full("s9"), "synthetic-v1", "synthetic-v2", 1)), "one strategy per sleeve"},
		"two entries, one rule":                       {entry("s8", full("s8")) + entry("s9", full("s9")), "one entry per rule"},
		"the core's rule twice":                       {entry("s9", strings.Replace(full("s9"), "synthetic-v1", core.RuleID, 1)), "one entry per rule"},
		"a reserved sleeve":                           {entry("s9", full("owner")), "is reserved"},
		"the overlay's own key":                       {entry("s9", full("overlay")), "overlay's allocation is mandate.sleeves.overlay_vol_pct"},
		"a scale off the ladder":                      {entry("s9", strings.Replace(full("s9"), "scale_pct = 25.0", "scale_pct = 30.0", 1)), "25, 50 or 100"},
		"an unknown stage":                            {entry("s9", strings.Replace(full("s9"), `stage = "shadow"`, `stage = "paper"`, 1)), "shadow, live or suspended"},
		"a short spec hash":                           {entry("s9", strings.Replace(full("s9"), hash, "abc", 1)), "64-character"},
		"allocations over the target":                 {entry("s9", strings.Replace(full("s9"), "risk_allocation_pct = 2.0", "risk_allocation_pct = 6.0", 1)), "the machine's allocations sit inside the budget"},
		"an unknown entry key":                        {entry("s9", full("s9")+"lookback_days = 10\n"), "unknown risk policy key"},
		"a short entry isin":                          {entry("s9", full("s9")+"isin = \"ABC\"\n"), "twelve upper-case letters and digits"},
		"a bad entry band":                            {entry("s9", full("s9")+"band_pct = 100.0\n"), "band_pct must be in (0, 100)"},
		"a kill bound of zero":                        {entry("s9", full("s9")+"kill_drawdown_k = 0.0\n"), "kill_drawdown_k must be positive"},
		"a live kill bound that cannot fire":          {entry("s9", strings.Replace(full("s9"), `stage = "shadow"`, `stage = "live"`, 1)+"kill_drawdown_k = 1.0\n"), "cannot fire before the sleeve is gone"},
		"the core's scale off the ladder once staged": {"stage = \"shadow\"\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			m, path := newTestRiskPolicyManager(t, registerFile("6", ""))
			good := m.snapshot().policy
			body := registerFile("7", tc.extra)
			if tc.want == "" {
				// The core's first form at scale 25 is on the ladder; 30 is not.
				body = strings.Replace(registerFile("7", tc.extra), "scale_pct = 25.0", "scale_pct = 30.0", 1)
				tc.want = "once mandate.core.stage is written"
			}
			writePolicyTestFile(t, path, body)
			m.reload()
			got := m.snapshot()
			if got.status != rpc.RiskPolicyStatusError || got.policy != good || !strings.Contains(got.message, tc.want) {
				t.Fatalf("status %s message %q, want a refusal naming %q with the last good policy kept (%v)", got.status, got.message, tc.want, got.policy == good)
			}
		})
	}
}

// A half-written entry holds only itself, names every missing key, and never
// reaches the constitution's approval list; the overlay entry reads its
// allocation from [mandate.sleeves].
func TestRegisterMissingKeysHoldOnlyTheirStrategy(t *testing.T) {
	extra := "\n[mandate.strategies.s9]\nrule = \"synthetic-v1\"\nstage = \"live\"\n" +
		"\n[mandate.strategies.ov]\nrule = \"synthetic-v2\"\nspec_hash = \"" + strings.Repeat("cd", 32) + "\"\nsleeve = \"overlay\"\nstage = \"shadow\"\nscale_pct = 25.0\n"
	m, _ := newTestRiskPolicyManager(t, registerFile("6", extra))
	got := m.snapshot()
	if got.status != rpc.RiskPolicyStatusActive {
		t.Fatalf("status %s %q", got.status, got.message)
	}
	if keys := got.policy.UnapprovedKeys(); len(keys) != 0 {
		t.Fatalf("a half-written entry reached the approval list: %v", keys)
	}
	entries := got.policy.Mandate.Register()
	if len(entries) != 3 || entries[1].ID != "ov" || entries[2].ID != "s9" {
		t.Fatalf("register order %+v", entries)
	}
	want := []string{"mandate.strategies.s9.spec_hash", "mandate.strategies.s9.sleeve", "mandate.strategies.s9.scale_pct", "mandate.strategies.s9.risk_allocation_pct"}
	if !slices.Equal(entries[2].Unwritten, want) {
		t.Fatalf("s9 unwritten %v, want %v", entries[2].Unwritten, want)
	}
	if ov := entries[1]; ov.AllocationKey != "mandate.sleeves.overlay_vol_pct" || ov.AllocationPct == nil || *ov.AllocationPct != 4 || len(ov.Unwritten) != 0 {
		t.Fatalf("overlay entry %+v", ov)
	}
	for _, r := range register.Resolve(entries, register.Catalogue())[1:] {
		if r.State != register.StateHold || r.Reserves() {
			t.Fatalf("%s resolved to %+v, want a hold", r.Entry.ID, r)
		}
	}
}
