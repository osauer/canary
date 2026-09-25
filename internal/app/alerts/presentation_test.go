package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestActionablePresentationsUseShortHumanCopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code  rpc.AlertPresentationCode
		title string
		body  string
	}{
		{rpc.AlertPresentationPortfolioStress, "Portfolio risk", "watch or action level"},
		{rpc.AlertPresentationRegimeMarketStress, "Market warning", "crossed a warning level"},
		{rpc.AlertPresentationRulebookSingleNameExposure, "Exposure to one underlying", "Rulebook concentration limit"},
		{rpc.AlertPresentationRulebookOptionLinePremium, "Premium at risk", "One option position"},
		{rpc.AlertPresentationRulebookCatalystCoverage, "Earnings timing", "expires before the next earnings announcement"},
		{rpc.AlertPresentationRulebookHedgeIntegrity, "Index protection size", "assigned to portfolio protection"},
		{rpc.AlertPresentationRulebookFXExposure, "Foreign-currency exposure", "above its Rulebook level"},
		{rpc.AlertPresentationRulebookExtrinsicBudget, "Option time value at risk", "time remaining in long options"},
		{rpc.AlertPresentationRiskPolicyDrawdownLatched, "Drawdown latch open", "has not confirmed"},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			t.Parallel()
			got, ok := PresentationFor(tt.code, rpc.AlertEpisodeOpen)
			if !ok || got.Title != tt.title || !strings.Contains(got.Body, tt.body) {
				t.Fatalf("presentation=%+v ok=%v, want title %q and body containing %q", got, ok, tt.title, tt.body)
			}
			if strings.Contains(got.Body, "Tap for") || strings.Contains(got.Body, "open Rules") || strings.Contains(got.Body, "open Stress") || strings.Contains(got.Body, "open Market") {
				t.Fatalf("presentation contains navigation boilerplate: %q", got.Body)
			}
		})
	}
}

func TestEscalatedPresentationDescribesCurrentResult(t *testing.T) {
	t.Parallel()
	got, ok := PresentationFor(rpc.AlertPresentationRegimeMarketStress, rpc.AlertEpisodeEscalated)
	if !ok {
		t.Fatal("escalated presentation missing")
	}
	if strings.Contains(got.Body, "Escalated:") {
		t.Fatalf("historical lifecycle leaked into current copy: %q", got.Body)
	}
}

// A deferred or held pre-authorised submission is still waiting: its notice
// copy names what it waits for and never reads as resolved.
func TestWaitingProtectionPresentationsReadAsPending(t *testing.T) {
	t.Parallel()
	for code, want := range map[rpc.AlertPresentationCode]string{
		rpc.AlertPresentationProtectionAutoDeferred:       "Deferred: trading frozen · resubmits when lifted",
		rpc.AlertPresentationProtectionAutoHeld:           "Held: hand order working",
		rpc.AlertPresentationProtectionAutoHeldUnverified: "Held: open orders unreadable",
	} {
		got, ok := PresentationFor(code, rpc.AlertEpisodeOpen)
		if !ok || !strings.HasPrefix(got.Body, want) || !strings.Contains(got.Body, "Open Protection to veto it.") {
			t.Fatalf("%s presentation = %+v ok=%v, want body starting %q", code, got, ok, want)
		}
		for _, resolved := range []string{"Resolved", "recovered", "placed the", "was placed"} {
			if strings.Contains(got.Title+" "+got.Body, resolved) {
				t.Fatalf("%s reads as resolved: %+v", code, got)
			}
		}
		episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceProtection, rpc.AlertKindProtectionAutomatic, "waiting")
		if err != nil {
			t.Fatal(err)
		}
		occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "sequence:1")
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
		if err := rpc.ValidateAlertCandidate(rpc.AlertCandidate{
			EpisodeKey: episode, OccurrenceKey: occurrence, EvidenceFingerprint: "sha256:" + strings.Repeat("c", 64),
			Source: rpc.AlertSourceProtection, Kind: rpc.AlertKindProtectionAutomatic, PresentationCode: code,
			State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityAct, EvidenceHealth: rpc.AlertEvidenceCurrent,
			Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
		}); err != nil {
			t.Fatalf("%s is not a valid Protection presentation code: %v", code, err)
		}
	}
}
