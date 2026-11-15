package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
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
		{rpc.AlertPresentationRulebookSingleNameExposure, "Worst-case loss on one issuer", "worst-case loss is above its Rulebook level"},
		{rpc.AlertPresentationRulebookDeltaSwing, "Delta swing on one issuer", "at its Rulebook watch level"},
		{rpc.AlertPresentationRulebookClusterStress, "Cluster falling together", "joint fall"},
		{rpc.AlertPresentationRulebookLossBudget, "Issuer loss against risk capital", "against your risk capital"},
		{rpc.AlertPresentationRulebookMarginHeadroom, "Margin headroom", "Excess liquidity is below its Rulebook level"},
		{rpc.AlertPresentationRulebookOptionLinePremium, "Premium at risk", "One option position"},
		{rpc.AlertPresentationRulebookCatalystCoverage, "Earnings timing", "expires before the next earnings announcement"},
		{rpc.AlertPresentationRulebookHedgeIntegrity, "Index protection size", "missing or outside its Rulebook sizing range"},
		{rpc.AlertPresentationRulebookCashSellOnly, "Premium budget", "option buys are held"},
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
		rpc.AlertPresentationProtectionAutoHeldMandate:    "Held: risk not measurable",
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

// The cash sweep's pre-authorised notice has its own copy (no "now"
// variant: a sweep row always waits the full window) and is a valid
// Protection presentation code.
func TestCashSweepAutomaticPresentation(t *testing.T) {
	t.Parallel()
	code := rpc.AlertPresentationProtectionAutoCashSweep
	got, ok := PresentationFor(code, rpc.AlertEpisodeOpen)
	if !ok || !strings.Contains(got.Title, "Cash sweep") || !strings.Contains(got.Body, "full veto window") || !strings.Contains(got.Body, "Open Protection to veto it.") {
		t.Fatalf("presentation = %+v ok=%v", got, ok)
	}
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceProtection, rpc.AlertKindProtectionAutomatic, "sweep")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "sequence:1")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	if err := rpc.ValidateAlertCandidate(rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: occurrence, EvidenceFingerprint: "sha256:" + strings.Repeat("d", 64),
		Source: rpc.AlertSourceProtection, Kind: rpc.AlertKindProtectionAutomatic, PresentationCode: code,
		State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityAct, EvidenceHealth: rpc.AlertEvidenceCurrent,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}); err != nil {
		t.Fatalf("not a valid Protection presentation code: %v", err)
	}
}

// The mandate's forced reduction has its own notice copy, with no "now"
// variant (every row waits the full window), and is a valid Protection code.
func TestMandateReductionAutomaticPresentation(t *testing.T) {
	t.Parallel()
	code := rpc.AlertPresentationProtectionAutoMandateReduction
	got, ok := PresentationFor(code, rpc.AlertEpisodeOpen)
	if !ok || !strings.Contains(got.Title, "risk budget") || !strings.Contains(got.Body, "full veto window") || !strings.Contains(got.Body, "Open Protection to veto it.") {
		t.Fatalf("presentation = %+v ok=%v", got, ok)
	}
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceProtection, rpc.AlertKindProtectionAutomatic, "mandate")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "sequence:1")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC)
	if err := rpc.ValidateAlertCandidate(rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: occurrence, EvidenceFingerprint: "sha256:" + strings.Repeat("e", 64),
		Source: rpc.AlertSourceProtection, Kind: rpc.AlertKindProtectionAutomatic, PresentationCode: code,
		State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityAct, EvidenceHealth: rpc.AlertEvidenceCurrent,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}); err != nil {
		t.Fatalf("not a valid Protection presentation code: %v", err)
	}
}
