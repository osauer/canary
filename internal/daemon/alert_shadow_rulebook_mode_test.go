package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAlertShadowRulebookEmitsOnlyAlertMode(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	result := rulebookModeTestResult(now)
	result.Rules[0].Mode = risk.RuleModeAlert
	result.Rules[0].Status = risk.RuleStatusWatch
	result.Rules[1].Mode = risk.RuleModeTrack
	result.Rules[1].Status = risk.RuleStatusAct
	result.Rules[2].Mode = risk.RuleModeOff
	result.Rules[2].Status = risk.RuleStatusWatch

	got := alertShadowMapRulebook(alertShadowBrokerScope{account: "redacted", mode: "paper"}, result, now)
	if !got.Covered || got.EvidenceHealth != rpc.AlertEvidenceCurrent {
		t.Fatalf("batch=%+v, want current covered Rulebook", got)
	}
	if len(got.Observations) != 1 || got.Observations[0].PresentationCode != rpc.AlertPresentationRulebookSingleNameExposure {
		t.Fatalf("observations=%+v, want only alert-mode watch", got.Observations)
	}
}

func TestAlertShadowRulebookRejectsUnknownMode(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	result := rulebookModeTestResult(now)
	result.Rules[0].Mode = "future"

	got := alertShadowMapRulebook(alertShadowBrokerScope{account: "redacted", mode: "paper"}, result, now)
	if got.Covered || got.EvidenceHealth != rpc.AlertEvidenceError {
		t.Fatalf("batch=%+v, want fail-closed invalid mode", got)
	}
}

func rulebookModeTestResult(now time.Time) rpc.RulesResult {
	result := rpc.RulesResult{
		Enabled: true,
		Status:  "ok",
		AsOf:    now,
		PolicyFingerprint: &rpc.Fingerprint{
			Version: rpc.RulebookPolicyFingerprintVersion,
			Key:     "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	for _, source := range alertShadowCanonicalRulebookHealth {
		result.InputHealth = append(result.InputHealth, rpc.SourceHealth{Source: source, Status: rpc.SourceStatusOK, AsOf: now})
	}
	for _, canonical := range alertShadowCanonicalRulebookRows {
		result.Rules = append(result.Rules, risk.RuleRow{
			ID: canonical.ID, Number: canonical.Number, Mode: risk.RuleModeAlert, Status: risk.RuleStatusPass,
		})
	}
	return result
}
