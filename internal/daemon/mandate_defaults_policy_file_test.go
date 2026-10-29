package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
)

// mandateDefaultsBase is a complete current constitution with an active
// [mandate] and neither hedge nor fallback table.
func mandateDefaultsBase() string {
	return strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + testOrderLimitsTOML + mandateTableTOML
}

func migrateMandateDefaultsForTest(t *testing.T, in string) (string, []string) {
	t.Helper()
	k := policyFileKind{name: PolicyFileConstitution, migrate: migrateConstitutionPolicyFileFrom(config.Trading{}, true)}
	out, changes, _, err := previewPolicyFileMigration(k, []byte(in), "test")
	if err != nil {
		t.Fatalf("migration refused: %v", err)
	}
	return string(out), changes
}

// Owner decision 2026-10-29 07:06 CET: an install with an active mandate
// gains [mandate.iv_fallback] (65, 15, 8; 5, 20) and [mandate.index_hedge]
// max_contracts 25 at daemon start, once, with a higher policy_version, so
// an option without its own volatility no longer blinds the verdict and the
// Reduce hedge can be placed on the trader's request.
func TestMigrationWritesTheMandateDefaultsOnce(t *testing.T) {
	in := mandateDefaultsBase()
	out, changes := migrateMandateDefaultsForTest(t, in)
	c := decodeTestConstitution(t, []byte(out))
	if c.PolicyVersion != 7 {
		t.Fatalf("policy_version = %d, want 7; changes %q", c.PolicyVersion, changes)
	}
	f := c.Mandate.IVFallback
	if f == nil || f.ShortBandPct == nil || *f.ShortBandPct != 65 || *f.MonthBandPct != 15 || *f.LongBandPct != 8 || *f.MeasuredPct != 5 || *f.RangePct != 20 {
		t.Fatalf("iv_fallback = %+v", f)
	}
	if n, why := c.Mandate.IndexHedgeMaxContracts(); n != 25 || why != "" {
		t.Fatalf("index hedge max = %d %q", n, why)
	}
	before := decodeTestConstitution(t, []byte(in))
	if *before.Mandate.MaxLossPct != *c.Mandate.MaxLossPct || *before.Mandate.VolTargetPct != *c.Mandate.VolTargetPct || before.Mandate.StartedAt != c.Mandate.StartedAt {
		t.Fatalf("mandate numbers changed: %+v -> %+v", before.Mandate, c.Mandate)
	}
	if !strings.Contains(out, "max_contracts = 25  # owner decision 2026-10-29 07:06 CET") {
		t.Fatalf("the written key lacks its provenance:\n%s", out)
	}
	if again, changes := migrateMandateDefaultsForTest(t, out); again != out || len(changes) != 0 {
		t.Fatalf("a second start migrated again: %q", changes)
	}

	// A table the owner deletes after the write stays deleted.
	deleted := strings.Replace(out, "[mandate.index_hedge]\nmax_contracts = 25  # owner decision 2026-10-29 07:06 CET\n", "", 1)
	if deleted == out {
		t.Fatalf("fixture: the hedge table is not where expected:\n%s", out)
	}
	if again, changes := migrateMandateDefaultsForTest(t, deleted); again != deleted || len(changes) != 0 {
		t.Fatalf("a deleted table was written again: %q", changes)
	}
}

// A table the owner wrote is kept as written; only the missing one is added.
func TestMigrationKeepsAWrittenMandateTable(t *testing.T) {
	in := mandateDefaultsBase() + "\n[mandate.index_hedge]\nmax_contracts = 4\n"
	out, _ := migrateMandateDefaultsForTest(t, in)
	c := decodeTestConstitution(t, []byte(out))
	if n, _ := c.Mandate.IndexHedgeMaxContracts(); n != 4 || c.Mandate.IVFallback == nil || strings.Count(out, "[mandate.index_hedge]") != 1 {
		t.Fatalf("hedge max %d, fallback %+v:\n%s", n, c.Mandate.IVFallback, out)
	}
}

// Without an active [mandate] neither table means anything; nothing is
// written.
func TestMigrationWritesNoMandateDefaultsWithoutAMandate(t *testing.T) {
	in := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + testOrderLimitsTOML
	if out, changes := migrateMandateDefaultsForTest(t, in); out != in || len(changes) != 0 {
		t.Fatalf("a file without a mandate migrated: %q", changes)
	}
}

// A file that carries the tables commented out, as the template and the
// proposals of 2026-10-28 wrote them, has them uncommented in place: one
// header each, the explanation above it kept.
func TestMigrationUncommentsTheProposedMandateTables(t *testing.T) {
	in := mandateDefaultsBase() + `
# What an option without its own implied volatility may do to the verdict.
# [mandate.iv_fallback]
# short_band_pct = 65.0
# month_band_pct = 15.0
# long_band_pct = 8.0
# measured_pct = 5.0
# range_pct = 20.0

# The Reduce index hedge.
# [mandate.index_hedge]
# max_contracts = 25
`
	out, _ := migrateMandateDefaultsForTest(t, in)
	c := decodeTestConstitution(t, []byte(out))
	if n, _ := c.Mandate.IndexHedgeMaxContracts(); n != 25 || c.Mandate.IVFallback == nil || c.Mandate.IVFallback.RangePct == nil {
		t.Fatalf("tables not active:\n%s", out)
	}
	for _, s := range []string{"[mandate.iv_fallback]", "[mandate.index_hedge]", "short_band_pct", "max_contracts"} {
		if n := strings.Count(out, s); n != 1 {
			t.Fatalf("%q appears %d times:\n%s", s, n, out)
		}
	}
	if !strings.Contains(out, "# The Reduce index hedge.\n[mandate.index_hedge]\n") {
		t.Fatalf("the hedge table left its explanation:\n%s", out)
	}
}

// A file that lacks [order_limits] keys too gains everything with one
// policy_version raise.
func TestMigrationRaisesPolicyVersionOnceForBoth(t *testing.T) {
	in := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + mandateTableTOML
	out, _ := migrateMandateDefaultsForTest(t, in)
	if c := decodeTestConstitution(t, []byte(out)); c.PolicyVersion != 7 || len(c.OrderLimits.MissingKeys()) != 0 || c.Mandate.IndexHedge == nil {
		t.Fatalf("version %d:\n%s", c.PolicyVersion, out)
	}
}
