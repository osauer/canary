package cli

import (
	"encoding/json"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestSettingsPatchFromAssignmentBuildsNestedPatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		assignment string
		feature    string
		want       any
	}{
		{"features.stock_protection.enabled=null", "stock_protection", nil},
	} {
		raw, err := settingsPatchFromAssignment(tc.assignment)
		if err != nil {
			t.Fatalf("settingsPatchFromAssignment(%q): %v", tc.assignment, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal patch: %v", err)
		}
		features := got["features"].(map[string]any)
		feature := features[tc.feature].(map[string]any)
		if feature["enabled"] != tc.want {
			t.Fatalf("%s enabled = %#v, want %#v", tc.feature, feature["enabled"], tc.want)
		}
	}
}

func TestSettingsPatchFromAssignmentRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	if _, err := settingsPatchFromAssignment("trading.mode=paper"); err == nil {
		t.Fatal("unsupported read-only key succeeded")
	}
}

func TestSettingsSubcommandIndexHandlesHoistedFlags(t *testing.T) {
	t.Parallel()
	if got := settingsSubcommandIndex([]string{"--json", "show"}); got != 1 {
		t.Fatalf("settingsSubcommandIndex(--json show) = %d, want 1", got)
	}
	if got := settingsSubcommandIndex([]string{"--json", "set", "features.stock_protection.enabled=true"}); got != 1 {
		t.Fatalf("settingsSubcommandIndex(--json set ...) = %d, want 1", got)
	}
}

func TestSettingsPatchFromAssignmentTradingFreeze(t *testing.T) {
	t.Parallel()
	raw, err := settingsPatchFromAssignment("trading.freeze=true")
	if err != nil {
		t.Fatalf("settingsPatchFromAssignment(trading.freeze=true): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	trading := got["trading"].(map[string]any)
	if trading["freeze"] != true {
		t.Fatalf("trading.freeze = %#v, want true", trading["freeze"])
	}
	if _, err := settingsPatchFromAssignment("trading.freeze=2"); err == nil {
		t.Fatal("non-boolean trading.freeze accepted")
	}
}

func TestSettingsPatchFromAssignmentRulebook(t *testing.T) {
	t.Parallel()
	rulebookPatch := func(t *testing.T, assignment string) map[string]any {
		t.Helper()
		raw, err := settingsPatchFromAssignment(assignment)
		if err != nil {
			t.Fatalf("settingsPatchFromAssignment(%q): %v", assignment, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal patch: %v", err)
		}
		return got["features"].(map[string]any)["rulebook"].(map[string]any)
	}

	if rb := rulebookPatch(t, "features.rulebook.enabled=false"); rb["enabled"] != false {
		t.Fatalf("rulebook.enabled = %#v, want false", rb["enabled"])
	}
	if _, err := settingsPatchFromAssignment("features.rulebook.enabled=2"); err == nil {
		t.Fatal("non-boolean rulebook.enabled accepted")
	}

	// Per-symbol upsert: date strings pass through verbatim (the daemon owns
	// format validation) and the symbol segment is upper-cased.
	rb := rulebookPatch(t, "features.rulebook.earnings_overrides.now=2026-07-22Tamc")
	if got := rb["earnings_overrides"].(map[string]any)["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("override NOW = %#v, want date string under normalized key", got)
	}
	rb = rulebookPatch(t, "features.rulebook.earnings_overrides.NOW=null")
	if got, exists := rb["earnings_overrides"].(map[string]any)["NOW"]; !exists || got != nil {
		t.Fatalf("per-symbol null must serialize as explicit null, got %#v (exists=%v)", got, exists)
	}
	rb = rulebookPatch(t, "features.rulebook.earnings_overrides=null")
	if got, exists := rb["earnings_overrides"]; !exists || got != nil {
		t.Fatalf("bare-key null must clear all overrides, got %#v (exists=%v)", got, exists)
	}
	if _, err := settingsPatchFromAssignment("features.rulebook.earnings_overrides=2026-07-22"); err == nil {
		t.Fatal("bare earnings_overrides key must reject non-null values")
	}
}

// TestSettingsPatchCoversRegistry: every key in the shared settings registry
// must be settable through the CLI grammar (with a kind-appropriate value and
// with null) — the daemon-accepts-but-CLI-rejects drift class.
func TestSettingsPatchCoversRegistry(t *testing.T) {
	t.Parallel()
	for _, spec := range rpc.SettingsKeys() {
		key, value := spec.Key, "true"
		switch spec.Kind {
		case rpc.SettingsKindFloat:
			value = "1500.5"
		case rpc.SettingsKindInt:
			value = "3"
		case rpc.SettingsKindDateMap:
			key, value = spec.Key+".AAPL", "2026-08-04Tamc"
		}
		if _, err := settingsPatchFromAssignment(key + "=" + value); err != nil {
			t.Errorf("registry key %s rejected by CLI grammar: %v", key, err)
		}
		if _, err := settingsPatchFromAssignment(key + "=null"); err != nil {
			t.Errorf("registry key %s must accept null: %v", key, err)
		}
	}
}
