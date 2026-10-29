package daemon

import (
	"bytes"
	"os"
	"path/filepath"
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
	k := policyFileKind{name: PolicyFileConstitution, migrate: migrateConstitutionPolicyFileFrom(config.Trading{}, true, true)}
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

// startWithConstitution runs daemon start's ensure step against a
// constitution under HOME, with the daemon's SQLite authority at db.
func startWithConstitution(t *testing.T, home, db string) string {
	t.Helper()
	t.Setenv("HOME", home)
	set := policyTestSet(t)
	core := openTestCoreStore(t, db)
	defer core.Close()
	s := &Server{cfg: &config.Resolved{
		Rulebook:      config.Rulebook{PolicyFile: set.Rulebook},
		AutoTrade:     config.AutoTrade{PolicyFile: set.Protection},
		Opportunities: config.Opportunities{PolicyFile: set.Opportunity},
	}, ensurePolicyFiles: true, version: "v9.9.9", logger: NewLogger(&bytes.Buffer{}, "error"), coreStore: core}
	s.ensurePolicyFilesOnStart()
	return filepath.Join(home, ".config", "ibkr", "policies", "risk-policy.toml")
}

// The hedge table's presence is the permission to place a hedge. An owner
// who wrote both default tables, and later deletes the hedge table to take
// that permission back, keeps it deleted: no start writes 25 again, though
// no Canary note ever reached the file. The same holds when the owner
// tidied away the note a Canary write left.
func TestADeletedHedgeTableStaysDeletedAcrossStarts(t *testing.T) {
	ownerTables := "\n[mandate.iv_fallback]\nshort_band_pct = 65.0\nmonth_band_pct = 15.0\nlong_band_pct = 8.0\nmeasured_pct = 5.0\nrange_pct = 20.0\n\n[mandate.index_hedge]\nmax_contracts = 4\n"
	for _, tc := range []struct {
		name  string
		start string
		hedge string
	}{
		{"owner wrote both", mandateDefaultsBase() + ownerTables, "[mandate.index_hedge]\nmax_contracts = 4\n"},
		{"canary wrote, note tidied away", mandateDefaultsBase(), "[mandate.index_hedge]\nmax_contracts = 25  # owner decision 2026-10-29 07:06 CET\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			db := filepath.Join(t.TempDir(), "state", "daemon.db")
			path := filepath.Join(home, ".config", "ibkr", "policies", "risk-policy.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.start), 0o600); err != nil {
				t.Fatal(err)
			}
			startWithConstitution(t, home, db)
			written := readPolicyTestFile(t, path)
			if !strings.Contains(string(written), tc.hedge) {
				t.Fatalf("first start: hedge table not as expected:\n%s", written)
			}
			var kept []string
			for line := range strings.SplitSeq(strings.Replace(string(written), tc.hedge, "", 1), "\n") {
				if !strings.Contains(line, mandateDefaultsNote) {
					kept = append(kept, line)
				}
			}
			deleted := strings.Join(kept, "\n")
			if err := os.WriteFile(path, []byte(deleted), 0o600); err != nil {
				t.Fatal(err)
			}
			// Two starts within one second would share a backup stamp and
			// fail on the second backup, passing for the wrong reason.
			backups, _ := filepath.Glob(path + ".bak-*")
			for _, b := range backups {
				if err := os.Remove(b); err != nil {
					t.Fatal(err)
				}
			}
			startWithConstitution(t, home, db)
			after := readPolicyTestFile(t, path)
			if c := decodeTestConstitution(t, after); c.Mandate.IndexHedge != nil {
				t.Fatalf("a start wrote the deleted hedge table again:\n%s", after)
			}
			if string(after) != deleted {
				t.Fatalf("a start changed the file after the deletion:\n%s", after)
			}
			// `canary policy ensure` has no daemon record: its plan never
			// proposes the defaults, so --apply-plan cannot write them back.
			for _, a := range EnsurePolicyFiles(PolicyFileSet{Constitution: path}, EnsureOptions{DryRun: true, Release: "v9.9.9"}) {
				if strings.Contains(strings.Join(a.Changes, "\n"), "[mandate.") {
					t.Fatalf("the CLI plan proposes the deleted table: %q", a.Changes)
				}
			}
		})
	}
}

// A hedge table the owner switched off by commenting it out, with a number
// of their own, is the owner's: a start neither activates it nor puts the
// proposal's 25 in its place. The template's proposal, commented with the
// proposal's values, is still activated in place.
func TestMigrationLeavesAnOwnerCommentedTable(t *testing.T) {
	in := mandateDefaultsBase() + "\n# off for now\n# [mandate.index_hedge]\n# max_contracts = 4\n"
	out, _ := migrateMandateDefaultsForTest(t, in)
	c := decodeTestConstitution(t, []byte(out))
	if c.Mandate.IndexHedge != nil || !strings.Contains(out, "# [mandate.index_hedge]\n# max_contracts = 4\n") {
		t.Fatalf("the owner's commented table was activated or rewritten:\n%s", out)
	}
	if c.Mandate.IVFallback == nil {
		t.Fatalf("the missing fallback table was not written:\n%s", out)
	}
}
