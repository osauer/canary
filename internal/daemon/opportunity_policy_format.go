package daemon

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Presence is checked before defaults: schema 1 preserves historical omissions;
// schema 2 requires the operator to see every setting of an enabled detector.
func opportunityPolicyPresence(p *opportunityPolicy, md toml.MetaData) error {
	for _, entry := range []struct{ key, meaning string }{
		{"profile", "legacy label only; selects no preset and has no effect on permission"},
		{"authority.exercise_reduce_only", "retired; code always requires close/reduce-only exercise"},
		{"authority.auto_submit", "automatic opportunity exercise is unsupported; use explicit confirmation"},
		{"buckets.option_exercise.allow_no_option_bid", "retired; the detector always requires an option bid"},
	} {
		if md.IsDefined(strings.Split(entry.key, ".")...) {
			p.diagnostics = append(p.diagnostics, rpc.PolicyDiagnostic{Key: entry.key, Feature: "option_exercise", Message: entry.meaning})
		}
	}
	keys := []string{"enabled", "min_total_gain", "min_gain_pct_intrinsic", "require_rth", "max_quote_age", "require_american_style"}
	if p.SchemaVersion == 2 {
		if !md.IsDefined("buckets", "option_exercise", "enabled") {
			return fmt.Errorf("buckets.option_exercise.enabled is required in schema 2; set false to disable the detector")
		}
		if !p.Buckets.OptionExercise.Enabled {
			return nil
		}
		for _, key := range keys {
			if !md.IsDefined("buckets", "option_exercise", key) {
				return fmt.Errorf("buckets.option_exercise.%s is required when option exercise is enabled in schema 2", key)
			}
		}
		if strings.TrimSpace(p.Buckets.OptionExercise.MaxQuoteAge) == "" {
			return fmt.Errorf("buckets.option_exercise.max_quote_age must be an explicit positive duration in schema 2")
		}
		return nil
	}
	if p.SchemaVersion > 1 {
		return nil // validator reports the unsupported format
	}
	if !md.IsDefined("buckets", "option_exercise") {
		p.diagnostics = append(p.diagnostics, rpc.PolicyDiagnostic{Key: "buckets.option_exercise", Feature: "option_exercise", Message: "legacy omission uses Canary's complete detector defaults; materialise these values before migrating"})
		return nil
	}
	for _, key := range keys {
		if !md.IsDefined("buckets", "option_exercise", key) {
			meaning := "legacy omission retains zero/false; migration must preserve that value rather than choose a new default"
			if key == "max_quote_age" {
				meaning = "legacy omission retains the 30s default; materialise it before migrating"
			}
			p.diagnostics = append(p.diagnostics, rpc.PolicyDiagnostic{Key: "buckets.option_exercise." + key, Feature: "option_exercise", Message: meaning})
		}
	}
	return nil
}
