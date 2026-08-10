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
		{rpc.AlertPresentationPortfolioStress, "Portfolio stress", "Tap to see the measured drivers."},
		{rpc.AlertPresentationRegimeMarketStress, "Market stress", "Tap to see the contributing bands"},
		{rpc.AlertPresentationRulebookSingleNameExposure, "Single-name exposure", "Tap to see the measured value and cap."},
		{rpc.AlertPresentationRulebookOptionLinePremium, "Option-line exposure", "Tap to see the measured value and cap."},
		{rpc.AlertPresentationRulebookCatalystCoverage, "Catalyst evidence incomplete", "Tap to see which evidence is missing"},
		{rpc.AlertPresentationRulebookHedgeIntegrity, "Index-put sizing", "hedge or a directional short"},
		{rpc.AlertPresentationRulebookFXExposure, "Currency exposure", "Tap to see the measured value and cap."},
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
