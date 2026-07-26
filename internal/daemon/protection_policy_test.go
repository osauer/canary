package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestProtectionPolicyDefaultDriftAndHigherVersion(t *testing.T) {
	now := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	pm := newProtectionPolicyManager("", true, time.Second, func() time.Time { return now })
	pm.reload()
	_, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusDefault {
		t.Fatalf("default status=%q, want default", st.Status)
	}

	path := filepath.Join(t.TempDir(), "policy.toml")
	pm.path = path
	writePolicy(t, path, 1, 6)
	pm.reload()
	_, st = pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusDrift {
		t.Fatalf("same-version changed policy status=%q, want drift", st.Status)
	}

	writePolicy(t, path, 2, 6)
	pm.reload()
	p, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusActive || p.PolicyVersion != 2 {
		t.Fatalf("higher version status=%q version=%d, want active v2", st.Status, p.PolicyVersion)
	}
}

func TestProtectionPolicyInvalidHigherVersionBlocksWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.toml")
	writePolicy(t, path, 2, -1)
	pm := newProtectionPolicyManager(path, true, time.Second, time.Now)
	pm.reload()
	_, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusError {
		t.Fatalf("invalid policy status=%q, want error", st.Status)
	}
	if len(st.Blockers) == 0 {
		t.Fatal("invalid policy should expose blockers")
	}
}

func TestProtectionPolicyTrailingStopDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	if !policy.Buckets.TrailingStop.Enabled || !policy.Buckets.TrailingStop.StockETF.Enabled {
		t.Fatalf("stock/ETF trailing defaults disabled: %+v", policy.Buckets.TrailingStop)
	}
	if policy.Buckets.TrailingStop.Options.Enabled {
		t.Fatal("option trailing stop default enabled, want opt-in")
	}
	if raw := string(mustMarshalJSON(t, policy.Authority)); strings.Contains(raw, "paper_only") {
		t.Fatalf("authority JSON still exposes paper_only: %s", raw)
	}
	if err := validateProtectionPolicy(policy); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}

	policy.Buckets.TrailingStop.StockETF.DefaultPct = 20
	if err := validateProtectionPolicy(policy); err == nil || !strings.Contains(err.Error(), "trailing_stop.stock_etf") {
		t.Fatalf("invalid stock trail bounds err=%v, want trailing_stop.stock_etf", err)
	}
}

func TestProtectionPolicyTrailingStopTIF(t *testing.T) {
	t.Parallel()
	def := defaultProtectionPolicy()
	if def.Buckets.TrailingStop.TIF != "" || def.Buckets.TrailingStop.effectiveTIF() != rpc.OrderTIFDay {
		t.Fatalf("default tif = %q (effective %q), want unset/DAY", def.Buckets.TrailingStop.TIF, def.Buckets.TrailingStop.effectiveTIF())
	}
	// omitempty keeps the unset value out of the fingerprint JSON, so
	// pre-tif policy files keep their fingerprints across the upgrade.
	if raw := string(mustMarshalJSON(t, def.Buckets)); strings.Contains(raw, `"tif"`) {
		t.Fatalf("unset tif leaks into fingerprint JSON: %s", raw)
	}

	gtc := def
	gtc.Buckets.TrailingStop.TIF = "gtc"
	if gtc.Buckets.TrailingStop.effectiveTIF() != rpc.OrderTIFGTC {
		t.Fatalf("effectiveTIF(gtc) = %q, want GTC", gtc.Buckets.TrailingStop.effectiveTIF())
	}
	if err := validateProtectionPolicy(gtc); err != nil {
		t.Fatalf("lowercase gtc policy invalid: %v", err)
	}
	if fingerprintProtectionPolicy(gtc).Key == fingerprintProtectionPolicy(def).Key {
		t.Fatal("setting tif must change the policy fingerprint")
	}

	bad := def
	bad.Buckets.TrailingStop.TIF = "IOC"
	if err := validateProtectionPolicy(bad); err == nil || !strings.Contains(err.Error(), "trailing_stop.tif") {
		t.Fatalf("invalid tif err = %v, want trailing_stop.tif", err)
	}
	bad.Buckets.TrailingStop.Enabled = false
	if err := validateProtectionPolicy(bad); err == nil || !strings.Contains(err.Error(), "trailing_stop.tif") {
		t.Fatalf("disabled-bucket invalid tif err = %v, want rejection at file-write time", err)
	}
}

func TestProtectionPolicyFileTIFGTCParses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "policy.toml")
	body := `kind = "ibkr.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = 2
profile = "theta-priority-mvp"

[authority]
close_reduce_only = true
auto_submit = false

[buckets.theta_hygiene]
enabled = true
max_dte = 21
min_abs_theta_per_day = 5.0
max_spread_pct_of_mid = 25.0

[buckets.risk_reduction]
enabled = true
single_name_target_pct_nlv = 25.0
max_order_notional = 10000.0

[buckets.trailing_stop]
enabled = true
tif = "GTC"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pm := newProtectionPolicyManager(path, false, time.Second, time.Now)
	pm.reload()
	p, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusActive {
		t.Fatalf("policy status = %q (%s), want active", st.Status, st.Message)
	}
	if p.Buckets.TrailingStop.effectiveTIF() != rpc.OrderTIFGTC {
		t.Fatalf("file tif effective = %q, want GTC", p.Buckets.TrailingStop.effectiveTIF())
	}
	// [buckets.trailing_stop] present without sub-tables: stock_etf and
	// options sub-policies must be backfilled from the embedded default.
	if !p.Buckets.TrailingStop.StockETF.Enabled {
		t.Fatalf("stock_etf defaults not backfilled: %+v", p.Buckets.TrailingStop)
	}
}

func writePolicy(t *testing.T, path string, version int, theta float64) {
	t.Helper()
	body := []byte(`kind = "ibkr.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = ` + strconv.Itoa(version) + `
profile = "theta-priority-mvp"

[authority]
close_reduce_only = true
auto_submit = false

[buckets.theta_hygiene]
enabled = true
max_dte = 21
min_abs_theta_per_day = ` + strconv.FormatFloat(theta, 'f', 1, 64) + `
max_spread_pct_of_mid = 25.0

[buckets.risk_reduction]
enabled = true
single_name_target_pct_nlv = 25.0
max_order_notional = 10000.0
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return raw
}
