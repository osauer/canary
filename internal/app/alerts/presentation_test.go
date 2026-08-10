package alerts

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestActionablePresentationsNameTapDestination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code  rpc.AlertPresentationCode
		title string
		body  string
	}{
		{rpc.AlertPresentationPortfolioStress, "Portfolio risk", "Tap for the drivers and the response."},
		{rpc.AlertPresentationRegimeMarketStress, "Market warning", "confirmation status"},
		{rpc.AlertPresentationRulebookSingleNameExposure, "Exposure to one underlying", "Rulebook concentration limit"},
		{rpc.AlertPresentationRulebookOptionLinePremium, "Premium at risk", "One option position"},
		{rpc.AlertPresentationRulebookCatalystCoverage, "Earnings timing", "expires before the next earnings announcement"},
		{rpc.AlertPresentationRulebookHedgeIntegrity, "Index protection size", "assigned to portfolio protection"},
		{rpc.AlertPresentationRulebookFXExposure, "Foreign-currency exposure", "above its Rulebook level"},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			t.Parallel()
			got, ok := PresentationFor(tt.code, rpc.AlertEpisodeOpen)
			if !ok || got.Title != tt.title || !strings.Contains(got.Body, tt.body) {
				t.Fatalf("presentation=%+v ok=%v, want title %q and body containing %q", got, ok, tt.title, tt.body)
			}
			if strings.Contains(got.Body, "open Rules") || strings.Contains(got.Body, "open Stress") || strings.Contains(got.Body, "open Market") {
				t.Fatalf("presentation still contains dead-end navigation: %q", got.Body)
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
